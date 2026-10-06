package delivery

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-669 (follow-up to CRW-659): a keeping decision (outcome decision_reply, generationEffect
// stays) that is settled dispatched into a turn admits that turn in the same transaction, so the
// daemon's census reads the continuation turn and observes its end. Before this change the turn the
// reply started was neither the generation's anchor nor admitted, so a continuation that died
// without a receipt was invisible to the parent. The admission is the relay's own record of the
// message it carried: actor relay, evidence bound to the generation's dispatch turn, and no parent
// intervention. These tests use temporary synthetic stores and a fake host only.

// admitBound is the evidence an admission against generation 1's anchor carries.
const admitBound = "explicit_admission_bound:" + dispatchTurn

// admittedTurn reads the generation_turns row of turn in generation 1, or nil.
func (d *decWorld) admittedTurn(turn string) Row {
	d.t.Helper()
	return d.one("SELECT evidence, actor, detail FROM generation_turns WHERE relationship_id = ? AND execution_generation = 1 AND turn_id = ?", d.rid, turn)
}

// c1: an answer on a receipt the relay saw end, dispatched into the child's continuation turn,
// admits that turn in the same transaction: one row bound to the generation's anchor with actor
// relay and the decision named, one turn_admitted journal row, and no parent intervention. The
// receipt the child then emits from that turn with the claim the message prints is accepted.
func TestDCA01_AKeepingDecisionAdmitsTheTurnItDispatched(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "continue from where you stopped", ""))
	d.mustAttempt(event, nil)
	row := d.row(event)
	if row.S("state") != Dispatched || strings.TrimSpace(row.S("dispatch_turn_id")) == "" {
		t.Fatalf("the decision delivery = %v, want dispatched with a turn id", row)
	}
	turn := row.S("dispatch_turn_id")

	admitted := d.admittedTurn(turn)
	if admitted == nil {
		t.Fatalf("the turn the decision was dispatched into (%s) was not admitted: no generation_turns row", turn)
	}
	if admitted.S("evidence") != admitBound || admitted.S("actor") != "relay" || admitted.S("detail") != "decision reply "+event {
		t.Fatalf("the admission = %v, want evidence %q, actor relay and detail %q", admitted, admitBound, "decision reply "+event)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ? AND execution_generation = 1 AND turn_id = ?", d.rid, turn); n != 1 {
		t.Fatalf("generation_turns rows for the dispatched turn: %d, want 1", n)
	}
	entry := d.one("SELECT detail FROM journal WHERE kind = 'turn_admitted' AND subject = ?", turn)
	if entry == nil || !strings.Contains(entry.S("detail"), "relay") {
		t.Fatalf("the turn_admitted journal row = %v, want one naming actor relay", entry)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'turn_admitted' AND subject = ?", turn); n != 1 {
		t.Fatalf("turn_admitted journal rows: %d, want 1", n)
	}
	// the relay carried the message, so nothing here is a direct parent intervention
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'direct_parent_intervention'"); n != 0 {
		t.Fatalf("direct_parent_intervention journal rows: %d, want none", n)
	}

	// the child's receipt from the admitted turn is accepted with the claim the message prints
	payload := d.executionPayload(d.rid, 1, "failed", 3, turnRef{child, turn, "completed"})
	if _, err := d.accept(payload, store.AcceptOptions{Continuation: d.claimJSON(dispatchTurn)}); err != nil {
		t.Fatalf("a receipt from the admitted continuation turn with the continuation claim: %v", err)
	}
}

// c1: the admission is ON CONFLICT DO NOTHING. A row that is already there (an earlier claim of
// the same turn) keeps its evidence, actor and detail, and no second row is written.
func TestDCA02_AnAdmissionThatAlreadyExistsIsLeftAsItIs(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	// The continuation turn is one the host already holds, so its id is known before the send.
	d.host.startTurn(child, "turn-decision-2", "inProgress", "")
	d.host.script = []string{"steer_existing"}
	d.exec("INSERT INTO generation_turns(relationship_id, execution_generation, turn_id, evidence, actor, detail, admitted_at) VALUES(?,1,'turn-decision-2','child_claim','child','the child claimed it','2023-11-14T22:13:20Z')", d.rid)

	d.mustAttempt(event, nil)
	if got := d.row(event).S("dispatch_turn_id"); got != "turn-decision-2" {
		t.Fatalf("the decision was dispatched into %q, want turn-decision-2", got)
	}
	admitted := d.admittedTurn("turn-decision-2")
	if admitted == nil || admitted.S("evidence") != "child_claim" || admitted.S("actor") != "child" || admitted.S("detail") != "the child claimed it" {
		t.Fatalf("the existing admission was rewritten: %v", admitted)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ? AND execution_generation = 1 AND turn_id = 'turn-decision-2'", d.rid); n != 1 {
		t.Fatalf("generation_turns rows for the turn: %d, want 1", n)
	}
}

// c1: only a keeping decision admits. A decision that opens a generation carries the child into a
// new generation the scheduler binds separately, and a completion delivery to the parent is no
// decision at all: neither admits the turn it was dispatched into.
func TestDCA03_OnlyAKeepingDecisionAdmitsItsTurn(t *testing.T) {
	t.Run("split_approval", func(t *testing.T) {
		d := newDecWorld(t)
		event := decEvent(d.mustReply(DecisionSplitApproval, "keep the endpoint half", pyjson.Text(d.setDigest())))
		d.mustAttempt(event, nil)
		turn := d.row(event).S("dispatch_turn_id")
		if turn == "" {
			t.Fatalf("the advancing decision was not dispatched")
		}
		if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ? AND turn_id = ?", d.rid, turn); n != 0 {
			t.Fatalf("an advancing decision admitted its turn: %d rows", n)
		}
	})
	t.Run("scope_change", func(t *testing.T) {
		d := newDecWorld(t)
		event := decEvent(d.mustReply(DecisionScopeChange, "keep the endpoint half", pyjson.Text(d.setDigest())))
		d.mustAttempt(event, nil)
		turn := d.row(event).S("dispatch_turn_id")
		if turn == "" {
			t.Fatalf("the advancing decision was not dispatched")
		}
		if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ? AND turn_id = ?", d.rid, turn); n != 0 {
			t.Fatalf("an advancing decision admitted its turn: %d rows", n)
		}
	})
	t.Run("a completion to the parent", func(t *testing.T) {
		d := newDecWorld(t)
		d.mustAttempt(d.blocked, nil)
		turn := d.row(d.blocked).S("dispatch_turn_id")
		if turn == "" {
			t.Fatalf("the completion was not dispatched")
		}
		if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ? AND turn_id = ?", d.rid, turn); n != 0 {
			t.Fatalf("a completion delivery admitted its turn: %d rows", n)
		}
	})
}

// c1: only a settle that is dispatched admits. A keeping decision whose send was deferred busy,
// withheld before the send, or left in the recipient's inbox started no turn, so there is nothing
// to admit.
func TestDCA04_OnlyADispatchedSettleAdmits(t *testing.T) {
	for _, c := range []struct{ name, script, state string }{
		{"deferred_busy", "busy", DeferredBusy},
		{"withheld_pre_send", "read_fail", WithheldPreSend},
		{"inbox_only", "approval_policy", InboxOnly},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newObsWorld(t, "interrupted")
			event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
			d.host.script = []string{c.script}
			d.mustAttempt(event, nil)
			if got := d.row(event).S("state"); got != c.state {
				t.Fatalf("the decision delivery is %s, want %s", got, c.state)
			}
			if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ?", d.rid); n != 0 {
				t.Fatalf("a %s settle admitted a turn: %d rows", c.state, n)
			}
			if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'turn_admitted'"); n != 0 {
				t.Fatalf("a %s settle journalled an admission: %d rows", c.state, n)
			}
		})
	}
}

// c1: the admission is bound to the generation's own dispatch turn. A generation that has none
// admits nothing, so no row can name an anchor that does not exist.
func TestDCA05_AGenerationWithoutADispatchTurnAdmitsNothing(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	d.exec("UPDATE generations SET dispatch_turn_id = NULL WHERE relationship_id = ? AND execution_generation = 1", d.rid)
	d.mustAttempt(event, nil)
	turn := d.row(event).S("dispatch_turn_id")
	if turn == "" {
		t.Fatalf("the decision was not dispatched")
	}
	if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ? AND execution_generation = 1 AND turn_id = ?", d.rid, turn); n != 0 {
		t.Fatalf("a generation with no dispatch turn admitted %d rows", n)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'turn_admitted' AND subject = ?", turn); n != 0 {
		t.Fatalf("a generation with no dispatch turn journalled %d admissions", n)
	}
}

// c1: a decision whose send response was lost is recovered by the daemon's reconciliation, which
// promotes the delivery to dispatched from the turn it finds in the child's thread. That is the same
// settle as a send, so it admits the continuation turn too: whether the first transport response
// arrived does not decide whether the parent can see that turn's end.
func TestDCA07_ARecoveredDecisionAdmitsItsContinuationTurn(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	// the send's response is lost: the attempt is left held_uncertain, with no turn id
	d.host.script = []string{"in_progress"}
	record := d.mustAttempt(event, nil)
	request := pyjson.Text(record.Get("requestId"))
	if state := pyjson.Text(record.Get("deliveryState")); state != HeldUncertain {
		t.Fatalf("the lost send settled %s, want held_uncertain", state)
	}
	if turn := d.row(event).S("dispatch_turn_id"); turn != "" {
		t.Fatalf("the lost send recorded turn %q, want none", turn)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ?", d.rid); n != 0 {
		t.Fatalf("a lost send admitted %d turns", n)
	}

	// the message did reach the child, in a turn that carries the request id
	turn := d.host.startTurn(child, "turn-recovered", "inProgress", "[codex-session-relay] parent decision\nrequestId: "+request+"\n")
	now := d.clock.Now()
	if _, err := NewReconciler(d.delivery).ReconcileAttempt(d.ctx, request, d.host, &now); err != nil {
		t.Fatalf("the recovery: %v", err)
	}
	row := d.row(event)
	if row.S("state") != Dispatched || row.S("dispatch_turn_id") != turn.TurnID {
		t.Fatalf("the recovered delivery = %v, want dispatched into %s", row, turn.TurnID)
	}
	admitted := d.admittedTurn(turn.TurnID)
	if admitted == nil {
		t.Fatalf("the recovered turn %s was not admitted: no generation_turns row", turn.TurnID)
	}
	if admitted.S("evidence") != admitBound || admitted.S("actor") != "relay" || admitted.S("detail") != "decision reply "+event {
		t.Fatalf("the admission of the recovered turn = %v, want evidence %q, actor relay and detail %q", admitted, admitBound, "decision reply "+event)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'turn_admitted' AND subject = ?", turn.TurnID); n != 1 {
		t.Fatalf("turn_admitted journal rows for the recovered turn: %d, want 1", n)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'direct_parent_intervention'"); n != 0 {
		t.Fatalf("direct_parent_intervention journal rows: %d, want none", n)
	}
}

// c1: a recovered turn the child then reports from needs no claim either, exactly as the sent one.
func TestDCA08_ARecoveredTurnIsAcceptedWithoutAClaim(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	d.host.script = []string{"in_progress"}
	record := d.mustAttempt(event, nil)
	request := pyjson.Text(record.Get("requestId"))
	turn := d.host.startTurn(child, "turn-recovered-2", "inProgress", "[codex-session-relay] parent decision\nrequestId: "+request+"\n")
	now := d.clock.Now()
	if _, err := NewReconciler(d.delivery).ReconcileAttempt(d.ctx, request, d.host, &now); err != nil {
		t.Fatalf("the recovery: %v", err)
	}
	payload := d.executionPayload(d.rid, 1, "failed", 4, turnRef{child, turn.TurnID, "completed"})
	if _, err := d.accept(payload, store.AcceptOptions{}); err != nil {
		t.Fatalf("a receipt from the recovered turn without a claim: %v", err)
	}
}

// c1: the admission is in the same transaction as the send it describes. When the settle fails after
// it, the delivery row and the admission roll back together: there is no dispatched delivery whose
// turn nothing admitted, and no admission for a send that did not settle.
func TestDCA09_TheAdmissionRollsBackWithTheSettleThatFailed(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	// The journal row settle writes after the admission is refused, so the whole transaction aborts.
	d.exec("CREATE TEMP TRIGGER crw669_fail_attempted BEFORE INSERT ON journal WHEN NEW.kind = 'delivery_attempted' BEGIN SELECT RAISE(ABORT, 'the settle failed after the admission'); END")
	if _, err := d.attempt(event, nil); err == nil {
		t.Fatal("the settle succeeded although its journal write was refused")
	}
	d.exec("DROP TRIGGER crw669_fail_attempted")
	if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ?", d.rid); n != 0 {
		t.Fatalf("a failed settle left %d generation_turns rows", n)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'turn_admitted'"); n != 0 {
		t.Fatalf("a failed settle left %d turn_admitted journal rows", n)
	}
	if state := d.row(event).S("state"); state == Dispatched {
		t.Fatalf("the delivery is %s although the settle rolled back", state)
	}
}

// c1: the admission follows the event's own generation. A relationship that moved past the decision's
// generation before the send is attempted never reaches the settle: the delivery is superseded as a
// stale generation, so the turn is admitted into no generation at all.
func TestDCA10_ADecisionOnAStaleGenerationAdmitsNothing(t *testing.T) {
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	mustDo(t, d.store.Transaction(d.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := OpenGenerationIn(ctx, d.store, d.clock, d.rid, "other-dispatch", "initial_assignment", nil)
		return err
	}))
	record := d.mustAttempt(event, nil)
	if state := pyjson.Text(record.Get("deliveryState")); state != Superseded {
		t.Fatalf("the decision on a stale generation settled %s, want superseded", state)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM generation_turns WHERE relationship_id = ?", d.rid); n != 0 {
		t.Fatalf("a decision on a stale generation admitted %d turns", n)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'turn_admitted'"); n != 0 {
		t.Fatalf("a decision on a stale generation journalled %d admissions", n)
	}
}
