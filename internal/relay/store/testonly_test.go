package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Code the product no longer calls, kept for the tests that drive it (decision 52).

// RenderAttempt is identity.render_attempt: an explicit nil test, so zero renders as "0" and
// is never quietly treated as the literal "null".
func RenderAttempt(attempt *int) string {
	if attempt == nil {
		return "null"
	}
	return strconv.Itoa(*attempt)
}

func ParseRequestID(request string) (string, int, error) {
	match := regexp.MustCompile(`^del-([0-9a-f]{12})-a([0-9]+)$`).FindStringSubmatch(request)
	if match == nil {
		return "", 0, ErrInvalidIdentity
	}
	number, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, fmt.Errorf("parse attempt: %w", err)
	}
	return match[1], number, nil
}

func AckProof(event, turn string) (string, error) {
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(event) || strings.TrimSpace(turn) == "" {
		return "", ErrInvalidIdentity
	}
	return digest(event+"|"+turn, 64), nil
}

// Outcomes are every outcome a completion receipt may carry (identity.OUTCOMES).
var Outcomes = []string{"ready_for_review", "failed", "interrupted", "blocked_needs_input"}

// RecordObservation is record_observation: the daemon's (thread, turn, terminal status) key
// deduplicates its own stream, and the per-assignment settlement is recorded beside it.
func (in ReceiptIntake) RecordObservation(ctx context.Context, turn TurnReference, classification ObservationOutcome, relationshipID sql.NullString) error {
	now := in.Now()
	return in.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO observations (thread_id,turn_id,terminal_status,relationship_id,classification,event_id,observed_at) VALUES (?,?,?,?,?,NULL,?)`, turn.ThreadID, turn.TurnID, turn.Status, relationshipID, string(classification), now); err != nil {
			return fmt.Errorf("record observation: %w", err)
		}
		if !relationshipID.Valid {
			return nil
		}
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO assignment_settlements (relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES (?,?,?,?,?)`, relationshipID, turn.ThreadID, turn.TurnID, turn.Status, now)
		return err
	})
}

// AcceptChildReceipt is accept_child_receipt: every refusal is recorded for an operator.
func (in ReceiptIntake) AcceptChildReceipt(ctx context.Context, payload []byte, observation TurnReference) (StoredReceipt, error) {
	return in.AcceptChildReceiptWith(ctx, payload, observation, AcceptOptions{})
}

// Deliverable is intake.deliverable: only a final event may be handed to a parent.
func (s *Store) Deliverable(ctx context.Context, eventID string) (bool, error) {
	var stage string
	err := s.q(ctx).QueryRowContext(ctx, `SELECT stage FROM events WHERE event_id=?`, eventID).Scan(&stage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deliverable %q: %w", eventID, err)
	}
	return stage == StageFinal, nil
}

func (l Location) PhysicalIdentity() string {
	return strconv.FormatUint(l.Device, 10) + ":" + strconv.FormatUint(l.Inode, 10)
}

func (l Location) LogLocation() string {
	return fmt.Sprintf("%d:%d:%s", l.LogDevice, l.LogInode, l.LogName)
}

type ReadResult struct {
	Readable bool
	Rows     []Challenge
	Device   uint64
	Inode    uint64
	Links    uint64
	Detail   string
}

// ReadChallengeRows is read_only_rows over store_challenge.
func ReadChallengeRows(ctx context.Context, selection StateSelection) ReadResult {
	var rows []Challenge
	read := ReadOnlyRows(ctx, selection, "SELECT nonce,written_by,written_at FROM store_challenge ORDER BY nonce", nil, func(r RowScanner) error {
		var row Challenge
		if err := r.Scan(&row.Nonce, &row.WrittenBy, &row.WrittenAt); err != nil {
			return err
		}
		rows = append(rows, row)
		return nil
	})
	if !read.Readable || read.Detail != "" {
		rows = nil
	}
	return ReadResult{Readable: read.Readable, Rows: rows, Device: read.Device, Inode: read.Inode, Links: read.Links, Detail: read.Detail}
}

// OpenWith is Open with explicit options (contention tests use a zero busy timeout to observe a
// held lock without waiting on a clock).
func OpenWith(ctx context.Context, path, socketPath string, options OpenOptions) (*Store, error) {
	return openFenced(ctx, path, socketPath, options)
}
