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

type Ledger struct{ db *sql.DB }
type Receipt map[string]any

func Open(path string) (*Ledger, error) {
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
	return &Ledger{db}, nil
}
func (l *Ledger) Close() error { return l.db.Close() }

func Endpoint(socket, state string) (string, *Ledger, error) {
	supplied, err := filepath.Abs(socket)
	if err != nil {
		return "", nil, fmt.Errorf("absolute socket: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(supplied)
	if errors.Is(err, os.ErrNotExist) {
		// Resolve dangling aliases too: Path.resolve(strict=False) follows the
		// symlink before accepting its missing final target.
		current := supplied
		for range 40 {
			parent, parentErr := filepath.EvalSymlinks(filepath.Dir(current))
			if parentErr != nil {
				return "", nil, fmt.Errorf("canonical socket parent: %w", parentErr)
			}
			current = filepath.Join(parent, filepath.Base(current))
			target, linkErr := os.Readlink(current)
			if errors.Is(linkErr, os.ErrNotExist) || errors.Is(linkErr, os.ErrInvalid) {
				canonical = current
				break
			}
			if linkErr != nil {
				return "", nil, fmt.Errorf("canonical socket link: %w", linkErr)
			}
			if filepath.IsAbs(target) {
				current = target
			} else {
				current = filepath.Join(parent, target)
			}
		}
		if canonical == "" {
			return "", nil, fmt.Errorf("canonical socket: too many symlinks")
		}
	} else if err != nil {
		return "", nil, fmt.Errorf("canonical socket: %w", err)
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return "", nil, fmt.Errorf("absolute state: %w", err)
	}
	name := func(path string) string {
		hash := sha256.Sum256([]byte(path))
		return filepath.Join(state, "operations-"+hex.EncodeToString(hash[:])[:16]+".sqlite3")
	}
	l, err := Open(name(canonical))
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
	if stored != fingerprint {
		version := receipt["fingerprintVersion"]
		if version == nil {
			version = json.Number("1")
		}
		if version != json.Number("1") || legacy == nil {
			return nil, ErrConflict
		}
		old, err := Fingerprint(method, legacy())
		if err != nil {
			return nil, err
		}
		if old != stored {
			return nil, ErrConflict
		}
	}
	return receipt, nil
}

func (l *Ledger) Begin(ctx context.Context, id, method string, params map[string]any, legacy func() map[string]any) (bool, Receipt, error) {
	if err := checkRequestID(id); err != nil {
		return false, nil, err
	}
	fp, err := Fingerprint(method, params)
	if err != nil {
		return false, nil, err
	}
	receipt := Receipt{"requestId": id, "operation": method, "status": "in_progress_or_unknown", "startedAt": float64(time.Now().UnixNano()) / 1e9, "retrySafe": false, "fingerprintVersion": 2}
	raw, err := json.Marshal(receipt)
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
	if stored != fp {
		version := previous["fingerprintVersion"]
		if version == nil {
			version = json.Number("1")
		}
		if version != json.Number("1") || legacy == nil {
			return false, nil, ErrConflict
		}
		old, err := Fingerprint(method, legacy())
		if err != nil {
			return false, nil, err
		}
		if old != stored {
			return false, nil, ErrConflict
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
		raw, err = json.Marshal(receipt)
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
