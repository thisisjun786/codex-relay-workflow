package delivery

import (
	"strings"
	"testing"
)

func TestReconcilePass_receipt_gains_turn_id(t *testing.T) {
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"in_progress"}
	request := str(f.mustAttempt(event, nil), "requestId")
	f.host.ledger[request] = Obj{{Key: "status", Value: Accepted}}
	f.clock.Advance(1000)
	rc := NewReconciler(f.delivery)
	pass := func() ReconcileReport {
		t.Helper()
		var report ReconcileReport
		mustDo(t, ReconcilePass(f.ctx, rc, f.host, 8, f.clock.Now(), &report))
		return report
	}
	if report := pass(); report.Reconciled != 1 || f.row(event).S("state") != HeldUncertain {
		t.Fatalf("accepted receipt without turn must remain held: report=%+v delivery=%v", report, f.row(event))
	}
	if report := pass(); report.Reconciled != 0 || report.Skipped != 1 {
		t.Fatalf("unchanged receipt must skip: %+v", report)
	}
	fingerprint := f.one("SELECT fingerprint FROM reconcile_gate WHERE request_id = ?", request).S("fingerprint")
	f.host.ledger[request] = Obj{{Key: "status", Value: Accepted}, {Key: "turnId", Value: "turn-42"}}
	if report := pass(); report.Reconciled != 1 || report.Skipped != 0 {
		t.Fatalf("receipt with new turn must reconcile: %+v", report)
	}
	row := f.row(event)
	if row.S("state") != Dispatched || row.S("dispatch_turn_id") != "turn-42" {
		t.Fatalf("receipt turn must dispatch delivery: %v", row)
	}
	if got := f.one("SELECT fingerprint FROM reconcile_gate WHERE request_id = ?", request).S("fingerprint"); got == fingerprint || !strings.Contains(got, "turn-42") {
		t.Fatalf("fingerprint must track the turn id: before=%q after=%q", fingerprint, got)
	}
}
