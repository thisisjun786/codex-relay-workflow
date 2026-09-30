package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The store-selection refusals every delivery command prints before its handler (cli.py
// _selection_refusal): ambiguous, unidentified and wrong-socket, byte for byte with Python's
// once the program name each side prints for itself is replaced by one token. Each runtime is
// asked on a store it owns: on the other runtime's, check_start refuses first (decision 31).
// Python's answer is recorded; the Go side's store is the one Python creates, recorded too
// (recordedPythonStore), in a fixed tree because it names its socket's path.
func TestCLI_store_selection_refusals_match_python(t *testing.T) {
	root := repoRoot(t)
	// createStore has Python create a store at path, recording socket as its socket when named.
	createStore := func(t *testing.T, path, socket string, env []string) {
		script := "import sys;from codex_session_relay.store import Store;Store(sys.argv[1]).close()"
		args := []string{path}
		if socket != "" {
			script = "import sys;from codex_session_relay.store import Store;Store(sys.argv[1], socket_path=sys.argv[2]).close()"
			args = append(args, socket)
		}
		py := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-c", script}, args...)...)
		py.Dir = filepath.Join(root, "packages", "codex-session-relay")
		py.Env = env
		mustDo(t, py.Run())
	}
	for _, tc := range []struct {
		name  string
		store func(xs string) string
		state bool
	}{
		{"unidentified", func(xs string) string {
			return filepath.Join(xs, "codex-session-relay", "0123456789abcdef", "relay.sqlite3")
		}, false},
		{"wrong socket", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outs [2]string
			var codes [2]int
			sockets := parityTree(t)
			for i, python := range []bool{true, false} {
				home := t.TempDir()
				if !python {
					home = parityTree(t)
				}
				xs := filepath.Join(home, "xs")
				env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xs, "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
				args := []string{"--socket", filepath.Join(sockets, "app.sock"), "claim", "--event", "e"}
				var path, socket string
				if tc.store != nil {
					path = tc.store(xs)
				}
				if tc.state {
					state := filepath.Join(home, "state")
					path, socket = filepath.Join(state, "relay.sqlite3"), filepath.Join(sockets, "other.sock")
					args = append([]string{"--state", state}, args...)
				}
				setup := func() {
					if path != "" {
						mustDo(t, os.MkdirAll(filepath.Dir(path), 0o700))
						createStore(t, path, socket, env)
					}
				}
				var out string
				if python {
					answer := pyAnswer(t, "refusal", func() ([]byte, error) {
						setup()
						cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
						cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
						cmd.Env = env
						out, err := cmd.Output()
						return []byte(fmt.Sprintf("%d\n%s", exitCode(err), liveNeutral(string(out)))), nil
					}, pyoracle.Substitute(home, "<home>"), pyoracle.Substitute(sockets, "<sockets>"))
					code, text, _ := strings.Cut(string(answer), "\n")
					n, err := strconv.Atoi(code)
					mustDo(t, err)
					codes[i], out = n, text
				} else {
					if pyoracle.Live() {
						setup()
					}
					if path != "" {
						recordedPythonStore(t, "store", path)
					}
					if tc.state {
						testsupport.HandOver(t, path, "go")
					}
					cmd := exec.Command(crwBinary(t), append([]string{"relay"}, args...)...)
					cmd.Env = env
					raw, err := cmd.Output()
					if e, ok := err.(*exec.ExitError); ok {
						codes[i] = e.ExitCode()
					}
					out = string(raw)
				}
				side := &cliSide{home: home}
				text := strings.ReplaceAll(side.normal(out), sockets, "<sockets>")
				outs[i] = programPath.ReplaceAllString(text, `"$1<program> `)
			}
			if codes[0] != 2 || codes[1] != 2 || outs[0] != outs[1] {
				t.Fatalf("exit python %d go %d\npython:\n%s\ngo:\n%s", codes[0], codes[1], outs[0], outs[1])
			}
		})
	}
}

// recordedPythonStore is a store Python created at path (recordedStore), fenced as Python left
// it: its mirror is published again from the store's own stamp (testsupport.Rehome), because the
// recording neutralized the id the created mirror names.
func recordedPythonStore(t *testing.T, key, path string) {
	t.Helper()
	recordedStore(t, key, path)
	if err := os.Remove(filepath.Join(filepath.Dir(path), "takeover.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	testsupport.Rehome(t, path)
}

// programPath is the program a recovery line names: each side names its own (Python its console
// script path, `crw relay` the argparse prog the multi-call entry passes, cli.ExecuteAs).
var programPath = regexp.MustCompile(`"(env -u CODEX_SESSION_RELAY_STATE )?(?:/\S*(?:/codex-session-relay|/crw)(?: relay)?|'crw relay') `)

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}

// Both installed entry points produce recovery commands through the built binary. The alias
// bytes equal Python's (recorded) when Python is invoked under that same alias path; the multi-call
// command names the actual crw executable and runs successfully when pasted into a shell. Each
// runtime answers the store while it owns it: on the other runtime's, check_start refuses first.
func TestCLI_selection_recovery_program_parity_and_execution(t *testing.T) {
	root := repoRoot(t)
	home := parityTree(t)
	state := filepath.Join(home, "state")
	sockets := filepath.Join(home, "sockets")
	mustDo(t, os.MkdirAll(state, 0o700))
	mustDo(t, os.MkdirAll(sockets, 0o700))
	recorded := filepath.Join(sockets, "recorded.sock")
	wanted := filepath.Join(sockets, "wanted.sock")
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
	if pyoracle.Live() {
		seed := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", "import sys;from codex_session_relay.store import Store;Store(sys.argv[1], socket_path=sys.argv[2]).close()", filepath.Join(state, "relay.sqlite3"), recorded)
		seed.Dir = filepath.Join(root, "packages", "codex-session-relay")
		seed.Env = env
		mustDo(t, seed.Run())
	}
	// The store Python created, as recorded; it names the recorded socket's path: a fixed home.
	recordedPythonStore(t, "seed", filepath.Join(state, "relay.sqlite3"))

	built := crwBinary(t)
	alias := filepath.Join(filepath.Dir(built), "codex-session-relay")
	mustDo(t, os.Symlink(built, alias))
	args := []string{"--state", state, "--socket", wanted, "claim", "--event", "e"}
	db := filepath.Join(state, "relay.sqlite3")
	testsupport.HandOver(t, db, "go")
	goAlias := exec.Command(alias, args...)
	goAlias.Env = env
	got, gotErr := goAlias.Output()
	if exitCode(gotErr) != 2 {
		t.Fatalf("alias exit=%d: %s", exitCode(gotErr), got)
	}
	pyScript := `import sys
from codex_session_relay import cli
sys.argv=[sys.argv[1],*sys.argv[2:]]
raise SystemExit(cli.main())`
	answer := pyAnswer(t, "alias recovery", func() ([]byte, error) {
		testsupport.HandOver(t, db, "python")
		python := exec.Command(filepath.Join(root, ".venv", "bin", "python"), append([]string{"-c", pyScript, alias}, args...)...)
		python.Dir = filepath.Join(root, "packages", "codex-session-relay")
		python.Env = env
		want, wantErr := python.Output()
		testsupport.HandOver(t, db, "go")
		return []byte(fmt.Sprintf("%d\n%s", exitCode(wantErr), want)), nil
	}, pyoracle.Substitute(alias, "<alias>"), pyoracle.Substitute(home, "<home>"))
	code, want, _ := strings.Cut(string(answer), "\n")
	if code != "2" || string(got) != want {
		t.Fatalf("alias recovery differs from Python\nGo exit=%d\n%s\nPython exit=%s\n%s", exitCode(gotErr), got, code, want)
	}

	multi := exec.Command(built, append([]string{"relay"}, args...)...)
	multi.Env = env
	out, multiErr := multi.Output()
	if exitCode(multiErr) != 2 {
		t.Fatalf("crw relay exit=%d: %s", exitCode(multiErr), out)
	}
	var refusal struct {
		Recover []string `json:"recover"`
	}
	if err := json.Unmarshal(out, &refusal); err != nil || len(refusal.Recover) == 0 {
		t.Fatalf("recovery JSON %s: %v", out, err)
	}
	pasted := exec.Command("sh", "-c", refusal.Recover[0])
	pasted.Env = env
	if result, err := pasted.CombinedOutput(); err != nil {
		t.Fatalf("recovery command %q: %v\n%s", refusal.Recover[0], err, result)
	}
}
