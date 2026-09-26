package delivery

import (
	"testing"
)

func (v *vcu) dispatched() string {
	event := v.queuedEvent(regOpts{recipients: []string{parent, child}})
	v.mustAttempt(event, nil)
	v.clock.Advance(5)
	return event
}

func (v *vcu) verifyPending(now *float64) []any {
	results, err := v.ack.VerifyPendingAcks(v.ctx, v.host, 8, now)
	mustDo(v.t, err)
	return results
}

func runVCUAck(t *testing.T, mode string, goSide func(v *vcu, out map[string]any)) {
	tree := t.TempDir()
	python := runPython(t, tree, "vcu_ack", mode)
	v := newVCU(t, tree)
	out := map[string]any{}
	goSide(v, out)
	for k, want := range python.Out {
		requireSameJSON(t, mode+"."+k, out[k], want)
	}
	requireSameTables(t, v.fixture, python)
}

func TestVCU11_an_offline_ack_is_recorded_intent_and_upgraded_by_a_host(t *testing.T) {
	t.Run("without an adapter", func(t *testing.T) {
		runVCUAck(t, "offline", func(v *vcu, out map[string]any) {
			e := v.dispatched()
			rec, err := v.ack.Acknowledge(v.ctx, e, "parent-own-turn", AckProof(e, "parent-own-turn"), true, nil, nil)
			mustDo(t, err)
			out["ack"] = rec
			if str(rec, "_verified") != "unverified_turn" || v.one("SELECT tier FROM ack_evidence WHERE event_id = ?", e).S("tier") != "unverified" {
				t.Fatalf("ack %v", rec)
			}
		})
	})
	t.Run("an unverified ack produces no verdict", func(t *testing.T) {
		runVCUAck(t, "no_verdict", func(v *vcu, out map[string]any) {
			e := v.dispatched()
			rec, err := v.ack.Acknowledge(v.ctx, e, "parent-own-turn", AckProof(e, "parent-own-turn"), true, nil, nil)
			mustDo(t, err)
			out["ack"] = rec
			out["r"] = v.verdict(e, "verified", "v1", nil, nil, nil)
			if out["r"].(map[string]any)["reason"] != NotAcknowledged {
				t.Fatal("not_acknowledged")
			}
		})
	})
	t.Run("the dispatch turn alone never verifies", func(t *testing.T) {
		runVCUAck(t, "dispatch_turn", func(v *vcu, out map[string]any) {
			e := v.dispatched()
			turn := v.row(e).S("dispatch_turn_id")
			rec, err := v.ack.Acknowledge(v.ctx, e, turn, AckProof(e, turn), true, nil, nil)
			mustDo(t, err)
			out["ack"] = rec
			if str(rec, "_verified") != "unverified_turn" {
				t.Fatal("unverified_turn")
			}
		})
	})
	t.Run("verify_pending_acks upgrades", func(t *testing.T) {
		runVCUAck(t, "upgrade", func(v *vcu, out map[string]any) {
			e := v.pendingAck()
			out["ack"] = v.lastAck
			out["r"] = v.verifyPending(nil)
			if v.row(e).S("state") != Acknowledged {
				t.Fatal("acknowledged")
			}
		})
	})
}

func (v *vcu) pendingAck() string {
	e := v.dispatched()
	v.host.startTurn(parent, "parent-own-turn", "inProgress", "")
	rec, err := v.ack.Acknowledge(v.ctx, e, "parent-own-turn", AckProof(e, "parent-own-turn"), true, nil, nil)
	mustDo(v.t, err)
	v.lastAck = rec
	return e
}

func TestVCU12_a_deferred_ack_promotion_is_rechecked(t *testing.T) {
	t.Run("generation advanced", func(t *testing.T) {
		runVCUAck(t, "advanced", func(v *vcu, out map[string]any) {
			e := v.pendingAck()
			before := v.one("SELECT * FROM acks WHERE event_id = ?", e)
			out["ack"] = v.lastAck
			v.advance()
			out["r"] = v.verifyPending(nil)
			after := v.one("SELECT * FROM acks WHERE event_id = ?", e)
			if v.row(e).S("state") == Acknowledged || after.S("record") != before.S("record") || after.S("verified") != "unverified_turn" || after.I("accepted") != before.I("accepted") {
				t.Fatal("the authored intent is preserved and not promoted")
			}
		})
	})
	t.Run("paused", func(t *testing.T) {
		runVCUAck(t, "paused", func(v *vcu, out map[string]any) {
			v.pendingAck()
			out["ack"] = v.lastAck
			v.setStatusBy("paused", parent)
			out["r"] = v.verifyPending(nil)
		})
	})
	t.Run("an unchanged refusal is not rejournalled", func(t *testing.T) {
		runVCUAck(t, "twice", func(v *vcu, out map[string]any) {
			e := v.pendingAck()
			out["ack"] = v.lastAck
			v.advance()
			out["r"] = v.verifyPending(at(v.clock.Now()))
			first := v.count("SELECT COUNT(*) AS c FROM journal WHERE subject = ? AND kind = 'ack_verification_withheld'", e)
			out["r2"] = v.verifyPending(at(v.clock.Now() + 10000))
			if v.count("SELECT COUNT(*) AS c FROM journal WHERE subject = ? AND kind = 'ack_verification_withheld'", e) != first {
				t.Fatal("rejournalled")
			}
		})
	})
	t.Run("next_check_at", func(t *testing.T) {
		runVCUAck(t, "next_check", func(v *vcu, out map[string]any) {
			e := v.pendingAck()
			out["ack"] = v.lastAck
			v.advance()
			out["r"] = v.verifyPending(at(100))
			if v.one("SELECT next_check_at FROM ack_evidence WHERE event_id = ?", e).F("next_check_at") <= 100 {
				t.Fatal("next_check_at")
			}
			out["r2"] = v.verifyPending(at(101))
		})
	})
	t.Run("promotes once the blocker clears", func(t *testing.T) {
		runVCUAck(t, "cleared", func(v *vcu, out map[string]any) {
			e := v.pendingAck()
			out["ack"] = v.lastAck
			v.setStatusBy("paused", parent)
			out["r"] = v.verifyPending(at(100))
			v.setStatusBy("active", parent)
			out["r2"] = v.verifyPending(at(100000))
			if v.row(e).S("state") != Acknowledged {
				t.Fatal("promoted")
			}
		})
	})
}
