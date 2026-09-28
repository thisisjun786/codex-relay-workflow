package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test24_SCH_35_OmissionRequiresReading(t *testing.T) {
	f := fixture24(t)
	o := f.obligation(t)
	o.Kind = "unreported"
	o.Subject = "turn-without-report"
	o.ID = hash32(o.Kind + "|" + o.RelationID + "|" + o.Subject)
	o.Basis = map[string]any{"table": nil, "schema": "reporting-observation/1", "turn": o.Subject}
	_, err := f.c.Stage(f.ctx, o, "", f.at)
	if err == nil {
		t.Fatal("omission without reading staged")
	}
}
func Test24_SCH_36_ReadbackAssertsRecipientOrRefuses(t *testing.T) {
	f, h, id, _ := delivered24(t)
	_, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "intruder", h, 1_700_000_002)
	var refused Refusal
	if !errors.As(err, &refused) || refused.Reason != "recipient_not_authorized" {
		t.Fatalf("foreign assertion %v", err)
	}
	read, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "supervisor", h, 1_700_000_002)
	if err != nil || read["assertedBy"] != "supervisor" {
		t.Fatalf("recipient assertion %v %v", read, err)
	}
}
func Test24_SCH_37_RenderedReadbackCommandIsComplete(t *testing.T) {
	f, h, id, _ := delivered24(t)
	bytes := h.sends[0]
	if !strings.Contains(bytes, "--socket YOUR_RELAY_SOCKET supervisor-read --message "+id) || !strings.Contains(bytes, "--turn YOUR_TURN_ID --proof YOUR_PROOF --as supervisor") || !strings.Contains(bytes, "Full record: "+f.c.Program) {
		t.Fatalf("rendered lines %q", bytes)
	}
}
func Test24_SCH_39_AttemptedReportIsNeverReaddressed(t *testing.T) {
	f, h, id, _ := delivered24(t)
	_ = h
	o := f.obligation(t)
	if err := f.s.ArchiveScopeBinding(f.ctx, "b-supervisor", "archived", "b-successor", f.at); err != nil {
		t.Fatal(err)
	}
	b := store.ScopeBindingsRow{BindingID: "b-successor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "successor", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t2", UpdatedAt: "t2"}
	if err := f.s.InsertScopeBinding(f.ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RepointScopeLink(f.ctx, "lnk-project", "active", "parent", "successor", f.at); err != nil {
		t.Fatal(err)
	}
	_, err := f.c.Stage(f.ctx, o, "", f.at)
	var refused Refusal
	if !errors.As(err, &refused) || refused.Reason != "relation_owner_drift" {
		t.Fatalf("attempted drift %v", err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.RecipientTaskID != "supervisor" {
		t.Fatalf("frozen %+v %v", row, err)
	}
}
func Test24_SCH_40_RateIsSharedWithDelivery(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	service := delivery.NewService(f.s, delivery.SystemClock{})
	if refused, err := service.ReserveSend(f.ctx, "supervisor", 1_700_000_000); err != nil || refused != "" {
		t.Fatalf("delivery reserve %q %v", refused, err)
	}
	result, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_001)
	if err != nil || result != nil {
		t.Fatalf("supervisor paced %v %v", result, err)
	}
}
