package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test28SendDeadlineRaisesAndSavesBareUnknown(t *testing.T) {
	// This test checks what a send does when its execution budget runs out, so how long the runner
	// takes for the ledger writes and host calls before the guard must not decide it. Timeout is
	// 10ms; on a loaded host the send's own setup outlasted the caller's budget and Send returned
	// HostUnavailable instead of the deadline (CRW-1161). The send runs in a synctest bubble: its
	// budgets are timers on the bubble's clock, which moves only while every goroutine of the bubble
	// waits, so the guard that waits for the deadline still gets it and a stalled runner cannot
	// expire the budgets early (as Test28_BAD_19_GuardBudgets does). Close ends the ledger's
	// goroutines inside the bubble, which synctest requires.
	var settled any
	synctest.Test(t, func(t *testing.T) {
		l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ops.sqlite3"), ledger.Options{Encode: encodeReceipt})
		if err != nil {
			t.Fatal(err)
		}
		rpc := &scriptRPC{answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}, resume()}}
		a := New(Options{RPC: rpc, Ledger: l, Timeout: 10 * time.Millisecond, CallerSlack: time.Second})
		defer func() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		}()
		guard := func(ctx context.Context) (map[string]any, error) { <-ctx.Done(); return nil, ctx.Err() }
		_, err = a.Send(context.Background(), "deadline", "thread", "message", &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}, guard, 1)
		var timeout *delivery.HostError
		if !errors.As(err, &timeout) || timeout.Kind != "TimeoutError" {
			t.Fatalf("caller error %T %v", err, err)
		}
		receipt, err := l.Get(context.Background(), "deadline")
		if err != nil {
			t.Fatal(err)
		}
		if receipt["status"] != "outcome_unknown" {
			t.Fatal(receipt)
		}
		if _, present := receipt["error"]; present {
			t.Fatalf("deadline receipt has error: %v", receipt)
		}
		// The receipt's times are the wall clock's.
		settled, err = withoutWallClock(map[string]any{"caller": map[string]any{"error": timeout.Kind, "detail": timeout.Message}, "receipt": receipt})
		if err != nil {
			t.Fatal(err)
		}
	})
	// The golden is read on the outer t: it keys on that test's name and the order of its checks.
	expectJSON(t, "deadline", settled)
}
