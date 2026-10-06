package delivery

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-668 (follow-up to CRW-659): a later receipt that makes a decision stale is any final,
// unsuppressed receipt of the same generation whose producer is the child or the relay's own
// observation of an ended turn, in the order the relay saw the receipts. Before this change the
// two later-receipt checks counted child receipts only, so an older observed end stayed answerable
// after a newer one, and the answer to it stayed deliverable. These tests use temporary synthetic
// stores and a fake host only.

// observedEnd takes one daemon observation of an admitted turn of generation 1 ending in outcome,
// the way the daemon does once it has seen that turn end, and returns its event id.
func (d *decWorld) observedEnd(turn, outcome string) string {
	d.t.Helper()
	d.exec("INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES(?,1,?,'explicit_admission_bound:'||?,'child','admitted','2023-11-14T22:13:20Z')", d.rid, turn, dispatchTurn)
	stored, err := d.intake.DaemonObservation(d.ctx, d.rid, store.TurnReference{ThreadID: child, TurnID: turn, Status: outcome})
	mustDo(d.t, err)
	if stored.Stage != store.StageFinal {
		d.t.Fatalf("a daemon observation of a %s turn is %s, want final", outcome, stored.Stage)
	}
	return stored.EventID
}

// c1/c2: an answer on an observed end that a newer end of the same generation has moved past is
// refused superseded_revision with a wording that says the relay saw the later end; the newer end
// is answered.
func TestDecisionObservedEnd01_AnOlderObservedEndIsNotAnsweredAfterANewerOne(t *testing.T) {
	t.Parallel()
	d := newObsWorld(t, "interrupted")
	older := d.blocked
	d.clock.Advance(5)
	newer := d.observedEnd("business", "failed")

	d.blocked = older
	text := d.refused(DecisionAnswer, "go on", "", SupersededRevision)
	for _, want := range []string{"relay observed", "later", newer} {
		if !strings.Contains(text, want) {
			t.Fatalf("the refusal does not say %q: %s", want, text)
		}
	}

	d.blocked = newer
	out := d.mustReply(DecisionAnswer, "go on", "")
	if field(out, "answersEvent") != newer || field(out, "answersOutcome") != "failed" {
		t.Fatalf("the answer to the newer end = %v", out)
	}
}

// c1/c2: the newer end must be later in the order the relay saw the receipts. An observed end that
// came first does not block an answer to the end that came after it.
func TestDecisionObservedEnd02_TheNewestObservedEndIsTheOneAnswered(t *testing.T) {
	t.Parallel()
	d := newObsWorld(t, "failed")
	older := d.blocked
	d.clock.Advance(5)
	newer := d.observedEnd("business", "interrupted")

	d.blocked = older
	d.refused(DecisionAnswer, "go on", "", SupersededRevision)

	d.blocked = newer
	if out := d.mustReply(DecisionAnswer, "go on", ""); field(out, "answersEvent") != newer {
		t.Fatalf("the answer went to %v, want the newer end", out)
	}
}

// c1/c2: a queued answer to the older observed end is superseded once a newer end of the same
// generation arrives, so it is never sent.
func TestDecisionObservedEnd03_AQueuedAnswerIsSupersededByANewerObservedEnd(t *testing.T) {
	t.Parallel()
	d := newObsWorld(t, "interrupted")
	event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
	if state := d.row(event).S("state"); state != Queued {
		t.Fatalf("the decision delivery is %s before the later end, want queued", state)
	}
	d.clock.Advance(5)
	d.observedEnd("business", "failed")

	d.attempt(event, nil)
	if row := d.row(event); row.S("state") != Superseded || row.S("hold_reason") != SupersededRevision {
		t.Fatalf("the answer to the older end after a newer one: %v", row)
	}
	if len(d.host.sends) != 0 {
		t.Fatalf("%d messages were sent", len(d.host.sends))
	}
}

// c1/c2: the child's own later receipt keeps its wording, so the refusal still names the child when
// the child, not the relay, reported again.
func TestDecisionObservedEnd04_AChildsLaterReceiptKeepsItsWording(t *testing.T) {
	t.Parallel()
	d := newObsWorld(t, "interrupted")
	d.clock.Advance(5)
	d.emit("failed", "completed", store.AcceptOptions{})
	text := d.refused(DecisionAnswer, "go on", "", SupersededRevision)
	if !strings.Contains(text, "the child reported again") {
		t.Fatalf("the refusal for a child's later receipt changed: %s", text)
	}
}
