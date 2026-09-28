package testsupport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const PythonCompatibilityBuild = "codex-session-relay/0.2.0"

// FencePythonFixture completes a Go-built test store exactly as the installed Python fence does.
// It is test-only: production Go stores remain unfenced until the todo-30 ownership controller
// takes them over. Existing ownership is never reset.
func FencePythonFixture(t testing.TB, db *sql.DB, path, socketPath string, publishMirror ...bool) {
	t.Helper()
	ctx := context.Background()
	var storeID, created string
	if err := db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='store_id'").Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='store_created_at'").Scan(&created); err != nil {
		t.Fatal(err)
	}
	var fenced int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_meta WHERE key IN ('writer_protocol','owner','owner_epoch','takeover_id','rollback_allowed','python_compatibility_build')").Scan(&fenced); err != nil {
		t.Fatal(err)
	}
	if fenced != 0 && fenced != 6 {
		t.Fatalf("testsupport: partial ownership fixture: %d/6 keys", fenced)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Dir(path)
	for _, name := range []string{"write-gate.lock", "takeover.lock"} {
		file, err := os.OpenFile(filepath.Join(state, name), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if fenced == 0 {
		for _, pair := range [][2]string{
			{"writer_protocol", "1"}, {"owner", "python"}, {"owner_epoch", "1"},
			{"takeover_id", ""}, {"rollback_allowed", "1"},
			{"python_compatibility_build", PythonCompatibilityBuild},
		} {
			if _, err := db.ExecContext(ctx, "INSERT INTO schema_meta(key,value) VALUES(?,?)", pair[0], pair[1]); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		expected := map[string]string{
			"writer_protocol": "1", "owner": "python", "owner_epoch": "1", "takeover_id": "",
			"rollback_allowed": "1", "python_compatibility_build": PythonCompatibilityBuild,
		}
		rows, err := db.QueryContext(ctx, "SELECT key,value FROM schema_meta WHERE key IN ('writer_protocol','owner','owner_epoch','takeover_id','rollback_allowed','python_compatibility_build')")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				t.Fatal(err)
			}
			if expected[key] != value {
				t.Fatalf("testsupport: refusing ownership reset for %s=%q", key, value)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if len(publishMirror) > 0 && !publishMirror[0] {
		return
	}
	database, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	scope := sha256.Sum256([]byte(socketPath))
	record := map[string]any{
		"protocol": 1,
		"storeId":  storeID,
		"database": map[string]any{
			"realPath": path, "device": device(database), "inode": inode(database),
			"walDirectoryDevice": device(directory), "walDirectoryInode": inode(directory),
			"walBasename": filepath.Base(path),
		},
		"appServerSocket": socketPath,
		"scopeKey":        hex.EncodeToString(scope[:])[:16],
		"epoch":           1, "owner": "python", "phase": "active",
		"transition": nil, "holder": nil, "controller": nil,
		"rollbackAllowed":          true,
		"pythonCompatibilityBuild": PythonCompatibilityBuild,
		"relayRPCSocket":           filepath.Join(state, "control.sock"),
		"updatedAt":                created,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.TrimSuffix(encoded, []byte{'\n'})
	temporary, err := os.CreateTemp(state, ".takeover.json.*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(temporary.Name()) })
	if err := os.Chmod(temporary.Name(), 0600); err != nil {
		t.Fatal(err)
	}
	_, writeErr := temporary.Write(raw)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary.Name(), filepath.Join(state, "takeover.json")); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		t.Fatal(err)
	}
}

func device(info os.FileInfo) uint64 { return uint64(info.Sys().(*syscall.Stat_t).Dev) }
func inode(info os.FileInfo) uint64  { return info.Sys().(*syscall.Stat_t).Ino }
