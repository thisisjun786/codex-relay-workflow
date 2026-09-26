package store

import (
	"context"
	"database/sql"
	"fmt"
)

// RecordRelationship creates the assignment and its first generation as one fact.
// The caller supplies Python-compatible identifiers and serialized JSON fields.
func (s *Store) RecordRelationship(ctx context.Context, relationship Relationship, generation Generation, parentHostID, childHostID string) error {
	if err := validatedTurnID(generation.DispatchTurnID); err != nil {
		return err
	}
	if generation.AnchorState == AnchorBound && !generation.DispatchTurnID.Valid {
		return refuse(ReasonUnboundGeneration, "a bound generation needs its dispatch turn id")
	}
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, relationship.ID, relationship.IssueKey, relationship.Status, relationship.ParentTaskID, parentHostID, relationship.ChildTaskID, childHostID, relationship.Generation, relationship.ArtifactRoots, relationship.AllowedRecipients, relationship.CreatedAt, relationship.UpdatedAt); err != nil {
			return fmt.Errorf("insert relationship: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at,bound_at) VALUES (?,?,?,?,?,?,?)`, generation.RelationshipID, generation.Number, generation.DispatchRequestID, generation.AnchorState, generation.DispatchTurnID, generation.OpenedAt, generation.BoundAt); err != nil {
			return fmt.Errorf("insert generation: %w", err)
		}
		return nil
	})
}

// RecordEvent writes a receipt without decoding and re-encoding its JSON bytes.
func (s *Store) RecordEvent(ctx context.Context, event Event, firstSeenAt string) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,path_binding_mode,stage,first_seen_at,last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.ID, event.RelationshipID, event.Generation, event.RevisionHash, event.Outcome, event.Producer, event.TurnThreadID, event.TurnID, event.TurnStatus, event.Receipt, event.PathBinding, event.Stage, firstSeenAt, firstSeenAt)
		return err
	})
}

// ReobserveEvent counts a duplicate without replacing the original receipt or stage.
func (s *Store) ReobserveEvent(ctx context.Context, eventID, observedAt string) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `UPDATE events SET observation_count=observation_count+1,last_seen_at=? WHERE event_id=?`, observedAt, eventID)
		return err
	})
}

func (s *Store) RecordObservation(ctx context.Context, observation Observation) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO observations (thread_id,turn_id,terminal_status,relationship_id,classification,event_id,observed_at) VALUES (?,?,?,?,?,?,?)`, observation.ThreadID, observation.TurnID, observation.TerminalStatus, observation.RelationshipID, observation.Classification, observation.EventID, observation.ObservedAt)
		return err
	})
}

// RecordDelivery queues a recipient-bound event without altering its frozen receipt.
func (s *Store) RecordDelivery(ctx context.Context, delivery Delivery) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO deliveries (event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, delivery.EventID, delivery.RelationshipID, delivery.Kind, delivery.RecipientTaskID, delivery.RecipientThreadID, delivery.State, delivery.AttemptCount, delivery.HoldReason, delivery.CreatedAt, delivery.UpdatedAt)
		return err
	})
}

// RecordAttempt freezes exactly the message allocated to this attempt in the same commit.
func (s *Store) RecordAttempt(ctx context.Context, attempt Attempt, message AttemptMessage) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO attempts (request_id,event_id,attempt_no,kind,internal_state,state,record,sealed,observed_at) VALUES (?,?,?,?,?,?,?,?,?)`, attempt.RequestID, attempt.EventID, attempt.Number, attempt.Kind, attempt.InternalState, attempt.State, attempt.Record, attempt.Sealed, attempt.ObservedAt); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_messages (request_id,event_id,attempt_no,kind,message,rendered_at) VALUES (?,?,?,?,?,?)`, message.RequestID, message.EventID, message.Number, message.Kind, message.Message, message.RenderedAt); err != nil {
			return fmt.Errorf("freeze attempt message: %w", err)
		}
		return nil
	})
}

func (s *Store) RecordAck(ctx context.Context, ack Ack) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO acks (event_id,record,ack_turn_id,accepted,verified,rejection_reason,ack_at) VALUES (?,?,?,?,?,?,?)`, ack.EventID, ack.Record, ack.TurnID, ack.Accepted, ack.Verified, ack.RejectionReason, ack.At)
		return err
	})
}

func (s *Store) RecordVerdict(ctx context.Context, verdict Verdict) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO verdicts (event_id,record,verdict,next_generation,verdict_turn_id,decided_at) VALUES (?,?,?,?,?,?)`, verdict.EventID, verdict.Record, verdict.Decision, verdict.NextGeneration, verdict.TurnID, verdict.DecidedAt)
		return err
	})
}
