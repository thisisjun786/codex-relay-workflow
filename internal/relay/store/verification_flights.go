package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// VerificationIdentity contains only controller-derived facts. The current base
// is deliberately not a hosted-job key: each caller must scan its own range.
type VerificationIdentity struct {
	Repository, Head, WorkflowDigest, Rules string
	WorkflowID                              int64
	Inputs                                  map[string]string
	Tools                                   map[string]string
}

func (v VerificationIdentity) Key() (string, string, error) {
	if v.Repository == "" || v.Head == "" || v.WorkflowID <= 0 || v.WorkflowDigest == "" || v.Rules == "" || v.Tools == nil {
		return "", "", errors.New("incomplete verification identity")
	}
	if v.Inputs == nil {
		v.Inputs = map[string]string{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), string(raw), nil
}

type VerificationFlight struct {
	Key, Identity, Ref, State string
	RunID                     int64
	FirstAttempt, Record      []byte
}

var verificationFlightZone = []string{
	`CREATE TABLE IF NOT EXISTS verification_flights (
 verification_key TEXT PRIMARY KEY NOT NULL CHECK (verification_key <> ''),
 identity_json TEXT NOT NULL,
 ci_ref TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('dispatch_unknown','running','retry_unknown','completed')),
 run_id INTEGER NOT NULL DEFAULT 0,
 first_attempt BLOB NOT NULL DEFAULT X'',
 record BLOB NOT NULL DEFAULT X'',
 CHECK (state <> 'completed' OR (run_id > 0 AND length(record) > 0))
 )`,
	`CREATE TRIGGER IF NOT EXISTS verification_flights_pk_guard BEFORE INSERT ON verification_flights
WHEN EXISTS (SELECT 1 FROM verification_flights WHERE verification_key = NEW.verification_key)
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: verification_flights.verification_key'); END`,
	`CREATE TRIGGER IF NOT EXISTS verification_flights_identity BEFORE UPDATE ON verification_flights
 WHEN NEW.verification_key <> OLD.verification_key OR NEW.identity_json <> OLD.identity_json OR NEW.ci_ref <> OLD.ci_ref
 OR (OLD.run_id <> 0 AND NEW.run_id <> OLD.run_id) OR (OLD.state = 'completed')
 BEGIN SELECT RAISE(ABORT, 'verification identity and completion are immutable'); END`,
	`CREATE TRIGGER IF NOT EXISTS verification_flights_no_delete BEFORE DELETE ON verification_flights
 BEGIN SELECT RAISE(ABORT, 'verification dispatch intents must be retained'); END`,
	`CREATE TRIGGER IF NOT EXISTS verification_flights_rowid_guard BEFORE INSERT ON verification_flights
WHEN NEW.rowid <> -1 AND EXISTS (SELECT 1 FROM verification_flights WHERE rowid = NEW.rowid)
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: verification_flights.rowid'); END`,
	`CREATE TRIGGER IF NOT EXISTS verification_flights_rowid_minus1_mark_guard BEFORE INSERT ON verification_flights
WHEN NEW.rowid = -1
BEGIN DELETE FROM dag_zone_rowid_marks WHERE table_name = 'verification_flights';
  INSERT INTO dag_zone_rowid_marks (table_name) SELECT 'verification_flights' WHERE EXISTS (SELECT 1 FROM verification_flights WHERE rowid = -1); END`,
	`CREATE TRIGGER IF NOT EXISTS verification_flights_rowid_minus1_guard AFTER INSERT ON verification_flights
WHEN NEW.rowid = -1 AND EXISTS (SELECT 1 FROM dag_zone_rowid_marks WHERE table_name = 'verification_flights')
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: verification_flights.rowid'); END`,
}

func (s *Store) VerificationFlight(ctx context.Context, key string) (v VerificationFlight, err error) {
	err = s.Q(ctx).QueryRowContext(ctx, `SELECT verification_key,identity_json,ci_ref,state,run_id,first_attempt,record FROM verification_flights WHERE verification_key=?`, key).
		Scan(&v.Key, &v.Identity, &v.Ref, &v.State, &v.RunID, &v.FirstAttempt, &v.Record)
	return
}

// ClaimVerification commits uncertainty BEFORE the only caller allowed to send a
// dispatch proceeds. Neither restart, timeout nor an empty listing releases it.
// limit is controller CI capacity; zero leaves the existing unbounded policy.
func (s *Store) ClaimVerification(ctx context.Context, identity VerificationIdentity, ref string, limit int) (v VerificationFlight, dispatch bool, err error) {
	if s.InTransaction(ctx) {
		return v, false, errors.New("dispatch intent requires its own durable transaction")
	}
	key, raw, err := identity.Key()
	if err != nil || ref == "" {
		return v, false, errors.Join(err, errors.New("verification needs a complete identity and immutable ref"))
	}
	err = s.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var e error
		v, e = s.VerificationFlight(ctx, key)
		if e == nil {
			if v.Identity != raw {
				return errors.New("verification identity conflict")
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if limit > 0 {
			var active int
			if e = s.Q(ctx).QueryRowContext(ctx, `SELECT count(*) FROM verification_flights WHERE state <> 'completed'`).Scan(&active); e != nil {
				return e
			}
			if active >= limit {
				return errors.New("controller CI capacity is reserved")
			}
		}
		_, e = s.Q(ctx).ExecContext(ctx, `INSERT INTO verification_flights (verification_key,identity_json,ci_ref,state) VALUES (?,?,?,'dispatch_unknown')`, key, raw, ref)
		if e != nil {
			return e
		}
		dispatch = true
		v, e = s.VerificationFlight(ctx, key)
		return e
	})
	return
}

func (s *Store) ObserveVerificationRun(ctx context.Context, key string, runID int64) error {
	if runID <= 0 {
		return errors.New("invalid verification run ID")
	}
	return s.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		v, e := s.VerificationFlight(ctx, key)
		if e != nil {
			return e
		}
		if v.RunID != 0 && v.RunID != runID {
			return errors.New("verification run identity conflict")
		}
		if v.State != "dispatch_unknown" {
			return nil
		}
		_, e = s.Q(ctx).ExecContext(ctx, `UPDATE verification_flights SET run_id=?,state='running' WHERE verification_key=?`, runID, key)
		return e
	})
}

// ClaimVerificationRetry retains attempt one's evidence before a failed-job
// rerun. Only the winning transaction may send it; others reconcile attempt two.
func (s *Store) ClaimVerificationRetry(ctx context.Context, key string, runID int64, first []byte) (send bool, err error) {
	if s.InTransaction(ctx) {
		return false, errors.New("retry intent requires its own durable transaction")
	}
	if len(first) == 0 {
		return false, errors.New("retry requires first-attempt evidence")
	}
	err = s.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		v, e := s.VerificationFlight(ctx, key)
		if e != nil {
			return e
		}
		if v.RunID != runID {
			return errors.New("retry run identity conflict")
		}
		if v.State != "running" {
			return nil
		}
		_, e = s.Q(ctx).ExecContext(ctx, `UPDATE verification_flights SET state='retry_unknown',first_attempt=? WHERE verification_key=?`, first, key)
		send = e == nil
		return e
	})
	return
}

func (s *Store) CompleteVerification(ctx context.Context, key string, runID int64, record []byte) (raw []byte, err error) {
	if len(record) == 0 {
		return nil, errors.New("completion needs observed run evidence")
	}
	err = s.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		v, e := s.VerificationFlight(ctx, key)
		if e != nil {
			return e
		}
		if v.RunID != runID || runID <= 0 {
			return fmt.Errorf("completion run identity conflict")
		}
		if v.State == "completed" {
			raw = v.Record
			return nil
		}
		if v.State != "running" && v.State != "retry_unknown" {
			return errors.New("run has not been identified")
		}
		_, e = s.Q(ctx).ExecContext(ctx, `UPDATE verification_flights SET state='completed',record=? WHERE verification_key=?`, record, key)
		raw = record
		return e
	})
	return
}

// CompletedVerificationRecords offers original receipts to the existing tree
// reuse judge. A miss is conservative; it never converts a failed flight to pass.
func (s *Store) CompletedVerificationRecords(ctx context.Context, repository string) ([][]byte, error) {
	rows, err := s.Q(ctx).QueryContext(ctx, `SELECT record FROM verification_flights WHERE state='completed' AND json_extract(identity_json,'$.Repository')=? ORDER BY rowid DESC LIMIT 64`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
