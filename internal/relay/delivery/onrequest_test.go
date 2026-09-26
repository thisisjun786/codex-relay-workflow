package delivery

import (
	"testing"
)

// test_on_request_delivery.py ORD-1..ORD-5 here; ORD-6..ORD-9 in onrequest_adapter_test.go.

func runORD(t *testing.T, mode string, goSide func(f *fixture, out map[string]any)) {
	tree := t.TempDir()
	python := runPython(t, tree, "ord", mode)
	f := newFixture(t, tree)
	out := map[string]any{}
	goSide(f, out)
	for k, want := range python.Out {
		requireSameJSON(t, mode+"."+k, out[k], want)
	}
	if mode != "record" {
		requireSameTables(t, f, python)
	}
}

func usable(data string) map[string]any {
	err := (&TaskSettings{Data: loadsObj(data)}).RequireUsable()
	if err != nil {
		return refusalOf(err)
	}
	return map[string]any{"ok": nil}
}

func TestORD01_an_on_request_record_is_usable_and_untrusted_granular_or_missing_are_refused(t *testing.T) {
	runORD(t, "record", func(f *fixture, out map[string]any) {
		out["onRequest"] = usable(rawSettings("/parent", "on-request"))
		var refused []any
		for _, bad := range []string{
			rawSettings("/parent", "untrusted"),
			replaceApproval(rawSettings("/parent", "X"), `{"granular": {}}`),
			replaceApproval(rawSettings("/parent", "X"), `null`),
		} {
			r := usable(bad)
			switch r["reason"] {
			case UnsupportedApprovalPolicy, SettingsMistyped, SettingsIncomplete:
			default:
				t.Fatalf("refused with %v", r)
			}
			refused = append(refused, r)
		}
		out["refused"] = refused
		out["resume"] = []any{(&TaskSettings{Data: loadsObj(rawSettings("/parent", "never"))}).ResumeParams("t-1"), (&TaskSettings{Data: loadsObj(rawSettings("/parent", "on-request"))}).ResumeParams("t-1")}
	})
}

func replaceApproval(settings, raw string) string {
	o := loadsObj(settings)
	v, _ := loads(raw)
	return dumps(set(o, "approvalPolicy", v))
}

func TestORD02_resume_params_never_carry_an_approval_policy(t *testing.T) {
	for _, policy := range []string{"never", "on-request"} {
		params := (&TaskSettings{Data: loadsObj(rawSettings("/parent", policy))}).ResumeParams("t-1")
		if _, present := get(params, "approvalPolicy"); present {
			t.Fatalf("%s: resume carries approvalPolicy", policy)
		}
	}
	// The whole params object is compared with Python's in ORD-1's "resume" row.
}

func TestORD03_an_on_request_parent_is_woken_once(t *testing.T) {
	runORD(t, "woken", func(f *fixture, out map[string]any) {
		event := f.queuedEvent(regOpts{parentSettings: rawSettings("/parent", "on-request")})
		f.host.threads[parent].approvalPolicy = "on-request"
		record := f.mustAttempt(event, nil)
		out["record"] = record
		f.clock.Advance(100000)
		out["eligible"] = []any{}
		if str(record, "deliveryState") != Dispatched || str(record, "recipientApprovalPolicy") != "on-request" || len(f.host.sends) != 1 || len(f.eligible()) != 0 {
			t.Fatalf("record %v", record)
		}
	})
}

func TestORD04_a_parent_waiting_on_its_approver_is_busy_then_woken_once(t *testing.T) {
	runORD(t, "busy", func(f *fixture, out map[string]any) {
		event := f.queuedEvent(regOpts{parentSettings: rawSettings("/parent", "on-request")})
		f.host.threads[parent].approvalPolicy = "on-request"
		f.host.script = []string{"busy"}
		out["busy"] = f.mustAttempt(event, nil)
		out["record"] = f.mustAttempt(event, at(f.row(event).F("next_eligible_at")))
		started := 0
		for _, s := range f.host.sends {
			if s.outcome == "accepted" {
				started++
			}
		}
		if started != 1 {
			t.Fatalf("%d turn/starts", started)
		}
	})
}

func TestORD05_a_folded_start_settles_once_across_restarts(t *testing.T) {
	runORD(t, "folded", func(f *fixture, out map[string]any) {
		event := f.queuedEvent(regOpts{parentSettings: rawSettings("/parent", "on-request")})
		f.host.threads[parent].approvalPolicy = "on-request"
		existing := f.host.startTurn(parent, "", "inProgress", "")
		f.host.script = []string{"steer_existing"}
		out["record"] = f.mustAttempt(event, nil)
		if f.one("SELECT 1 AS x FROM acks WHERE event_id = ?", event) != nil {
			t.Fatal("dispatch alone is not an acknowledgement")
		}
		ack := NewAck(f.delivery)
		var acks []any
		for i := 0; i < 2; i++ {
			r, err := ack.Acknowledge(f.ctx, event, existing.TurnID, AckProof(event, existing.TurnID), true, nil, f.host)
			mustDo(t, err)
			acks = append(acks, r)
		}
		out["acks"] = acks
		f.host.finishTurn(parent, existing.TurnID, "completed")
		for _, th := range f.host.threads {
			th.status = "notLoaded"
		}
		restarted := NewService(f.store, f.clock)
		f.clock.Advance(100000)
		after, err := restarted.Attempt(f.ctx, event, f.host, at(f.clock.Now()), "")
		mustDo(t, err)
		out["after"] = after
		if f.count("SELECT COUNT(*) AS c FROM acks WHERE event_id = ?", event) != 1 || len(f.host.sends) != 1 {
			t.Fatal("settles once, sends once")
		}
	})
}
