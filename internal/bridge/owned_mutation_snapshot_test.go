package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// Python's schedule: one send holds the mutation lock (paused at its thread/read) while a
// second send waits for it, and the caller mutates the dictionary it gave the waiting send.
func Test_test_a_caller_mutating_its_settings_cannot_split_identity_from_dispatch(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/resume", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	paused, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	idle := map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}
	host.Script("thread/read", fakehost.Reply{Result: idle, Paused: paused, Release: release})
	host.Respond("thread/read", fakehost.Reply{Result: idle})
	atLock := make(chan struct{})
	var once sync.Once
	b.waiting = func(requestID string) {
		if requestID == "waiting" {
			once.Do(func() { close(atLock) })
		}
	}
	pair := func() map[string]any { return map[string]any{"model": "explicit-model", "reasoning_effort": "high"} }
	holding := make(chan map[string]any, 1)
	go func() {
		receipt, _ := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "holding", ThreadID: "thread-1", Message: "hold", Expected: pair()})
		holding <- receipt
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("holding send never reached the host")
	}
	settings := pair()
	waiting := make(chan map[string]any, 1)
	go func() {
		receipt, _ := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "waiting", ThreadID: "thread-1", Message: "hello", Expected: settings})
		waiting <- receipt
	}()
	select {
	case <-atLock:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting send never reached the mutation lock")
	}
	settings["model"] = "openai/gpt-6-astra"
	close(release)
	select {
	case receipt := <-holding:
		if receipt["status"] != "accepted" {
			t.Fatalf("holding=%v", receipt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holding hung")
	}
	var dispatched map[string]any
	select {
	case dispatched = <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting hung")
	}
	if dispatched["status"] != "accepted" || object(dispatched["executionPolicy"])["model"] != "explicit-model" {
		t.Fatalf("dispatched=%v", dispatched)
	}
	replay, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "waiting", ThreadID: "thread-1", Message: "hello", Expected: pair()})
	if err != nil || replay["replayed"] != true || replay["turnId"] != dispatched["turnId"] {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	mutated := map[string]any{"model": "openai/gpt-6-astra", "reasoning_effort": "high"}
	if _, err = b.SendMessageToThread(context.Background(), SendMessage{RequestID: "waiting", ThreadID: "thread-1", Message: "hello", Expected: mutated}); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("mutated settings replayed: %v", err)
	}
}
