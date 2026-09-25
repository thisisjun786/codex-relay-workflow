package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Transaction runs a group of writes on one connection under BEGIN IMMEDIATE.
// A failed body or COMMIT rolls back while preserving the original failure.
func (s *Store) Transaction(ctx context.Context, run func(*sql.Conn) error) (err error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("transaction connection: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	defer func() {
		if err != nil {
			if _, rollbackErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
			}
		}
	}()
	if err = run(conn); err != nil {
		return fmt.Errorf("transaction body: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Relationship is the assignment identity read by status and delivery commands.
type Relationship struct {
	ID                string
	IssueKey          string
	Status            string
	ParentTaskID      string
	ChildTaskID       string
	Generation        int64
	ArtifactRoots     string
	AllowedRecipients string
	CreatedAt         string
	UpdatedAt         string
}

func (s *Store) Relationship(ctx context.Context, id string) (Relationship, error) {
	var row Relationship
	err := s.DB.QueryRowContext(ctx, `SELECT relationship_id, issue_key, status, parent_task_id, child_task_id,
 execution_generation, artifact_roots, allowed_recipients, created_at, updated_at
 FROM relationships WHERE relationship_id=?`, id).Scan(&row.ID, &row.IssueKey, &row.Status,
		&row.ParentTaskID, &row.ChildTaskID, &row.Generation, &row.ArtifactRoots,
		&row.AllowedRecipients, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return Relationship{}, fmt.Errorf("relationship %q: %w", id, err)
	}
	return row, nil
}

type Generation struct {
	RelationshipID    string
	Number            int64
	DispatchRequestID string
	AnchorState       string
	DispatchTurnID    sql.NullString
	OpenedAt          string
	BoundAt           sql.NullString
}

func (s *Store) Generation(ctx context.Context, relationshipID string, number int64) (Generation, error) {
	var row Generation
	err := s.DB.QueryRowContext(ctx, `SELECT relationship_id, execution_generation, dispatch_request_id,
 anchor_state, dispatch_turn_id, opened_at, bound_at FROM generations
 WHERE relationship_id=? AND execution_generation=?`, relationshipID, number).Scan(
		&row.RelationshipID, &row.Number, &row.DispatchRequestID, &row.AnchorState,
		&row.DispatchTurnID, &row.OpenedAt, &row.BoundAt)
	if err != nil {
		return Generation{}, fmt.Errorf("generation %q/%d: %w", relationshipID, number, err)
	}
	return row, nil
}

type Event struct {
	ID             string
	RelationshipID string
	Generation     int64
	RevisionHash   string
	Outcome        string
	Producer       string
	TurnThreadID   string
	TurnID         string
	TurnStatus     string
	Receipt        string
	Stage          string
}

func (s *Store) Event(ctx context.Context, id string) (Event, error) {
	var row Event
	err := s.DB.QueryRowContext(ctx, `SELECT event_id, relationship_id, execution_generation, revision_hash,
 outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage
 FROM events WHERE event_id=?`, id).Scan(&row.ID, &row.RelationshipID, &row.Generation,
		&row.RevisionHash, &row.Outcome, &row.Producer, &row.TurnThreadID,
		&row.TurnID, &row.TurnStatus, &row.Receipt, &row.Stage)
	if err != nil {
		return Event{}, fmt.Errorf("event %q: %w", id, err)
	}
	return row, nil
}

type Delivery struct {
	EventID           string
	RelationshipID    string
	Kind              string
	RecipientTaskID   string
	RecipientThreadID string
	State             string
	AttemptCount      int64
	HoldReason        sql.NullString
	CreatedAt         string
	UpdatedAt         string
}

func (s *Store) Delivery(ctx context.Context, eventID string) (Delivery, error) {
	var row Delivery
	err := s.DB.QueryRowContext(ctx, `SELECT event_id, relationship_id, kind, recipient_task_id,
 recipient_thread_id, state, attempt_count, hold_reason, created_at, updated_at
 FROM deliveries WHERE event_id=?`, eventID).Scan(&row.EventID, &row.RelationshipID,
		&row.Kind, &row.RecipientTaskID, &row.RecipientThreadID, &row.State,
		&row.AttemptCount, &row.HoldReason, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery %q: %w", eventID, err)
	}
	return row, nil
}

type Attempt struct {
	RequestID     string
	EventID       string
	Number        int64
	Kind          string
	InternalState string
	State         sql.NullString
	Record        sql.NullString
	Sealed        bool
	ObservedAt    string
}

func (s *Store) Attempt(ctx context.Context, requestID string) (Attempt, error) {
	var row Attempt
	err := s.DB.QueryRowContext(ctx, `SELECT request_id, event_id, attempt_no, kind,
 internal_state, state, record, sealed, observed_at FROM attempts WHERE request_id=?`, requestID).Scan(
		&row.RequestID, &row.EventID, &row.Number, &row.Kind, &row.InternalState,
		&row.State, &row.Record, &row.Sealed, &row.ObservedAt)
	if err != nil {
		return Attempt{}, fmt.Errorf("attempt %q: %w", requestID, err)
	}
	return row, nil
}

type Challenge struct {
	Nonce     string
	WrittenBy string
	WrittenAt string
}

func (s *Store) Challenge(ctx context.Context, nonce string) (Challenge, error) {
	var row Challenge
	err := s.DB.QueryRowContext(ctx, `SELECT nonce, written_by, written_at FROM store_challenge
 WHERE nonce=?`, nonce).Scan(&row.Nonce, &row.WrittenBy, &row.WrittenAt)
	if err != nil {
		return Challenge{}, fmt.Errorf("challenge %q: %w", nonce, err)
	}
	return row, nil
}

func (s *Store) WriteChallenge(ctx context.Context, challenge Challenge) error {
	return s.Transaction(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO store_challenge (nonce, written_by, written_at)
   VALUES (?,?,?)`, challenge.Nonce, challenge.WrittenBy, challenge.WrittenAt)
		return err
	})
}

type JournalEntry struct {
	ID      int64
	At      string
	Kind    string
	Subject string
	Detail  string
}

func (s *Store) Journal(ctx context.Context, kind, subject string) ([]JournalEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, at, kind, subject, detail FROM journal
 WHERE kind=? AND subject=? ORDER BY seq`, kind, subject)
	if err != nil {
		return nil, fmt.Errorf("journal query: %w", err)
	}
	defer rows.Close()
	var entries []JournalEntry
	for rows.Next() {
		var entry JournalEntry
		if err := rows.Scan(&entry.ID, &entry.At, &entry.Kind, &entry.Subject, &entry.Detail); err != nil {
			return nil, fmt.Errorf("journal scan: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal rows: %w", err)
	}
	return entries, nil
}

func (s *Store) AppendJournal(ctx context.Context, entry JournalEntry) error {
	return s.Transaction(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO journal (at, kind, subject, detail)
   VALUES (?,?,?,?)`, entry.At, entry.Kind, entry.Subject, entry.Detail)
		return err
	})
}
