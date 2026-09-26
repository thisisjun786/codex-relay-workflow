package delivery

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_supersession.py SUP-1..SUP-8. The daemon's observation pass (todo 29) is replaced by the
// two calls it makes for a finished child turn: resolve_staged, then annotate_predecessors.

func (f *fixture) advanceTo(number int) {
	turn := "turn-dispatch-" + string(rune('0'+number))
	f.host.startTurn(child, turn, "inProgress", "")
	mustDo(f.t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := OpenGenerationIn(ctx, f.store, f.clock, f.rid, "dispatch-"+string(rune('0'+number)), "needs_changes_revision", turn)
		return err
	}))
}

func (f *fixture) queuedOutcome(outcome string) string {
	rid := f.register(regOpts{})
	var payload Obj
	if outcome == "ready_for_review" {
		payload = f.readyPayload(rid, 1, []string{f.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	} else {
		status := map[string]string{"failed": "failed", "interrupted": "interrupted"}[outcome]
		if status == "" {
			status = "completed"
		}
		payload = f.executionPayload(rid, 1, outcome, 1, assigned(status))
	}
	_, err := f.accept(payload, store.AcceptOptions{})
	mustDo(f.t, err)
	_, err = f.delivery.Enqueue(f.ctx, str(payload, "eventId"), "", "")
	mustDo(f.t, err)
	return str(payload, "eventId")
}

func (f *fixture) declare(successor, older string) {
	receipt, err := f.delivery.Receipt(f.ctx, older)
	mustDo(f.t, err)
	_, err = execSQL(f.ctx, f.store, "UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?", str(receipt, "revisionHash"), successor)
	mustDo(f.t, err)
}

func (f *fixture) item(event string) Obj {
	it, err := f.delivery.SnapshotItem(f.ctx, event)
	mustDo(f.t, err)
	out := Obj{}
	for _, k := range []string{"state", "reported", "phase", "supersededNote", "holdReason"} {
		v, _ := get(it, k)
		out = append(out, F{Key: k, Value: v})
	}
	return out
}

func runSUP(t *testing.T, mode string, goSide func(f *fixture, out map[string]any)) {
	tree := t.TempDir()
	python := runPython(t, tree, "sup", mode)
	f := newFixture(t, tree)
	out := map[string]any{}
	goSide(f, out)
	for k, want := range python.Out {
		requireSameJSON(t, mode+"."+k, out[k], want)
	}
	requireSameTables(t, f, python)
}

func TestSUP01_an_advanced_generation_annotates_older_deliveries_without_rewriting_them(t *testing.T) {
	for _, mode := range []string{"queued", "outstanding", "capped", "withheld_cap", "dispatched", "current", "mark"} {
		t.Run(mode, func(t *testing.T) {
			runSUP(t, mode, func(f *fixture, out map[string]any) {
				e := f.queuedOutcome("ready_for_review")
				switch mode {
				case "outstanding", "current", "mark":
					f.host.script = []string{"transport_unknown"}
					if mode == "mark" {
						f.clock.Advance(3600)
					}
					f.mustAttempt(e, nil)
				case "capped":
					_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = ?, hold_reason = ? WHERE event_id = ?", DeferredBusy, "busy_cap", e)
					mustDo(t, err)
				case "withheld_cap":
					_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = ?, hold_reason = ? WHERE event_id = ?", WithheldPreSend, "presend_cap", e)
					mustDo(t, err)
				case "dispatched":
					f.mustAttempt(e, nil)
				}
				before := f.row(e).S("state")
				f.advanceTo(2)
				if mode == "current" {
					_, err := execSQL(f.ctx, f.store, "DELETE FROM delivery_supersession WHERE event_id = ?", e)
					mustDo(t, err)
					f.advanceTo(3)
					rows, err := all(f.ctx, f.store, "SELECT event_id FROM delivery_supersession")
					mustDo(t, err)
					if len(rows) != 1 || rows[0].S("event_id") != e {
						t.Fatal("only the older generation's outstanding send is annotated")
					}
				}
				if mode == "mark" {
					mustDo(t, f.delivery.MarkSuperseded(f.ctx, e, StaleGeneration))
				}
				if f.row(e).S("state") != before {
					t.Fatal("annotated, never rewritten")
				}
				out["item"] = f.item(e)
			})
		})
	}
}

func TestSUP02_an_annotated_delivery_reports_as_history(t *testing.T) {
	runSUP(t, "dispatched", func(f *fixture, out map[string]any) {
		e := f.queuedOutcome("ready_for_review")
		f.mustAttempt(e, nil)
		f.advanceTo(2)
		it := f.item(e)
		if str(it, "phase") != "superseded:"+StaleGeneration || str(it, "state") != Dispatched {
			t.Fatalf("item %v", it)
		}
		out["item"] = it
	})
}

func TestSUP03_a_final_successor_annotates_its_predecessor(t *testing.T) {
	for _, mode := range []string{"terminal", "queued_pred", "outstanding_pred"} {
		t.Run(mode, func(t *testing.T) {
			runSUP(t, mode, func(f *fixture, out map[string]any) {
				older := f.queuedOutcome("ready_for_review")
				turn := assigned("completed")
				if mode == "outstanding_pred" {
					f.host.startTurn(child, dispatchTurn, "inProgress", "")
					turn = assigned("inProgress")
				}
				if mode != "queued_pred" {
					f.host.script = []string{"transport_unknown"}
					f.mustAttempt(older, nil)
				}
				s := f.readyPayload(f.rid, 1, []string{f.artifact("newer.txt", "the corrected deliverable")}, 2, turn)
				_, err := f.accept(s, store.AcceptOptions{})
				mustDo(t, err)
				f.declare(str(s, "eventId"), older)
				if mode == "outstanding_pred" {
					_, err := f.intake.ResolveStaged(f.ctx, store.TurnReference{ThreadID: child, TurnID: dispatchTurn, Status: "completed"})
					mustDo(t, err)
					mustDo(t, f.delivery.AnnotatePredecessors(f.ctx, str(s, "eventId")))
				} else {
					_, err := f.delivery.Enqueue(f.ctx, str(s, "eventId"), "", "")
					mustDo(t, err)
				}
				it := f.item(older)
				note, _ := get(it, "supersededNote")
				if note == nil || str(note.(Obj), "reason") != SupersededRevision {
					t.Fatalf("item %v", it)
				}
				out["item"] = it
			})
		})
	}
}

func TestSUP04_a_re_emitted_final_receipt_still_reports_its_stage(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "sup", "reemit")
	f := newFixture(t, tree)
	rid := f.register(regOpts{})
	payload := f.readyPayload(rid, 1, []string{f.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	first, err := f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	again, err := f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	want := python.Out["again"].(map[string]any)
	if first.Stage != "final" || !again.Duplicate || again.Stage != "final" || want["_stage"] != "final" || want["_duplicate"] != true {
		t.Fatalf("again %+v", again)
	}
	requireSameJSON(t, "record", loadsObj(again.Record), withoutUnderscored(want))
	requireSameTables(t, f, python)
}

func TestSUP05_a_later_execution_only_outcome_survives_a_final_revision_head(t *testing.T) {
	runSUP(t, "exec_only", func(f *fixture, out map[string]any) {
		reviewable := f.queuedOutcome("ready_for_review")
		f.mustAttempt(reviewable, nil)
		later := f.executionPayload(f.rid, 1, "failed", 1, assigned("failed"))
		_, err := f.accept(later, store.AcceptOptions{})
		mustDo(t, err)
		_, err = f.delivery.Enqueue(f.ctx, str(later, "eventId"), "", "")
		mustDo(t, err)
		var reason string
		mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
			reason, err = f.delivery.SupersessionReason(ctx, str(later, "eventId"))
			return err
		}))
		if reason != "" {
			t.Fatalf("suppressed: %s", reason)
		}
		out["reason"] = nil
		out["state"] = f.row(str(later, "eventId")).S("state")
	})
}

func TestSUP06_enqueueing_clears_the_intent_it_satisfies(t *testing.T) {
	for _, mode := range []string{"intent", "stranded"} {
		t.Run(mode, func(t *testing.T) {
			runSUP(t, mode, func(f *fixture, out map[string]any) {
				e := f.queuedOutcome("ready_for_review")
				if mode == "intent" {
					_, err := execSQL(f.ctx, f.store, "DELETE FROM deliveries WHERE event_id = ?", e)
					mustDo(t, err)
				}
				mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
					return f.delivery.RecordIntentIn(ctx, e, f.rid, "completion", parent, "the relationship was paused", f.clock.Now())
				}))
				row, err := f.delivery.Enqueue(f.ctx, e, "", "")
				mustDo(t, err)
				out["row"] = row
				if f.one("SELECT 1 AS x FROM delivery_intent WHERE event_id = ?", e) != nil {
					t.Fatal("the intent outlived its delivery")
				}
			})
		})
	}
}

func TestSUP07_an_older_generation_is_never_sent(t *testing.T) {
	for _, outcome := range []string{"ready_for_review", "blocked_needs_input", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			runSUP(t, "presend_"+outcome, func(f *fixture, out map[string]any) {
				e := f.queuedOutcome(outcome)
				f.advanceTo(2)
				generations := f.count("SELECT COUNT(*) AS c FROM generations")
				f.clock.Advance(3600)
				record := f.mustAttempt(e, nil)
				out["record"] = record
				if str(record, "supersededReason") != StaleGeneration || f.row(e).S("hold_reason") != StaleGeneration || len(f.host.sends) != 0 || f.count("SELECT COUNT(*) AS c FROM generations") != generations {
					t.Fatalf("record %v", record)
				}
			})
		})
	}
	t.Run("after a busy wait", func(t *testing.T) {
		runSUP(t, "busy_release", func(f *fixture, out map[string]any) {
			e := f.queuedOutcome("ready_for_review")
			f.host.threads[parent].status = "active"
			out["first"] = f.mustAttempt(e, nil)
			f.advanceTo(2)
			f.host.threads[parent].status = "idle"
			f.clock.Advance(3600)
			out["record"] = f.mustAttempt(e, nil)
			if len(f.host.sends) != 0 {
				t.Fatal("the wait ending is not permission to send")
			}
		})
	})
}

func TestSUP08_a_newer_final_revision_supersedes_and_a_staged_one_does_not(t *testing.T) {
	t.Run("final successor", func(t *testing.T) {
		runSUP(t, "newer", func(f *fixture, out map[string]any) {
			older := f.queuedOutcome("ready_for_review")
			n := f.readyPayload(f.rid, 1, []string{f.artifact("newer.txt", "the corrected deliverable")}, 2, assigned("completed"))
			_, err := f.accept(n, store.AcceptOptions{})
			mustDo(t, err)
			f.declare(str(n, "eventId"), older)
			_, err = f.delivery.Enqueue(f.ctx, str(n, "eventId"), "", "")
			mustDo(t, err)
			f.clock.Advance(3600)
			out["record"] = f.mustAttempt(older, nil)
			f.clock.Advance(3600)
			out["sent"] = f.mustAttempt(str(n, "eventId"), nil)
		})
	})
	t.Run("staged successor", func(t *testing.T) {
		runSUP(t, "staged_successor", func(f *fixture, out map[string]any) {
			older := f.queuedOutcome("ready_for_review")
			st := f.readyPayload(f.rid, 1, []string{f.artifact("staged.txt", "still being written")}, 2, assigned("inProgress"))
			_, err := f.accept(st, store.AcceptOptions{})
			mustDo(t, err)
			f.clock.Advance(3600)
			record := f.mustAttempt(older, nil)
			if str(record, "deliveryState") != Dispatched {
				t.Fatal("a staged claim is not a replacement")
			}
			out["record"] = record
		})
	})
}
