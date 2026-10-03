package appserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func noRelease(t *testing.T, host *fakehost.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if host.WaitCount(ctx, "thread/unsubscribe", 1) == nil {
		t.Fatal("released a root with pending turn or never-run retention")
	}
}

func TestRefusedWatchWaitsForPendingTurn(t *testing.T) {
	c, host := subscriptionClient(t)
	first, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	first.Finish("pending", false)
	refused, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	refused.Finish("", false)
	noRelease(t, host)
	announceEnd(t, c, host, "pending")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitCount(ctx, "thread/unsubscribe", 2); err == nil {
		t.Fatal("duplicate root release")
	}
}

func TestReplacementConnectionRetiresEachWatch(t *testing.T) {
	c, host := subscriptionClient(t)
	first, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	first.Finish("old", false)
	c.mu.Lock()
	old := c.conn
	c.conn = nil // Reproduce replacement before the old reader's loss callback.
	c.mu.Unlock()
	t.Cleanup(func() { _ = old.CloseNow() })
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	c.subscriptions.lost(old)
	next.Finish("new", false)
	announceEnd(t, c, host, "new")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
	for {
		c.subscriptions.mu.Lock()
		n := len(c.subscriptions.roots)
		c.subscriptions.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("retired reader left %d roots", n)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAcknowledgedCreationSurvivesOutcomeCancellation(t *testing.T) {
	c, host := subscriptionClient(t)
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "root"}}})
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithOutcomeHook(ctx, func(method string) {
		if method == "thread/start" {
			cancel()
		}
	})
	if _, err := c.Call(ctx, "thread/start", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	refused, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	refused.Finish("", false)
	noRelease(t, host)
	announceEnd(t, c, host, "first-durable-turn")
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredScopeCannotSendOnReplacementSocket(t *testing.T) {
	c, host := subscriptionClient(t)
	w, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	w.Finish("old", false)
	c.mu.Lock()
	old := c.conn
	c.mu.Unlock()
	c.retire(old)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"thread/resume", "turn/start", "thread/unsubscribe"} {
		_, err := c.Call(w.Context(context.Background()), method, map[string]any{"threadId": "root"})
		var lost *TransportError
		if !errors.As(err, &lost) || host.Count(method) != 0 {
			t.Fatalf("%s err=%v calls=%v", method, err, host.Requests())
		}
	}
}

func TestUncertainTurnKeepsItsProofUntilItsOwnTerminal(t *testing.T) {
	for _, ack := range []bool{true, false} {
		t.Run(map[bool]string{true: "ack-cancelled", false: "reply-unavailable"}[ack], func(t *testing.T) {
			c, host := subscriptionClient(t)
			old, err := c.WatchTurn(context.Background(), "root")
			if err != nil {
				t.Fatal(err)
			}
			old.Finish("older", false)
			w, err := c.WatchTurn(context.Background(), "root")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = w.Context(ctx)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			reply := fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "uncertain"}}}
			if ack {
				ctx = WithOutcomeHook(ctx, func(method string) {
					if method == "turn/start" {
						cancel()
					}
				})
			} else {
				reply.Paused = entered
				reply.Release = release
			}
			host.Respond("turn/start", reply)
			done := make(chan error, 1)
			go func() { _, err := c.Call(ctx, "turn/start", map[string]any{"threadId": "root"}); done <- err }()
			if !ack {
				subscriptionSignal(t, entered)
				cancel()
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel=%v", err)
			}
			w.Finish("", false)
			if !ack {
				close(release)
			}
			announceEnd(t, c, host, "older")
			noRelease(t, host)
			announceEnd(t, c, host, "uncertain")
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFailedReleaseProofSurvivesARefusedWatch(t *testing.T) {
	c, host := subscriptionClient(t)
	c.subscriptions.retryFloor = 10 * time.Millisecond // Before worker startup.
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/unsubscribe", fakehost.Reply{Error: &fakehost.RPCError{Code: -1, Message: "temporary"}, Paused: entered, Release: release})
	w, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	w.Finish("completed", false)
	announceEnd(t, c, host, "completed")
	subscriptionSignal(t, entered)
	refused := make(chan error, 1)
	go func() {
		next, err := c.WatchTurn(context.Background(), "root")
		if err == nil {
			next.Finish("", false)
		}
		refused <- err
	}()
	close(release)
	if err := <-refused; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 2); err != nil {
		t.Fatal(err)
	}
}
