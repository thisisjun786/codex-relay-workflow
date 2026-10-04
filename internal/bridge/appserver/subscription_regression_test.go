package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// Host reception does not prove Write returned and disarmed its cancellation
// callback. Observe that boundary before cancelling a still-unanswered call.
func subscriptionTransmitted(c *Client, method string) <-chan struct{} {
	sent := make(chan struct{}, 1)
	write := c.writeFrame
	if write == nil {
		write = (*websocket.Conn).Write
	}
	c.writeFrame = func(ws *websocket.Conn, ctx context.Context, typ websocket.MessageType, raw []byte) error {
		if err := write(ws, ctx, typ, raw); err != nil {
			return err
		}
		var frame struct{ Method string }
		_ = json.Unmarshal(raw, &frame)
		if frame.Method == method {
			select {
			case sent <- struct{}{}:
			default:
			}
		}
		return nil
	}
	return sent
}

func singleSubscriptionRelease(t *testing.T, c *Client, host *fakehost.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
	// Wait for the successful release to retire its proof before counting attempts.
	for {
		c.subscriptions.mu.Lock()
		n := len(c.subscriptions.roots)
		c.subscriptions.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("release left %d roots", n)
		case <-time.After(time.Millisecond):
		}
	}
	if host.Count("initialize") != 1 || host.Count("thread/unsubscribe") != 1 {
		t.Fatalf("release reconnected or retried: %v", host.Requests())
	}
}

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
	first := rootWatch(t, c)
	first.Finish("pending", false)
	refused := rootWatch(t, c)
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
	first := rootWatch(t, c)
	first.Finish("old", false)
	c.mu.Lock()
	old := c.conn
	c.conn = nil // Reproduce replacement before the old reader's loss callback.
	c.mu.Unlock()
	t.Cleanup(func() { _ = old.CloseNow() })
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := rootWatch(t, c)
	for _, method := range []string{"thread/resume", "turn/start", "thread/unsubscribe"} {
		_, err := c.Call(first.Context(context.Background()), method, map[string]any{"threadId": "root"})
		var lost *TransportError
		if !errors.As(err, &lost) || host.Count(method) != 0 {
			t.Fatalf("%s err=%v calls=%v", method, err, host.Requests())
		}
	}
	c.subscriptions.lost(old)
	next.Finish("new", false)
	raw, _ := json.Marshal(map[string]any{"threadId": "root", "turn": map[string]any{"id": "new"}})
	c.subscriptions.terminal(old, raw)
	noRelease(t, host)
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
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-hook", true: "late-ack"}[late], func(t *testing.T) {
			c, host := subscriptionClient(t)
			sent := subscriptionTransmitted(c, "thread/start")
			entered, release := make(chan struct{}, 1), make(chan struct{})
			reply := fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "root"}}}
			if late {
				reply.Paused = entered
				reply.Release = release
			}
			host.Respond("thread/start", reply)
			ctx, cancel := context.WithCancel(context.Background())
			ctx = WithOutcomeHook(ctx, func(method string) {
				if method == "thread/start" && !late {
					cancel()
				}
			})
			done := make(chan error, 1)
			go func() { _, err := c.Call(ctx, "thread/start", nil); done <- err }()
			if late {
				subscriptionSignal(t, entered)
				subscriptionSignal(t, sent)
				cancel()
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel=%v", err)
			}
			if late {
				close(release)
				if _, err := c.Call(context.Background(), "probe/barrier", nil); err == nil {
					t.Fatal("unscripted probe unexpectedly succeeded")
				}
			}
			refused := rootWatch(t, c)
			refused.Finish("", false)
			noRelease(t, host)
			announceEnd(t, c, host, "first-durable-turn")
			singleSubscriptionRelease(t, c, host)
		})
	}
}

func TestUncertainTurnKeepsItsProofUntilItsOwnTerminal(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		ack, refused bool
	}{{"ack-cancelled", true, false}, {"reply-unavailable", false, false}, {"late-refusal", false, true}} {
		ack := scenario.ack
		t.Run(scenario.name, func(t *testing.T) {
			c, host := subscriptionClient(t)
			sent := subscriptionTransmitted(c, "turn/start")
			old := rootWatch(t, c)
			old.Finish("older", false)
			w := rootWatch(t, c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = w.Context(ctx)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			reply := fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "uncertain"}}}
			if scenario.refused {
				reply.Result = nil
				reply.Error = &fakehost.RPCError{Code: -1, Message: "refused"}
			}
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
				subscriptionSignal(t, sent)
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
			if !scenario.refused {
				noRelease(t, host)
				announceEnd(t, c, host, "uncertain")
			}
			singleSubscriptionRelease(t, c, host)
		})
	}
}

func TestFailedReleaseProofSurvivesARefusedWatch(t *testing.T) {
	for _, noWatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-refusal", true: "cancelled-creation-no-watch"}[noWatch], func(t *testing.T) {
			c, host := subscriptionClient(t)
			c.subscriptions.retryFloor = 10 * time.Millisecond // Before worker startup.
			entered, release := make(chan struct{}, 1), make(chan struct{})
			host.Script("thread/unsubscribe", fakehost.Reply{Error: &fakehost.RPCError{Code: -1, Message: "temporary"}, Paused: entered, Release: release})
			if noWatch {
				host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "root"}}})
				ctx, cancel := context.WithCancel(context.Background())
				_, _ = c.Call(WithOutcomeHook(ctx, func(string) { cancel() }), "thread/start", nil)
			} else {
				w := rootWatch(t, c)
				w.Finish("completed", false)
			}
			announceEnd(t, c, host, "completed")
			subscriptionSignal(t, entered)
			if noWatch {
				close(release)
			} else {
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
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := host.WaitCount(ctx, "thread/unsubscribe", 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCancelledAdmissionCannotPruneReleaseOwnership(t *testing.T) {
	c, host := subscriptionClient(t)
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "root"}}})
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = c.Call(WithOutcomeHook(ctx, func(string) { cancel() }), "thread/start", nil)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/unsubscribe", fakehost.Reply{Paused: entered, Release: release})
	announceEnd(t, c, host, "first")
	subscriptionSignal(t, entered)
	c.subscriptions.mu.Lock()
	original := c.subscriptions.roots["root"]
	c.subscriptions.mu.Unlock()
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if w, err := c.WatchTurn(ctx, "root"); err == nil {
		w.Finish("", false)
		t.Fatal("cancelled admission succeeded")
	}
	c.subscriptions.mu.Lock()
	current := c.subscriptions.roots["root"]
	c.subscriptions.mu.Unlock()
	if current != original {
		close(release)
		t.Fatal("cancelled admission pruned worker-owned root")
	}
	done := make(chan error, 1)
	go func() {
		w, err := c.WatchTurn(context.Background(), "root")
		if err == nil {
			w.Finish("next", false)
		}
		done <- err
	}()
	select {
	case <-done:
		close(release)
		t.Fatal("new admission bypassed release gate")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func rootWatch(t *testing.T, c *Client) *TurnWatch {
	t.Helper()
	w, err := c.WatchTurn(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	return w
}
