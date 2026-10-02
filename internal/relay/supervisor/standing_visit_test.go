package supervisor

import (
	"fmt"
	"testing"
)

// CRW-299: the standing read of a project visit.

// Standing read every event of the project one statement at a time (three more for each), so the
// statements a call sent grew with the events. They are one batched read now, whatever the events.
func TestCRW299StandingStatementsDoNotGrowWithEvents(t *testing.T) {
	const relationships = 6
	cost := map[int]int64{}
	for _, events := range []int{2, 20} {
		w := newStandingWorld(t, relationships, events)
		cost[events] = w.statementsOf(func() {
			if _, err := w.c.Standing(w.ctx, worldProject, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	if cost[2] != cost[20] {
		t.Fatalf("Standing sent %d statements for %d relationships of 2 events and %d for the same relationships of 20 events: the read still goes event by event", cost[2], relationships, cost[20])
	}
}

// A project's history is mostly relationships that are archived and have long since gone up. The
// daemon's visit must cost what the relationships that can still report cost, however many
// archived ones the project holds.
func TestCRW299VisitCostDoesNotGrowWithReleasedRelationships(t *testing.T) {
	const active = 4
	cost := map[int]int64{}
	for _, released := range []int{0, 12} {
		w := newStandingWorld(t, active+released, 8)
		w.settle()
		for _, rid := range w.rels[:released] {
			w.archive(rid)
		}
		cost[released] = w.statementsOf(func() { w.visit() })
	}
	if cost[0] != cost[12] {
		t.Fatalf("a visit sent %d statements with no archived relationship and %d with 12 archived ones: the visit still reads what archiving released", cost[0], cost[12])
	}
}

// Only a relationship that nothing can be addressed for is left out of the visit: archived with
// no successor. An archived relationship its successor replaced still reaches the level above
// through the successor's issue edge, so its report is still staged.
func TestCRW299VisitSkipsReleasedRelationshipsAndKeepsSuperseded(t *testing.T) {
	w := newStandingWorld(t, 3, 3)
	w.archive(w.rels[0])
	w.exec("UPDATE relationships SET status='archived', superseded_by=? WHERE relationship_id=?", w.rels[2], w.rels[1])
	answer := w.visit()
	if refused := answer["refused"].([]any); len(refused) != 0 {
		t.Fatalf("the visit examined an archived relationship nothing can be addressed for: %v", refused)
	}
	rows, err := w.s.All(w.ctx, "SELECT relationship_id FROM supervisor_messages ORDER BY relationship_id")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Get("relationship_id").(string))
	}
	if want := fmt.Sprint([]string{w.rels[1], w.rels[2]}); fmt.Sprint(got) != want {
		t.Fatalf("staged for %v, want %v: the superseded relationship keeps its report and the released one has none", got, want)
	}
}
