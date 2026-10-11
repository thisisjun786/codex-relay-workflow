package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// A creation whose caller keeps its watch (CreateThreadWatched) holds the subscription and the thread's
// gate past the creation, sees the standby turn's turn/completed on the watch, and lets go on Release, once.
func TestCreateThreadWatchedKeepsTheWatchUntilReleased(t *testing.T) {
	b, host, in := subscriptionHost(t)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "held-turn"}}})
	receipt, watch, err := b.CreateThreadWatched(context.Background(), in)
	if err != nil || receipt["status"] != "accepted" || watch == nil {
		t.Fatalf("receipt=%v watch=%v err=%v", receipt, watch, err)
	}
	if watch.TurnCompleted("held-turn") || watch.TurnCompleted("") {
		t.Fatal("a turn that has not ended is not completed")
	}
	host.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{terminalNote("release-root", "other-turn")}})
	if _, err := b.RPC.Call(context.Background(), "probe/end", nil); err != nil {
		t.Fatal(err)
	}
	if watch.TurnCompleted("held-turn") {
		t.Fatal("another turn's end is not this turn's")
	}
	host.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{terminalNote("release-root", "held-turn")}})
	if _, err := b.RPC.Call(context.Background(), "probe/end", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !watch.TurnCompleted("held-turn") {
		if time.Now().After(deadline) {
			t.Fatal("the watch never reported the standby turn's turn/completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The turn has ended, but the watch holds the subscription until it is released.
	short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	if err := host.WaitCount(short, "thread/unsubscribe", 1); err == nil {
		t.Fatal("the subscription was released while the caller kept the watch")
	}
	cancel()
	watch.Release()
	watch.Release()
	long, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := host.WaitCount(long, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("released watch kept its subscription: %v", err)
	}
	if host.Count("thread/unsubscribe") != 1 {
		t.Fatalf("one unsubscribe: %d", host.Count("thread/unsubscribe"))
	}
}

// A creation that did not end accepted hands over no watch: the recovery that follows takes the gate itself.
func TestCreateThreadWatchedKeepsNothingOfAFailedCreation(t *testing.T) {
	b, host, in := subscriptionHost(t)
	host.Respond("turn/start", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "turn refused"}})
	_, watch, _ := b.CreateThreadWatched(context.Background(), in)
	if watch != nil {
		t.Fatal("a failed creation kept its watch")
	}
}
