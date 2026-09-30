package hook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func Test33ControlDefaultStorePython(t *testing.T) {
	for _, name := range []string{"default_receipted", "default_missing_receipt", "intent_pin", "request_pin"} {
		t.Run(name, func(t *testing.T) {
			// A fixed path: the receipt's revision and event ids digest the artifact paths.
			home := hookHomeAt(t, canonicalRoot(t), 5)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// control_default.py prepare lays out a real owner's receipt store and asks the Python
			// CLI; the fixture, its answer and the marker files it left are recorded
			// (pythonFixture). Its iterdump of the stores is dropped: this test compares the
			// stores before and after the Go owner itself.
			raw, _ := pythonFixture(t, "prepare", home, func() ([]byte, error) {
				return pythonScript(t, nil, nil, "testdata/control_default.py", "prepare", home, name)
			}, func() error { return textFiles(home) })
			stores := storesUnder(t, home)
			var fixture struct{ State, Decision, GuardState string }
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
			// control_default.py compare: the wire answer is the CLI's verdict, compact, and its
			// indented envelope is the CLI's stdout byte for byte; the Go owner published the
			// marker files the Python guard did and changed no store.
			expected, err := os.ReadFile(filepath.Join(home, "python.stdout"))
			if err != nil {
				t.Fatal(err)
			}
			verdict, err := Decode(expected)
			if err != nil {
				t.Fatal(err)
			}
			if want := evidence.Dumps(verdict, true, false, true) + "\n"; string(response) != want {
				t.Fatalf("wire %q, want %q", response, want)
			}
			wire, err := Decode(response)
			if err != nil || evidence.DumpsIndent(wire, 2, false, true)+"\n" != string(expected) {
				t.Fatalf("envelope %v:\n%s\nwant\n%s", err, evidence.DumpsIndent(wire, 2, false, true), expected)
			}
			pythonFiles, err := os.ReadFile(filepath.Join(home, "python.files.text.json"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := canonicalJSON(t, markerFiles(t, filepath.Join(home, "markers"))), canonicalJSON(t, pythonFiles); got != want {
				t.Fatalf("marker files\n go     %s\n python %s", got, want)
			}
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
				if len(rows) != 1 || rows[0]["guardState"] != fixture.GuardState || rows[0]["guardDecision"] != fixture.Decision {
					t.Fatal(rows)
				}
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

// textFiles rewrites control_default.py prepare's python.files.json, each marker file's bytes in
// hex, as python.files.text.json with each file's bytes as text, which a recording's
// substitutions reach, and drops its iterdump of the stores (stores.json).
func textFiles(home string) error {
	raw, err := os.ReadFile(filepath.Join(home, "python.files.json"))
	if err != nil {
		return err
	}
	var files map[string]struct {
		Bytes string `json:"bytes"`
		Mode  int    `json:"mode"`
	}
	if err = json.Unmarshal(raw, &files); err != nil {
		return err
	}
	out := map[string]map[string]any{}
	for name, file := range files {
		content, err := hex.DecodeString(file.Bytes)
		if err != nil {
			return err
		}
		if !utf8.Valid(content) {
			return fmt.Errorf("%s is not UTF-8", name)
		}
		out[name] = map[string]any{"text": string(content), "mode": file.Mode}
	}
	encoded, err := encodeJSON(out)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(home, "python.files.text.json"), encoded, 0o600); err != nil {
		return err
	}
	for _, name := range []string{"python.files.json", "stores.json"} {
		if err = os.Remove(filepath.Join(home, name)); err != nil {
			return err
		}
	}
	return nil
}
