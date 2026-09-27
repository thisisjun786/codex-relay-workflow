package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The store-selection refusals every delivery command prints before its handler (cli.py
// _selection_refusal): ambiguous, unidentified and wrong-socket, byte for byte with Python's
// once the program name each side prints for itself is replaced by one token.
func TestCLI_store_selection_refusals_match_python(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, xs string)
		state bool
	}{
		{"unidentified", func(t *testing.T, xs string) {
			dir := filepath.Join(xs, "codex-session-relay", "0123456789abcdef")
			mustDo(t, os.MkdirAll(dir, 0o700))
			py := exec.Command("uv", "run", "--no-sync", "python", "-c", "import sys;from codex_session_relay.store import Store;Store(sys.argv[1]).close()", filepath.Join(dir, "relay.sqlite3"))
			py.Dir = filepath.Join(root, "packages", "codex-session-relay")
			mustDo(t, py.Run())
		}, false},
		{"wrong socket", func(t *testing.T, xs string) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outs [2]string
			var codes [2]int
			sockets := t.TempDir()
			for i, python := range []bool{true, false} {
				home := t.TempDir()
				xs := filepath.Join(home, "xs")
				tc.setup(t, xs)
				env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xs, "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
				args := []string{"--socket", filepath.Join(sockets, "app.sock"), "claim", "--event", "e"}
				if tc.state {
					state := filepath.Join(home, "state")
					mustDo(t, os.MkdirAll(state, 0o700))
					py := exec.Command("uv", "run", "--no-sync", "python", "-c", "import sys;from codex_session_relay.store import Store;Store(sys.argv[1], socket_path=sys.argv[2]).close()", filepath.Join(state, "relay.sqlite3"), filepath.Join(sockets, "other.sock"))
					py.Dir = filepath.Join(root, "packages", "codex-session-relay")
					py.Env = env
					mustDo(t, py.Run())
					args = append([]string{"--state", state}, args...)
				}
				var cmd *exec.Cmd
				if python {
					cmd = exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
					cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
				} else {
					cmd = exec.Command(crwBinary(t), append([]string{"relay"}, args...)...)
				}
				cmd.Env = env
				out, err := cmd.Output()
				if e, ok := err.(*exec.ExitError); ok {
					codes[i] = e.ExitCode()
				}
				side := &cliSide{home: home}
				text := strings.ReplaceAll(side.normal(string(out)), sockets, "<sockets>")
				outs[i] = programPath.ReplaceAllString(text, `"$1<program> `)
			}
			if codes[0] != 2 || codes[1] != 2 || outs[0] != outs[1] {
				t.Fatalf("exit python %d go %d\npython:\n%s\ngo:\n%s", codes[0], codes[1], outs[0], outs[1])
			}
		})
	}
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
// bytes equal live Python when Python is invoked under that same alias path; the multi-call
// command names the actual crw executable and runs successfully when pasted into a shell.
func TestCLI_selection_recovery_program_parity_and_execution(t *testing.T) {
	root := repoRoot(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	sockets := filepath.Join(home, "sockets")
	mustDo(t, os.MkdirAll(state, 0o700))
	mustDo(t, os.MkdirAll(sockets, 0o700))
	recorded := filepath.Join(sockets, "recorded.sock")
	wanted := filepath.Join(sockets, "wanted.sock")
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
	seed := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", "import sys;from codex_session_relay.store import Store;Store(sys.argv[1], socket_path=sys.argv[2]).close()", filepath.Join(state, "relay.sqlite3"), recorded)
	seed.Dir = filepath.Join(root, "packages", "codex-session-relay")
	seed.Env = env
	mustDo(t, seed.Run())

	built := crwBinary(t)
	alias := filepath.Join(filepath.Dir(built), "codex-session-relay")
	mustDo(t, os.Symlink(built, alias))
	args := []string{"--state", state, "--socket", wanted, "claim", "--event", "e"}
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
	python := exec.Command(filepath.Join(root, ".venv", "bin", "python"), append([]string{"-c", pyScript, alias}, args...)...)
	python.Dir = filepath.Join(root, "packages", "codex-session-relay")
	python.Env = env
	want, wantErr := python.Output()
	if exitCode(wantErr) != 2 || string(got) != string(want) {
		t.Fatalf("alias recovery differs from Python\nGo exit=%d\n%s\nPython exit=%d\n%s", exitCode(gotErr), got, exitCode(wantErr), want)
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
