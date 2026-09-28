package testsupport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// SeedOwnership provisions a Python-produced empty schema only when absent.
// It refuses replacing an already-fenced fixture, so reopening tests cannot
// accidentally mask a corrupted mirror or reset a transition.
func SeedOwnership(ctx context.Context, path, socket, owner string) error {
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "takeover.json")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	fresh := errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0)
	if fresh {
		_, file, _, _ := runtime.Caller(0)
		raw, e := os.ReadFile(filepath.Join(filepath.Dir(file), "../../contract/fixtures/sqlite-ddl/python-store.sqlite3"))
		if e != nil {
			return e
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			return e
		}
	} else if err != nil {
		return err
	}
	db, err := ownership.OpenExisting(ctx, path, "rw")
	if err != nil {
		return err
	}
	if fresh {
		if _, err = db.ExecContext(ctx, "DELETE FROM schema_meta WHERE key='socket_path'"); err != nil {
			return errors.Join(err, db.Close())
		}
	}
	if socket != "" {
		if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO schema_meta VALUES('socket_path',?)", socket); err != nil {
			return errors.Join(err, db.Close())
		}
	}
	err = FenceFixture(ctx, db, path, socket, owner)
	return errors.Join(err, db.Close())
}

// FenceFixture stamps a stopped synthetic store. It is not a production
// initializer: callers create their fixture schema first and own its lifetime.
// Tests of refusal deliberately do not call this helper.
func FenceFixture(ctx context.Context, db *sql.DB, path, socket, owner string) error {
	physical, err := ownership.Physical(path)
	if err != nil {
		return err
	}
	path = physical.RealPath
	for _, key := range []string{"write-gate.lock", "takeover.lock"} {
		f, e := os.OpenFile(filepath.Join(filepath.Dir(path), key), os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
	}
	pairs := [][2]string{{"writer_protocol", "1"}, {"owner", owner}, {"owner_epoch", "1"}, {"takeover_id", ""}, {"rollback_allowed", "1"}, {"python_compatibility_build", ownership.PythonBuild}}
	for _, p := range pairs {
		if _, err = db.ExecContext(ctx, "INSERT OR REPLACE INTO schema_meta VALUES(?,?)", p[0], p[1]); err != nil {
			return err
		}
	}
	var id string
	if err = db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='store_id'").Scan(&id); err != nil {
		return err
	}
	if socket == "" {
		if e := db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='socket_path'").Scan(&socket); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	var sock, key *string
	if socket != "" {
		absolute, e := filepath.Abs(socket)
		if e != nil {
			return e
		}
		socket = absolute
		sock = &socket
		hash := sha256.Sum256([]byte(socket))
		value := fmt.Sprintf("%x", hash[:8])
		if scopeRoot := os.Getenv("CODEX_SESSION_RELAY_SCOPE_DIR"); scopeRoot != "" {
			if strings.HasPrefix(scopeRoot, "~/") {
				home, e := os.UserHomeDir()
				if e != nil {
					return e
				}
				scopeRoot = filepath.Join(home, scopeRoot[2:])
			}
			scopeRoot, e = filepath.Abs(scopeRoot)
			if e != nil {
				return e
			}
			salt := sha256.Sum256([]byte(scopeRoot))
			value = fmt.Sprintf("isolated-%x-%s", salt[:4], value)
		}
		key = &value
	}
	record := ownership.Record{Protocol: 1, StoreID: id, Database: physical, AppServerSocket: sock, ScopeKey: key, Epoch: 1, Owner: owner, Phase: "active", RollbackAllowed: true, PythonCompatibilityBuild: ownership.PythonBuild, RelayRPCSocket: filepath.Join(filepath.Dir(path), "control.sock")}
	return ownership.Publish(path, record, nil)
}
