package store

import (
	"context"
	"database/sql"
	"fmt"
)

// AttemptMessage holds the immutable bytes allocated to an attempt.
type AttemptMessage struct {
	RequestID  string
	EventID    string
	Number     int64
	Kind       string
	Message    string
	RenderedAt string
}

func (s *Store) AttemptMessage(ctx context.Context, requestID string) (AttemptMessage, error) {
	var row AttemptMessage
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,event_id,attempt_no,kind,message,rendered_at FROM attempt_messages WHERE request_id=?`, requestID).Scan(&row.RequestID, &row.EventID, &row.Number, &row.Kind, &row.Message, &row.RenderedAt)
	if err != nil {
		return AttemptMessage{}, fmt.Errorf("attempt message %q: %w", requestID, err)
	}
	return row, nil
}

type Ack struct {
	EventID         string
	Record          string
	TurnID          string
	Accepted        bool
	Verified        string
	RejectionReason sql.NullString
	At              string
}

func (s *Store) Ack(ctx context.Context, eventID string) (Ack, error) {
	var row Ack
	err := s.DB.QueryRowContext(ctx, `SELECT event_id,record,ack_turn_id,accepted,verified,rejection_reason,ack_at FROM acks WHERE event_id=?`, eventID).Scan(&row.EventID, &row.Record, &row.TurnID, &row.Accepted, &row.Verified, &row.RejectionReason, &row.At)
	if err != nil {
		return Ack{}, fmt.Errorf("ack %q: %w", eventID, err)
	}
	return row, nil
}

type Verdict struct {
	EventID        string
	Record         string
	Decision       string
	NextGeneration sql.NullInt64
	TurnID         string
	DecidedAt      string
}

func (s *Store) Verdict(ctx context.Context, eventID string) (Verdict, error) {
	var row Verdict
	err := s.DB.QueryRowContext(ctx, `SELECT event_id,record,verdict,next_generation,verdict_turn_id,decided_at FROM verdicts WHERE event_id=?`, eventID).Scan(&row.EventID, &row.Record, &row.Decision, &row.NextGeneration, &row.TurnID, &row.DecidedAt)
	if err != nil {
		return Verdict{}, fmt.Errorf("verdict %q: %w", eventID, err)
	}
	return row, nil
}

type Observation struct {
	ThreadID       string
	TurnID         string
	TerminalStatus string
	RelationshipID sql.NullString
	Classification string
	EventID        sql.NullString
	ObservedAt     string
}

func (s *Store) Observation(ctx context.Context, threadID, turnID, status string) (Observation, error) {
	var row Observation
	err := s.DB.QueryRowContext(ctx, `SELECT thread_id,turn_id,terminal_status,relationship_id,classification,event_id,observed_at FROM observations WHERE thread_id=? AND turn_id=? AND terminal_status=?`, threadID, turnID, status).Scan(&row.ThreadID, &row.TurnID, &row.TerminalStatus, &row.RelationshipID, &row.Classification, &row.EventID, &row.ObservedAt)
	if err != nil {
		return Observation{}, fmt.Errorf("observation %q/%q/%q: %w", threadID, turnID, status, err)
	}
	return row, nil
}

type GenerationTurn struct {
	RelationshipID string
	Generation     int64
	TurnID         string
	Evidence       string
	Actor          sql.NullString
	Detail         sql.NullString
	AdmittedAt     string
}

func (s *Store) GenerationTurn(ctx context.Context, relationshipID string, generation int64, turnID string) (GenerationTurn, error) {
	var row GenerationTurn
	err := s.DB.QueryRowContext(ctx, `SELECT relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at FROM generation_turns WHERE relationship_id=? AND execution_generation=? AND turn_id=?`, relationshipID, generation, turnID).Scan(&row.RelationshipID, &row.Generation, &row.TurnID, &row.Evidence, &row.Actor, &row.Detail, &row.AdmittedAt)
	if err != nil {
		return GenerationTurn{}, fmt.Errorf("generation turn %q/%d/%q: %w", relationshipID, generation, turnID, err)
	}
	return row, nil
}

type RevisionLineage struct {
	RelationshipID string
	Generation     int64
	EventID        string
	RevisionHash   string
	SupersedesHash sql.NullString
	DeclaredBy     string
	RecordedAt     string
}

func (s *Store) RevisionLineage(ctx context.Context, relationshipID string, generation int64, eventID string) (RevisionLineage, error) {
	var row RevisionLineage
	err := s.DB.QueryRowContext(ctx, `SELECT relationship_id,execution_generation,event_id,revision_hash,supersedes_hash,declared_by,recorded_at FROM revision_lineage WHERE relationship_id=? AND execution_generation=? AND event_id=?`, relationshipID, generation, eventID).Scan(&row.RelationshipID, &row.Generation, &row.EventID, &row.RevisionHash, &row.SupersedesHash, &row.DeclaredBy, &row.RecordedAt)
	if err != nil {
		return RevisionLineage{}, fmt.Errorf("revision lineage %q/%d/%q: %w", relationshipID, generation, eventID, err)
	}
	return row, nil
}

type AckEvidence struct {
	EventID     string
	Tier        string
	Detail      sql.NullString
	Attempts    int64
	LastReason  sql.NullString
	Fingerprint sql.NullString
	NextCheckAt sql.NullFloat64
	ObservedAt  string
}

func (s *Store) AckEvidence(ctx context.Context, eventID string) (AckEvidence, error) {
	var row AckEvidence
	err := s.DB.QueryRowContext(ctx, `SELECT event_id,tier,detail,attempts,last_reason,fingerprint,next_check_at,observed_at FROM ack_evidence WHERE event_id=?`, eventID).Scan(&row.EventID, &row.Tier, &row.Detail, &row.Attempts, &row.LastReason, &row.Fingerprint, &row.NextCheckAt, &row.ObservedAt)
	if err != nil {
		return AckEvidence{}, fmt.Errorf("ack evidence %q: %w", eventID, err)
	}
	return row, nil
}
