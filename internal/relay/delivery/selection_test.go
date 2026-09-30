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
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The store-selection refusals every delivery command prints before its handler (cli.py
// _selection_refusal): ambiguous, unidentified and wrong-socket, checked against the golden byte
// for byte once the program name is replaced by one token. The goldens began as Python's answers.
// The command is asked on a store the runtime owns (on the other runtime's, check_start refuses
// first, decision 31): the store the Python package created, a fixture, published again and taken
// over by Go. The wrong-socket store names its socket's path, so the socket directory and the home
// are fixed trees (parityTree).
func TestCLI_store_selection_refusals_match_python(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		store   func(xs string) string
		state   bool
	}{
		{"unidentified", "selection-unidentified.sqlite3", func(xs string) string {
			return filepath.Join(xs, "codex-session-relay", "0123456789abcdef", "relay.sqlite3")
		}, false},
		{"wrong socket", "selection-wrong-socket.sqlite3", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sockets, home := parityTree(t), parityTree(t)
			xs := filepath.Join(home, "xs")
			env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xs, "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
			args := []string{"--socket", filepath.Join(sockets, "app.sock"), "claim", "--event", "e"}
			var path string
			if tc.store != nil {
				path = tc.store(xs)
			}
			if tc.state {
				state := filepath.Join(home, "state")
				path = filepath.Join(state, "relay.sqlite3")
				args = append([]string{"--state", state}, args...)
			}
			if path != "" {
				pythonCreatedStore(t, tc.fixture, path)
			}
			if tc.state {
				testsupport.HandOver(t, path, "go")
			}
			cmd := exec.Command(crwBinary(t), append([]string{"relay"}, args...)...)
			cmd.Env = env
			raw, err := cmd.Output()
			code := exitCode(err)
			side := &cliSide{home: home}
			text := strings.ReplaceAll(side.normal(string(raw)), sockets, "<sockets>")
			text = programPath.ReplaceAllString(text, `"$1<program> `)
			golden.Check(t, "refusal", []byte(fmt.Sprintf("%d\n%s", code, text)))
			if code != 2 {
				t.Fatalf("exit %d\n%s", code, text)
			}
		})
	}
}

// pythonCreatedStore writes the named fixture, a store the Python package created (Store(path)
// with or without a socket path), at path, fenced as Python left it: its mirror is published again
// from the store's own stamp (testsupport.Rehome), because the fixture's id was neutralized.
func pythonCreatedStore(t *testing.T, fixture, path string) {
	t.Helper()
	mustDo(t, os.MkdirAll(filepath.Dir(path), 0o700))
	mustDo(t, os.WriteFile(path, golden.Fixture(t, fixture), 0o600))
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
// bytes are checked against the golden, which began as Python's answer invoked under that same
// alias path; the multi-call command names the actual crw executable and runs successfully when
// pasted into a shell. The store is the one the Python package created (a fixture), taken over by
// Go: on the other runtime's store, check_start refuses first.
func TestCLI_selection_recovery_program_parity_and_execution(t *testing.T) {
	home := parityTree(t)
	state := filepath.Join(home, "state")
	sockets := filepath.Join(home, "sockets")
	mustDo(t, os.MkdirAll(state, 0o700))
	mustDo(t, os.MkdirAll(sockets, 0o700))
	wanted := filepath.Join(sockets, "wanted.sock")
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
	// The store Python created with <home>/sockets/recorded.sock as its socket: a fixed home.
	pythonCreatedStore(t, "selection-recovery.sqlite3", filepath.Join(state, "relay.sqlite3"))

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
	golden.Check(t, "alias recovery", []byte(fmt.Sprintf("%d\n%s", exitCode(gotErr), got)), golden.Substitute(alias, "<alias>"), golden.Substitute(home, "<home>"))

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
