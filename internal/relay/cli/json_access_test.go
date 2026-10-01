package cli_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// jsonAccessCase is one case of testdata/json_access.py as the Python oracle ran it: the home it
// ran in, as it found it, its argv and its forge environment.
type jsonAccessCase struct {
	Home   string            `json:"home"`
	Argv   []string          `json:"argv"`
	Env    map[string]string `json:"env"`
	Before treeImage         `json:"before"`
}

var accessTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

// Test24JSONAccessBytes: every representative case of testdata/json_access.py (JSON accessor
// parity, the scripted forge and a SQLite seed per invocation), through the built binary, in the
// home the case ran in (a fixture): exit, stdout (wall-clock fields normalized), stderr and every
// table after the command, including stored JSON text without parsing, against the goldens (the
// installed Python console entry point's answers, at first). The forge is fakeGH, the Go twin of
// the testdata/gh the Python oracle ran against.
func Test24JSONAccessBytes(t *testing.T) {
	binary, _ := packageBinary(t)
	// The seeded home lies under this fixed directory: the markers name digests of its paths.
	tmp := fixedTree(t, t.Name())
	// The cases (testdata/fixtures/json-access-cases.json.gz).
	var cases []jsonAccessCase
	fixtureJSON(t, "json-access-cases.json", &cases, [2]string{"<fixture>", tmp})
	if len(cases) < 30 {
		t.Fatalf("only %d representative cases", len(cases))
	}
	for i, c := range cases {
		var compared map[string]any
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
			goOut := accessTimestamp.ReplaceAll(stdout.Bytes(), []byte("<time>"))
			// Exit, stdout (wall-clock fields masked) and stderr as bytes, and every table
			// that holds rows after the command.
			compared = map[string]any{"code": code, "stdout": encodeData(goOut), "stderr": encodeData(stderr.Bytes()), "after": after.Tables}
		})
		if compared != nil {
			expectJSON(t, fmt.Sprintf("json_access.py --representative %d", i), compared, tmp)
		}
	}
}
