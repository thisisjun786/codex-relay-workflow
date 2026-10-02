package supervisor

import (
	"database/sql"
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

// Only a relationship that nothing can be addressed for is left out of the visit: archived, and
// its issue's owner and edge released. An archived relationship its successor replaced still
// reaches the level above through the successor's issue edge, so its report is still staged.
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

func (w *standingWorld) messageRelationships() []string {
	w.tb.Helper()
	rows, err := w.s.All(w.ctx, "SELECT relationship_id FROM supervisor_messages ORDER BY relationship_id")
	if err != nil {
		w.tb.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row.Get("relationship_id").(string))
	}
	return out
}

// Registering a new child for the issue of an archived relationship (no successor named) gives its
// report an addressee again, so the visit must go on raising it: archived alone does not release a
// relationship.
func TestCRW299VisitKeepsAnArchivedRelationshipWhoseIssueWasTakenAgain(t *testing.T) {
	w := newStandingWorld(t, 2, 3)
	w.archive(w.rels[0])
	w.retake(w.rels[0])
	answer := w.visit()
	if refused := answer["refused"].([]any); len(refused) != 0 {
		t.Fatalf("refused: %v", refused)
	}
	if got, want := fmt.Sprint(w.messageRelationships()), fmt.Sprint(w.rels); got != want {
		t.Fatalf("staged for %s, want %s: the report of the archived relationship whose issue was taken again was not raised", got, want)
	}
}

// The same relationship with a report already staged and held because its hierarchy was gone
// (hierarchy_unresolved, what an attempt does once the assignment is archived): the visit keeps
// the hold while nothing can be addressed, and re-addresses the report once the issue is taken
// again.
func TestCRW299VisitReleasesAHierarchyHoldWhenTheIssueIsTakenAgain(t *testing.T) {
	w := newStandingWorld(t, 2, 3)
	w.visit()
	w.exec("UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved' WHERE relationship_id=?", w.rels[0])
	w.archive(w.rels[0])
	w.visit()
	hold := func() sql.NullString {
		var hold sql.NullString
		if err := w.s.DB.QueryRowContext(w.ctx, "SELECT hold_reason FROM supervisor_messages WHERE relationship_id=?", w.rels[0]).Scan(&hold); err != nil {
			t.Fatal(err)
		}
		return hold
	}
	if h := hold(); !h.Valid || h.String != "hierarchy_unresolved" {
		t.Fatalf("the hold on a report nothing can be addressed for changed to %v", h)
	}
	w.retake(w.rels[0])
	w.visit()
	if h := hold(); h.Valid {
		t.Fatalf("the report is addressable again but the visit left it held as %q", h.String)
	}
}
