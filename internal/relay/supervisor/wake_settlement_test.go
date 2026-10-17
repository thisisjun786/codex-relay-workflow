package supervisor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type canceledWakeRead struct {
	sendHost
	cancel context.CancelFunc
	read   bool
}

func (*canceledWakeRead) ListTurnIDs(context.Context, string, int) ([]any, error) {
	return nil, nil
}

func (h *canceledWakeRead) ReadTurn(ctx context.Context, _, _ string) (*delivery.TurnInfo, error) {
	h.read = true
	h.cancel()
	return nil, ctx.Err()
}

// CRW-1664: optional wake observation must not discard an accepted delivery.
func TestReviewWakeReadMustNotLoseAcceptedReceipt(t *testing.T) {
	f := flowRecordFixture(t)
	id := stageFlowWake(t, f)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	h := &canceledWakeRead{sendHost: sendHost{status: "notLoaded"}, cancel: cancel}
	result, err := f.c.Attempt(ctx, id, h, flowSteerNow)
	row, readErr := f.c.Get(f.ctx, id)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !h.read || ctx.Err() != context.Canceled || len(h.sends) != 1 {
		t.Fatalf("cancellation did not follow one accepted send: read=%v ctx=%v sends=%d", h.read, ctx.Err(), len(h.sends))
	}
	if err != nil || row.State != delivery.Dispatched {
		t.Fatalf("accepted turn lost during optional evidence read: result=%v err=%v state=%s", result, err, row.State)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || !attempts[0].TurnID.Valid {
		t.Fatalf("accepted receipt not retained: %+v %v", attempts, err)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(attempts[0].Record), &receipt); err != nil || receipt["deliveryState"] != delivery.Dispatched {
		t.Fatalf("wrong durable receipt: %s %v", attempts[0].Record, err)
	}
	var raw string
	if err := f.s.DB.QueryRowContext(f.ctx, `SELECT detail FROM journal WHERE kind=? AND subject=?`, delivery.WakeObserved, result["requestId"]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var wake delivery.WakeAttempt
	if err := json.Unmarshal([]byte(raw), &wake); err != nil || wake.NewTurn || wake.DeliveryState != delivery.Dispatched || wake.TurnID != attempts[0].TurnID.String {
		t.Fatalf("canceled observation claimed proof or lost receipt: %s %v", raw, err)
	}
	if _, err := f.c.Attempt(f.ctx, id, h, flowSteerNow+1); err != nil || len(h.sends) != 1 {
		t.Fatalf("accepted delivery was resent: sends=%d err=%v", len(h.sends), err)
	}
}
