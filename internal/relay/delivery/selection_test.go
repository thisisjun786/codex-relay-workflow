package delivery

import (
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
var programPath = regexp.MustCompile(`"(env -u CODEX_SESSION_RELAY_STATE )?(?:/\S*(?:/codex-session-relay|/crw)|'crw relay') `)
