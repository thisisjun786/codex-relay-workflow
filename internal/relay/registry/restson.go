package registry

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// What a verified ruling can have been built on (CRW-404). A parent that ruled an event verified may
// still change its mind with a needs_changes ruling, but only until something acts on the verified
// ruling: the plan accepts the event, the work is marked merged, or a merge turn of the assignment is
// carrying it to the target. RestsOn reads those three, so the verdict writer and any later decision
// about a ruling ask one question in one place.
const (
	// RestsAccepted is a plan acceptance (dag_acceptances) of the event, whatever its state.
	RestsAccepted = "accepted"
	// RestsMarkedMerged is a merged mark (assignment_marks) of the event.
	RestsMarkedMerged = "marked_merged"
	// RestsMergeTurn is a merge turn of the assignment that is merging, of unknown effect or landed.
	RestsMergeTurn = "merge_turn"
)

// Rest is what a ruling rests on. Kind is empty when nothing does.
type Rest struct {
	Kind string
	// ID is the acceptance id (RestsAccepted) or the turn id (RestsMergeTurn).
	ID string
	// Plan and Node name the plan node of an acceptance.
	Plan, Node string
	// State is the state of the turn; Head its candidate head.
	State, Head string
	// At is when the mark was written.
	At string
}

// mergeTurnStates are the states of a merge turn in which the work it carries is on its way to the
// target (merging), may be there already (unknown) or is there (landed). A turn that only waits or
// holds the lane is not in the list: the parent that found a base conflict holds that very turn.
var mergeTurnStates = []string{"merging", "unknown", "landed"}

// RestsOn answers what the ruling of eventID rests on. It runs inside the caller's transaction. The
// DAG zone is installed by every writable open, and the verdict command is not a read-only command,
// so the zone's tables are always there for the one caller (RecordVerdict).
//
// The merge turn is read per assignment and not per head: a turn names its candidate head, but no
// product code records the head of an event (nothing writes work_reports), so the two cannot be
// joined. A turn of the assignment that is merging, unknown or landed counts whatever head it names,
// the conservative side.
func RestsOn(ctx context.Context, s *store.Store, eventID string) (Rest, error) {
	event, err := s.One(ctx, "SELECT relationship_id FROM events WHERE event_id = ?", eventID)
	if err != nil || event == nil {
		return Rest{}, err
	}
	rid := colString(event, "relationship_id")
	accepted, err := s.One(ctx, "SELECT acceptance_id, plan_id, node_id FROM dag_acceptances WHERE relationship_id = ? AND event_id = ? ORDER BY accepted_at, acceptance_id LIMIT 1", rid, eventID)
	if err != nil {
		return Rest{}, err
	}
	if accepted != nil {
		return Rest{Kind: RestsAccepted, ID: colString(accepted, "acceptance_id"), Plan: colString(accepted, "plan_id"), Node: colString(accepted, "node_id")}, nil
	}
	mark, err := s.One(ctx, "SELECT marked_at FROM assignment_marks WHERE relationship_id = ? AND event_id = ? AND mark = 'merged' ORDER BY marked_at LIMIT 1", rid, eventID)
	if err != nil {
		return Rest{}, err
	}
	if mark != nil {
		return Rest{Kind: RestsMarkedMerged, At: colString(mark, "marked_at")}, nil
	}
	turn, err := s.One(ctx, "SELECT turn_id, state, candidate_head FROM merge_turns WHERE relationship_id = ? AND state IN ("+placeholders(len(mergeTurnStates))+") ORDER BY requested_at, turn_id LIMIT 1", stringArgs(rid, mergeTurnStates)...)
	if err != nil {
		return Rest{}, err
	}
	if turn != nil {
		return Rest{Kind: RestsMergeTurn, ID: colString(turn, "turn_id"), State: colString(turn, "state"), Head: colString(turn, "candidate_head")}, nil
	}
	return Rest{}, nil
}
