package manage

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// resumeSentClosingHost is a fake App Server that answers the handshake, thread/read,
// thread/resume and mcpServerStatus/list (every needed server disabled, nextCursor null), then
// closes the connection. The run's next step is turn/start, and on this socket that frame can only
// be withheld: the test seam probes the closed connection first, so the withheld turn/start is
// reached deterministically rather than by timing.
func resumeSentClosingHost(t *testing.T) (string, *resumeHostLog) {
	t.Helper()
	log := &resumeHostLog{}
	socket := fakehost.SocketPath(t, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &message) != nil || message.Method == "" {
				continue
			}
			log.add(message.Method)
			answer := func(result map[string]any) {
				frame, _ := json.Marshal(map[string]any{"id": message.ID, "result": result})
				_ = conn.Write(r.Context(), websocket.MessageText, frame)
			}
			switch message.Method {
			case "initialize":
				answer(map[string]any{"userAgent": "closing-host"})
			case "initialized":
			case "thread/read":
				answer(map[string]any{"thread": map[string]any{"model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "idle"}}})
			case "thread/resume":
				answer(map[string]any{"model": "m", "reasoningEffort": "xhigh"})
			case "mcpServerStatus/list":
				answer(map[string]any{"data": []any{map[string]any{"name": "alpha", "runtimeStatus": "disabled"}}, "nextCursor": nil})
				return
			default:
				frame, _ := json.Marshal(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": message.Method}})
				_ = conn.Write(r.Context(), websocket.MessageText, frame)
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	return socket, log
}

// resumeSentConfig is the configuration a run is driven with against a socket a test owns: the
// state the relay helper would resolve and the child_check section naming the server to stop.
func resumeSentConfig(t *testing.T, socket string) *Config {
	t.Helper()
	cfg := hostReadConfig(socket)
	cfg.Relay.State = "/nonexistent/crw-883-test-state" // only read to build an admit-turn line this run never reaches
	list, err := json.Marshal(map[string]any{"disabled_servers": []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.raw["child_check"] = list
	return cfg
}

// C1: a turn/start the client withheld because the connection had already ended was never sent, so
// no turn can have started and the refusal is an ordinary host_error. The fake host answers the
// three steps and closes; the seam then calls the same watch context until that call fails, which
// proves the connection is gone before turn/start runs.
func TestResumeSentWithheldTurnStartIsHostError(t *testing.T) {
	socket, log := resumeSentClosingHost(t)
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	resumeSentTurnStartSeam = func(ctx context.Context, client *appserver.Client) {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := client.Call(ctx, "mcpServerStatus/list", map[string]any{"threadId": "01child", "detail": "toolsAndAuthOnly", "limit": 500}); err != nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the fenced connection never failed, so turn/start was not withheld")
			}
		}
	}
	defer func() { resumeSentTurnStartSeam = nil }()

	_, err := resumeRun(context.Background(), e, resumeSentConfig(t, socket), resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != string(hostReadHostError) {
		t.Fatalf("err = %v, want host_error", err)
	}
	if code := resumeExit(failure); code != 3 {
		t.Errorf("host_error exits %d, want 3", code)
	}
	if !strings.HasPrefix(failure.Detail, "turn/start was not sent: ") || !strings.HasSuffix(failure.Detail, "; no turn was started") {
		t.Errorf("the refusal does not report an unsent turn/start: %q", failure.Detail)
	}
	if n := log.count("turn/start"); n != 0 {
		t.Errorf("the fake host received turn/start %d times, want 0", n)
	}
	if n := log.count("initialize"); n != 1 {
		t.Errorf("initialize ran %d times, want 1", n)
	}
}

// C2 contrast: a turn/start the host read before the answer was lost was sent, so the outcome is
// unknown and the refusal must stay turn_start_uncertain. This is the branch the withheld case
// must not take.
func TestResumeSentLostAnswerStaysUncertain(t *testing.T) {
	host := resumeHost(t, "idle")
	host.Respond("turn/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1011, Reason: "lost"}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeTurnStartUncertain {
		t.Fatalf("err = %v, want turn_start_uncertain", err)
	}
	if code := resumeExit(failure); code != 3 {
		t.Errorf("turn_start_uncertain exits %d, want 3", code)
	}
	if !strings.Contains(failure.Detail, "read the thread") {
		t.Errorf("the refusal does not tell the operator to read the thread: %q", failure.Detail)
	}
	if n := host.Count("turn/start"); n != 1 {
		t.Errorf("the fake host read turn/start %d times, want 1", n)
	}
}
