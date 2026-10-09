package appserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// TestTurnWatchTransmittedFollowsTheWrite pins the one fact the child-resume classification reads:
// a watch reports false until its own turn/start frame is about to be written. A call the client
// withholds before the write leaves it false, and the frame's write turns it true, so a later
// reader can tell an unsent turn/start from one whose answer was lost.
func TestTurnWatchTransmittedFollowsTheWrite(t *testing.T) {
	c, host := subscriptionClient(t)
	ctx := context.Background()
	w, err := c.WatchTurn(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Finish("turn-1", false)

	if w.Transmitted() {
		t.Fatal("a fresh watch reports a transmitted turn/start")
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"ok": true}})
	if _, err := c.Call(w.Context(ctx), "thread/read", map[string]any{"threadId": "root"}); err != nil {
		t.Fatal(err)
	}
	if w.Transmitted() {
		t.Fatal("a read on the watch marked turn/start transmitted")
	}

	// A call the client withholds before writing the frame never reached the host, so it must not
	// mark the watch transmitted.
	c.FailBeforeWrite("turn/start")
	if _, err := c.Call(w.Context(ctx), "turn/start", map[string]any{"threadId": "root"}); err == nil {
		t.Fatal("the injected failure let a turn/start through")
	}
	if w.Transmitted() {
		t.Fatal("a turn/start withheld before its write marked itself transmitted")
	}

	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	if _, err := c.Call(w.Context(ctx), "turn/start", map[string]any{"threadId": "root"}); err != nil {
		t.Fatal(err)
	}
	if !w.Transmitted() {
		t.Fatal("a written turn/start did not mark itself transmitted")
	}
}

// CRW-915: a watch retired after the request's first checks and before the mark is withheld, not
// marked transmitted. The retire check and the mark are one critical section, so Transmitted never
// reports a frame the client did not write, and the host receives no turn/start.
func TestTurnWatchRetiredBeforeTheMarkIsWithheldNotTransmitted(t *testing.T) {
	c, host := subscriptionClient(t)
	ctx := context.Background()
	w, err := c.WatchTurn(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Finish("", false)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})

	c.RetireBeforeWrite("turn/start")
	_, err = c.Call(w.Context(ctx), "turn/start", map[string]any{"threadId": "root"})
	var transport *TransportError
	if !errors.As(err, &transport) || !strings.Contains(transport.Reason, "request withheld") {
		t.Fatalf("a watch retired before the mark was not withheld: %v", err)
	}
	if w.Transmitted() {
		t.Fatal("a turn/start withheld after the watch retired marked itself transmitted")
	}
	for _, request := range host.Requests() {
		if request.Method == "turn/start" {
			t.Fatalf("the host received a turn/start the client withheld: %+v", request)
		}
	}
}

// CRW-915 contrast: a write that fails after the mark may have sent part of the frame, so the watch
// stays transmitted and the loss of its answer stays uncertain.
func TestTurnWatchFailedWriteStaysTransmitted(t *testing.T) {
	c, _ := subscriptionClient(t)
	ctx := context.Background()
	w, err := c.WatchTurn(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Finish("", false)
	c.writeFrame = func(*websocket.Conn, context.Context, websocket.MessageType, []byte) error {
		return errors.New("write failed")
	}
	if _, err := c.Call(w.Context(ctx), "turn/start", map[string]any{"threadId": "root"}); err == nil {
		t.Fatal("the failed write answered")
	}
	if !w.Transmitted() {
		t.Fatal("a turn/start whose write failed is not marked transmitted")
	}
}
