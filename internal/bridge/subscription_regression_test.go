package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func TestCreationCheckpointFailureRetainsNeverRunRoot(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "worktree"}[worktree], func(t *testing.T) {
			b, host, in := subscriptionHost(t)
			root := "release-root"
			in.Prompt = ""
			w := CreateWorktree{}
			if worktree {
				w = worktreeInput(t)
				worktreeHost(host, w.Destination)
				root = "thread-1"
			}
			injected := false
			store, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "fault.sqlite3"), ledger.Options{Encode: func(r ledger.Receipt) ([]byte, error) {
				if !injected && r["creation"] != nil {
					injected = true
					return nil, context.Canceled
				}
				return json.Marshal(r)
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			b.Ledger = store
			if worktree {
				_, err = b.CreateWorktreeThread(context.Background(), w)
			} else {
				_, err = b.CreateThread(context.Background(), in)
			}
			if !injected || !errors.Is(err, context.Canceled) {
				t.Fatalf("injected=%v err=%v", injected, err)
			}
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": root, "status": map[string]any{"type": "idle"}}}})
			bad := startReply(in.CWD)
			bad.Result["model"] = "wrong-model"
			host.Respond("thread/resume", bad)
			send := SendMessage{RequestID: "refused-after-checkpoint", ThreadID: root, Message: "first", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
			if r, err := b.SendMessageToThread(context.Background(), send); err != nil || r["status"] != "failed" {
				t.Fatalf("refusal=%v err=%v", r, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			if host.WaitCount(ctx, "thread/unsubscribe", 1) == nil {
				t.Fatal("checkpoint failure bypassed retention")
			}
			cancel()
			host.Respond("thread/resume", startReply(in.CWD))
			host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "first"}}, Before: []fakehost.Notification{terminalNote(root, "first")}})
			send.RequestID = "first-after-checkpoint"
			if r, err := b.SendMessageToThread(context.Background(), send); err != nil || r["status"] != "accepted" {
				t.Fatalf("send=%v err=%v", r, err)
			}
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFinalObservationCannotReconnectAfterTurnAck(t *testing.T) {
	b, host, in := subscriptionHost(t)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "accepted"}}})
	host.Respond("probe/close", fakehost.Reply{Close: &fakehost.CloseFrame{}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{}}})
	ctx := appserver.WithOutcomeHook(context.Background(), func(method string) {
		if method == "turn/start" {
			_, _ = b.RPC.Call(context.Background(), "probe/close", nil)
		}
	})
	r, err := b.CreateThread(ctx, in)
	if err != nil || r["status"] != "accepted" || host.Count("turn/start") != 1 || host.Count("thread/read") != 0 || host.Count("initialize") != 1 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
