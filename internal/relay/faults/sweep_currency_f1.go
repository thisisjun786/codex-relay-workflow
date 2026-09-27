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
	if text(event, "outcome") == "merge_turn_grant" {
		return mergeturn.GrantSupersessionFor(ctx, l.Store, text(event, "receipt"))
	}
	rel, e := l.one(ctx, "SELECT execution_generation FROM relationships WHERE relationship_id=?", text(event, "relationship_id"))
	if e != nil || rel == nil {
		return "", e
	}
	if integer(event, "execution_generation") < integer(rel, "execution_generation") {
		return "stale_generation", nil
	}
	if text(event, "outcome") == "revision" {
		answered, e := l.one(ctx, "SELECT 1 FROM events WHERE relationship_id=? AND execution_generation=? AND stage='final' AND suppressed_reason IS NULL AND event_id!=? AND outcome NOT IN ('merge_turn_grant')", text(event, "relationship_id"), integer(event, "execution_generation"), eventID)
		if e != nil || answered == nil {
			return "", e
		}
		return "superseded_revision", nil
	}
	switch text(event, "outcome") {
	case "failed", "interrupted", "blocked_needs_input":
		return "", nil
	}
	head, e := registry.HeadRevision(ctx, l.Store, text(event, "relationship_id"), integer(event, "execution_generation"))
	if e != nil {
		return "", e
	}
	if head.EventID == "" || head.EventID == eventID {
		return "", nil
	}
	successor, e := l.one(ctx, "SELECT stage FROM events WHERE event_id=?", head.EventID)
	if e != nil || successor == nil || text(successor, "stage") != "final" {
		return "", e
	}
	return "superseded_revision", nil
}
