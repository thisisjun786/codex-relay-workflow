package delivery

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_anchor_binding.py ANB-1..ANB-8. The daemon tick is todo 29; its binding contract is
// ported here as AnchorPasses (both passes, counted and not assigned, around reconciliation).

type anb struct {
	*vcu
	rc *Reconciler
}

func newANB(t *testing.T, tree string) *anb {
	v := newVCU(t, tree)
	return &anb{v, NewReconciler(v.delivery)}
}

func (a *anb) revisionPending() string {
	e := a.queuedEvent(regOpts{recipients: []string{parent, child}})
	a.mustAttempt(e, nil)
	a.clock.Advance(5)
	turn := a.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err := a.ack.Acknowledge(a.ctx, e, turn.TurnID, AckProof(e, turn.TurnID), true, nil, a.host)
	mustDo(a.t, err)
	_, err = a.ack.RecordVerdict(a.ctx, e, "needs_changes", "verdict-1", nil, nil, nil, nil)
	mustDo(a.t, err)
	return a.one("SELECT event_id FROM deliveries WHERE kind = ?", Revision).S("event_id")
}

func (a *anb) gen2() Obj {
	return generationRecord(a.one("SELECT * FROM generations WHERE relationship_id = ? AND execution_generation = 2", a.rid))
}

func (a *anb) childReceipt() map[string]any {
	anchor := a.one("SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = 2", a.rid).S("dispatch_turn_id")
	if anchor == "" {
		anchor = "turn-unbound"
	}
	payload := a.readyPayload(a.rid, 2, []string{a.artifact("fixed.txt", "the correction")}, 1, turnRef{child, anchor, "completed"})
	stored, err := a.accept(payload, store.AcceptOptions{})
	if err != nil {
		return refusalOf(err)
	}
	record := loadsObj(stored.Record)
	return map[string]any{"ok": append(record, F{Key: "_duplicate", Value: stored.Duplicate}, F{Key: "_pathBindingMode", Value: stored.PathBinding.String}, F{Key: "_stage", Value: stored.Stage})}
}

func (a *anb) lostSettleWrite() (string, string) {
	rev := a.revisionPending()
	a.clock.Advance(3600)
	a.host.script = []string{"in_progress"}
	record := a.mustAttempt(rev, at(a.clock.Now()))
	turn := a.host.startTurn(child, "", "inProgress", "")
	request := str(record, "requestId")
	a.host.ledger[request] = Obj{{Key: "requestId", Value: request}, {Key: "status", Value: "accepted"}, {Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "turnId", Value: turn.TurnID}}
	return rev, turn.TurnID
}

func (a *anb) recover() Obj {
	out, err := a.rc.RecoverOnStart(a.ctx, a.host, nil)
	mustDo(a.t, err)
	return out
}

func (a *anb) bindPending() []any {
	bound, err := a.ack.BindPendingAnchors(a.ctx)
	mustDo(a.t, err)
	return bound
}

func runANB(t *testing.T, mode string, goSide func(a *anb, out map[string]any)) {
	tree := t.TempDir()
	python := runPython(t, tree, "anb", mode)
	a := newANB(t, tree)
	out := map[string]any{}
	goSide(a, out)
	for k, want := range python.Out {
		requireSameJSON(t, mode+"."+k, out[k], want)
	}
	requireSameTables(t, a.fixture, python)
}

func (a *anb) dispatchRevision(script string) string {
	rev := a.revisionPending()
	a.clock.Advance(3600)
	if script != "" {
		a.host.script = []string{script}
	}
	return rev
}

func TestANB01_every_route_to_dispatched_binds_the_new_anchor(t *testing.T) {
	t.Run("a later tick", func(t *testing.T) {
		runANB(t, "tick", func(a *anb, out map[string]any) {
			rev := a.dispatchRevision("")
			out["record"] = a.mustAttempt(rev, at(a.clock.Now()))
			out["pending"] = str(a.gen2(), "anchorState")
			report := &AnchorReport{}
			reconciled := []any{}
			mustDo(t, AnchorPasses(a.ctx, a.ack, report, func() error {
				r := a.recover()
				v, _ := get(r, "reconciled")
				reconciled = v.([]any)
				return nil
			}))
			out["bound"] = []any{report.Passes[0], reconciled, report.Passes[1]}
			out["gen2"] = a.gen2()
			out["receipt"] = a.childReceipt()
		})
	})
	t.Run("the recovery helper", func(t *testing.T) {
		runANB(t, "recovery", func(a *anb, out map[string]any) {
			rev := a.dispatchRevision("")
			out["record"] = a.mustAttempt(rev, at(a.clock.Now()))
			out["pending"] = str(a.gen2(), "anchorState")
			out["bound"], out["again"] = a.bindPending(), a.bindPending()
			out["gen2"] = a.gen2()
			out["receipt"] = a.childReceipt()
		})
	})
	t.Run("a reconcile promotion", func(t *testing.T) {
		runANB(t, "reconcile", func(a *anb, out map[string]any) {
			rev := a.dispatchRevision("transport_unknown")
			out["record"] = a.mustAttempt(rev, at(a.clock.Now()))
			out["promoted"] = a.recover()
			out["bound"] = a.bindPending()
			out["gen2"] = a.gen2()
			out["state"] = a.row(rev).S("state")
		})
	})
	t.Run("inside the promoting transaction", func(t *testing.T) {
		runANB(t, "promotion", func(a *anb, out map[string]any) {
			rev, turn := a.lostSettleWrite()
			out["recovered"] = a.recover()
			out["gen2"] = a.gen2()
			if a.row(rev).S("state") != Dispatched || str(out["gen2"].(Obj), "dispatchTurnId") != turn {
				t.Fatal("bound in the promotion")
			}
			out["receipt"] = a.childReceipt()
		})
	})
}

func TestANB02_an_unbound_generation_refuses_the_childs_receipt(t *testing.T) {
	runANB(t, "unbound", func(a *anb, out map[string]any) {
		rev := a.dispatchRevision("")
		out["record"] = a.mustAttempt(rev, at(a.clock.Now()))
		out["pending"] = str(a.gen2(), "anchorState")
		out["receipt"] = a.childReceipt()
		if out["receipt"].(map[string]any)["reason"] != "unbound_generation" {
			t.Fatal("unbound_generation")
		}
	})
}

func TestANB03_a_disagreeing_promotion_is_recorded_not_swallowed(t *testing.T) {
	runANB(t, "conflict", func(a *anb, out map[string]any) {
		a.lostSettleWrite()
		_, err := BindAnchor(a.ctx, a.store, a.clock, a.rid, 2, "a-different-turn")
		mustDo(t, err)
		out["recovered"] = a.recover()
		out["gen2"] = a.gen2()
		if a.one("SELECT detail FROM journal WHERE kind = 'anchor_conflict'") == nil {
			t.Fatal("the disagreement is journalled")
		}
	})
}

func TestANB04_a_stale_reconciliation_binds_nothing(t *testing.T) {
	runANB(t, "stale", func(a *anb, out map[string]any) {
		rev, _ := a.lostSettleWrite()
		_, err := execSQL(a.ctx, a.store, "UPDATE deliveries SET attempt_count = attempt_count + 1 WHERE event_id = ?", rev)
		mustDo(t, err)
		// Python forces the snapshot check (_is_current) to True; the guarded UPDATE decides.
		forceCurrent = true
		defer func() { forceCurrent = false }()
		out["recovered"] = a.recover()
		out["gen2"] = a.gen2()
		if str(out["gen2"].(Obj), "anchorState") != "anchor_pending" {
			t.Fatal("a stale attempt bound the generation")
		}
	})
}

func TestANB05_a_revision_request_is_retired_once_the_child_answers(t *testing.T) {
	for _, mode := range []string{"retired_ready", "retired_failed", "retired_daemon"} {
		t.Run(mode, func(t *testing.T) {
			runANB(t, mode, func(a *anb, out map[string]any) {
				rev := a.dispatchRevision("")
				out["record"] = a.mustAttempt(rev, at(a.clock.Now()))
				out["pending"] = str(a.gen2(), "anchorState")
				a.bindPending()
				anchor := str(a.gen2(), "dispatchTurnId")
				switch mode {
				case "retired_ready":
					out["receipt"] = a.childReceipt()
				case "retired_failed":
					payload := a.executionPayload(a.rid, 2, "failed", 1, turnRef{child, anchor, "completed"})
					stored, err := a.accept(payload, store.AcceptOptions{})
					mustDo(t, err)
					out["receipt"] = map[string]any{"ok": append(loadsObj(stored.Record), F{Key: "_duplicate", Value: false}, F{Key: "_pathBindingMode", Value: nil}, F{Key: "_stage", Value: stored.Stage})}
				default:
					a.host.startTurn(child, anchor, "failed", "")
					stored, err := a.intake.DaemonObservation(a.ctx, a.rid, store.TurnReference{ThreadID: child, TurnID: anchor, Status: "failed"})
					mustDo(t, err)
					out["receipt"] = append(loadsObj(stored.Record), F{Key: "_duplicate", Value: false}, F{Key: "_pathBindingMode", Value: nil}, F{Key: "_stage", Value: stored.Stage})
				}
				reason, err := a.delivery.SupersessionReason(a.ctx, rev)
				mustDo(t, err)
				out["reason"] = reason
				if reason != SupersededRevision {
					t.Fatal("superseded_revision")
				}
			})
		})
	}
}

func TestANB06_binding_is_idempotent_and_needs_a_dispatch(t *testing.T) {
	for _, mode := range []string{"idempotent", "never"} {
		t.Run(mode, func(t *testing.T) {
			runANB(t, mode, func(a *anb, out map[string]any) {
				script := ""
				if mode == "never" {
					script = "busy"
				}
				rev := a.dispatchRevision(script)
				out["record"] = a.mustAttempt(rev, at(a.clock.Now()))
				out["pending"] = str(a.gen2(), "anchorState")
				out["bound"], out["again"] = a.bindPending(), a.bindPending()
				out["gen2"] = a.gen2()
			})
		})
	}
}

func TestANB07_a_revision_promoted_during_a_tick_binds_in_that_tick(t *testing.T) {
	var order []string
	report := &AnchorReport{}
	f := newANB(t, "")
	mustDo(t, AnchorPasses(f.ctx, anchorBinderFunc(func() ([]any, error) { order = append(order, "bind"); return nil, nil }), report, func() error {
		order = append(order, "reconcile")
		return nil
	}))
	if len(order) != 3 || order[0] != "bind" || order[1] != "reconcile" || order[2] != "bind" {
		t.Fatalf("order %v", order)
	}
}

func TestANB08_both_binding_passes_are_counted(t *testing.T) {
	calls := 0
	report := &AnchorReport{}
	f := newANB(t, "")
	mustDo(t, AnchorPasses(f.ctx, anchorBinderFunc(func() ([]any, error) {
		calls++
		if calls == 1 {
			return []any{"a", "b"}, nil
		}
		return []any{}, nil
	}), report, func() error { return nil }))
	if report.AnchorsBound != 2 {
		t.Fatalf("anchorsBound %d", report.AnchorsBound)
	}
}
