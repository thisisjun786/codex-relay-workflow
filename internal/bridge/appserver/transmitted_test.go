package appserver

import (
	"context"
	"testing"

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
