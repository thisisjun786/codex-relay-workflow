package delivery

import "testing"

func TestReceiptPullSurvivesReconciliationOfAnEarlierBusyAttempt(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot", true: "guarded write"}[force], func(t *testing.T) {
			f := newFixture(t, "")
			event := f.queuedEvent(regOpts{})
			f.host.script = []string{"busy"}
			f.mustAttempt(event, nil)
			row := f.one("SELECT request_id FROM attempts WHERE event_id=?", event)
			request := row.S("request_id")
			if request == "" {
				t.Fatal("no earlier busy attempt")
			}
			f.host.ledger[request] = Obj{{Key: "status", Value: FailedStatus}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active"}}}, {Key: "retrySafe", Value: true}}
			f.host.threads[parent].status = "active"
			f.host.startTurn(parent, pullTurn, "inProgress", "")
			_, err := f.pull(event, pullTurn, parent, parent)
			mustDo(t, err)
			forceCurrent = force
			defer func() { forceCurrent = false }()
			_, err = NewReconciler(f.delivery).ReconcileAttempt(f.ctx, request, f.host, at(f.clock.Now()))
			mustDo(t, err)
			got := f.row(event)
			if got.S("state") != InboxOnly || got.S("dispatch_evidence") != ReceiptPullEvidence || got.S("dispatch_turn_id") != pullTurn {
				t.Fatalf("old attempt overwrote pull: %+v", got)
			}
		})
	}
}
