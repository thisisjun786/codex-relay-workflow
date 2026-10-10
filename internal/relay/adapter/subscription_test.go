package adapter

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func TestRelayResumesAgainAfterTerminalSubscriptionRelease(t *testing.T) {
	host := fakehost.Start(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "operations.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: client, Ledger: l, Policy: execution.Policy{}})
	t.Cleanup(func() { _ = a.Close() })
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	settings := &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}
	for i := 1; i <= 2; i++ {
		turn := fmt.Sprintf("release-turn-%d", i)
		host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": turn}}, Before: []fakehost.Notification{{Method: "turn/completed", Params: map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": turn, "status": "completed"}}}}})
		r, err := a.SendMessage(context.Background(), fmt.Sprintf("release-request-%d", i), "thread-1", "synthetic delivery", settings)
		if err != nil || r.Get("status") != "accepted" || r.Get("turnId") != turn {
			t.Fatalf("delivery %d: receipt=%v err=%v", i, r, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = host.WaitCount(ctx, "thread/unsubscribe", i)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if host.Count("thread/resume") != 2 || host.Count("turn/start") != 2 {
		t.Fatalf("missing resubscription: %v", host.Requests())
	}
}

func TestReconnectGuardCannotDetachAcceptedDeliveryFromWatch(t *testing.T) {
	host := fakehost.Start(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "operations.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: client, Ledger: l, Policy: execution.Policy{}})
	t.Cleanup(func() { _ = a.Close() })
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("probe/close", fakehost.Reply{Close: &fakehost.CloseFrame{}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "detached"}}})
	guard := func(ctx context.Context) (map[string]any, error) {
		_, _ = client.Call(ctx, "probe/close", nil)
		return nil, nil
	}
	r, err := a.Send(context.Background(), "reconnect-guard", "thread-1", "synthetic", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}, guard, 1)
	if err != nil || r.Get("status") != "outcome_unknown" || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
