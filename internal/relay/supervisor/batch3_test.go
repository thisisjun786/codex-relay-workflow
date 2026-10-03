package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test24_SCH_27_UnsettledReadbackCanBeReplaced(t *testing.T) {
	t.Parallel()
	f, h, id, _ := delivered24(t)
	first, err := f.c.ReadBack(f.ctx, id, "missing", Proof(id, "missing"), "", h, 1_700_000_002)
	if err != nil || first["verified"] != "turn_not_found" {
		t.Fatalf("first %v %v", first, err)
	}
	second, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_003)
	if err != nil || second["verified"] != "host_read" || second["recorded"] != true {
		t.Fatalf("second %v %v", second, err)
	}
	var count int
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_readbacks WHERE message_id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows %d %v", count, err)
	}
}
func Test24_SCH_28_UncertainAttemptWithoutTurnHasUnknownOrigin(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	h := &sendHost{status: "idle", outcome: "unknown"}
	result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || result["deliveryState"] != "held_uncertain" {
		t.Fatalf("send %v %v", result, err)
	}
	read, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil || read["verified"] != "host_read" || read["turnOrigin"] != "unknown" {
		t.Fatalf("read %v %v", read, err)
	}
}
func Test24_SCH_29_ClaimReadsPacingUnderTransaction(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	_, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO recipient_rate(recipient_task_id,window_start,sends,last_send_at) VALUES ('supervisor',?,12,?)", float64(1699999200), float64(1_699_999_999))
	if err != nil {
		t.Fatal(err)
	}
	h := &sendHost{status: "idle"}
	result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || result != nil {
		t.Fatalf("paced %v %v", result, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "queued" || row.AttemptCount != 0 {
		t.Fatalf("row %v %v", row, err)
	}
}
func Test24_SCH_30_ExistingTurnPredatesSend(t *testing.T) {
	t.Parallel()
	f, h, id, _ := delivered24(t)
	h.turns = map[string]float64{"turn-supervisor-1": 1_699_999_990}
	read, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil || read["verified"] != "turn_predates_send" || read["turnOrigin"] != "relay_opened" {
		t.Fatalf("read %v %v", read, err)
	}
}
func Test24_SCH_31_ArchivedRecipientRecovers(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	h := &sendHost{status: "idle", archived: true}
	result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || result != nil {
		t.Fatalf("first %v %v", result, err)
	}
	h.archived = false
	result, err = f.c.Attempt(f.ctx, id, h, 1_700_000_060)
	if err != nil || result["deliveryState"] != "dispatched" {
		t.Fatalf("recovered %v %v", result, err)
	}
}
func Test24_SCH_32_SendReceiptReportsActualSend(t *testing.T) {
	t.Parallel()
	f, h, id, _ := delivered24(t)
	_ = f
	if len(h.sends) != 1 || !strings.Contains(h.sends[0], id) {
		t.Fatalf("host sends %+v", h.sends)
	}
}
func Test24_SCH_33_ClaimRechecksHierarchy(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "b-next", f.at); err != nil {
		t.Fatal(err)
	}
	_, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
	var refused Refusal
	if !errors.As(err, &refused) || refused.Reason != "unregistered_scope" {
		t.Fatalf("handover %v", err)
	}
}
