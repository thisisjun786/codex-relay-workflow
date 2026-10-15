package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const PriorAttemptsKept = 5

//lint:ignore ST1005 ledger.py:125 caller-visible message kept byte-identical to Python
var ErrUnknown = errors.New("Unknown request_id")
var ErrConflict = errors.New("request_id already belongs to different arguments; no action taken")

type Ledger struct {
	db      *sql.DB
	options Options
}
type Receipt map[string]any

func Open(path string) (*Ledger, error) {
	return OpenWithOptions(path, Options{})
}

func OpenWithOptions(path string, options Options) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("ledger directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("ledger file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("ledger file close: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=10000; CREATE TABLE IF NOT EXISTS operations (request_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, receipt TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create ledger: %w", err)
	}
	return &Ledger{db: db, options: options}, nil
}
func (l *Ledger) Close() error { return l.db.Close() }

func Endpoint(socket, state string) (string, *Ledger, error) {
	return EndpointWithOptions(socket, state, Options{})
}

func EndpointWithOptions(socket, state string, options Options) (string, *Ledger, error) {
	supplied, err := filepath.Abs(socket)
	if err != nil {
		return "", nil, fmt.Errorf("absolute socket: %w", err)
	}
	canonical, err := canonicalEndpoint(supplied)
	if err != nil {
		return "", nil, err
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return "", nil, fmt.Errorf("absolute state: %w", err)
	}
	name := func(path string) string {
		hash := sha256.Sum256([]byte(path))
		return filepath.Join(state, "operations-"+hex.EncodeToString(hash[:])[:16]+".sqlite3")
	}
	l, err := OpenWithOptions(name(canonical), options)
	if err != nil {
		return "", nil, err
	}
	if canonical != supplied {
		if _, err := os.Stat(name(supplied)); err == nil {
			if err := l.ImportLegacy(context.Background(), name(supplied)); err != nil {
				l.Close()
				return "", nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			l.Close()
			return "", nil, fmt.Errorf("legacy ledger: %w", err)
		}
	}
	return canonical, l, nil
}

// CanonicalEndpoint is the socket EndpointWithOptions resolves before it opens the ledger:
// absolute, then fully resolved, a dangling final alias followed. A caller that only needs the
// endpoint, a read that keeps no ledger, resolves it here instead of opening an operations ledger
// it will not use (CRW-680).
func CanonicalEndpoint(socket string) (string, error) {
	supplied, err := filepath.Abs(socket)
	if err != nil {
		return "", fmt.Errorf("absolute socket: %w", err)
	}
	return canonicalEndpoint(supplied)
}

// canonicalEndpoint resolves an already absolute socket the way EndpointWithOptions did inline.
func canonicalEndpoint(supplied string) (string, error) {
	canonical, err := filepath.EvalSymlinks(supplied)
	if errors.Is(err, os.ErrNotExist) {
		// Resolve dangling aliases too: Path.resolve(strict=False) follows the
		// symlink before accepting its missing final target.
		current := supplied
		for range 40 {
			parent, parentErr := filepath.EvalSymlinks(filepath.Dir(current))
			if parentErr != nil {
				return "", fmt.Errorf("canonical socket parent: %w", parentErr)
			}
			current = filepath.Join(parent, filepath.Base(current))
			target, linkErr := os.Readlink(current)
			if errors.Is(linkErr, os.ErrNotExist) || errors.Is(linkErr, os.ErrInvalid) {
				canonical = current
				break
			}
			if linkErr != nil {
				return "", fmt.Errorf("canonical socket link: %w", linkErr)
			}
			if filepath.IsAbs(target) {
				current = target
			} else {
				current = filepath.Join(parent, target)
			}
		}
		if canonical == "" {
			return "", fmt.Errorf("canonical socket: too many symlinks")
		}
	} else if err != nil {
		return "", fmt.Errorf("canonical socket: %w", err)
	}
	return canonical, nil
}

// checkRequestID is ledger.py _fingerprint's bound: 1-128 characters, counted as Python len.
func checkRequestID(id string) error {
	if n := utf8.RuneCountInString(id); n == 0 || n > 128 {
		return errors.New("request_id must contain 1\u2013128 characters")
	}
	return nil
}

func (l *Ledger) Lookup(ctx context.Context, id, method string, params map[string]any, legacy func() map[string]any) (Receipt, error) {
	if err := checkRequestID(id); err != nil {
		return nil, err
	}
	fingerprint, err := Fingerprint(method, params)
	if err != nil {
		return nil, err
	}
	var stored, raw string
	err = l.db.QueryRowContext(ctx, `SELECT fingerprint, receipt FROM operations WHERE request_id=?`, id).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	receipt, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if err := agrees(method, stored, fingerprint, receipt, legacy); err != nil {
		return nil, err
	}
	return receipt, nil
}

// Held reads the row on id once and answers its receipt, nil with no error when no row holds id, and whether the row
// belongs to a call of method with these params (same), compared as Lookup compares them (the legacy fingerprint
// included for a version-1 receipt). Unlike Lookup it answers a row of other arguments, with same false, instead of
// ErrConflict, and like Get it does not check id: the status, the operation and the comparison all come from one read.
func (l *Ledger) Held(ctx context.Context, id, method string, params map[string]any, legacy func() map[string]any) (receipt Receipt, same bool, err error) {
	fingerprint, err := Fingerprint(method, params)
	if err != nil {
		return nil, false, err
	}
	var stored, raw string
	err = l.db.QueryRowContext(ctx, `SELECT fingerprint, receipt FROM operations WHERE request_id=?`, id).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("held: %w", err)
	}
	if receipt, err = decode(raw); err != nil {
		return nil, false, err
	}
	switch err := agrees(method, stored, fingerprint, receipt, legacy); {
	case errors.Is(err, ErrConflict):
		return receipt, false, nil
	case err != nil:
		return nil, false, err
	}
	return receipt, true, nil
}

// agrees reports whether the row stored under fingerprint stored, holding receipt, belongs to a call of method whose own
// fingerprint is fingerprint: the same fingerprint, or, for a version-1 (pre-upgrade) receipt, the fingerprint of the
// legacy params. A row of other arguments is ErrConflict.
func agrees(method, stored, fingerprint string, receipt Receipt, legacy func() map[string]any) error {
	if stored == fingerprint {
		return nil
	}
	version := receipt["fingerprintVersion"]
	if version == nil {
		version = json.Number("1")
	}
	if version != json.Number("1") || legacy == nil {
		return ErrConflict
	}
	old, err := Fingerprint(method, legacy())
	if err != nil {
		return err
	}
	if old != stored {
		return ErrConflict
	}
	return nil
}

func (l *Ledger) Begin(ctx context.Context, id, method string, params map[string]any, legacy func() map[string]any) (bool, Receipt, error) {
	return l.BeginUnless(ctx, id, method, params, legacy, nil)
}

// BeginUnless is Begin that asks refuse, inside Begin's own transaction, before it begins an attempt on id: held is nil
// when no row held id (the row Begin would insert), otherwise the not_attempted receipt it would re-run. An error from
// refuse ends the transaction with nothing written, the insert included, and is returned. A row in any other status is
// returned for replay without asking refuse, as Begin returns it. A nil refuse is Begin.
func (l *Ledger) BeginUnless(ctx context.Context, id, method string, params map[string]any, legacy func() map[string]any, refuse func(held Receipt) error) (bool, Receipt, error) {
	if err := checkRequestID(id); err != nil {
		return false, nil, err
	}
	fp, err := Fingerprint(method, params)
	if err != nil {
		return false, nil, err
	}
	receipt := Receipt{"requestId": id, "operation": method, "status": "in_progress_or_unknown", "startedAt": float64(time.Now().UnixNano()) / 1e9, "retrySafe": false, "fingerprintVersion": 2}
	if l.options.Now != nil {
		receipt["startedAt"] = l.options.Now()
	}
	raw, err := l.encode(receipt)
	if err != nil {
		return false, nil, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO operations VALUES (?,?,?)`, id, fp, raw)
	if err != nil {
		return false, nil, fmt.Errorf("begin receipt: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	var stored, previousRaw string
	if err := tx.QueryRowContext(ctx, `SELECT fingerprint,receipt FROM operations WHERE request_id=?`, id).Scan(&stored, &previousRaw); err != nil {
		return false, nil, err
	}
	previous, err := decode(previousRaw)
	if err != nil {
		return false, nil, err
	}
	if err := agrees(method, stored, fp, previous, legacy); err != nil {
		return false, nil, err
	}
	if refuse != nil && (inserted == 1 || previous["status"] == "not_attempted") {
		var held Receipt
		if inserted == 0 {
			held = previous
		}
		// The deferred rollback leaves the ledger as this transaction found it.
		if err := refuse(held); err != nil {
			return false, nil, err
		}
	}
	if inserted == 0 && previous["status"] == "not_attempted" {
		history, _ := previous["priorAttempts"].([]any)
		history = append(history, Receipt{"status": previous["status"], "error": previous["error"], "updatedAt": previous["updatedAt"]})
		if len(history) > PriorAttemptsKept {
			history = history[len(history)-PriorAttemptsKept:]
		}
		attempt := 1
		if n, ok := previous["attempt"].(json.Number); ok {
			parsed, err := n.Int64()
			if err != nil {
				return false, nil, err
			}
			attempt = int(parsed)
		}
		receipt["attempt"] = attempt + 1
		receipt["priorAttempts"] = history
		raw, err = l.encode(receipt)
		if err != nil {
			return false, nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE operations SET receipt=? WHERE request_id=?`, raw, id); err != nil {
			return false, nil, err
		}
		previous = receipt
		inserted = 1
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("commit receipt: %w", err)
	}
	return inserted == 1, previous, nil
}

// Retire ends the not_attempted receipt on id for good, in one write transaction and only while it is still
// not_attempted with exactly these params' fingerprint (or, for a version-1 receipt, the legacy params', as Lookup
// compares): its status becomes failed, a terminal status every build replays and never re-runs, with retrySafe false
// and reason as its error, followed by the error of the outcome it ends; priorAttempts, attempt and every other member
// stay as they were. It answers whether it retired the receipt and the receipt as the ledger now holds it: nil when no
// row holds id, and the stored receipt, untouched, when it is no longer not_attempted. A row of other arguments, or of
// another operation (the fingerprint covers the method), is ErrConflict and is untouched. Its one caller is the bridge's
// retirement of a create_thread reservation made before prompts were required (bridge retireBeforePrompt), the one
// exception to the rule that a stored receipt is rewritten only by a call with its own arguments.
func (l *Ledger) Retire(ctx context.Context, id, method string, params map[string]any, legacy func() map[string]any, reason string) (retired bool, now Receipt, err error) {
	if err := checkRequestID(id); err != nil {
		return false, nil, err
	}
	fp, err := Fingerprint(method, params)
	if err != nil {
		return false, nil, err
	}
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("retire receipt: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	// BEGIN IMMEDIATE takes the write lock before the read, so the receipt read is the one the write replaces: a
	// transaction that read first and wrote later could be refused as busy by another writer on the ledger.
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, nil, fmt.Errorf("retire receipt: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, rollback := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			err = errors.Join(err, rollback)
		}
	}()
	var stored, raw string
	err = conn.QueryRowContext(ctx, `SELECT fingerprint, receipt FROM operations WHERE request_id=?`, id).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("retire lookup: %w", err)
	}
	previous, err := decode(raw)
	if err != nil {
		return false, nil, err
	}
	if err = agrees(method, stored, fp, previous, legacy); err != nil {
		return false, nil, err
	}
	if previous["status"] != "not_attempted" {
		return false, previous, nil
	}
	next := make(Receipt, len(previous)+1)
	for k, v := range previous {
		next[k] = v
	}
	next["status"] = "failed"
	next["retrySafe"] = false
	next["error"] = reason
	if ended, _ := previous["error"].(string); ended != "" {
		next["error"] = reason + " (the not_attempted outcome it ends: " + ended + ")"
	}
	next["updatedAt"] = float64(time.Now().UnixNano()) / 1e9
	if l.options.Now != nil {
		next["updatedAt"] = l.options.Now()
	}
	encoded, err := l.encode(next)
	if err != nil {
		return false, nil, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE operations SET receipt=? WHERE request_id=? AND fingerprint=?`, encoded, id, stored); err != nil {
		return false, nil, fmt.Errorf("retire receipt: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, nil, fmt.Errorf("commit retirement: %w", err)
	}
	committed = true
	// The receipt as stored, decoded from what was written, as Get answers it.
	if now, err = decode(string(encoded)); err != nil {
		return false, nil, err
	}
	return true, now, nil
}
