package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Relationship statuses and generation vocabulary shared with Python's registry.py.
const (
	StatusActive       = "active"
	AnchorBound        = "bound"
	AnchorPending      = "anchor_pending"
	ReasonInitial      = "initial_assignment"
	ReasonNeedsChanges = "needs_changes_revision"
)

func isDeactivation(status string) bool {
	return status == "paused" || status == "cancelled" || status == "archived"
}

func isLive(status string) bool { return status == StatusActive || status == "paused" }

// validatedTurnID is registry.validated_turn_id: absent, or an exact non-blank turn id.
func validatedTurnID(turn sql.NullString) error {
	if turn.Valid && strings.TrimSpace(turn.String) == "" {
		return refuse(ReasonUnboundGeneration, "an anchor needs an exact dispatch turn id, not %q", turn.String)
	}
	return nil
}

// CurrentRelationship is registry.get: the relationship with its generation invariant enforced,
// so a pointer at a generation that is not retained is refused on every public read.
func (s *Store) CurrentRelationship(ctx context.Context, id string) (Relationship, error) {
	relationship, err := s.Relationship(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Relationship{}, refuse(ReasonUnregisteredRelationship, "no relationship %q", id)
	}
	if err != nil {
		return Relationship{}, err
	}
	if _, err := s.Generation(ctx, id, relationship.Generation); errors.Is(err, sql.ErrNoRows) {
		return Relationship{}, refuse(ReasonUnknownGeneration, "%q points at generation %d which is not retained", id, relationship.Generation)
	} else if err != nil {
		return Relationship{}, err
	}
	return relationship, nil
}

// RegistryGeneration is registry.generation: the invariant is checked before the row is read.
func (s *Store) RegistryGeneration(ctx context.Context, id string, number int64) (Generation, error) {
	if _, err := s.CurrentRelationship(ctx, id); err != nil {
		return Generation{}, err
	}
	return s.Generation(ctx, id, number)
}

// SetStatus is deactivation only (registry.set_status). Reactivation must go through resume,
// which restates the generation and scope; a dead assignment is never brought back here.
func (s *Store) SetStatus(ctx context.Context, id, status, at string) error {
	if !isDeactivation(status) {
		return refuse(ReasonRelationshipNotActive, "bad status %q", status)
	}
	if _, err := s.CurrentRelationship(ctx, id); err != nil {
		return err
	}
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var before string
		if err := conn.QueryRowContext(ctx, `SELECT status FROM relationships WHERE relationship_id=?`, id).Scan(&before); err != nil {
			return fmt.Errorf("status before write: %w", err)
		}
		if !isLive(before) && isLive(status) {
			return refuse(ReasonRelationshipNotActive, "%q is %q, so %q would bring it back to life", id, before, status)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE relationships SET status=?,updated_at=? WHERE relationship_id=?`, status, at, id); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		return journal(ctx, conn, "status_changed", id, `{"status": `+quoteJSON(status)+`}`, at)
	})
}

// Supersede archives a relationship in favour of its successor (registry.supersede).
func (s *Store) Supersede(ctx context.Context, id, successor, at string) error {
	if _, err := s.CurrentRelationship(ctx, id); err != nil {
		return err
	}
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE relationships SET superseded_by=?,status='archived',updated_at=? WHERE relationship_id=?`, successor, at, id); err != nil {
			return fmt.Errorf("supersede: %w", err)
		}
		return journal(ctx, conn, "superseded", id, `{"by": `+quoteJSON(successor)+`}`, at)
	})
}

// OpenGeneration opens the next generation of an active relationship (registry.open_generation).
// A replay of the same dispatch request id returns its generation instead of opening another.
func (s *Store) OpenGeneration(ctx context.Context, id, dispatchRequest, reason string, dispatchTurn sql.NullString, at string) (int64, error) {
	if reason != ReasonInitial && reason != ReasonNeedsChanges {
		return 0, refuse(ReasonUnknownGeneration, "bad reason %q", reason)
	}
	if err := validatedTurnID(dispatchTurn); err != nil {
		return 0, err
	}
	var number int64
	err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var status string
		var current int64
		var supersededBy sql.NullString
		err := conn.QueryRowContext(ctx, `SELECT execution_generation,status,superseded_by FROM relationships WHERE relationship_id=?`, id).Scan(&current, &status, &supersededBy)
		if errors.Is(err, sql.ErrNoRows) {
			return refuse(ReasonUnregisteredRelationship, "no relationship %q", id)
		}
		if err != nil {
			return fmt.Errorf("current generation: %w", err)
		}
		if status != StatusActive || supersededBy.Valid && supersededBy.String != "" {
			return refuse(ReasonRelationshipNotActive, "relationship %q is not active", id)
		}
		// Refused before the replay lookup, as Python's require_active: a replay is not a way
		// to read a generation back out of a relationship that is no longer active.
		replay := conn.QueryRowContext(ctx, `SELECT execution_generation FROM generations WHERE relationship_id=? AND dispatch_request_id=?`, id, dispatchRequest).Scan(&number)
		if replay == nil {
			return nil
		}
		if !errors.Is(replay, sql.ErrNoRows) {
			return fmt.Errorf("generation replay: %w", replay)
		}
		number = current + 1
		anchor, bound := AnchorPending, sql.NullString{}
		if dispatchTurn.Valid {
			anchor, bound = AnchorBound, sql.NullString{String: at, Valid: true}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES (?,?,?,?,?,?,?,?)`, id, number, dispatchRequest, anchor, dispatchTurn, reason, at, bound); err != nil {
			return fmt.Errorf("insert generation: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE relationships SET execution_generation=?,updated_at=? WHERE relationship_id=?`, number, at, id); err != nil {
			return fmt.Errorf("advance generation: %w", err)
		}
		if err := journal(ctx, conn, "generation_opened", id, fmt.Sprintf(`{"generation": %d, "reason": %s}`, number, quoteJSON(reason)), at); err != nil {
			return err
		}
		// Deliveries left behind by the advance are annotated, never rewritten (registry.py
		// _supersede_older_deliveries_in).
		_, err = conn.ExecContext(ctx, `INSERT INTO delivery_supersession (event_id,reason,noted_at,applied) SELECT d.event_id,'stale_generation',?,0 FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE d.relationship_id=? AND e.execution_generation<? AND e.outcome NOT IN ('merge_turn_grant') AND d.state IN ('queued','sending','held_uncertain','dispatched','deferred_busy','withheld_pre_send','inbox_only') ON CONFLICT(event_id) DO NOTHING`, at, id, number)
		if err != nil {
			return fmt.Errorf("annotate superseded deliveries: %w", err)
		}
		return nil
	})
	return number, err
}

func journal(ctx context.Context, conn *sql.Conn, kind, subject, detail, at string) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)`, at, kind, subject, detail); err != nil {
		return fmt.Errorf("journal %s: %w", kind, err)
	}
	return nil
}

func quoteJSON(text string) string {
	var buf strings.Builder
	appendPythonString(&buf, text)
	return buf.String()
}
