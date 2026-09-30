package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test28SendDeadlineRaisesAndSavesBareUnknown(t *testing.T) {
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ops.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	rpc := &scriptRPC{answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}, resume()}}
	a := New(Options{RPC: rpc, Ledger: l, Timeout: 10 * time.Millisecond, CallerSlack: time.Second})
	defer a.Close()
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
	settled, err := withoutWallClock(map[string]any{"caller": map[string]any{"error": timeout.Kind, "detail": timeout.Message}, "receipt": receipt})
	if err != nil {
		t.Fatal(err)
	}
	expectJSON(t, "deadline", settled)
}
