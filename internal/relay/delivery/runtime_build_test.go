package delivery

import (
	"os"
	"testing"
)

func TestAttemptRuntimeBuildRecordedBeforeTransport(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	executable, err := os.Executable()
	mustDo(t, err)
	host := &hooked{Adapter: f.host, send: func(request, thread, message string, settings *TaskSettings) (Obj, error) {
		row, err := one(f.ctx, f.store, "SELECT record FROM attempts WHERE request_id=?", request)
		mustDo(t, err)
		identity, ok := loadsObj(row.S("record")).Get("runtime").(Obj)
		if !ok || identity.Get("build") != "dev" || identity.Get("executable") != executable {
			t.Fatalf("in-flight runtime %v, executable %s", identity, executable)
		}
		return f.host.SendMessage(f.ctx, request, thread, message, settings)
	}}
	out, err := f.delivery.Attempt(f.ctx, event, host, nil, "")
	mustDo(t, err)
	identity, ok := out.Get("runtime").(Obj)
	if !ok || identity.Get("build") != "dev" || identity.Get("executable") != executable {
		t.Fatalf("settled runtime %v", out.Get("runtime"))
	}
	stored, err := one(f.ctx, f.store, "SELECT record FROM attempts WHERE event_id=?", event)
	mustDo(t, err)
	requireSameJSON(t, "stored runtime", loadsObj(stored.S("record")).Get("runtime"), jsonable(identity))
}

func TestReconciliationKeepsSenderRuntimeAndLegacyAbsence(t *testing.T) {
	t.Parallel()
	for _, age := range []string{"in_flight", "settled", "legacy_null", "legacy_record"} {
		for _, path := range []string{"accepted", "presend", "token", "none"} {
			t.Run(age+"/"+path, func(t *testing.T) {
				f := newFixture(t, "")
				event := f.queuedEvent(regOpts{})
				f.host.script = []string{"in_progress"}
				first := f.mustAttempt(event, nil)
				request := first.Get("requestId").(string)
				sender := Obj{{Key: "build", Value: "sender-build"}, {Key: "executable", Value: "/synthetic/sender/crw"}}
				record := append(Obj{}, first...)
				record = record.Set("runtime", sender)
				var stored any = dumps(record)
				if age == "legacy_null" {
					stored = nil
				} else if age == "legacy_record" {
					legacy := Obj{}
					for _, field := range record {
						if field.Key != "runtime" {
							legacy = append(legacy, field)
						}
					}
					stored = dumps(legacy)
				}
				state := "settled"
				if age == "in_flight" || age == "legacy_null" {
					state = "in_flight"
				}
				_, err := execSQL(f.ctx, f.store, "UPDATE attempts SET internal_state=?, record=? WHERE request_id=?", state, stored, request)
				mustDo(t, err)
				attempt, err := one(f.ctx, f.store, "SELECT * FROM attempts WHERE request_id=?", request)
				mustDo(t, err)
				delivery := f.row(event)
				f.clock.Advance(10)
				rc := NewReconciler(f.delivery)
				var out Obj
				switch path {
				case "accepted", "presend":
					receipt := Obj{{Key: "status", Value: Accepted}, {Key: "turnId", Value: "recipient-turn"}}
					evidence := ReceiptTurnID
					if path == "presend" {
						receipt = Obj{{Key: "status", Value: FailedStatus}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "thread_busy"}}}}
						evidence = ConfirmedPreSendRejection
					}
					out, err = rc.settleFromReceipt(f.ctx, attempt, delivery, Classify(receipt), evidence, "fixture", f.clock.Now(), false, nil, nil)
				case "token":
					out, err = rc.settleFromScan(f.ctx, attempt, delivery, TokenScan{Found: true, TurnID: "recipient-turn"}, "fixture", "token found", f.clock.Now())
				case "none":
					out, err = rc.stayHeld(f.ctx, attempt, delivery, "fixture", "not scanned", nil)
				}
				mustDo(t, err)
				after := out.Get("record").(Obj)
				runtime, present := after.Lookup("runtime")
				old := age == "legacy_null" || age == "legacy_record"
				if present == old {
					t.Fatalf("runtime presence %v, legacy %v: %v", present, old, after)
				}
				if !old {
					requireSameJSON(t, "sender identity", runtime, jsonable(sender))
				}
				wantAt := f.clock.ISO()
				if age == "settled" || age == "legacy_record" {
					if path == "token" || path == "none" {
						wantAt = first.Get("observedAt").(string)
					}
				}
				if after.Get("observedAt") != wantAt {
					t.Fatalf("observedAt %v, want %s", after.Get("observedAt"), wantAt)
				}
				mustDo(t, AssertAttemptInvariants(after))
				view, err := f.delivery.SnapshotItem(f.ctx, event)
				mustDo(t, err)
				if view.Get("eventId") != event {
					t.Fatal(view)
				}
			})
		}
	}
}
