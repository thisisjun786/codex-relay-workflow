package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

type reviewSelectionFixture struct {
	Env                               map[string]string
	State, Socket, Root, Program, Now string
	Stop                              json.RawMessage
}

func prepareSelection(t *testing.T, name string) (string, reviewSelectionFixture) {
	t.Helper()
	home := hookHome(t, 5)
	cmd := exec.Command(python(t), "testdata/review_selection.py", "prepare", home, name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f reviewSelectionFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"} {
		t.Setenv(key, f.Env[key])
	}
	// The in-process owner evaluates only under its own marker root (ownerPaths), configured as
	// the relay is: the root the fixture's hook settings name.
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", f.Root)
	return home, f
}
func Test33ReviewD6(t *testing.T) {
	for _, name := range []string{"wrong_socket", "ambiguous", "unidentified", "override"} {
		t.Run(name, func(t *testing.T) {
			home, f := prepareSelection(t, name)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := os.MkdirAll(f.State, 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(f.State, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					err = HandleControl(ctx, conn, f.State)
				}
				done <- err
			}()
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			stop, err := decodeObject(f.Stop)
			if err != nil {
				t.Fatal(err)
			}
			options := GuardOptions{Root: f.Root, Now: f.Now, Mode: Hold, SocketPath: f.Socket, Program: f.Program}
			got, err := RequestGuard(ctx, conn, stop, options)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			writeTest(t, filepath.Join(home, "actual.json"), []byte(evidence.Dumps(got, true, false, true)+"\n"))
			cmd := exec.Command(python(t), "testdata/review_selection.py", "compare", home)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			t.Logf("%s", out)
			// The in-process path uses this exact shared evaluator, not just the RPC mapping.
			options.DefaultDBPath = ownerFallback(f.State, f.Socket, f.Program)
			inprocess, err := evaluateOwner(ctx, stop, options)
			if err != nil || evidence.Dumps(inprocess, true, false, true) != evidence.Dumps(got, true, false, true) {
				t.Fatal(inprocess, err, got)
			}
			if name == "wrong_socket" {
				if err = listener.Close(); err != nil {
					t.Fatal(err)
				}
				serverDone, _ := fakeControl(t, home, func(c net.Conn) error {
					raw, err := io.ReadAll(c)
					if len(raw) != 0 {
						t.Errorf("in-process hook sent RPC: %s", raw)
					}
					return err
				})
				command := hookCommand(t, home, string(f.Stop))
				command.Env = f.EnvSlice()
				var stderr bytes.Buffer
				command.Stderr = &stderr
				out, err := command.Output()
				if err != nil || len(out) != 0 || stderr.Len() != 0 {
					t.Fatalf("%v %s %s", err, out, &stderr)
				}
				awaitHost(t, serverDone)
				rows := rowsAt(t, home)
				if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_refused" || rows[0]["held"] != false {
					t.Fatal(rows)
				}
			}
		})
	}
}
func (f reviewSelectionFixture) EnvSlice() []string {
	out := make([]string, 0, len(f.Env))
	for k, v := range f.Env {
		out = append(out, k+"="+v)
	}
	return out
}

// recordingConn keeps what the owner wrote back, so a test can read the frame the
// requester received beside the journal row it wrote from it.
type recordingConn struct {
	net.Conn
	written bytes.Buffer
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.written.Write(p)
	return c.Conn.Write(p)
}

func Test33ReviewD8(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
		lifted        bool
	}{{"relative_xdg", "relative_xdg", true}, {"legacy", "legacy", true}, {"legacy_live_state_guard", "legacy", false}} {
		t.Run(tc.name, func(t *testing.T) {
			home, f := prepareSelection(t, tc.fixture)
			// These selections are the default state roots of the fixture's own HOME, which
			// the live-state guard covers. With the guard lifted the in-process owner reads the
			// receipt there, as the todo-42 runbook's processes do (cutover.md); with the guard
			// in force the owner answers the refusal instead of closing without a word.
			if tc.lifted {
				t.Setenv("CRW_ALLOW_LIVE_STATE", "1")
			} else {
				t.Setenv("CRW_ALLOW_LIVE_STATE", "")
			}
			built := binary(t)
			t.Chdir(home)
			path, err := RoutingState(Object{{Key: "socketPath", Value: f.Socket}})
			if err != nil || path != f.State {
				t.Fatal(path, err, f.State)
			}
			if !filepath.IsAbs(path) {
				t.Fatal(path)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(path, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			var served *recordingConn
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					served = &recordingConn{Conn: conn}
					err = HandleControl(ctx, served, path)
				}
				done <- err
			}()
			command := exec.CommandContext(ctx, built, "hook")
			command.Env = f.EnvSlice()
			command.Stdin = bytes.NewReader(f.Stop)
			out, err := command.CombinedOutput()
			// The legacy store is readable and names no receipt for the ready turn, so the
			// owner holds the Stop, as Python's hold-mode guard-evaluate does for this fixture;
			// the relative XDG root selects no store and the Stop is released silently. A host
			// error record never holds the Stop.
			want := ""
			if tc.name == "legacy" {
				want = `{"decision": "block", "reason": "Readiness is declared but no receipt stands at the current head revision for this session and turn. Emit the receipt over the actual artifacts.", "continue": true}`
			}
			if err != nil || string(out) != want {
				t.Fatalf("%v %q", err, out)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			rows := rowsAt(t, home)
			if tc.lifted {
				if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_answered" {
					t.Fatal(rows)
				}
				return
			}
			// The refusal reaches the requester as the relay's host record, the answer the CLI
			// gives for the same refusal and control.py gives for a guard that raised. The row
			// is the adapters' error-record row (contract/fixtures/records, "an error record at
			// each exit code"): the outcome and reading name it, and detail stays null there.
			frame := "{\"error\": \"host\", \"detail\": \"store: live state requires CRW_ALLOW_LIVE_STATE=1\"}\n"
			if served == nil || served.written.String() != frame {
				t.Fatalf("owner answered %q, want %q", served.written.String(), frame)
			}
			if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_host_error" || rows[0]["stdoutReading"] != "said_an_error_record" ||
				rows[0]["exitCode"] != float64(3) || rows[0]["held"] != false || rows[0]["detail"] != nil {
				t.Fatal(rows)
			}
		})
	}
}

func Test33ReviewD11(t *testing.T) {
	home, f := prepareSelection(t, "clock")
	ctx := context.Background()
	db, err := fixtureStore(ctx, filepath.Join(home, "clock.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	stop, err := decodeObject(f.Stop)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Evaluate(ctx, stop, GuardOptions{Root: f.Root, Mode: Hold, DBPath: filepath.Join(home, "clock.sqlite3"), Clock: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(home, "actual.json"), []byte(evidence.Dumps(v, true, false, true)+"\n"))
	cmd := exec.Command(python(t), "testdata/review_selection.py", "clock", home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Logf("%s", out)
}
