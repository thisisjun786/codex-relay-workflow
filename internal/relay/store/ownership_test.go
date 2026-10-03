package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// stateDir is an owner-only directory, as both runtimes create S (0700); t.TempDir
// follows the test's umask, and a group-writable directory's locks are refused (D3).
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	return dir
}

func Test30AllWritableEntrypointsFenced(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"open", "options", "registration", "probe"} {
		t.Run(entry, func(t *testing.T) {
			path := filepath.Join(stateDir(t), "relay.sqlite3")
			db, err := fixtureOpen(t.Context(), path, "")
			must(t, err)
			must(t, db.Close())
			raw, err := ownership.OpenExisting(t.Context(), path, "rw")
			must(t, err)
			_, err = raw.Exec("UPDATE schema_meta SET value='python' WHERE key='owner'")
			must(t, err)
			must(t, raw.Close())
			record, err := ownership.ReadRecord(path)
			must(t, err)
			record.Owner = "python"
			must(t, ownership.Publish(path, record, nil))
			before, err := os.ReadFile(path)
			must(t, err)
			switch entry {
			case "open":
				opened, e := Open(t.Context(), path, "")
				if e == nil {
					_ = opened.Close()
					t.Fatal("foreign writer admitted")
				}
			case "options":
				opened, e := OpenWith(t.Context(), path, "", OpenOptions{})
				if e == nil {
					_ = opened.Close()
					t.Fatal("OpenWith bypassed fence")
				}
			case "registration":
				// The fence raises its refusal out of the hold rather than yielding it.
				err := RegistrationHold(t.Context(), path, func(*sql.Conn, string) error {
					t.Fatal("registration bypassed fence")
					return nil
				})
				if RefusalReason(err) != "store_owned_by_other" {
					t.Fatalf("registration refusal %v", err)
				}
			case "probe":
				p := Probe(t.Context(), StateSelection{Path: filepath.Dir(path)})
				if p.Access.DBWritable {
					t.Fatal("probe bypassed fence")
				}
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if string(before) != string(after) {
				t.Fatal("refusal changed database")
			}
		})
	}
}
func Test30GateHeldUntilConnectionClosed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	db, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	gate := filepath.Join(filepath.Dir(path), "write-gate.lock")
	if lock, e := ownership.Lock(gate, true, false); e == nil {
		_ = lock.Close()
		t.Fatal("write gate not held for connection lifetime")
	}
	must(t, db.Close())
	lock, err := ownership.Lock(gate, true, false)
	must(t, err)
	must(t, lock.Close())
}

// Decision 30: an absent store (no database, no mirror, no gate) is created by the
// runtime that finds it, with itself as owner at epoch 1, exactly like the fence
// release's absent-store initialization for Python. A fresh install has no Python.
func Test30AbsentStoreCreatedAsGoEpochOne(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing/relay.sqlite3")
	socket := filepath.Join(t.TempDir(), "app.sock")
	db, err := Open(t.Context(), path, socket)
	must(t, err)
	stamp, err := ownership.ReadStamp(t.Context(), db.DB)
	must(t, err)
	must(t, db.Close())
	if stamp.Owner != "go" || stamp.Epoch != 1 || stamp.TakeoverID != "" || !stamp.RollbackAllowed || stamp.CompatibilityBuild != ownership.CompatibilityBuild {
		t.Fatalf("fresh stamp = %+v", stamp)
	}
	record, err := ownership.ReadRecord(path)
	must(t, err)
	requireMirrorAgrees(t, path, record, stamp)
	if record.Phase != "active" || record.Owner != "go" || record.Epoch != 1 || record.AppServerSocket == nil || *record.AppServerSocket != socket {
		t.Fatalf("fresh mirror = %+v", record)
	}
	for _, name := range []string{"", "missing"} {
		info, e := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(path)), name))
		must(t, e)
		if name == "missing" && info.Mode().Perm() != 0700 {
			t.Fatalf("state directory mode %v", info.Mode().Perm())
		}
	}
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database mode %v", info.Mode().Perm())
	}
	// The store it made is admitted again on the next open.
	again, err := Open(t.Context(), path, socket)
	must(t, err)
	must(t, again.Close())
}

// Anything partially present is not "absent": admission refuses it and nothing is
// created or repaired. A legacy (unfenced) database is never adopted by Go.
func Test30PartialOrLegacyStoreNeverInitialized(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"gate-only", func(t *testing.T, path string) {
			must(t, os.WriteFile(filepath.Join(filepath.Dir(path), "write-gate.lock"), nil, 0600))
		}},
		{"mirror-only", func(t *testing.T, path string) {
			must(t, os.WriteFile(filepath.Join(filepath.Dir(path), "takeover.json"), []byte("{}"), 0600))
		}},
		{"legacy-database", func(t *testing.T, path string) {
			legacy, err := fixtureOpen(t.Context(), path, "")
			must(t, err)
			_, err = legacy.DB.Exec("DELETE FROM schema_meta WHERE key IN ('writer_protocol','owner','owner_epoch','takeover_id','rollback_allowed','python_compatibility_build')")
			must(t, err)
			must(t, legacy.Close())
			must(t, os.Remove(filepath.Join(filepath.Dir(path), "takeover.json")))
			must(t, os.Remove(filepath.Join(filepath.Dir(path), "write-gate.lock")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stateDir(t)
			path := filepath.Join(dir, "relay.sqlite3")
			tc.setup(t, path)
			before := snapshotDir(t, dir)
			if db, err := Open(t.Context(), path, ""); err == nil {
				_ = db.Close()
				t.Fatal("partial store admitted")
			}
			if after := snapshotDir(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed the state directory:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	out := map[string]string{}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		must(t, err)
		out[entry.Name()] = string(raw)
	}
	return out
}

func Test30SchemaRefusalNoRepair(t *testing.T) {
	t.Parallel()
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	db, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	_, err = db.DB.Exec("DROP TABLE attempt_messages")
	must(t, err)
	must(t, db.Close())
	before, err := os.ReadFile(path)
	must(t, err)
	if opened, e := Open(t.Context(), path, ""); e == nil {
		_ = opened.Close()
		t.Fatal("missing required table was repaired")
	}
	after, err := os.ReadFile(path)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("schema refusal changed source")
	}
}

func Test30CreateAbsentCrashProcess(t *testing.T) {
	// Serial: child-process entry point (a parent test re-executes this binary for it); it assigns the createFault seam and exits the process.
	path, point := os.Getenv("CRW_CRASH_DB"), os.Getenv("CRW30_CREATE_POINT")
	if path == "" || point == "" {
		return
	}
	createFault = func(p string) error {
		if p == point {
			os.Exit(91)
		}
		return nil
	}
	db, err := Open(t.Context(), path, "")
	if err == nil {
		_ = db.Close()
	}
	t.Fatalf("crash point %s not reached: %v", point, err)
}

// Audit finding 59: an absent store is built and stamped under a temporary name and
// linked into place, so D never exists without its six ownership keys. A crash at
// either durable edge leaves a partial store that admission refuses (decision D0).
func Test30CreateAbsentNeverExposesUnstampedDatabase(t *testing.T) {
	t.Parallel()
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	db, err := Open(t.Context(), path, "")
	must(t, err)
	must(t, db.Close())
	stamp, err := ownership.SnapshotMeta(t.Context(), path)
	must(t, err)
	if stamp.Owner != "go" || stamp.Epoch != 1 || !stamp.RollbackAllowed {
		t.Fatalf("stamp %+v", stamp)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".relay-create-*")); len(leftovers) != 0 {
		t.Fatalf("temporary database left behind: %v", leftovers)
	}
	for _, point := range []string{"built", "linked"} {
		t.Run(point, func(t *testing.T) {
			dir := stateDir(t)
			path := filepath.Join(dir, "relay.sqlite3")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^Test30CreateAbsentCrashProcess$")
			child.Env = append(os.Environ(), "CRW_CRASH_DB="+path, "CRW30_CREATE_POINT="+point)
			raw, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 91 {
				t.Fatalf("child: %v %s", err, raw)
			}
			_, statErr := os.Stat(path)
			switch point {
			case "built":
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("database visible before it was stamped: %v", statErr)
				}
			case "linked":
				must(t, statErr)
				stamp, err := ownership.SnapshotMeta(t.Context(), path)
				must(t, err)
				if stamp.Owner != "go" || stamp.Epoch != 1 {
					t.Fatalf("linked database without the go stamp: %+v", stamp)
				}
			}
			db, err := Open(t.Context(), path, "")
			if point == "built" {
				if err == nil {
					_ = db.Close()
					t.Fatal("partial store admitted")
				}
				return
			}
			// A creator that died after linking its stamped database left no mirror. The stamp
			// alone admits a writer (decision 56); nothing publishes the missing mirror.
			must(t, err)
			must(t, db.Close())
			if _, err := os.Stat(filepath.Join(dir, "takeover.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the missing mirror was published: %v", err)
			}
		})
	}
}

// requireMirrorAgrees fails the test unless the mirror states what the durable stamp does, for
// the file it sits beside.
func requireMirrorAgrees(t *testing.T, path string, record ownership.Record, stamp ownership.Stamp) {
	t.Helper()
	physical, err := ownership.Physical(path)
	must(t, err)
	socket := ""
	if record.AppServerSocket != nil {
		socket = *record.AppServerSocket
	}
	if record.Owner != stamp.Owner || record.Epoch != stamp.Epoch || record.StoreID != stamp.StoreID || record.Database != physical ||
		record.RollbackAllowed != stamp.RollbackAllowed || record.CompatibilityBuild != stamp.CompatibilityBuild || socket != stamp.SocketPath {
		t.Fatalf("mirror %+v disagrees with stamp %+v", record, stamp)
	}
}
