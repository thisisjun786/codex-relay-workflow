package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// StoreDelivery is delivery.DeliveryService as MergeTurn uses it. Channel is grant_channel_in
// (read-only); Queue is queue_grant_in.
type StoreDelivery struct{ Store *store.Store }

// Channel is DeliveryService.grant_channel_in: the turn's own assignment and nothing else.
func (d StoreDelivery) Channel(ctx context.Context, relationship sql.NullString, recipient, grant, project string) (string, string, error) {
	if !relationship.Valid || relationship.String == "" {
		return "the turn names no assignment", "", nil
	}
	rid := relationship.String
	row, err := d.Store.One(ctx, "SELECT parent_task_id, status, superseded_by, allowed_recipients FROM relationships WHERE relationship_id = ?", rid)
	if err != nil {
		return "", "", err
	}
	if row == nil {
		return fmt.Sprintf("assignment %s is not in this store", pyRepr(rid)), "", nil
	}
	if row.Get("status") != "active" || row.Get("superseded_by") != nil {
		return fmt.Sprintf("assignment %s is not active", pyRepr(rid)), "", nil
	}
	parent, _ := row.Get("parent_task_id").(string)
	if parent != recipient {
		return fmt.Sprintf("assignment %s is addressed to parent %s, not to %s", pyRepr(rid), pyRepr(parent), pyRepr(recipient)), "", nil
	}
	scope, err := d.Store.One(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
	if err != nil {
		return "", "", err
	}
	if project != "" && scope != nil && scope.Get("project_key") != project {
		attached, _ := scope.Get("project_key").(string)
		return fmt.Sprintf("assignment %s is attached to project %s, not to this turn's %s", pyRepr(rid), pyRepr(attached), pyRepr(project)), "", nil
	}
	var allowed []any
	if text, ok := row.Get("allowed_recipients").(string); ok {
		_ = json.Unmarshal([]byte(text), &allowed)
	}
	if !slices.Contains(allowed, any(recipient)) {
		return fmt.Sprintf("%s is not in the recipients assignment %s authorizes", pyRepr(recipient), pyRepr(rid)), "", nil
	}
	return "", GrantEventID(rid, grant), nil
}

// Queue inserts the relay-owned final event and its delivery in the promotion transaction.
// A replay of the same grant converges on the same event and delivery identities.
func (d StoreDelivery) Queue(ctx context.Context, eventID, relationship, recipient, receipt, grant, at string) error {
	q := d.Store.Querier(ctx)
	var generation int64
	if err := q.QueryRowContext(ctx, "SELECT execution_generation FROM relationships WHERE relationship_id = ?", relationship).Scan(&generation); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO events
		(event_id, relationship_id, execution_generation, revision_hash, outcome, producer,
		 attempt, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at,
		 last_seen_at, observation_count)
		VALUES (?,?,?,?,?,?,NULL,?,?,?,?, 'final', ?,?,1)`,
		eventID, relationship, generation, noDeliverable, "merge_turn_grant", "relay",
		recipient, grant, "completed", receipt, at, at); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries
		(event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state,
		 attempt_count, next_eligible_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,0,NULL,?,?)`, eventID, relationship, "merge_turn_grant",
		recipient, recipient, "queued", at, at); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, "delivery_queued", eventID, `{"kind":"merge_turn_grant"}`); err != nil {
		return err
	}
	// The new relay notice does not answer a child's correction. Annotate earlier
	// notices only if their own turn's currency has changed.
	predecessors, err := d.Store.All(ctx, `SELECT d.event_id, e.receipt FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.relationship_id=? AND e.execution_generation=? AND e.outcome='merge_turn_grant' AND d.event_id!=? AND d.state IN ('queued','sending','held_uncertain','dispatched','inbox_only','deferred_busy','withheld_pre_send')`, relationship, generation, eventID)
	if err != nil {
		return err
	}
	for _, prior := range predecessors {
		reason, err := GrantSupersessionFor(ctx, d.Store, prior.Get("receipt").(string))
		if err != nil {
			return err
		}
		if reason == "" {
			continue
		}
		if _, err = q.ExecContext(ctx, "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied) VALUES (?,?,?,0) ON CONFLICT(event_id) DO NOTHING", prior.Get("event_id"), reason, at); err != nil {
			return err
		}
	}
	return nil
}

const noDeliverable = "0000000000000000000000000000000000000000000000000000000000000000"

// GrantEventID is identity.merge_turn_grant_event_id.
func GrantEventID(relationship, grant string) string {
	return sha256Hex(relationship + "|" + grant + "|merge_turn_grant|null|null")[:32]
}
