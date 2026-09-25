package fakehost_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// frame is what a client reads back: the App Server envelope, loosely.
type frame struct {
	ID     json.RawMessage    `json:"id"`
	Method string             `json:"method"`
	Result map[string]any     `json:"result"`
	Error  *fakehost.RPCError `json:"error"`
}

type client struct {
	t    *testing.T
	ctx  context.Context
	conn *websocket.Conn
}

// dial connects the way the bridge client will: WebSocket over the unix socket, uri
// ws://localhost/, compression off.
func dial(t *testing.T, server *fakehost.Server) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", server.SocketPath)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: transport},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadLimit(fakehost.MaxFrameBytes)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &client{t: t, ctx: ctx, conn: conn}
}

func (c *client) sendRaw(raw string) {
	c.t.Helper()
	if err := c.conn.Write(c.ctx, websocket.MessageText, []byte(raw)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) send(message map[string]any) {
	c.t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		c.t.Fatalf("encode: %v", err)
	}
	c.sendRaw(string(encoded))
}

func (c *client) read() frame {
	c.t.Helper()
	_, raw, err := c.conn.Read(c.ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var got frame
	if err := json.Unmarshal(raw, &got); err != nil {
		c.t.Fatalf("decode %s: %v", raw, err)
	}
	return got
}

// handshake is the Python bridge's own: initialize, then the initialized notification.
func (c *client) handshake() frame {
	c.t.Helper()
	c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo":   map[string]any{"name": "codex_thread_bridge", "version": "0.0.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}})
	answer := c.read()
	c.send(map[string]any{"method": "initialized", "params": map[string]any{}})
	return answer
}

func TestHandshake_isAccepted_whenClientSendsPythonInitializeThenInitialized(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	server.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	c := dial(t, server)

	// When
	answer := c.handshake()
	c.send(map[string]any{"id": 2, "method": "thread/goal/get", "params": map[string]any{"threadId": "t1"}})
	after := c.read()

	// Then
	if answer.Error != nil || answer.Result["userAgent"] != fakehost.UserAgent {
		t.Fatalf("initialize answer = %+v", answer)
	}
	if string(after.ID) != "2" || after.Error != nil {
		t.Fatalf("request after handshake = %+v", after)
	}
	requests := server.Requests()
	var params struct {
		ClientInfo   struct{ Name string } `json:"clientInfo"`
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(requests[0].Params, &params); err != nil {
		t.Fatalf("initialize params: %v", err)
	}
	if len(requests) != 3 || requests[0].Method != "initialize" || requests[1].Method != "initialized" ||
		requests[1].ID != nil || params.ClientInfo.Name != "codex_thread_bridge" || !params.Capabilities.ExperimentalAPI {
		t.Fatalf("requests = %+v, initialize params = %+v", requests, params)
	}
}

func TestRequest_isRefusedNotInitialized_whenSentBeforeInitialize(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	server.Respond("thread/goal/get", fakehost.Reply{})
	c := dial(t, server)

	// When
	c.send(map[string]any{"id": 1, "method": "thread/goal/get", "params": map[string]any{}})

	// Then
	if got := c.read(); got.Error == nil || got.Error.Message != "not initialized" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestRequest_isAnsweredMethodNotFound_whenMethodUnscripted(t *testing.T) {
	// Given
	c := dial(t, fakehost.Start(t))
	c.handshake()

	// When
	c.send(map[string]any{"id": 2, "method": "thread/fork", "params": map[string]any{}})

	// Then
	if got := c.read(); got.Error == nil || got.Error.Code != -32601 || got.Error.Message != "thread/fork" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestFrame_isRejectedWith1009_whenLargerThan16MiB(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	c := dial(t, server)
	c.handshake()

	// When
	c.sendRaw(`{"id":2,"method":"thread/read","params":{"padding":"` + strings.Repeat("x", fakehost.MaxFrameBytes) + `"}}`)
	_, _, err := c.conn.Read(c.ctx)

	// Then
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("read after oversized frame: %v", err)
	}
	if server.Count("thread/read") != 0 {
		t.Fatal("the oversized request was dispatched")
	}
}

func TestFrame_isRecordedMalformedAndNotDispatched_whenItCarriesJSONRPC(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	server.Respond("thread/read", fakehost.Reply{})
	c := dial(t, server)
	c.handshake()

	// When
	c.sendRaw(`{"jsonrpc":"2.0","id":7,"method":"thread/read","params":{}}`)
	c.send(map[string]any{"id": 8, "method": "thread/read", "params": map[string]any{}})
	next := c.read()

	// Then
	if string(next.ID) != "8" {
		t.Fatalf("first answer after the jsonrpc frame = %+v, want id 8", next)
	}
	malformed := server.Malformed()
	if len(malformed) != 1 || !strings.Contains(malformed[0].Reason, "jsonrpc") || server.Count("thread/read") != 1 {
		t.Fatalf("malformed = %+v, thread/read count = %d", malformed, server.Count("thread/read"))
	}
}

func TestServerRequest_recordsClientRefusal_whenReplyRaisesApprovalRequest(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	server.Script("turn/start", fakehost.Reply{
		Result:         map[string]any{"turn": map[string]any{"id": "turn-1"}},
		ServerRequests: []string{fakehost.ApprovalMethods[2]},
	})
	server.Respond("thread/goal/get", fakehost.Reply{})
	c := dial(t, server)
	c.handshake()
	c.send(map[string]any{"id": 2, "method": "turn/start", "params": map[string]any{"threadId": "t1"}})
	raised := c.read()

	// When
	c.send(map[string]any{"id": json.RawMessage(raised.ID), "error": map[string]any{"code": -32601, "message": "no"}})

	// Then
	turn := c.read()
	c.send(map[string]any{"id": 3, "method": "thread/goal/get", "params": map[string]any{}})
	c.read() // answered only after the refusal above was recorded: frames are handled in order
	requests, answers := server.ServerRequests(), server.Answers()
	if raised.Method != fakehost.ApprovalMethods[2] || string(turn.ID) != "2" || len(requests) != 1 ||
		len(answers) != 1 || string(answers[0].ID) != `"`+requests[0].ID+`"` {
		t.Fatalf("raised=%+v turn=%+v requests=%+v answers=%+v", raised, turn, requests, answers)
	}
}

func TestDelayedReply_isOvertaken_whenLaterRequestIsImmediate(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	server.Script("thread/read", fakehost.Reply{Delay: time.Hour})
	server.Respond("thread/goal/get", fakehost.Reply{})
	c := dial(t, server)
	c.handshake()

	// When
	c.send(map[string]any{"id": 2, "method": "thread/read", "params": map[string]any{}})
	c.send(map[string]any{"id": 3, "method": "thread/goal/get", "params": map[string]any{}})

	// Then
	if got := c.read(); string(got.ID) != "3" {
		t.Fatalf("first answer = %+v, want the undelayed id 3", got)
	}
}

func TestPaddedReply_exceedsClientLimit_whenPadBytesPastIt(t *testing.T) {
	// Given
	server := fakehost.Start(t)
	server.Script("thread/turns/list", fakehost.Reply{PadBytes: 64 * 1024, Before: []fakehost.Notification{{Method: "thread/status/changed"}}})
	c := dial(t, server)
	c.handshake()
	c.conn.SetReadLimit(32 * 1024)

	// When
	c.send(map[string]any{"id": 2, "method": "thread/turns/list", "params": map[string]any{}})
	notification := c.read()
	_, _, err := c.conn.Read(c.ctx)

	// Then
	if notification.Method != "thread/status/changed" || !errors.Is(err, websocket.ErrMessageTooBig) {
		t.Fatalf("notification=%+v err=%v", notification, err)
	}
}
