package hook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// LookupReceipt uses one read-only snapshot for relationship, generation and head.
// Artifact I/O starts only after that snapshot is closed.
func LookupReceipt(ctx context.Context, path string, fallback func() (string, error), relationship, session, turn, generation, dispatch any) (Object, bool, error) {
	if !delivery.Named(relationship) || !delivery.Named(session) || !delivery.Named(turn) {
		return nil, true, nil
	}
	if path == "" && fallback != nil {
		var err error
		path, err = fallback()
		if err != nil {
			return nil, false, err
		}
	}
	if path == "" {
		return nil, false, nil
	}
	timeout := 250 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(0, time.Until(deadline)))
	}
	ro, err := store.OpenReadOnly(ctx, path, timeout)
	if errors.Is(err, store.ErrLiveState) {
		// The live-state guard (test isolation, CRW_REFUSE_LIVE_STATE=1) is a refusal to
		// report, never a store that merely reads as unreadable.
		return nil, false, err
	}
	if err != nil {
		return nil, false, nil
	}
	defer ro.Close()
	if _, err = ro.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return nil, false, nil
	}
	rollback := func() error { _, err := ro.ExecContext(ctx, "ROLLBACK"); return err }
	defer func() { _ = rollback() }()
	base := Object{{Key: "relationshipId", Value: relationship}, {Key: "sessionId", Value: session}, {Key: "turnId", Value: turn}, {Key: "atCurrentHead", Value: false}}
	answer := func(ev string, extra ...Field) (Object, bool, error) {
		o := set(base, "evidence", ev)
		return append(o, extra...), true, nil
	}
	var status, roots string
	var superseded sql.NullString
	var current int64
	err = ro.QueryRowContext(ctx, "SELECT status, superseded_by, execution_generation, artifact_roots FROM relationships WHERE relationship_id = ?", relationship).Scan(&status, &superseded, &current, &roots)
	if errors.Is(err, sql.ErrNoRows) {
		return answer("relationship_absent")
	}
	if err != nil {
		return nil, false, nil
	}
	if status != "active" || (superseded.Valid && superseded.String != "") {
		return answer("relationship_not_active")
	}
	if generation != nil {
		n, ok := evidence.IntOf(generation)
		if !ok || n != current {
			return answer("registration_generation_mismatch", Field{Key: "detail", Value: "the assignment registered generation " + pyvalue.Str(generation) + " and the relationship now stands on generation " + fmt.Sprint(current)})
		}
	}
	if dispatch != nil {
		var opened sql.NullString
		err = ro.QueryRowContext(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", relationship, current).Scan(&opened)
		if errors.Is(err, sql.ErrNoRows) {
			return answer("generation_absent", Field{Key: "detail", Value: "the relationship reports generation " + fmt.Sprint(current) + " and the store holds no record of which dispatch opened it"})
		}
		if err != nil {
			return nil, false, nil
		}
		if !delivery.SameIdentity(opened.String, dispatch) {
			return answer("generation_dispatch_mismatch", Field{Key: "detail", Value: "the relationship stands on generation " + fmt.Sprint(current) + ", which a different dispatch request opened"})
		}
	}
	head, err := delivery.HeadRevisionFrom(ctx, ro, text(relationship), current)
	if err != nil {
		return nil, false, nil
	}
	if slices.Contains([]string{delivery.Fork, delivery.Cycle, delivery.UnknownPredecessor, delivery.Disconnected}, text(get(head, "evidence"))) {
		return answer("head_" + text(get(head, "evidence")))
	}
	if !pyvalue.Truthy(get(head, "eventId")) {
		return answer("no_reviewable_revision")
	}
	var id, stage, producer, payload string
	var revision, thread, eventTurn, manifest sql.NullString
	err = ro.QueryRowContext(ctx, "SELECT event_id, stage, revision_hash, turn_thread_id, turn_id, producer, receipt, manifest_ref FROM events WHERE event_id = ?", get(head, "eventId")).Scan(&id, &stage, &revision, &thread, &eventTurn, &producer, &payload, &manifest)
	if rollbackErr := rollback(); rollbackErr != nil {
		return nil, false, nil
	}
	if err == sql.ErrNoRows {
		return answer("no_reviewable_revision")
	}
	if err != nil {
		return nil, false, nil
	}
	if producer != "child" || (stage != "staged" && stage != "final") {
		return answer("head_is_not_a_child_receipt")
	}
	if !delivery.SameIdentity(thread.String, session) || !delivery.SameIdentity(eventTurn.String, turn) {
		return answer("head_belongs_to_another_turn")
	}
	// The omission reader reads the same stored receipt through the same two functions.
	rv, pv, unreadable := store.DecodeStoredReceipt(roots, payload)
	if unreadable != "" {
		return answer("stored_receipt_unreadable", Field{Key: "detail", Value: unreadable}, Field{Key: "eventId", Value: id})
	}
	state, binding, detail, err := store.DeliverableState(ctx, pv, manifest.String, rv)
	if err != nil {
		return nil, false, err
	}
	if state == store.DeliverableUnverifiable {
		return append(set(base, "evidence", "deliverable_unverifiable"), Field{Key: "detail", Value: nullable(detail)}), false, nil
	}
	if state == store.DeliverableChanged {
		return answer("artifacts_changed_since_receipt", Field{Key: "detail", Value: nullable(detail)}, Field{Key: "eventId", Value: id}, Field{Key: "revisionHash", Value: nullable(revision.String)})
	}
	base = set(base, "atCurrentHead", true)
	return answer("at_head", Field{Key: "eventId", Value: id}, Field{Key: "revisionHash", Value: nullable(revision.String)}, Field{Key: "stage", Value: stage}, Field{Key: "deliverableBinding", Value: binding})
}

// DeliverableState is guard.deliverable_state: store.DeliverableState, which the omission reader
// calls as well, so a stored receipt is judged alike by both. The store reads the values
// DecodeRecord makes of a stored receipt; this exported entry has always also accepted values a
// caller built without decoding (an object as a map, a manifest or the roots as a list of strings
// or maps), so those are normalized here to the ordered objects and lists the store reads.
func DeliverableState(ctx context.Context, payload any, reference string, rootsValue any) (string, string, string, error) {
	if o, ok := evidence.Object(payload); ok {
		normalized := append(Object(nil), o...)
		if manifest, ok := evidence.List(get(o, "manifest")); ok {
			normalized = set(normalized, "manifest", any(manifest))
		}
		payload = normalized
	}
	if roots, ok := evidence.List(rootsValue); ok {
		rootsValue = roots
	}
	return store.DeliverableState(ctx, payload, reference, rootsValue)
}
