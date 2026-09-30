package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
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
	for _, entry := range []string{"open", "options", "hold", "registration", "probe"} {
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
			case "hold":
				h, why := HoldForWrite(t.Context(), path, time.Second)
				if h != nil || why == "" {
					if h != nil {
						_ = h.Release()
					}
					t.Fatal("hold bypassed fence")
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
func Test30TransactionEntryAndDraining(t *testing.T) {
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	db, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	defer db.Close()
	r, err := ownership.ReadRecord(path)
	must(t, err)
	r.Phase = "draining"
	r.Transition = &ownership.Transition{ID: "drain-test", From: "go", To: "python", TargetEpoch: 2}
	must(t, ownership.Publish(path, r, nil))
	if other, e := Open(t.Context(), path, ""); e == nil {
		_ = other.Close()
		t.Fatal("new writer admitted while draining")
	}
	must(t, db.Transaction(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
		_, e := conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES('test:admitted','finished')")
		return e
	}))
	r.Epoch++
	must(t, ownership.Publish(path, r, nil))
	called := false
	err = db.Transaction(t.Context(), func(context.Context, *sql.Conn) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("transaction did not revalidate admitted epoch")
	}
}

// Decision 30: an absent store (no database, no mirror, no gate) is created by the
// runtime that finds it, with itself as owner at epoch 1, exactly like the fence
// release's absent-store initialization for Python. A fresh install has no Python.
func Test30AbsentStoreCreatedAsGoEpochOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing/relay.sqlite3")
	socket := filepath.Join(t.TempDir(), "app.sock")
	db, err := Open(t.Context(), path, socket)
	must(t, err)
	stamp, err := ownership.ReadStamp(t.Context(), db.DB)
	must(t, err)
	must(t, db.Close())
	if stamp.Owner != "go" || stamp.Epoch != 1 || stamp.TakeoverID != "" || !stamp.RollbackAllowed || stamp.PythonCompatibilityBuild != ownership.PythonBuild {
		t.Fatalf("fresh stamp = %+v", stamp)
	}
	record, err := ownership.ReadRecord(path)
	must(t, err)
	must(t, ownership.Validate(path, record, stamp))
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

func Test30BeforePragmaRechecksPhysicalStore(t *testing.T) {
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	seed, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	must(t, seed.Close())
	changed := false
	db, err := OpenWith(t.Context(), path, "", OpenOptions{OnConnect: func() {
		if changed {
			return
		}
		changed = true
		must(t, os.Rename(path, path+".held"))
		must(t, os.WriteFile(path, nil, 0600))
	}})
	if err == nil {
		_ = db.Close()
		t.Fatal("initializer wrote to swapped store")
	}
	info, e := os.Stat(path)
	must(t, e)
	if info.Size() != 0 {
		t.Fatal("swapped database was initialized")
	}
}

func Test30SchemaRefusalNoRepair(t *testing.T) {
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
			if db, err := Open(t.Context(), path, ""); err == nil {
				_ = db.Close()
				t.Fatal("partial store admitted")
			}
		})
	}
}

// A bare ownership refusal raised after admission (the takeover inbox's drain revalidating
// its transaction) answers as a refused admission does: reason store_owned_by_other, the
// fence's words, and the refusal (with its queueable mark) still reachable. Anything else is
// left alone.
func Test31AsOwnershipRefusalAnswersAsAdmissionDoes(t *testing.T) {
	cause := &ownership.Refused{Detail: "store is draining", Queueable: true}
	err := AsOwnershipRefusal(fmt.Errorf("drain: %w", cause))
	var refused *RefusedError
	var inner *ownership.Refused
	if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" || refused.Detail != "the relay store is draining" || !errors.As(err, &inner) || !inner.Queueable {
		t.Fatalf("%#v", err)
	}
	already := &RefusedError{Reason: "unassigned_turn", Detail: "x", cause: cause}
	other := errors.New("disk I/O error")
	if AsOwnershipRefusal(already) != error(already) || AsOwnershipRefusal(other) != other || AsOwnershipRefusal(nil) != nil {
		t.Fatal("a non-ownership error was rewritten")
	}
}

// The marker commands' check_start (CheckStartLikeFence) and the service preflight answer the
// scope identity in validate's words and order (ownership.py validate, test_fence.py
// test_a_drifted_scope_identity_refuses_like_go and
// test_a_scope_key_from_another_lock_authority_is_refused_like_go): a scopeKey another lock
// authority gave the socket is refused before the owner is judged, whoever owns the store, and a
// record whose socket names no scope is refused as an invalid identity.
func TestCheckStartLikeFence_words_the_scope_identity_as_validate_does(t *testing.T) {
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	socket := filepath.Join(dir, "app.sock")
	bound := filepath.Join(t.TempDir(), "bound")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", bound)
	testsupport.Create(t, path, socket, "go")
	detail := func(err error) string {
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" {
			t.Fatalf("not an ownership refusal: %#v", err)
		}
		return refused.Detail
	}
	for _, owner := range []string{"go", "python"} {
		t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", bound)
		testsupport.HandOver(t, path, owner)
		want := "the relay store belongs to another runtime"
		if owner == "go" {
			if err := CheckStartLikeFence(t.Context(), path, socket); err != nil {
				t.Fatalf("%s-owned store under the binding authority: %v", owner, err)
			}
		} else if got := detail(CheckStartLikeFence(t.Context(), path, socket)); got != want {
			t.Fatalf("%s-owned store under the binding authority: %q", owner, got)
		}
		t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(t.TempDir(), "other"))
		want = "scope key disagrees with lock authority"
		if got := detail(CheckStartLikeFence(t.Context(), path, socket)); got != want {
			t.Errorf("%s-owned store, marker preflight under another authority: %q", owner, got)
		}
		if got := detail(StartPreflight(t.Context(), path, socket)); got != want {
			t.Errorf("%s-owned store, service preflight under another authority: %q", owner, got)
		}
		if s, err := Open(t.Context(), path, socket); err == nil {
			_ = s.Close()
			t.Errorf("%s-owned store admitted a writer under another authority", owner)
		} else if got := detail(err); got != want {
			t.Errorf("%s-owned store, writer under another authority: %q", owner, got)
		}
	}
	// A socket beside no scope key: an invalid identity, before the owner.
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", bound)
	mirror := filepath.Join(dir, "takeover.json")
	raw, err := os.ReadFile(mirror)
	must(t, err)
	var record map[string]any
	must(t, json.Unmarshal(raw, &record))
	record["scopeKey"] = nil
	raw, err = json.Marshal(record)
	must(t, err)
	must(t, os.WriteFile(mirror, raw, 0o600))
	if got := detail(CheckStartLikeFence(t.Context(), path, socket)); got != "invalid socket/scope identity" {
		t.Errorf("a socket with a null scope key: %q", got)
	}
}

// check_start(path, socket=K) judges a torn binding K's opener would complete (socket_path
// committed as K, the mirror still unbound) without that socket_path, as ownership.py's
// `unbound(...) or meta` does, so a second defect of the owner's store is named rather than the
// torn binding. Without a socket, or with another one, the torn binding is the refusal
// (test_fence.py, fence_parity_test.go
// TestSocketBinding_completes_a_torn_binding_only_for_its_socket).
func TestCheckStartLikeFence_judges_a_torn_binding_as_its_socket_would_complete_it(t *testing.T) {
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	socket := filepath.Join(dir, "app.sock")
	testsupport.Create(t, path, "", "go")
	db, err := sql.Open("sqlite", path)
	must(t, err)
	_, err = db.Exec("INSERT INTO schema_meta VALUES ('socket_path', ?)", socket)
	must(t, err)
	must(t, db.Close())
	detail := func(err error) string {
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" {
			t.Fatalf("not an ownership refusal: %#v", err)
		}
		return refused.Detail
	}
	if err := CheckStartLikeFence(t.Context(), path, socket); err != nil {
		t.Fatalf("the torn binding's own socket: %v", err)
	}
	mirror := filepath.Join(dir, "takeover.json")
	raw, err := os.ReadFile(mirror)
	must(t, err)
	var record map[string]any
	must(t, json.Unmarshal(raw, &record))
	record["storeId"] = "wrong"
	raw, err = json.Marshal(record)
	must(t, err)
	must(t, os.WriteFile(mirror, raw, 0o600))
	for _, check := range []struct{ socket, want string }{
		{socket, "ownership record disagrees with the durable store"},
		{"", "scope without socket"},
		{filepath.Join(dir, "other.sock"), "scope without socket"},
	} {
		if got := detail(CheckStartLikeFence(t.Context(), path, check.socket)); got != check.want {
			t.Errorf("socket %q: %q, want %q", check.socket, got, check.want)
		}
	}
	// Another runtime's torn binding is never this runtime's to complete (unbound's owner).
	db, err = sql.Open("sqlite", path)
	must(t, err)
	_, err = db.Exec("UPDATE schema_meta SET value='python' WHERE key='owner'")
	must(t, err)
	must(t, db.Close())
	record["owner"] = "python"
	raw, err = json.Marshal(record)
	must(t, err)
	must(t, os.WriteFile(mirror, raw, 0o600))
	if got := detail(CheckStartLikeFence(t.Context(), path, socket)); got != "scope without socket" {
		t.Errorf("the other runtime's torn binding: %q", got)
	}
}
