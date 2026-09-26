package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNestedTransaction is Python sqlite3's refusal of BEGIN inside an open transaction.
var ErrNestedTransaction = errors.New("cannot start a transaction within a transaction")

// openTx is the transaction a context carries: Python's self.db.in_transaction, bound to the
// call chain that opened it rather than to the Store, so a concurrent caller (a context without
// it) waits for the one connection while a nested call (a context with it) is refused at once.
type openTx struct {
	store     *Store
	conn      *sql.Conn
	composing bool
}

type openTxKey struct{}

// Compose deliberately joins nested Transaction calls into one commit (store.py composing()).
// Outside this scope, a Transaction inside a Transaction is ErrNestedTransaction.
func (s *Store) Compose(ctx context.Context, run func(context.Context, *sql.Conn) error) error {
	return s.Transaction(ctx, func(txCtx context.Context, conn *sql.Conn) error {
		return run(context.WithValue(txCtx, openTxKey{}, openTx{store: s, conn: conn, composing: true}), conn)
	})
}

// querier is what a store read or domain write needs: the pool or the one connection a
// transaction holds.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// q is where a read runs: on the open transaction's connection when ctx carries one of this
// store's, so it sees that transaction's own writes (Python reads on self.db, the one
// connection); otherwise on the pool, waiting for the connection like any other caller.
func (s *Store) q(ctx context.Context) querier {
	if open, ok := ctx.Value(openTxKey{}).(openTx); ok && open.store == s {
		return open.conn
	}
	return s.DB
}

// Transaction runs a group of writes on the store's one connection under BEGIN IMMEDIATE, and
// hands run a context carrying the open transaction; store calls inside run must use it.
// A failed body or COMMIT rolls back while preserving the original failure.
func (s *Store) Transaction(ctx context.Context, run func(context.Context, *sql.Conn) error) (err error) {
	if open, ok := ctx.Value(openTxKey{}).(openTx); ok && open.store == s {
		if open.composing {
			return run(ctx, open.conn)
		}
		return ErrNestedTransaction
	}
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
	if err = run(context.WithValue(ctx, openTxKey{}, openTx{store: s, conn: conn}), conn); err != nil {
		return fmt.Errorf("transaction body: %w", err)
	}
	if s.faultHook != nil {
		s.faultHook()
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
	err := s.q(ctx).QueryRowContext(ctx, `SELECT relationship_id, issue_key, status, parent_task_id, child_task_id,
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
	err := s.q(ctx).QueryRowContext(ctx, `SELECT relationship_id, execution_generation, dispatch_request_id,
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
	PathBinding    sql.NullString
	Stage          string
}

func (s *Store) Event(ctx context.Context, id string) (Event, error) {
	var row Event
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id, relationship_id, execution_generation, revision_hash,
 outcome, producer, turn_thread_id, turn_id, turn_status, receipt, path_binding_mode, stage
 FROM events WHERE event_id=?`, id).Scan(&row.ID, &row.RelationshipID, &row.Generation,
		&row.RevisionHash, &row.Outcome, &row.Producer, &row.TurnThreadID,
		&row.TurnID, &row.TurnStatus, &row.Receipt, &row.PathBinding, &row.Stage)
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
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id, relationship_id, kind, recipient_task_id,
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
	err := s.q(ctx).QueryRowContext(ctx, `SELECT request_id, event_id, attempt_no, kind,
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
	err := s.q(ctx).QueryRowContext(ctx, `SELECT nonce, written_by, written_at FROM store_challenge
 WHERE nonce=?`, nonce).Scan(&row.Nonce, &row.WrittenBy, &row.WrittenAt)
	if err != nil {
		return Challenge{}, fmt.Errorf("challenge %q: %w", nonce, err)
	}
	return row, nil
}

func (s *Store) WriteChallenge(ctx context.Context, challenge Challenge) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
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
	rows, err := s.q(ctx).QueryContext(ctx, `SELECT seq, at, kind, subject, detail FROM journal
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
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO journal (at, kind, subject, detail)
   VALUES (?,?,?,?)`, entry.At, entry.Kind, entry.Subject, entry.Detail)
		return err
	})
}

// Q exposes the ctx-aware querier to domain packages: the open transaction's connection when ctx
// carries one of this store's, otherwise the pool. Readers in other packages go through it so
// they see the transaction's own uncommitted writes, as Python's one connection does.
func (s *Store) Q(ctx context.Context) Querier { return s.q(ctx) }
