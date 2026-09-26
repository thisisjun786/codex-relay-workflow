package delivery

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Todo 21 QA (happy path): emit -> deliver -> claim -> ack -> verdict on a temp state dir, every
// row of the resulting store equal to the Python run of the same fixture.
func TestQA_emit_deliver_claim_ack_round_trip_rows_equal_python(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "qa")
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
	requireSameJSON(t, "delivered", delivered, python.Out["delivered"])
	requireSameJSON(t, "claim", claim, python.Out["claim"])
	requireSameJSON(t, "ack", acked, python.Out["ack"])
	requireSameJSON(t, "verdict", verdict, python.Out["verdict"])
	dup := python.Out["duplicate"].(map[string]any)["ok"].(map[string]any)
	if !stored.Duplicate || dup["_duplicate"] != true {
		t.Fatal("a re-emitted revision is a duplicate on both sides")
	}
	requireSameTables(t, f, python)
}
