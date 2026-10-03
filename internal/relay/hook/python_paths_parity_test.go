package hook

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A path the settings or the Stop payload name reaches the system as os.fsencode's bytes: a
// surrogate escape (U+DCFF) is the byte it stands for, and a lone surrogate nothing encodes
// (U+D800) is the ValueError Python's adapter catches. The answers are the golden, which began as
// Python's own functions' over the same str: the transcript scan (stopadapter.event_identity),
// the claim (stopadapter.claim_event) and the journal row (stopadapter.journal).
func TestAPathFromJSONReachesTheSystemAsPythonEncodesIt(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "j\xff") // a directory whose name is not UTF-8
	if err := os.MkdirAll(filepath.Join(journal, "20260930"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(journal, "20260930", strings.Repeat("a", 32)+".json"), []byte("{}\n"))
	writeTest(t, filepath.Join(journal, "transcript.jsonl"), nil)
	spelled := pyvalue.FSDecode(journal)    // the str Python holds for it, as JSON decodes "\udcff"
	unencodable := spelled + "\xed\xa0\x80" // ... followed by U+D800, which os.fsencode refuses
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}, {Key: "last_assistant_message", Value: "x"}}
	var reasons []any
	for _, path := range []string{spelled + "/transcript.jsonl", spelled + "/missing.jsonl", unencodable} {
		_, identity := EventIdentity(context.Background(), append(Object{}, stop...).Set("transcript_path", path))
		reasons = append(reasons, identity.Get("reason"))
	}
	config := Object{{Key: "journalRoot", Value: unencodable}}
	slot := Slot{"20260930", strings.Repeat("b", 32)}
	claim, _ := ClaimEvent(context.Background(), config, strings.Repeat("k", 64), Object{{Key: "answerItem", Value: "i"}}, stop, slot, "")
	row, _ := Journal(context.Background(), config, Object{{Key: "adapterOutcome", Value: "guard_answered"}}, Slot{"20260930", strings.Repeat("c", 32)})
	if row != "" {
		t.Errorf("journal row under %q: %q", unencodable, row)
	}
	// The answers, each character position a codec error counts through root spelled relative to
	// root's length (toRootPositions).
	answers := Object{{Key: "reasons", Value: reasons}, {Key: "claim", Value: claim}, {Key: "row", Value: nullable(row)}}
	golden.Check(t, "answers", toRootPositions([]byte(pyjson.Dumps(answers, pyjson.Options{Indent: 2})+"\n"), root), golden.Substitute(root, "<ROOT>"))
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(before, after, func(a, b os.DirEntry) bool { return a.Name() == b.Name() }) {
		t.Errorf("a root os.fsencode refuses was created: %v, then %v", before, after)
	}
}

// settingsOverride is the variable Python's configuration_path read.
const settingsOverride = "CRW_COMPLETION_HOOK_CONFIG"

// The settings path and the host ledger are the ones Python names: Path.home() when no Codex
// home is set (an empty HOME is the root, an unset one the passwd entry), and a relative path
// made absolute against the working directory the kernel names (os.path.abspath), not the $PWD
// spelling that reached it through a symbolic link: the paths are the golden, which began as the
// paths Python named. (The settings override the adapter read, CRW_COMPLETION_HOOK_CONFIG, is
// retired, decision 66.)
func TestTheSettingsPathAndHostLedgerAreThePathsPythonNames(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "real", "wd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias", "wd")
	unset := "(unset)"
	cases := []struct{ name, home, codexHome, override string }{
		{"an empty HOME", "", unset, ""},
		{"HOME unset", unset, unset, ""},
		{"a relative CODEX_HOME", filepath.Join(root, "home"), "codex", ""},
		{"a CODEX_HOME of two leading slashes", filepath.Join(root, "home"), "/" + filepath.Join(root, "codex"), ""},
		{"a CODEX_HOME of two leading slashes and a dot", filepath.Join(root, "home"), "/" + filepath.Join(root, ".", "codex") + "/./", ""},
		{"the root as CODEX_HOME", filepath.Join(root, "home"), "/", ""},
		{"a CODEX_HOME of two slashes alone", filepath.Join(root, "home"), "//", ""},
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	named := map[string][]string{}
	t.Chdir(alias)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for key, value := range map[string]string{"HOME": c.home, "CODEX_HOME": c.codexHome, settingsOverride: c.override} {
				if value != unset {
					t.Setenv(key, value)
					continue
				}
				t.Setenv(key, "") // restored after the test
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			settings, err := configurationPath("", nil)
			if err != nil {
				t.Errorf("settings: %v", err)
			}
			host, err := hostLedger()
			if err != nil {
				t.Errorf("host ledger: %v", err)
			}
			named[c.name] = []string{settings, host}
		})
	}
	// The goldens are read and written in the package directory.
	if err = os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	// The passwd entry's home, which an unset HOME names, is spelled <PASSWD_HOME>.
	goldenSubstitutions := []golden.Option{golden.Substitute(filepath.Join(root, "home"), "<ROOT_HOME>"), golden.Substitute(root, "<ROOT>")}
	if account, err := user.Current(); err == nil && account.HomeDir != "/" {
		goldenSubstitutions = append(goldenSubstitutions, golden.Substitute(account.HomeDir, "<PASSWD_HOME>"))
	}
	golden.CheckJSON(t, "paths", named, goldenSubstitutions...)
}

// The owner's control socket is dialled in the directory each source names. A dbPath is the
// settings' str, so a surrogate escape in it (U+DCFF) is the byte it stands for, as Python's
// socket_guard connects to os.fsencode(str(state / "control.sock")). A directory the environment
// selects is already the bytes it names: a literal ED B3 BF run in XDG_STATE_HOME stays those
// bytes, as the owner that listens there spells it, and is not re-encoded to the byte 0xff.
func TestTheControlSocketIsDialledWhereItsSourceNamesIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		config func(home string) (Object, string)
	}{
		{"a dbPath from the settings", func(home string) (Object, string) {
			state := filepath.Join(home, "s\xff")
			return Object{{Key: "dbPath", Value: pyvalue.FSDecode(state) + "/relay.sqlite3"}}, state
		}},
		{"a state directory from the environment", func(home string) (Object, string) {
			xdg := filepath.Join(home, "x\xed\xb3\xbf")
			t.Setenv("XDG_STATE_HOME", xdg)
			// Settings naming no socket are routed to the directory discovery scopes by the
			// default socket under CODEX_HOME (home here).
			scope, err := store.SocketScope(filepath.Join(home, "app-server-control", "app-server-control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			return nil, filepath.Join(xdg, "codex-session-relay", scope)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t, 5)
			t.Setenv("CODEX_HOME", home)
			t.Setenv("HOME", home)
			t.Setenv("CODEX_SESSION_RELAY_STATE", "")
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg"))
			pinned, state := c.config(home)
			config := Object{{Key: "configVersion", Value: int64(1)}, {Key: "mode", Value: "observe"}, {Key: "relayExecutable", Value: filepath.Join(home, "never-run")}, {Key: "markerRoot", Value: filepath.Join(home, "markers")}, {Key: "timeoutSeconds", Value: 5.0}, {Key: "journalRoot", Value: filepath.Join(home, "journal")}}
			writeTest(t, filepath.Join(home, ConfigName), []byte(pyjson.Dumps(append(config, pinned...), pyjson.Options{})))
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(state, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					defer conn.Close()
					if _, err = readFrame(conn); err == nil {
						_, err = io.WriteString(conn, `{"decision":"release","state":"unmanaged","hook_output":{}}`+"\n")
					}
				}
				done <- err
			}()
			var out bytes.Buffer
			if code := runAdapter(context.Background(), nil, pipeInput(t, []byte(`{"session_id":"s","turn_id":"t"}`)), &out, time.Now(), nil); code != 0 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s", code, &out)
			}
			if rows := rowsAt(t, home); len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_answered" {
				t.Fatalf("the hook did not reach the owner listening at %q: %v", state, rows)
			}
			awaitHost(t, done)
		})
	}
}
