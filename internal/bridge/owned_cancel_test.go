package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_cancellation_keeps_unknown_receipt_and_prevents_retry(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	paused := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	host.Respond("thread/start", fakehost.Reply{Result: startReply(cwd).Result, Paused: paused, Release: release})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	input := createInput(cwd, "cancel")
	go func() { _, err := b.CreateThread(ctx, input); result <- err }()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("thread/start did not reach host")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not propagated: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	receipt, err := b.GetOperation(context.Background(), "cancel")
	if err != nil || receipt["status"] != "outcome_unknown" || receipt["retrySafe"] != false || len(receipt["attemptedEffects"].([]any)) != 1 || receipt["attemptedEffects"].([]any)[0] != "thread/start" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	replay, err := b.CreateThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || host.Count("thread/start") != 1 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_cancellation_keeps_unknown_receipt_when_reader_outcome_wins(t *testing.T) {
	for _, outcome := range []string{"disconnect", "response", "rpc_error"} {
		t.Run(outcome, func(t *testing.T) {
			b, host := testBridge(t)
			cwd := t.TempDir()
			paused := make(chan struct{})
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			reply := startReply(cwd)
			if outcome == "rpc_error" {
				reply = fakehost.Reply{Error: &fakehost.RPCError{Code: -32603, Message: "rejected"}}
			}
			reply.Paused, reply.Release = paused, release
			host.Respond("thread/start", reply)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selected := make(chan struct{})
			ctx = appserver.WithOutcomeHook(ctx, func(method string) {
				if method == "thread/start" {
					// Force the reader branch to win, then cancel before it is interpreted.
					// Making both select cases ready alone would still leave the test random.
					cancel()
					close(selected)
				}
			})
			input := createInput(cwd, "cancel")
			input.Title, input.Prompt = "title", "work"
			result := make(chan error, 1)
			go func() { _, err := b.CreateThread(ctx, input); result <- err }()
			select {
			case <-paused:
			case <-time.After(5 * time.Second):
				t.Fatal("thread/start did not reach host")
			}
			if outcome == "disconnect" {
				// The reader can fail even while the host still withholds the answer.
				host.Close()
			} else {
				unblock()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancellation not propagated: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not finish")
			}
			select {
			case <-selected:
			default:
				t.Fatal("reader outcome was not selected")
			}
			receipt, err := b.GetOperation(context.Background(), "cancel")
			if err != nil || receipt["status"] != "outcome_unknown" || receipt["retrySafe"] != false || receipt["threadId"] != nil || len(receipt["attemptedEffects"].([]any)) != 1 || receipt["attemptedEffects"].([]any)[0] != "thread/start" {
				t.Errorf("receipt=%v err=%v", receipt, err)
			}
			replay, err := b.CreateThread(context.Background(), input)
			if err != nil || replay["replayed"] != true || host.Count("thread/start") != 1 || host.Count("thread/name/set") != 0 || host.Count("turn/start") != 0 {
				t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
			}
		})
	}
}

func Test_test_cancelling_before_anything_was_sent_leaves_the_id_usable(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	created := createInput(cwd, "create")
	created.Prompt = "work"
	first, err := b.CreateThread(context.Background(), created)
	if err != nil || first["status"] != "accepted" {
		t.Fatalf("create=%v err=%v", first, err)
	}
	paused := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	host.Script("thread/read", fakehost.Reply{Paused: paused, Release: release})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	input := SendMessage{RequestID: "cancel-early", ThreadID: "thread-1", Message: "instruction", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	go func() { _, err := b.SendMessageToThread(ctx, input); result <- err }()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("thread/read did not reach host")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not propagated: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	receipt, err := b.GetOperation(context.Background(), "cancel-early")
	if err != nil || receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || len(receipt["attemptedEffects"].([]any)) != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	close(release)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(t.TempDir()))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	retried, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || retried["status"] != "accepted" || retried["replayed"] == true || retried["attempt"] != 2 || host.Count("turn/start") != 2 {
		t.Fatalf("retried=%v err=%v calls=%v", retried, err, host.Requests())
	}
}
