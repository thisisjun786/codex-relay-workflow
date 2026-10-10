package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func subscriptionHost(t *testing.T) (*Bridge, *fakehost.Server, CreateThread) {
	t.Helper()
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "release-root"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	return b, host, CreateThread{RequestID: "release-create", CWD: cwd, Prompt: "finish", Sandbox: "read-only", Model: "explicit-model", Effort: "high"}
}

func terminalNote(thread, turn string) fakehost.Notification {
	return fakehost.Notification{Method: "turn/completed", Params: map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": "completed"}}}
}

func TestFinishedCreateReleasesSubscriptionEvenWhenCompletionPrecedesAck(t *testing.T) {
	b, host, in := subscriptionHost(t)
	// Fill the public notification buffer before the terminal. Lifecycle observation
	// must not depend on a best-effort notification consumer.
	before := make([]fakehost.Notification, 80)
	for i := range before {
		before[i] = fakehost.Notification{Method: "thread/status/changed", Params: map[string]any{}}
	}
	before = append(before, terminalNote("release-root", "release-turn"))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "release-turn"}}, Before: before})
	receipt, err := b.CreateThread(context.Background(), in)
	if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "release-turn" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("finished root subscription was retained: %v", err)
	}
	if _, err := b.CreateThread(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if host.Count("turn/start") != 1 || host.Count("thread/unsubscribe") != 1 {
		t.Fatalf("replay changed subscription ownership: %v", host.Requests())
	}
}

func TestCreateHoldsSubscriptionUntilItsOwnTurnEnds(t *testing.T) {
	b, host, in := subscriptionHost(t)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "held-turn"}}})
	if _, err := b.CreateThread(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	host.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{terminalNote("other-root", "held-turn"), terminalNote("release-root", "other-turn")}})
	if _, err := b.RPC.Call(context.Background(), "probe/end", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err == nil {
		t.Fatal("an in-progress turn lost its subscription on an unrelated terminal")
	}
	cancel()
	host.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{terminalNote("release-root", "held-turn")}})
	if _, err := b.RPC.Call(context.Background(), "probe/end", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
}

func TestNeverRunRetentionSurvivesRefusedAndCancelledInitialSends(t *testing.T) {
	b, host, in := subscriptionHost(t)
	in.Prompt = ""
	if _, err := b.CreateThread(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "release-root", "status": map[string]any{"type": "idle"}}}})
	bad := startReply(in.CWD)
	bad.Result["model"] = "other-model"
	host.Respond("thread/resume", bad)
	send := SendMessage{RequestID: "initial-refused", ThreadID: "release-root", Message: "first", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	if r, err := b.SendMessageToThread(context.Background(), send); err != nil || r["status"] != "failed" {
		t.Fatalf("refusal=%v err=%v", r, err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/resume", fakehost.Reply{Result: startReply(in.CWD).Result, Paused: entered, Release: release})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	send.RequestID = "initial-cancelled"
	go func() { _, err := b.SendMessageToThread(ctx, send); done <- err }()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("resume did not reach the isolated host")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	close(release)
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	if host.WaitCount(ctx, "thread/unsubscribe", 1) == nil {
		t.Fatal("a newer refusal/cancellation orphaned a never-run root")
	}
	cancel()
	host.Respond("thread/resume", startReply(in.CWD))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "first-turn"}}, Before: []fakehost.Notification{terminalNote("release-root", "first-turn")}})
	send.RequestID = "initial-success"
	if r, err := b.SendMessageToThread(context.Background(), send); err != nil || r["status"] != "accepted" {
		t.Fatalf("initial delivery=%v err=%v", r, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
}
