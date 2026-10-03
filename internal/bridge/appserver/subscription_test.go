package appserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func subscriptionClient(t *testing.T) (*Client, *fakehost.Server) {
	t.Helper()
	host := fakehost.Start(t)
	c, err := Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	return c, host
}

func announceEnd(t *testing.T, c *Client, host *fakehost.Server, turn string) {
	t.Helper()
	host.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{{Method: "turn/completed", Params: map[string]any{"threadId": "root", "turn": map[string]any{"id": turn, "status": "completed"}}}}})
	if _, err := c.Call(context.Background(), "probe/end", nil); err != nil {
		t.Fatal(err)
	}
}

func subscriptionSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("lifecycle signal not observed")
	}
}

func TestUnsubscribeGateOrdersNextResume(t *testing.T) {
	c, host := subscriptionClient(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/unsubscribe", fakehost.Reply{Paused: entered, Release: release})
	w, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	w.Finish("first", false)
	announceEnd(t, c, host, "first")
	subscriptionSignal(t, entered)
	done := make(chan error, 1)
	go func() {
		next, err := c.WatchTurn(context.Background(), "root")
		if err == nil {
			host.Respond("thread/resume", fakehost.Reply{})
			_, err = c.Call(next.Context(context.Background()), "thread/resume", nil)
			next.Finish("second", false)
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("resume raced unsubscribe: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	announceEnd(t, c, host, "second")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 2); err != nil {
		t.Fatal(err)
	}
}

func TestCloseCancelsAndDrainsRelease(t *testing.T) {
	c, host := subscriptionClient(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/unsubscribe", fakehost.Reply{Paused: entered, Release: release})
	w, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	w.Finish("first", false)
	announceEnd(t, c, host, "first")
	subscriptionSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	// The fake's paused answer also pauses its close-frame reader. Observe the
	// release worker draining, then let the fake complete the close handshake.
	subscriptionSignal(t, c.subscriptions.done)
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if host.Count("thread/unsubscribe") != 1 {
		t.Fatal("shutdown reconnected or restarted release")
	}
}

func TestRetiredReaderCannotCompleteAReplacementWatch(t *testing.T) {
	c, host := subscriptionClient(t)
	w, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	w.Finish("old-turn", false)
	c.mu.Lock()
	old := c.conn
	c.mu.Unlock()
	c.retire(old)
	// failReader is the existing deterministic retired-reader seam. It must
	// remove old release work without affecting the replacement connection.
	c.failReader(old, &TransportError{Reason: "synthetic disconnect"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	next.Finish("new-turn", false)
	raw, _ := json.Marshal(map[string]any{"threadId": "root", "turn": map[string]any{"id": "new-turn"}})
	c.subscriptions.terminal(old, raw)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if host.WaitCount(ctx, "thread/unsubscribe", 1) == nil {
		t.Fatal("old connection completed the new subscription watch")
	}
	cancel()
	announceEnd(t, c, host, "new-turn")
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
}
