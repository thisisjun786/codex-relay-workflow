package hook

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
)

// serveOnce serves one connection on state/control.sock with the owner's HandleControl, as the daemon does.
func serveOnce(t *testing.T, state string) {
	t.Helper()
	tmploctest.CheckSocket(t, socketRootOf(t, filepath.Join(state, "control.sock")), filepath.Join(state, "control.sock"))
	address, release, err := ControlAddress(filepath.Join(state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	release()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = HandleControl(ctx, conn, state)
	}()
}

func ownedState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(socketDir(t, "crwh"), "state") // control.sock lives in it (CRW-1393)
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return state
}

// CRW-1309: the owner answers the marker-root question with the root it resolves in its own environment.
func TestControlAnswersTheOwnersMarkerRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rx", "markers")
	t.Setenv(delivery.MarkerEnv, root)
	state := ownedState(t)
	serveOnce(t, state)
	got, err := AskOwnerMarkerRoot(context.Background(), state)
	if err != nil || got != root {
		t.Fatalf("asked the owner: %q %v, want %q", got, err, root)
	}
}

// No daemon on the socket: the error says the daemon did not answer, for the caller to tell it from a root.
func TestAskOwnerMarkerRootWithNoDaemonIsUnanswered(t *testing.T) {
	_, err := AskOwnerMarkerRoot(context.Background(), ownedState(t))
	if !errors.Is(err, ErrOwnerMarkerRootUnanswered) || !strings.Contains(err.Error(), "control.sock") {
		t.Fatalf("got %v, want the unanswered error naming control.sock", err)
	}
}

// A parameter on the question is no question the owner answers: nobody chooses the owner's root.
func TestControlRejectsAMarkerRootQuestionWithParameters(t *testing.T) {
	state := ownedState(t)
	serveOnce(t, state)
	conn, err := net.Dial("unix", filepath.Join(state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "{\"protocol\":1,\"method\":\"marker-root\",\"params\":{\"markerRoot\":\"/x\"}}\n"); err != nil {
		t.Fatal(err)
	}
	row, err := readFrame(conn)
	if err != nil || len(row) != 2 || row.Get("requestRejected") != true {
		t.Fatalf("got %v %v, want requestRejected", row, err)
	}
}

// A daemon that answers a root that is not absolute, or the host record, has named no root.
func TestAskOwnerMarkerRootRefusesAnAnswerThatIsNoAbsoluteRoot(t *testing.T) {
	for name, answer := range map[string]string{
		"relative":   `{"protocol":1,"markerRoot":"markers"}`,
		"host":       `{"error":"host","detail":"cannot resolve"}`,
		"rejected":   `{"protocol":1,"requestRejected":true}`,
		"no root":    `{"protocol":1}`,
		"wrong type": `{"protocol":1,"markerRoot":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			state := ownedState(t)
			tmploctest.CheckSocket(t, socketRootOf(t, filepath.Join(state, "control.sock")), filepath.Join(state, "control.sock"))
			address, release, err := ControlAddress(filepath.Join(state, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
			release()
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = readFrame(conn)
				_, _ = io.WriteString(conn, answer+"\n")
			}()
			got, err := AskOwnerMarkerRoot(context.Background(), state)
			if !errors.Is(err, ErrOwnerMarkerRootUnanswered) || got != "" {
				t.Fatalf("got %q %v, want the unanswered error", got, err)
			}
		})
	}
}
