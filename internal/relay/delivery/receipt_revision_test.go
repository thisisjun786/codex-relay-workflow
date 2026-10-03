package delivery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func checkRevisionReceipt(t *testing.T, s *store.Store, q ReceiptQuery, evidence, event string) {
	t.Helper()
	ctx := context.Background()
	viaStore, storeReadable, storeErr := LookupStoredReceipt(ctx, s, q)
	viaPath, pathReadable, pathErr := LookupStoredReceiptAt(ctx, s.Path, nil, time.Second, q)
	for _, got := range []struct {
		answer   Obj
		readable bool
		err      error
	}{{viaStore, storeReadable, storeErr}, {viaPath, pathReadable, pathErr}} {
		if got.err != nil || !got.readable || got.answer.Get("evidence") != evidence || got.answer.Get("atCurrentHead") != (evidence == "at_head") {
			t.Fatalf("lookup: %v, readable=%v, err=%v; want %s", got.answer, got.readable, got.err, evidence)
		}
		if event != "" && got.answer.Get("eventId") != event {
			t.Fatalf("head event: %v, want %s", got.answer, event)
		}
		if evidence == "registration_generation_mismatch" && !strings.HasPrefix(pyjson.Text(got.answer.Get("detail")), "the assignment registered generation ") {
			t.Fatal("registration mismatch lost its original detail", got.answer)
		}
	}
}

func TestRevisionReceiptRequiresTheOriginalRegistrationAndEveryRelayLink(t *testing.T) {
	t.Parallel()
	v := newVCU(t, "")
	request := v.requestCorrection(v.acknowledged(""))
	requestID := request.Get("eventId").(string)
	ready := v.emitCorrection(nil, "")
	event := ready.Get("eventId").(string)
	q := ReceiptQuery{Relationship: v.rid, Session: child, Turn: ready.Get("turnRef").(Obj).Get("turnId"), Generation: int64(1), Dispatch: "dispatch-1"}
	checkRevisionReceipt(t, v.store, q, "at_head", event)
	for _, stamp := range []any{"1", 1.5, false, int64(0), int64(3), []any{int64(1)}} {
		changed := q
		changed.Generation = stamp
		checkRevisionReceipt(t, v.store, changed, "registration_generation_mismatch", "")
	}
	for _, dispatch := range []any{nil, "other-dispatch", "", "dispatch-\xff", int64(1)} {
		changed := q
		changed.Dispatch = dispatch
		checkRevisionReceipt(t, v.store, changed, "registration_generation_mismatch", "")
	}
	for _, c := range []struct{ name, table, column, value, original string }{
		{"new-assignment", "generations", "reason", "initial_assignment", "needs_changes_revision"},
		{"wrong-dispatch", "generations", "dispatch_request_id", "revision-foreign", "revision-" + requestID},
		{"wrong-relationship", "events", "relationship_id", "rel-other", v.rid},
		{"wrong-generation", "events", "execution_generation", "3", "2"},
		{"wrong-producer", "events", "producer", "child", "relay"},
		{"wrong-outcome", "events", "outcome", "decision_reply", "revision_request"},
		{"not-final", "events", "stage", "staged", "final"},
	} {
		t.Run(c.name, func(t *testing.T) {
			where, id := "event_id = ?", requestID
			if c.table == "generations" {
				where, id = "relationship_id = ? AND execution_generation = 2", v.rid
			}
			statement := "UPDATE " + c.table + " SET " + c.column + " = ? WHERE " + where
			_, err := execSQL(v.ctx, v.store, statement, c.value, id)
			mustDo(t, err)
			checkRevisionReceipt(t, v.store, q, "registration_generation_mismatch", "")
			_, err = execSQL(v.ctx, v.store, statement, c.original, id)
			mustDo(t, err)
		})
	}
	_, err := execSQL(v.ctx, v.store, "UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?", requestID)
	mustDo(t, err)
	checkRevisionReceipt(t, v.store, q, "registration_generation_mismatch", "")
	_, err = execSQL(v.ctx, v.store, "UPDATE events SET suppressed_reason = NULL WHERE event_id = ?", requestID)
	mustDo(t, err)
	changed := q
	changed.Turn = "another-turn"
	checkRevisionReceipt(t, v.store, changed, "head_belongs_to_another_turn", "")
	_, err = execSQL(v.ctx, v.store, "UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?", event)
	mustDo(t, err)
	checkRevisionReceipt(t, v.store, q, "no_reviewable_revision", "")
	_, err = execSQL(v.ctx, v.store, "UPDATE events SET suppressed_reason = NULL WHERE event_id = ?", event)
	mustDo(t, err)
	v.artifact("out.txt", "changed after receipt")
	checkRevisionReceipt(t, v.store, q, "artifacts_changed_since_receipt", event)
}

func TestRevisionReceiptContinuesAcrossRepeatedCorrections(t *testing.T) {
	t.Parallel()
	v := newVCU(t, "")
	request := v.requestCorrection(v.acknowledged(""))
	second := v.emitCorrection(nil, "second")
	v.acknowledgeCorrection(second)
	v.requestCorrection(second.Get("eventId").(string))
	third := v.emitCorrection(nil, "third")
	q := ReceiptQuery{Relationship: v.rid, Session: child, Turn: third.Get("turnRef").(Obj).Get("turnId"), Generation: int64(1), Dispatch: "dispatch-1"}
	checkRevisionReceipt(t, v.store, q, "at_head", third.Get("eventId").(string))
	_, err := execSQL(v.ctx, v.store, "UPDATE events SET producer = 'child' WHERE event_id = ?", request.Get("eventId"))
	mustDo(t, err)
	checkRevisionReceipt(t, v.store, q, "registration_generation_mismatch", "")
}

func TestRevisionReceiptSkipsAnUnsentWithdrawnGeneration(t *testing.T) {
	t.Parallel()
	v := newVCU(t, "")
	first := v.acknowledged("")
	_, err := OpenGenerationIn(v.ctx, v.store, v.clock, v.rid, "unsent-dispatch", "initial_assignment", nil)
	mustDo(t, err)
	_, err = execSQL(v.ctx, v.store, "INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES ('fixture-plan', 'fixture-project', 'parent', ?)", v.clock.ISO())
	mustDo(t, err)
	_, err = execSQL(v.ctx, v.store, `INSERT INTO dag_generation_withdrawals
(relationship_id, execution_generation, plan_id, node_id, dispatch_request_id, opened_reason, restored_generation, reason, withdrawn_by_task_id, withdrawn_at)
VALUES (?, 2, 'fixture-plan', 'fixture-node', 'unsent-dispatch', 'initial_assignment', 1, 'not sent', 'parent', ?)`, v.rid, v.clock.ISO())
	mustDo(t, err)
	_, err = execSQL(v.ctx, v.store, "UPDATE relationships SET execution_generation = 1 WHERE relationship_id = ?", v.rid)
	mustDo(t, err)
	v.requestCorrection(first)
	ready := v.emitCorrection(nil, "after withdrawal")
	if ready.Get("executionGeneration") != int64(3) {
		t.Fatal("withdrawn ordinal was reused", ready)
	}
	q := ReceiptQuery{Relationship: v.rid, Session: child, Turn: ready.Get("turnRef").(Obj).Get("turnId"), Generation: int64(1), Dispatch: "dispatch-1"}
	checkRevisionReceipt(t, v.store, q, "at_head", ready.Get("eventId").(string))
}

func TestRevisionReceiptContinuesAfterAnAdvancingDecision(t *testing.T) {
	t.Parallel()
	d := newDecWorld(t)
	decision := d.mustReply(DecisionSplitApproval, "continue the reduced scope", pyjson.Text(d.setDigest()))
	if _, err := BindAnchor(d.ctx, d.store, d.clock, d.rid, 2, "decision-anchor"); err != nil {
		t.Fatal(err)
	}
	ready := d.readyPayload(d.rid, 2, []string{d.artifact("out2.txt", "reduced scope")}, 1, turnRef{child, "decision-anchor", "inProgress"})
	staged, err := d.accept(ready, store.AcceptOptions{})
	if err != nil || staged.Stage != store.StageStaged {
		t.Fatalf("decision receipt: %+v, %v", staged, err)
	}
	q := ReceiptQuery{Relationship: d.rid, Session: child, Turn: "decision-anchor", Generation: int64(1), Dispatch: "dispatch-1"}
	checkRevisionReceipt(t, d.store, q, "at_head", ready.Get("eventId").(string))
	_, err = execSQL(d.ctx, d.store, "UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?", decision.Get("eventId"))
	mustDo(t, err)
	checkRevisionReceipt(t, d.store, q, "registration_generation_mismatch", "")
}
