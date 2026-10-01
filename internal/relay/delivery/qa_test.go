package delivery

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Todo 21 QA (happy path): emit -> deliver -> claim -> ack -> verdict on a temp state dir, every
// row of the resulting store checked against the golden, which began as the Python run of the same
// fixture.
func TestQA_emit_deliver_claim_ack_round_trip_rows_equal_python(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "qa")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	delivered := f.mustAttempt(event, nil)
	f.clock.Advance(5)
	turn := f.host.startTurn(parent, "ack-turn", "inProgress", "")
	ack := NewAck(f.delivery)
	claim, err := ack.ClaimVerification(f.ctx, event, "ack-turn")
	mustDo(t, err)
	acked, err := ack.Acknowledge(f.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, f.host)
	mustDo(t, err)
	verdict, err := ack.RecordVerdict(f.ctx, event, "verified", "verdict-1", nil, nil, nil, nil)
	mustDo(t, err)
	payload := f.readyPayload(f.rid, 1, []string{f.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	stored, err := f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	expected.same("delivered", delivered)
	expected.same("claim", claim)
	expected.same("ack", acked)
	expected.same("verdict", verdict)
	expected.same("duplicate", stored.Duplicate)
	if !stored.Duplicate {
		t.Fatal("a re-emitted revision is a duplicate")
	}
	expected.tables(f)
}
