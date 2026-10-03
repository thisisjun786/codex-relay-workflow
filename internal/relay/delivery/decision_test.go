package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The decision reply (CRW-394): a parent returns a decision for a blocked_needs_input receipt, the
// relay records it and the delivery engine carries it to the child, and the kind fixes whether the
// child's generation stays or advances. docs/relay/README.md, "Replying to a blocked receipt", is
// the contract. These tests use temporary synthetic stores and a fake host only.

type decWorld struct {
	*hl
	rid     string
	blocked string
}

// newDecWorld registers a relationship whose child may be written to, registers a criteria set, takes the
// child's blocked_needs_input receipt of generation 1 (emitted from the anchor turn) and queues its
// completion delivery to the parent, as the daemon does once that turn has ended.
func newDecWorld(t *testing.T, recipients ...string) *decWorld {
	t.Helper()
	return newDecWorldWith(t, nil, recipients...)
}

// newDecWorldWith is newDecWorld with a step that runs after the criteria are registered and before the blocked receipt is taken.
func newDecWorldWith(t *testing.T, before func(*decWorld), recipients ...string) *decWorld {
	t.Helper()
	h := newRulingHL(t)
	if len(recipients) == 0 {
		recipients = []string{parent, child}
	}
	rid := h.register(regOpts{recipients: recipients})
	h.registerCriteria(rrSet)
	d := &decWorld{hl: h, rid: rid}
	if before != nil {
		before(d)
	}
	d.blocked = d.emit("blocked_needs_input", "completed", store.AcceptOptions{})
	_, err := h.delivery.Enqueue(h.ctx, d.blocked, "", "")
	mustDo(t, err)
	return d
}

// emit takes one execution-only child receipt of generation 1 and returns its event id. The turn is the
// generation's anchor unless options carry a continuation claim for another one.
func (d *decWorld) emit(outcome, status string, options store.AcceptOptions) string {
	d.t.Helper()
	payload := d.executionPayload(d.rid, 1, outcome, 1, assigned(status))
	_, err := d.accept(payload, options)
	mustDo(d.t, err)
	return pyjson.Text(payload.Get("eventId"))
}

func (d *decWorld) reply(decision, note, digest string) (Obj, error) {
	return d.ack.RecordDecision(d.ctx, DecisionRequest{EventID: d.blocked, Decision: decision, Turn: "decision-turn-1", Note: note, CriteriaDigest: digest})
}

func (d *decWorld) mustReply(decision, note, digest string) Obj {
	d.t.Helper()
	out, err := d.reply(decision, note, digest)
	mustDo(d.t, err)
	return out
}

// refused asks and expects a refusal with reason want that left every row of every table as it was.
func (d *decWorld) refused(decision, note, digest, want string) string {
	d.t.Helper()
	before := d.rcFootprint()
	out, err := d.reply(decision, note, digest)
	if err == nil {
		d.t.Fatalf("a %s reply answered %v, want a %s refusal", decision, out, want)
	}
	if Reason(err) != want {
		d.t.Fatalf("a %s reply was refused %q (%v), want %q", decision, Reason(err), err, want)
	}
	if after := d.rcFootprint(); after != before {
		d.t.Fatalf("a refused reply wrote:\nbefore %s\nafter  %s", before, after)
	}
	return err.Error()
}

// deliver is the scheduler's delivery pass once, with the notes it reports (the tick helper of the harness does not carry them).
func (d *decWorld) deliver() TickCounts {
	d.t.Helper()
	var counts TickCounts
	mustDo(d.t, (&Scheduler{Delivery: d.delivery, Ack: d.ack, MaxSendsTick: 4}).Deliver(d.ctx, d.host, d.clock.Now(), &counts))
	return counts
}

func (d *decWorld) generation() int64 {
	r, err := LoadRelationship(d.ctx, d.store, d.rid)
	mustDo(d.t, err)
	return r.Generation
}

func (d *decWorld) sent() fakeSend {
	d.t.Helper()
	if len(d.host.sends) == 0 {
		d.t.Fatal("nothing was sent")
	}
	return d.host.sends[len(d.host.sends)-1]
}

func (d *decWorld) claimJSON(anchor string) []byte {
	return []byte(dumps(Obj{{Key: "anchorTurnId", Value: anchor}, {Key: "actor", Value: child}, {Key: "reason", Value: "continuing after the parent's decision"}}))
}

func decEvent(out Obj) string { return pyjson.Text(out.Get("eventId")) }

// c1: an answer keeps the generation, is recorded and queued to the child, and the message it sends carries the
// answer and the claim that admits the child's next turn.
func TestDR01_an_answer_keeps_the_generation_and_reaches_the_child(t *testing.T) {
	d := newDecWorld(t)
	out := d.mustReply(DecisionAnswer, "use the shorter table", "")
	if field(out, "decision") != "answer" || field(out, "generationEffect") != "stays" || field(out, "answersEvent") != d.blocked || field(out, "decisionTurnId") != "decision-turn-1" {
		t.Fatalf("the answer record = %v", out)
	}
	if got := d.generation(); got != 1 {
		t.Fatalf("an answer moved the relationship to generation %d", got)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM generations WHERE relationship_id = ?", d.rid); n != 1 {
		t.Fatalf("an answer opened a generation: %d rows", n)
	}
	event := decEvent(out)
	stored := d.one("SELECT outcome, producer, execution_generation, stage, revision_hash FROM events WHERE event_id = ?", event)
	if stored == nil || stored.S("outcome") != DecisionReply || stored.S("producer") != "relay" || stored.I("execution_generation") != 1 || stored.S("stage") != "final" || stored.S("revision_hash") != store.NoDeliverable {
		t.Fatalf("the stored event = %v", stored)
	}
	row := d.row(event)
	if row.S("kind") != Revision || row.S("recipient_task_id") != child || row.S("state") != Queued {
		t.Fatalf("the decision delivery = %v, want a queued revision_request to the child", row)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'decision_recorded' AND subject = ?", event); n != 1 {
		t.Fatalf("decision_recorded journal rows = %d", n)
	}
	// the delivery engine carries it
	d.mustAttempt(event, nil)
	if state := d.row(event).S("state"); state != Dispatched {
		t.Fatalf("the decision delivery is %s after an attempt, want dispatched", state)
	}
	message := d.sent()
	if message.thread != child {
		t.Fatalf("the decision went to %s", message.thread)
	}
	for _, want := range []string{"parent decision", "answer", "use the shorter table", d.blocked, "--continues-anchor " + dispatchTurn, "--continuation-actor " + child} {
		if !strings.Contains(message.message, want) {
			t.Fatalf("the message does not carry %q:\n%s", want, message.message)
		}
	}
	// the child's next turn is admitted only through the claim the message prints
	turn := d.host.threads[child].turns[len(d.host.threads[child].turns)-1].TurnID
	next := d.executionPayload(d.rid, 1, "failed", 2, turnRef{child, turn, "completed"})
	if _, err := d.accept(next, store.AcceptOptions{}); Reason(err) != "unassigned_turn" {
		t.Fatalf("a receipt from the reply's turn without a claim: %v, want unassigned_turn", err)
	}
	if _, err := d.accept(next, store.AcceptOptions{Continuation: d.claimJSON(dispatchTurn)}); err != nil {
		t.Fatalf("a receipt from the reply's turn with the claim the message prints: %v", err)
	}
}

// c1: a stop keeps the generation and the relationship status, and tells the child to report interrupted.
func TestDR02_a_stop_keeps_the_generation_and_the_relationship_status(t *testing.T) {
	d := newDecWorld(t)
	out := d.mustReply(DecisionStop, "the node is cancelled", "")
	if field(out, "generationEffect") != "stays" || d.generation() != 1 {
		t.Fatalf("a stop: %v, generation %d", out, d.generation())
	}
	if status := d.one("SELECT status FROM relationships WHERE relationship_id = ?", d.rid).S("status"); status != "active" {
		t.Fatalf("a stop changed the relationship status to %s", status)
	}
	d.mustAttempt(decEvent(out), nil)
	message := d.sent().message
	for _, want := range []string{"stop", "the node is cancelled", "interrupted", "--continues-anchor " + dispatchTurn} {
		if !strings.Contains(message, want) {
			t.Fatalf("the stop message does not carry %q:\n%s", want, message)
		}
	}
}

// c1: a criteria-changing decision opens the next generation, the older undelivered receipt is superseded, and the
// turn the reply opens is the anchor of the new generation.
func TestDR03_split_approval_and_scope_change_advance_the_generation(t *testing.T) {
	for _, decision := range []string{DecisionSplitApproval, DecisionScopeChange} {
		t.Run(decision, func(t *testing.T) {
			d := newDecWorld(t)
			digest := pyjson.Text(d.setDigest())
			out := d.mustReply(decision, "keep only the endpoint half", digest)
			if field(out, "generationEffect") != "advances" || fmt.Sprint(field(out, "nextExecutionGeneration")) != "2" || field(out, "criteriaDigest") != digest {
				t.Fatalf("the record = %v", out)
			}
			if got := d.generation(); got != 2 {
				t.Fatalf("the relationship is on generation %d", got)
			}
			event := decEvent(out)
			g := d.one("SELECT reason, dispatch_request_id, anchor_state FROM generations WHERE relationship_id = ? AND execution_generation = 2", d.rid)
			if g == nil || g.S("reason") != DecisionReply || g.S("dispatch_request_id") != "decision-"+event || g.S("anchor_state") != "anchor_pending" {
				t.Fatalf("generation 2 = %v", g)
			}
			if e := d.one("SELECT execution_generation FROM events WHERE event_id = ?", event); e.I("execution_generation") != 2 {
				t.Fatalf("the decision event is in generation %d", e.I("execution_generation"))
			}
			if n := d.count("SELECT COUNT(*) AS c FROM delivery_supersession WHERE event_id = ? AND reason = 'stale_generation'", d.blocked); n != 1 {
				t.Fatalf("the blocked receipt's delivery was not superseded by the new generation")
			}
			if rep := d.deliver(); len(rep.Notes) != 0 || d.row(event).S("state") != Dispatched {
				t.Fatalf("the scheduler did not deliver the decision cleanly: notes %v, state %s", rep.Notes, d.row(event).S("state"))
			}
			if !strings.Contains(d.sent().message, "keep only the endpoint half") || !strings.Contains(d.sent().message, digest) {
				t.Fatalf("the message lacks the decision or the criteria digest:\n%s", d.sent().message)
			}
			// the scheduler bound the dispatched turn as the anchor of generation 2: a receipt from it needs no claim and no --supersedes-revision
			g = d.one("SELECT anchor_state, dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = 2", d.rid)
			turn := d.host.threads[child].turns[len(d.host.threads[child].turns)-1].TurnID
			if g.S("anchor_state") != "bound" || g.S("dispatch_turn_id") != turn {
				t.Fatalf("generation 2 after the delivery = %v, want bound to %s", g, turn)
			}
			payload := d.readyPayload(d.rid, 2, []string{d.artifact("out2.txt", "the reduced deliverable")}, 1, turnRef{child, turn, "completed"})
			if _, err := d.accept(payload, store.AcceptOptions{}); err != nil {
				t.Fatalf("a receipt from the anchor of generation 2: %v", err)
			}
			head, err := HeadRevision(d.ctx, d.store, d.rid, 2)
			mustDo(t, err)
			ready := pyjson.Text(payload.Get("eventId"))
			if field(head, "eventId") != ready {
				t.Fatalf("the head of generation 2 = %v", head)
			}
			// the parent can rule it: the decision event in the generation does not disturb the head judgement
			_, err = d.delivery.Enqueue(d.ctx, ready, "", "")
			mustDo(t, err)
			d.attemptOn(ready, d.host, nil)
			d.clock.Advance(5)
			ackTurn := d.host.startTurn(parent, "ack-turn-g2", "inProgress", "")
			_, err = d.ack.Acknowledge(d.ctx, ready, ackTurn.TurnID, AckProof(ready, ackTurn.TurnID), true, nil, d.host)
			mustDo(t, err)
			d.hl.claim(ready, ackTurn.TurnID)
			if ruled := d.mustRule(ready, "verified", "v-g2", rrPassing, nil, nil); field(ruled, "verdict") != "verified" {
				t.Fatalf("the ruling on the receipt of generation 2 = %v", ruled)
			}
		})
	}
}

// c1: a reply is held while the child is busy and sent when it is idle.
func TestDR04_a_busy_child_holds_the_reply_until_it_is_idle(t *testing.T) {
	d := newDecWorld(t)
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	d.host.script = []string{"busy"}
	if record := d.mustAttempt(event, nil); record == nil || pyjson.Text(record.Get("deliveryState")) != DeferredBusy {
		t.Fatalf("the first attempt = %v, want deferred_busy", record)
	}
	d.clock.T = d.row(event).F("next_eligible_at")
	d.mustAttempt(event, at(d.clock.Now()))
	if state := d.row(event).S("state"); state != Dispatched {
		t.Fatalf("after the child went idle the reply is %s", state)
	}
}

// c3: a decision is never a verdict. A verdict on the blocked receipt keeps its refusal, a decision on a receipt that
// carries an artifact is refused, a different second decision is refused and the same one replays.
func TestDR05_a_decision_is_never_mixed_with_a_verdict(t *testing.T) {
	d := newDecWorld(t)
	d.attemptOn(d.blocked, d.host, nil)
	d.clock.Advance(5)
	turn := d.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err := d.ack.Acknowledge(d.ctx, d.blocked, turn.TurnID, AckProof(d.blocked, turn.TurnID), true, nil, d.host)
	mustDo(t, err)
	d.rcRefused(d.blocked, "needs_changes", "v1", rcRestoration(), nil, SupersededRevision)
	out := d.mustReply(DecisionAnswer, "go on", "")
	if n := d.count("SELECT COUNT(*) AS c FROM verdicts"); n != 0 {
		t.Fatalf("a decision wrote %d verdict rows", n)
	}
	d.rcRefused(d.blocked, "needs_changes", "v2", rcRestoration(), nil, DispositionConflict)
	d.rcRefused(d.blocked, "aborted", "v3", nil, "could not conclude", DispositionConflict)
	if n := d.count("SELECT COUNT(*) AS c FROM events WHERE outcome = ?", DecisionReply); n != 1 {
		t.Fatalf("a refused verdict left %d decision events", n)
	}
	// the same decision again replays and writes nothing; another kind, or the same kind with another note, is refused
	before := d.rcFootprint()
	again := d.mustReply(DecisionAnswer, "go on", "")
	if again.Get("_replay") != true || decEvent(again) != decEvent(out) || d.rcFootprint() != before {
		t.Fatalf("the same decision again: %v", again)
	}
	d.refused(DecisionAnswer, "go on, differently", "", DispositionConflict)
	text := d.refused(DecisionStop, "stop after all", "", DispositionConflict)
	rcMentions(t, text, "answer", "already")
	// the other way round: a receipt that was ruled aborted takes no decision
	v := newDecWorld(t)
	v.attemptOn(v.blocked, v.host, nil)
	v.clock.Advance(5)
	vTurn := v.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err = v.ack.Acknowledge(v.ctx, v.blocked, vTurn.TurnID, AckProof(v.blocked, vTurn.TurnID), true, nil, v.host)
	mustDo(t, err)
	v.mustRule(v.blocked, "aborted", "v1", nil, "could not conclude", nil)
	text = v.refused(DecisionAnswer, "go on", "", DispositionConflict)
	rcMentions(t, text, "aborted", "verdict")
	// a receipt that carries an artifact is ruled with a verdict
	r := newDecWorld(t)
	ready := r.readyPayload(r.rid, 1, []string{r.artifact("ready.txt", "done")}, 1, assigned("completed"))
	_, err = r.accept(ready, store.AcceptOptions{})
	mustDo(t, err)
	r.blocked = pyjson.Text(ready.Get("eventId"))
	text = r.refused(DecisionAnswer, "go on", "", DispositionConflict)
	rcMentions(t, text, "blocked_needs_input", "verdict")
}

// c1 and c3: every refusal names an existing reason and leaves every row as it was.
func TestDR06_a_refusal_writes_nothing(t *testing.T) {
	t.Run("an unknown receipt", func(t *testing.T) {
		d := newDecWorld(t)
		d.blocked = strings.Repeat("0", 32)
		d.refused(DecisionAnswer, "go on", "", NotClaimable)
	})
	t.Run("arguments", func(t *testing.T) {
		d := newDecWorld(t)
		digest := pyjson.Text(d.setDigest())
		d.refused("approve", "go on", "", MalformedReceipt)
		d.refused(DecisionAnswer, "  ", "", MalformedReceipt)
		d.refused(DecisionAnswer, "go on", digest, MalformedReceipt)
		d.refused(DecisionSplitApproval, "go on", "", MalformedReceipt)
		d.refused(DecisionStop, "go on", digest, MalformedReceipt)
		if _, err := d.ack.RecordDecision(d.ctx, DecisionRequest{EventID: d.blocked, Decision: DecisionAnswer, Note: "go on"}); Reason(err) != MalformedReceipt {
			t.Fatalf("a reply with no turn: %v", err)
		}
	})
	t.Run("criteria", func(t *testing.T) {
		d := newDecWorld(t)
		d.refused(DecisionScopeChange, "go on", strings.Repeat("a", 64), CriteriaSetChanged)
		u := newDecWorld(t)
		u.exec("DELETE FROM canonical_criteria WHERE relationship_id = ?", u.rid)
		u.refused(DecisionSplitApproval, "go on", strings.Repeat("a", 64), CriteriaUnregistered)
	})
	t.Run("the relationship", func(t *testing.T) {
		d := newDecWorld(t)
		d.setStatus("paused")
		d.refused(DecisionAnswer, "go on", "", RelationshipNotActive)
		a := newDecWorld(t, parent)
		a.refused(DecisionAnswer, "go on", "", RecipientNotAuthorized)
	})
	t.Run("a newer generation", func(t *testing.T) {
		d := newDecWorld(t)
		mustDo(t, d.store.Transaction(d.ctx, func(ctx context.Context, _ *sql.Conn) error {
			_, err := OpenGenerationIn(ctx, d.store, d.clock, d.rid, "other-dispatch", "initial_assignment", nil)
			return err
		}))
		d.refused(DecisionAnswer, "go on", "", StaleGeneration)
	})
	t.Run("the child reported again", func(t *testing.T) {
		d := newDecWorld(t)
		d.clock.Advance(5)
		d.emit("failed", "completed", store.AcceptOptions{})
		d.refused(DecisionAnswer, "go on", "", SupersededRevision)
	})
	t.Run("a receipt not yet final", func(t *testing.T) {
		d := newDecWorld(t)
		d.exec("UPDATE events SET stage = 'staged' WHERE event_id = ?", d.blocked)
		d.refused(DecisionAnswer, "go on", "", DispositionConflict)
	})
}

// c1: a decision that keeps the generation is not offered to the anchor binding: the generation is bound to its own anchor and the
// reply's turn is a later turn of it.
func TestDR08_a_decision_that_keeps_the_generation_does_not_rebind_its_anchor(t *testing.T) {
	d := newDecWorld(t)
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	if rep := d.deliver(); len(rep.Notes) != 0 || d.row(event).S("state") != Dispatched {
		t.Fatalf("the scheduler's delivery of a decision that keeps the generation: notes %v, state %s", rep.Notes, d.row(event).S("state"))
	}
	if out, err := d.ack.BindDispatchedRevision(d.ctx, event); out != nil || err != nil {
		t.Fatalf("the anchor binding of a decision that keeps the generation: %v, %v", out, err)
	}
	row := d.row(event)
	attempt := d.one("SELECT * FROM attempts WHERE event_id = ?", event)
	if bound, err := d.rc.bindPromotedAnchor(d.ctx, attempt, row, row.S("dispatch_turn_id")); bound != nil || err != nil {
		t.Fatalf("the promotion's anchor binding of a decision that keeps the generation: %v, %v", bound, err)
	}
	if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'anchor_conflict'"); n != 0 {
		t.Fatalf("%d anchor conflicts were journalled", n)
	}
	if got := d.one("SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = 1", d.rid).S("dispatch_turn_id"); got != dispatchTurn {
		t.Fatalf("the anchor of generation 1 moved to %s", got)
	}
}

// c1: a reply the child has moved past is not sent: it answers a question the child no longer asks.
func TestDR09_a_reply_is_superseded_when_the_child_has_reported_again(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "a receipt after the decision"
		if staged {
			name = "a receipt staged before the decision and final after it"
		}
		t.Run(name, func(t *testing.T) {
			d := newDecWorld(t)
			d.clock.Advance(5)
			var event string
			if staged {
				// the child's next receipt is seen but its turn has not been seen to end: it is not final, so the decision is allowed
				payload := d.readyPayload(d.rid, 1, []string{d.artifact("late.txt", "later work")}, 1, assigned("inProgress"))
				_, err := d.accept(payload, store.AcceptOptions{})
				mustDo(t, err)
				later := pyjson.Text(payload.Get("eventId"))
				d.clock.Advance(5)
				event = decEvent(d.mustReply(DecisionAnswer, "go on", ""))
				d.clock.Advance(5)
				d.exec("UPDATE events SET stage = 'final' WHERE event_id = ?", later)
			} else {
				event = decEvent(d.mustReply(DecisionAnswer, "go on", ""))
				d.clock.Advance(5)
				d.emit("failed", "completed", store.AcceptOptions{})
				d.clock.Advance(5)
			}
			d.attempt(event, nil)
			row := d.row(event)
			if row.S("state") != Superseded || row.S("hold_reason") != SupersededRevision {
				t.Fatalf("the reply after the child reported again: %v", row)
			}
			if len(d.host.sends) != 0 {
				t.Fatalf("%d messages were sent", len(d.host.sends))
			}
		})
	}
}

// c1: a decision that opened its generation goes through the anchor binding like a correction does: where the generation was bound to
// another turn by hand, the conflict is reported and not skipped.
func TestDR10_an_advancing_decision_reports_an_anchor_bound_to_another_turn(t *testing.T) {
	d := newDecWorld(t)
	event := decEvent(d.mustReply(DecisionScopeChange, "keep the endpoint half", pyjson.Text(d.setDigest())))
	_, err := BindAnchor(d.ctx, d.store, d.clock, d.rid, 2, "turn-bound-by-hand")
	mustDo(t, err)
	rep := d.deliver()
	noted := false
	for _, note := range rep.Notes {
		noted = noted || strings.Contains(note, "anchor_already_bound")
	}
	if !noted || d.row(event).S("state") != Dispatched {
		t.Fatalf("the anchor conflict of an advancing decision: notes %v, state %s", rep.Notes, d.row(event).S("state"))
	}
	if got := d.one("SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = 2", d.rid).S("dispatch_turn_id"); got != "turn-bound-by-hand" {
		t.Fatalf("generation 2 is bound to %s", got)
	}
}

// c1: the message carries the criteria the decision was made under, so the child works from what the parent approved even when the set
// is registered again before the message is sent.
func TestDR11_the_message_carries_the_criteria_the_decision_was_made_under(t *testing.T) {
	d := newDecWorld(t)
	digest := pyjson.Text(d.setDigest())
	d.mustReply(DecisionSplitApproval, "keep the endpoint half", digest)
	d.registerCriteria(rrEdited)
	if rep := d.deliver(); len(rep.Notes) != 0 {
		t.Fatalf("the scheduler's delivery: %v", rep.Notes)
	}
	message := d.sent().message
	for _, want := range []string{"the endpoint returns the agreed shape", "a malformed request is refused", digest} {
		if !strings.Contains(message, want) {
			t.Fatalf("the message does not carry %q:\n%s", want, message)
		}
	}
	if strings.Contains(message, "COMPLETELY different") {
		t.Fatalf("the message carries the set registered after the decision:\n%s", message)
	}
}

// c1: receipts first seen at one instant are ordered by the order they were stored in, not by their ids, which are hashes of the receipts.
func TestDR12_receipts_first_seen_at_one_instant_are_ordered_by_storage(t *testing.T) {
	rid, err := store.RelationshipID(parent, child, issue)
	mustDo(t, err)
	idOf := func(outcome string, attempt int) string {
		event, err := store.EventID(rid, 1, store.NoDeliverable, outcome, dispatchTurn, &attempt)
		mustDo(t, err)
		return event
	}
	attemptOf := func(smaller bool) int {
		for n := 2; n < 200; n++ {
			if (idOf("failed", n) < idOf("blocked_needs_input", 1)) == smaller {
				return n
			}
		}
		t.Fatal("no attempt number gives the wanted order of ids")
		return 0
	}
	emitAttempt := func(d *decWorld, attempt int) {
		_, err := d.accept(d.executionPayload(d.rid, 1, "failed", attempt, assigned("completed")), store.AcceptOptions{})
		mustDo(t, err)
	}
	t.Run("a newer receipt whose id sorts first still supersedes the answer", func(t *testing.T) {
		attempt := attemptOf(true)
		d := newDecWorld(t)
		event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
		emitAttempt(d, attempt)
		d.attempt(event, nil)
		if row := d.row(event); row.S("state") != Superseded || row.S("hold_reason") != SupersededRevision {
			t.Fatalf("the reply after a later receipt with a smaller id: %v", row)
		}
		r := newDecWorld(t)
		emitAttempt(r, attempt)
		r.refused(DecisionAnswer, "go on", "", SupersededRevision)
	})
	t.Run("an older receipt whose id sorts last does not block the answer", func(t *testing.T) {
		attempt := attemptOf(false)
		d := newDecWorldWith(t, func(d *decWorld) { emitAttempt(d, attempt) })
		if out, err := d.reply(DecisionAnswer, "go on", ""); err != nil {
			t.Fatalf("a reply to a receipt that is the newest, behind an older receipt with a larger id: %v (%v)", err, out)
		}
	})
}

// c1: the replies read back with their delivery, and the two commands run over the same writer and reader.
func TestDR07_the_replies_read_back_and_the_commands_run(t *testing.T) {
	d := newDecWorld(t)
	file := filepath.Join(t.TempDir(), "note.txt")
	mustDo(t, os.WriteFile(file, []byte("the long answer\nover two lines"), 0o644))
	previous := CommandClock
	CommandClock = d.clock
	t.Cleanup(func() { CommandClock = previous })
	// the command table and the parser: the commands are registered, take these options and refuse an unknown decision as usage
	run := func(command string, flags ...string) (any, error) {
		registered, ok := dispatch.Lookup(command)
		if !ok {
			t.Fatalf("%s is not registered", command)
		}
		parsed := argparse.Parse(command, flags)
		if parsed.Message != "" {
			t.Fatalf("%s %v: %s", command, flags, parsed.Message)
		}
		return registered.Run(d.ctx, dispatch.Services{Selection: store.StateSelection{Path: filepath.Join(d.tree, "gostate"), Source: "flag"}}, dispatch.Args{Parsed: parsed, Defaults: registered.Defaults})
	}
	if parsed := argparse.Parse("decision-reply", []string{"--event", d.blocked, "--decision", "approve", "--decision-turn", "t", "--note", "n"}); parsed.Message == "" {
		t.Fatal("an unknown decision was accepted by the parser")
	}
	out, err := run("decision-reply", "--event", d.blocked, "--decision", "answer", "--decision-turn", "decision-turn-1", "--note", "@"+file)
	mustDo(t, err)
	record := out.(Obj)
	shown, err := run("decision-show", "--relationship", d.rid)
	mustDo(t, err)
	list, _ := shown.(Obj).Lookup("decisions")
	items, _ := list.([]any)
	if len(items) != 1 {
		t.Fatalf("decision-show lists %d decisions: %v", len(items), shown)
	}
	item := items[0].(Obj)
	if field(item, "eventId") != decEvent(record) || field(item, "decision") != "answer" || field(item, "answersEvent") != d.blocked || field(item, "note") != "the long answer\nover two lines" {
		t.Fatalf("the reading = %v", item)
	}
	if delivery := sub(item, "delivery"); field(delivery, "state") != Queued {
		t.Fatalf("the reading's delivery = %v", delivery)
	}
	d.mustAttempt(decEvent(record), nil)
	shown, _ = run("decision-show", "--relationship", d.rid)
	list, _ = shown.(Obj).Lookup("decisions")
	if state := field(sub(list.([]any)[0].(Obj), "delivery"), "state"); state != Dispatched {
		t.Fatalf("after the send the reading's delivery state is %v", state)
	}
}
