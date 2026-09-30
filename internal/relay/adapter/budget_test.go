package adapter

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test28_BAD_19_GuardBudgets(t *testing.T) {
	for _, guardCount := range []int{0, 10} {
		root := t.TempDir()
		answers := []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}, resume()}
		for range guardCount {
			answers = append(answers, page(map[string]any{"id": "thread-1"}))
		}
		answers = append(answers, map[string]any{"turn": map[string]any{"id": "turn-guarded"}})
		rpc := &scriptRPC{answers: answers}
		l, err := ledger.OpenWithOptions(filepath.Join(root, "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
		if err != nil {
			t.Fatal(err)
		}
		a := New(Options{RPC: rpc, Ledger: l, Timeout: 200 * time.Millisecond, CallerSlack: 200 * time.Millisecond})
		t.Cleanup(func() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		})
		settings := &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}
		guard := func(ctx context.Context) (map[string]any, error) {
			for range guardCount {
				if _, err := a.HostCall(ctx, "thread/list", map[string]any{"limit": 1}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}
		receipt, err := a.Send(context.Background(), "send-guarded-budget", "thread-1", "hello", settings, guard, guardCount)
		if err != nil {
			t.Fatal(err)
		}
		invalid := []string{}
		for _, budget := range []any{-1, 11, true, 1.5, nil, "10"} {
			_, err := a.Send(context.Background(), "send-bad-budget", "thread-1", "hello", settings, nil, budget)
			if err == nil {
				t.Fatal("budget accepted", budget)
			}
			invalid = append(invalid, err.Error())
		}
		methods := []string{}
		for _, call := range rpc.calls {
			methods = append(methods, call.([]any)[0].(string))
		}
		execution, caller := a.Budgets(guardCount)
		expectJSON(t, "budget", map[string]any{"receipt": plain(receipt), "calls": methods, "invalid": invalid, "execution": execution.Seconds(), "caller": caller.Seconds()})
	}
}
