package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// jsonAccessRecorder runs testdata/json_access.py as it is - the installed Python console entry
// point against the built binary, case by case, each over a byte-identical SQLite seed - and
// keeps, for every case the Python oracle answered, the home it answered in (copied before the
// run), its argv, its forge environment, its answer and the store it left. tempfile's name
// sequence is seeded, so the home is the same path on every run under one TMPDIR.
const jsonAccessRecorder = `import base64, json, os, random, runpy, shutil, subprocess, sys, tempfile
names = tempfile._get_candidate_names()
names._rng, names._rng_pid = random.Random(0), os.getpid()
record, cases = sys.argv[1], sys.argv[2]
sys.argv = sys.argv[3:]
real_run = subprocess.run
calls = []
def run(command, *args, **kwargs):
    command = [str(part) for part in command]
    oracle = command[0].endswith("/.venv/bin/codex-session-relay") and command[3:] != ["route-reconcile"]
    if oracle:
        state = command[command.index("--state") + 1]
        home = os.path.dirname(state)
        case = os.path.join(cases, str(len(calls)))
        shutil.copytree(home, os.path.join(case, "home"), symlinks=True)
    done = real_run(command, *args, **kwargs)
    if oracle:
        shutil.copytree(state, os.path.join(case, "after"), symlinks=True)
        env = kwargs.get("env") or {}
        calls.append({"home": home, "argv": command[command.index("--state") + 2:],
                      "env": {k: v for k, v in env.items() if k.startswith("CRW_FORGE_")},
                      "code": done.returncode, "stdout64": base64.b64encode(done.stdout).decode(),
                      "stderr64": base64.b64encode(done.stderr).decode()})
    return done
subprocess.run = run
try:
    runpy.run_path(sys.argv[0], run_name="__main__")
finally:
    with open(record, "w") as handle:
        json.dump(calls, handle)
`

// jsonAccessCase is one case as the Python oracle answered it.
type jsonAccessCase struct {
	Home   string            `json:"home"`
	Argv   []string          `json:"argv"`
	Env    map[string]string `json:"env"`
	Code   int               `json:"code"`
	Out64  []byte            `json:"stdout64,omitempty"` // as the recorder wrote them
	Err64  []byte            `json:"stderr64,omitempty"`
	Stdout string            `json:"stdout"` // text, or "b64:" and base64 (encodeData)
	Stderr string            `json:"stderr"`
	Before treeImage         `json:"before"` // the home the oracle answered in
	After  []sqliteTable     `json:"after"`  // every table that held rows after it answered
}

var accessTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

// Test24JSONAccessBytes: every representative case of testdata/json_access.py (JSON accessor
// parity, the scripted forge and a SQLite seed per invocation), the built binary against the
// installed Python console entry point: exit, stdout (wall-clock fields normalized), stderr and
// every table after the command, including stored JSON text without parsing. Python's side of
// each case - the home it answered in, its answer and the store it left - is recorded (see
// pyoracle); the Go side runs live in the same home, rebuilt where Python did not run. Python ran
// against testdata/gh and Go runs against its Go twin (fakeGH).
func Test24JSONAccessBytes(t *testing.T) {
	root := repositoryRootPath()
	binary, _ := packageBinary(t)
	// The seeded home lies under this fixed directory: the markers name digests of its paths.
	tmp := fixedTree(t, t.Name())
	var cases []jsonAccessCase
	pyoracle.JSON(t, "json_access.py --representative", &cases, func() (any, error) {
		work := t.TempDir()
		record, caseDir := filepath.Join(work, "record.json"), filepath.Join(work, "cases")
		// CI runs the mutation-backed representatives, without race/short skips.
		// Run testdata/json_access.py without --representative for the full matrix.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "-c", jsonAccessRecorder, record, caseDir,
			"testdata/json_access.py", "--root", root, "--binary", binary, "--representative")
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("JSON accessor parity: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(record)
		if err != nil {
			return nil, err
		}
		var calls []jsonAccessCase
		if err = json.Unmarshal(raw, &calls); err != nil {
			return nil, err
		}
		for i := range calls {
			calls[i].Stdout, calls[i].Stderr, calls[i].Out64, calls[i].Err64 = encodeData(calls[i].Out64), encodeData(calls[i].Err64), nil, nil
			directory := filepath.Join(caseDir, fmt.Sprint(i))
			if calls[i].Before, err = snapshotTree(filepath.Join(directory, "home"), nil); err != nil {
				return nil, err
			}
			after, err := dumpSQLite(filepath.Join(directory, "after", "relay.sqlite3"))
			if err != nil {
				return nil, err
			}
			calls[i].After = after.Tables
		}
		return calls, nil
	}, append(placeholders(tmp), sameFixture)...)
	if len(cases) < 30 {
		t.Fatalf("only %d representative cases", len(cases))
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if err := testsupport.RemoveTempTree(c.Home); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := os.MkdirAll(c.Home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := materializeTree(c.Home, c.Before); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(c.Home, "state")
			testsupport.Rehome(t, filepath.Join(state, "relay.sqlite3"))
			var env []string
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "XDG_") && !strings.HasPrefix(entry, "CODEX") && !strings.HasPrefix(entry, "CRW_") {
					env = append(env, entry)
				}
			}
			env = append(env, "HOME="+c.Home, "XDG_STATE_HOME="+c.Home+"/xdg", "XDG_CONFIG_HOME="+c.Home+"/config", "XDG_CACHE_HOME="+c.Home+"/cache",
				"XDG_DATA_HOME="+c.Home+"/data", "CODEX_HOME="+c.Home+"/codex", goForgePath(t), "CRW_FORGE_SCENARIO=rich")
			for key, value := range c.Env {
				env = append(env, key+"="+value)
			}
			command := exec.Command(binary, append([]string{"relay", "--state", state}, c.Argv...)...)
			command.Env = env
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			code := 0
			if err := command.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			after, err := dumpSQLite(filepath.Join(state, "relay.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			pyStdout, err := decodeData(c.Stdout)
			if err != nil {
				t.Fatal(err)
			}
			pyStderr, err := decodeData(c.Stderr)
			if err != nil {
				t.Fatal(err)
			}
			goOut, pyOut := accessTimestamp.ReplaceAll(stdout.Bytes(), []byte("<time>")), accessTimestamp.ReplaceAll(pyStdout, []byte("<time>"))
			if code != c.Code || !bytes.Equal(goOut, pyOut) || !bytes.Equal(stderr.Bytes(), pyStderr) {
				t.Fatalf("%q: exit python=%d go=%d\npython stdout:\n%s\ngo stdout:\n%s\npython stderr: %q\ngo stderr: %q", c.Argv, c.Code, code, pyOut, goOut, pyStderr, stderr.Bytes())
			}
			if !reflect.DeepEqual(after.Tables, c.After) {
				got, _ := json.Marshal(after.Tables)
				want, _ := json.Marshal(c.After)
				t.Fatalf("%q: tables differ\npython: %s\ngo:     %s", c.Argv, want, got)
			}
		})
	}
}
