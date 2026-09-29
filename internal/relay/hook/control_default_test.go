package hook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func Test33ControlDefaultStorePython(t *testing.T) {
	for _, name := range []string{"default_receipted", "default_missing_receipt", "intent_pin", "request_pin"} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			prepare := exec.CommandContext(ctx, python(t), "testdata/control_default.py", "prepare", home, name)
			raw, err := prepare.CombinedOutput()
			if err != nil {
				t.Fatalf("Python CLI: %v\n%s", err, raw)
			}
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
			writeTest(t, filepath.Join(home, "go.response"), response)
			compare := exec.CommandContext(ctx, python(t), "testdata/control_default.py", "compare", home)
			out, err := compare.CombinedOutput()
			if err != nil {
				t.Fatalf("comparison: %v\n%s", err, out)
			}
			t.Logf("%s: %s", name, out)
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
