package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func TestCleanupObservationsOutliveErrorBound(t *testing.T) {
	c, host := subscriptionClient(t)
	c.subscriptions.retryFloor = time.Millisecond
	var calls atomic.Int32
	c.ConfigureSubscriptions(func(context.Context, string) (bool, error) { return calls.Add(1) > descendantErrorLimit+1, nil })
	w := rootWatch(t, c)
	w.Finish("done", false)
	announceEnd(t, c, host, "done")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != descendantErrorLimit+2 {
		t.Fatalf("hold retired early: %d", calls.Load())
	}
}
func TestCleanupFailuresAreBounded(t *testing.T) {
	for _, failure := range []error{errors.New("archive refusal"), &PhaseTimeout{Method: "thread/read", Phase: "ack", Bound: time.Second}, ErrDescendantsUnproved} {
		t.Run(failure.Error(), func(t *testing.T) {
			c, host := subscriptionClient(t)
			c.subscriptions.retryFloor = time.Millisecond
			var calls atomic.Int32
			wantCalls, unsubscribes := descendantErrorLimit, 1
			if errors.Is(failure, ErrDescendantsUnproved) {
				wantCalls, unsubscribes = 1, 2
				host.Script("thread/unsubscribe", fakehost.Reply{Error: &fakehost.RPCError{Message: "unsubscribe refusal"}}, fakehost.Reply{})
			}
			c.ConfigureSubscriptions(func(context.Context, string) (bool, error) { calls.Add(1); return false, failure })
			w := rootWatch(t, c)
			w.Finish("done", false)
			announceEnd(t, c, host, "done")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := host.WaitCount(ctx, "thread/unsubscribe", unsubscribes); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != int32(wantCalls) {
				t.Fatalf("failed cleanup attempts %d", calls.Load())
			}
		})
	}
}

func TestCleanupSuccessResetsErrorStreakAfterUnsubscribeFailure(t *testing.T) {
	c, host := subscriptionClient(t)
	c.subscriptions.retryFloor = time.Millisecond
	host.Script("thread/unsubscribe", fakehost.Reply{Error: &fakehost.RPCError{Message: "unsubscribe refusal"}}, fakehost.Reply{})
	var calls atomic.Int32
	c.ConfigureSubscriptions(func(context.Context, string) (bool, error) {
		n := calls.Add(1)
		if n < descendantErrorLimit || n == descendantErrorLimit+1 {
			return false, errors.New("cleanup error")
		}
		return true, nil
	})
	w := rootWatch(t, c)
	w.Finish("done", false)
	announceEnd(t, c, host, "done")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 2); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != descendantErrorLimit+2 {
		t.Fatalf("successful cleanup did not reset error streak: %d", calls.Load())
	}
}

func TestAcknowledgedTurnRearmsExhaustedCleanup(t *testing.T) {
	c, host := subscriptionClient(t)
	c.subscriptions.retryFloor = time.Millisecond
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/unsubscribe", fakehost.Reply{Paused: entered, Release: release, Error: &fakehost.RPCError{Message: "unsubscribe refusal"}}, fakehost.Reply{})
	var calls atomic.Int32
	c.ConfigureSubscriptions(func(context.Context, string) (bool, error) {
		if calls.Add(1) <= descendantErrorLimit {
			return false, errors.New("cleanup error")
		}
		return true, nil
	})
	first := rootWatch(t, c)
	first.Finish("old", false)
	announceEnd(t, c, host, "old")
	subscriptionSignal(t, entered)
	next := make(chan *TurnWatch, 1)
	go func() { w, _ := c.WatchTurn(context.Background(), "root"); next <- w }()
	// Observe the queued admission before allowing the old unsubscribe to fail.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.subscriptions.mu.Lock()
		queued := first.root.refs > len(first.root.watches)
		c.subscriptions.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			close(release)
			t.Fatal("new watch did not queue")
		}
	}
	close(release)
	w := <-next
	if w == nil {
		t.Fatal("new watch refused")
	}
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "new"}}}
	})
	if _, err := c.Call(w.Context(context.Background()), "turn/start", map[string]any{"threadId": "root"}); err != nil {
		t.Fatal(err)
	}
	w.Finish("new", false)
	announceEnd(t, c, host, "new")
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 2); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != descendantErrorLimit+1 {
		t.Fatalf("new turn inherited exhausted cleanup: %d", calls.Load())
	}
}
func TestCleanupBarrierFencesDirectDescendantAdmission(t *testing.T) {
	c, host := subscriptionClient(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	c.ConfigureSubscriptions(func(ctx context.Context, root string) (bool, error) {
		if root != "root" {
			return true, nil
		}
		entered <- struct{}{}
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	w := rootWatch(t, c)
	w.Finish("done", false)
	announceEnd(t, c, host, "done")
	subscriptionSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	next, err := c.WatchTurn(ctx, "descendant")
	cancel()
	if next != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("descendant raced cleanup: %v", err)
	}
	c.subscriptions.mu.Lock()
	n := c.subscriptions.admitted
	c.subscriptions.mu.Unlock()
	if n != 0 {
		t.Fatalf("canceled admission leaked %d", n)
	}
	close(release)
	next, err = c.WatchTurn(context.Background(), "descendant")
	if err != nil {
		t.Fatal(err)
	}
	next.Finish("", false)
}
func TestCleanupDrainsAdmittedOperations(t *testing.T) {
	c, host := subscriptionClient(t)
	entered := make(chan struct{}, 1)
	c.ConfigureSubscriptions(func(context.Context, string) (bool, error) { entered <- struct{}{}; return true, nil })
	other, err := c.WatchTurn(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	root := rootWatch(t, c)
	root.Finish("done", false)
	announceEnd(t, c, host, "done")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err == nil {
		t.Fatal("cleanup raced admitted operation")
	}
	cancel()
	select {
	case <-entered:
		t.Fatal("callback before drain")
	default:
	}
	other.Finish("pending", false)
	subscriptionSignal(t, entered)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
}
func TestCleanupKeepsCompletionThroughRefusedWatch(t *testing.T) {
	c, host := subscriptionClient(t)
	c.subscriptions.retryFloor = time.Millisecond
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	c.ConfigureSubscriptions(func(context.Context, string) (bool, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			return false, nil
		}
		return true, nil
	})
	first := rootWatch(t, c)
	first.Finish("old", false)
	refused := rootWatch(t, c)
	refused.Finish("", false)
	noRelease(t, host)
	if calls.Load() != 0 {
		t.Fatal("cleanup before terminal")
	}
	announceEnd(t, c, host, "old")
	subscriptionSignal(t, entered)
	refused = rootWatch(t, c)
	refused.Finish("", false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
}
func TestCleanupSocketLossNeverReconnects(t *testing.T) {
	c, host := subscriptionClient(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	result := make(chan error, 1)
	c.ConfigureSubscriptions(func(ctx context.Context, _ string) (bool, error) {
		entered <- struct{}{}
		<-release
		_, err := c.Call(ctx, "probe/bound", nil)
		result <- err
		return false, err
	})
	w := rootWatch(t, c)
	w.Finish("done", false)
	announceEnd(t, c, host, "done")
	subscriptionSignal(t, entered)
	c.mu.Lock()
	old := c.conn
	c.conn = nil
	c.mu.Unlock()
	c.subscriptions.lost(old)
	_ = old.CloseNow()
	close(release)
	var failure *TransportError
	if err := <-result; !errors.As(err, &failure) {
		t.Fatalf("lost cleanup scope %v", err)
	}
	noRelease(t, host)
	if host.Count("initialize") != 1 || host.Count("probe/bound") != 0 {
		t.Fatalf("cleanup reconnected: %+v", host.Requests())
	}
}
func TestCloseCancelsCleanupAndReleasesBarrier(t *testing.T) {
	c, host := subscriptionClient(t)
	entered := make(chan struct{}, 1)
	c.ConfigureSubscriptions(func(ctx context.Context, _ string) (bool, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return false, ctx.Err()
	})
	w := rootWatch(t, c)
	w.Finish("done", false)
	announceEnd(t, c, host, "done")
	subscriptionSignal(t, entered)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.subscriptions.mu.Lock()
	active, n := c.subscriptions.cleanupActive, c.subscriptions.admitted
	c.subscriptions.mu.Unlock()
	if active || n != 0 || host.Count("thread/unsubscribe") != 0 {
		t.Fatalf("shutdown active=%v admissions=%d", active, n)
	}
}
func TestLostWatchEndsAdmissionOnlyOnFinish(t *testing.T) {
	c, _ := subscriptionClient(t)
	w := rootWatch(t, c)
	c.mu.Lock()
	old := c.conn
	c.conn = nil
	c.mu.Unlock()
	c.subscriptions.lost(old)
	_ = old.CloseNow()
	c.subscriptions.mu.Lock()
	n := c.subscriptions.admitted
	c.subscriptions.mu.Unlock()
	if n != 1 {
		t.Fatalf("loss ended operation early: %d", n)
	}
	w.Finish("", false)
	c.subscriptions.mu.Lock()
	n = c.subscriptions.admitted
	c.subscriptions.mu.Unlock()
	if n != 0 {
		t.Fatalf("Finish leaked admission: %d", n)
	}
}
