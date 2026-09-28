package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os/exec"
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
		got := map[string]any{"receipt": plain(receipt), "calls": methods, "invalid": invalid}
		spec := map[string]any{"root": root, "guard": guardCount, "settings": authorized(), "resume": resume()}
		raw, _ := json.Marshal(spec)
		repo, _ := filepath.Abs("../../..")
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/budget_capture.py"))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("oracle %v %s", err, out)
		}
		var want map[string]any
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatal(err)
		}
		if math.Abs(want["execution"].(float64)-execution.Seconds()) > 1e-12 || math.Abs(want["caller"].(float64)-caller.Seconds()) > 1e-12 {
			t.Fatalf("budgets Go %v %v Python %v %v", execution, caller, want["execution"], want["caller"])
		}
		delete(want, "execution")
		delete(want, "caller")
		expected, _ := json.Marshal(want)
		actual, _ := json.Marshal(got)
		if !bytes.Equal(actual, expected) {
			t.Fatalf("Go %s Python %s", actual, expected)
		}
	}
}
