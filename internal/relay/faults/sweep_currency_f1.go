package faults

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The sweep asks the same read-only currency questions as the delivery sender.
func f1SupersessionReason(ctx context.Context, l *Ledger, eventID string) (string, error) {
	event, e := l.one(ctx, "SELECT relationship_id,execution_generation,outcome,event_id,receipt FROM events WHERE event_id=?", eventID)
	if e != nil || event == nil {
		return "", e
	}
	if event.Text("outcome") == "merge_turn_grant" {
		return mergeturn.GrantSupersessionFor(ctx, l.Store, event.Text("receipt"))
	}
	rel, e := l.one(ctx, "SELECT execution_generation FROM relationships WHERE relationship_id=?", event.Text("relationship_id"))
	if e != nil || rel == nil {
		return "", e
	}
	if integer(event, "execution_generation") < integer(rel, "execution_generation") {
		return "stale_generation", nil
	}
	if event.Text("outcome") == "revision" {
		answered, e := l.one(ctx, "SELECT 1 FROM events WHERE relationship_id=? AND execution_generation=? AND stage='final' AND suppressed_reason IS NULL AND event_id!=? AND outcome NOT IN ('merge_turn_grant')", event.Text("relationship_id"), integer(event, "execution_generation"), eventID)
		if e != nil || answered == nil {
			return "", e
		}
		return "superseded_revision", nil
	}
	if event.Text("outcome") == "decision_reply" {
		answered, _ := loadsMap(event.Text("receipt"))["answersEvent"].(string)
		later, e := l.one(ctx, "SELECT 1 FROM events c JOIN events b ON b.event_id=? WHERE c.relationship_id=? AND c.execution_generation=? AND c.stage='final' AND c.suppressed_reason IS NULL AND c.producer='child' AND c.event_id != b.event_id AND (c.first_seen_at > b.first_seen_at OR (c.first_seen_at = b.first_seen_at AND c.event_id > b.event_id))", answered, event.Text("relationship_id"), integer(event, "execution_generation"))
		if e != nil || later == nil {
			return "", e
		}
		return "superseded_revision", nil
	}
	switch event.Text("outcome") {
	case "failed", "interrupted", "blocked_needs_input":
		return "", nil
	}
	head, e := registry.HeadRevision(ctx, l.Store, event.Text("relationship_id"), integer(event, "execution_generation"))
	if e != nil {
		return "", e
	}
	if head.EventID == "" || head.EventID == eventID {
		return "", nil
	}
	successor, e := l.one(ctx, "SELECT stage FROM events WHERE event_id=?", head.EventID)
	if e != nil || successor == nil || successor.Text("stage") != "final" {
		return "", e
	}
	return "superseded_revision", nil
}
