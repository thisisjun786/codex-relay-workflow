package hook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test33ControlDefaultStorePython(t *testing.T) {
	for _, name := range []string{"default_receipted", "default_missing_receipt", "intent_pin", "request_pin"} {
		t.Run(name, func(t *testing.T) {
			// A fixed path: the receipt's revision and event ids digest the artifact paths.
			home := hookHomeAt(t, canonicalRoot(t), 5)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// A real owner's receipt store and the request for it, as control_default.py prepare
			// laid them out (a fixture). This test compares the stores before and after the Go
			// owner itself.
			raw, _ := layFixture(t, "control-default-"+name, home)
			stores := storesUnder(t, home)
			var fixture struct{ State string }
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			// The owner evaluates only under its own marker root (ownerPaths), configured as
			// the relay is: the root the fixture's settings and request name.
			t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", filepath.Join(home, "markers"))
			listener, err := net.Listen("unix", filepath.Join(fixture.State, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			// The server is listening before the client starts; done is registered
			// before dispatch, so completion needs no polling or timing sleeps.
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					err = HandleControl(ctx, conn, fixture.State)
				}
				done <- err
			}()
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			deadline, _ := ctx.Deadline()
			if err = conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			request, err := os.ReadFile(filepath.Join(home, "request.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write(append(request, '\n')); err != nil {
				t.Fatal(err)
			}
			response, err := bufio.NewReader(conn).ReadBytes('\n')
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
			// As control_default.py compare read the Python CLI's: the wire answer is the verdict,
			// compact, and its indented envelope the CLI's stdout byte for byte; the Go owner
			// published the marker files and changed no store.
			wire, err := Decode(response)
			if err != nil {
				t.Fatalf("%v: %s", err, response)
			}
			if compact := evidence.Dumps(wire, true, false, true) + "\n"; string(response) != compact {
				t.Fatalf("wire %q, not compact %q", response, compact)
			}
			golden.Check(t, "wire", response, golden.Substitute(home, "<ROOT>"))
			golden.Check(t, "stdout", []byte(evidence.DumpsIndent(wire, 2, false, true)+"\n"), golden.Substitute(home, "<ROOT>"))
			goldenCanonical(t, "marker files", markerFiles(t, filepath.Join(home, "markers")), golden.Substitute(home, "<ROOT>"))
			if after := storesUnder(t, home); !reflect.DeepEqual(after, stores) {
				t.Fatal("the Go guard changed a store")
			}
			if name == "default_receipted" {
				// Exercise the real crw hook client too: its settings and intent pin
				// no DB, and no takeover record grants it the in-process fast path.
				// hookEnv routes to home/state; serve the same owner on that socket.
				serverDone, _ := fakeControl(t, home, func(c net.Conn) error { return HandleControl(ctx, c, fixture.State) })
				var envelope struct {
					Params struct{ StopInput json.RawMessage }
				}
				if err := json.Unmarshal(request, &envelope); err != nil {
					t.Fatal(err)
				}
				command := hookCommand(t, home, string(envelope.Params.StopInput))
				var stderr bytes.Buffer
				command.Stderr = &stderr
				stdout, err := command.Output()
				if err != nil || len(stdout) != 0 || stderr.Len() != 0 {
					t.Fatalf("hook %v stdout=%s stderr=%s", err, stdout, &stderr)
				}
				awaitHost(t, serverDone)
				rows := rowsAt(t, home)
				if len(rows) != 1 {
					t.Fatal(rows)
				}
				golden.CheckJSON(t, "hook row", map[string]any{"guardState": rows[0]["guardState"], "guardDecision": rows[0]["guardDecision"]})
			}
		})
	}
}

// markerFiles is control_default.py files(root) with each file's bytes as text: every file under
// root by its path relative to root, with its mode.
func markerFiles(t *testing.T, root string) []byte {
	t.Helper()
	files := map[string]map[string]any{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[rel] = map[string]any{"text": string(raw), "mode": int(info.Mode().Perm())}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := encodeJSON(files)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
