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

func Test33ReviewD8(t *testing.T) {
	for _, name := range []string{"relative_xdg", "legacy"} {
		t.Run(name, func(t *testing.T) {
			home, f := prepareSelection(t, name)
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
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					err = HandleControl(ctx, conn, path)
				}
				done <- err
			}()
			command := exec.CommandContext(ctx, built, "hook")
			command.Env = f.EnvSlice()
			command.Stdin = bytes.NewReader(f.Stop)
			out, err := command.CombinedOutput()
			if err != nil || len(out) != 0 {
				t.Fatalf("%v %s", err, out)
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
			if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_answered" {
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
