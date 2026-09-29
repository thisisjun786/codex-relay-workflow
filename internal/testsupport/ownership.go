package testsupport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// Ownership fixtures.
//
// Every relay store is fenced (docs/port/decisions.md 14 and 30): schema_meta holds the six
// ownership keys and the takeover.json mirror beside the database names the owner, the epoch
// and the file's physical identity. Both runtimes refuse a store owned by the other, a partially
// fenced store and a mirror that names another file; Go also refuses a legacy unfenced store.
// A test that needs a store in a given ownership state puts it there with exactly one of these
// helpers, each of which produces only a state a real host can have:
//
//   - Create:   an absent store, as the owning runtime's absent-store initializer creates it.
//   - Fence:    a stopped unfenced store (a fixture, a pre-fence copy), stamped the same way.
//   - HandOver: a stopped fenced store after a completed takeover to the other runtime.
//   - Rehome:   a fenced store copied into another directory, with the copy's identity.
//   - Restamp:  a copy of a store its creator still owns, as the other runtime would have made it.
//
// RuntimeIdentity (identity.go), which OwnerNeutral applies to schema_meta, is the one
// normalization a whole-state comparison between the runtimes may apply. No helper edits a
// store that is in use: each takes the write gate exclusively, which every admitted writer of
// either runtime holds shared.

// PythonCompatibilityBuild is the pinned fence build both runtimes stamp and require. It stays
// an alias of ownership.PythonBuild because internal/relay/cli/doctor.go, a product file, still
// imports it from this package.
const PythonCompatibilityBuild = ownership.PythonBuild

// RuntimeOwner is what OwnerNeutral reports for a schema_meta owner that is its writer's own.
const RuntimeOwner = "<runtime owner>"

// OwnerNeutral is RuntimeIdentity for a schema_meta row of a store writer stamped, the
// documented runtime-identity rule a whole-state comparison of a Python-owned store with a
// Go-owned one applies: the row whose key is "owner" must read writer's own name ("python" or
// "go"), which then becomes RuntimeOwner; any other owner value fails the test and is returned
// unchanged. Every other key is returned unchanged. Apply it to schema_meta rows only.
func OwnerNeutral[V any](t testing.TB, writer Runtime, key string, value V) V {
	t.Helper()
	return RuntimeIdentity(t, writer, SchemaMeta, key, value)
}

// Create makes an absent store at dbPath owned by owner ("python" or "go") without running
// either runtime: the frozen Python-produced empty store (contract/fixtures/sqlite-ddl) with a
// freshly minted store_id and store_created_at, schema_meta.socket_path set to the canonical
// socket when one is given, then stamped as Fence does. Nothing may exist at dbPath but an
// empty file, which SQLite and the Python initializer both read as no database and which keeps
// its inode, and the directory may hold no mirror or write gate.
func Create(t testing.TB, dbPath, socket, owner string) {
	t.Helper()
	if err := create(context.Background(), dbPath, socket, owner); err != nil {
		t.Fatalf("testsupport.Create(%s, %q, %s): %v", dbPath, socket, owner, err)
	}
}

// Fence stamps a stopped, unfenced store (one a fixture wrote, or a copy made before the fence)
// exactly as owner's absent-store initializer stamps a new one: the six schema_meta keys
// (writer_protocol 1, owner, owner_epoch 1, takeover_id empty, rollback_allowed 1,
// python_compatibility_build), write-gate.lock and takeover.lock created 0600 and never
// replaced, and takeover.json published atomically with the file's physical identity and the
// App Server socket and scope key that schema_meta.socket_path gives. It does nothing to a
// store already fenced for owner, and fails the test on a partial fence, on a store owned by the
// other runtime (that is HandOver) and on a mirror naming another file (that is Rehome).
func Fence(t testing.TB, dbPath, owner string) {
	t.Helper()
	if err := fence(context.Background(), dbPath, owner); err != nil {
		t.Fatalf("testsupport.Fence(%s, %s): %v", dbPath, owner, err)
	}
}

// HandOver puts a stopped fenced store into the state a completed takeover to the runtime named
// by to leaves: owner=to, owner_epoch+1 and a deterministic 32-hex takeover_id committed in one
// transaction, then the mirror republished active with the transition {id, from, to,
// targetEpoch} both runtimes' validators require of an installed takeover. Holder and
// controller are null: no process of the takeover is alive on a stopped store. A store already
// owned by to is left as it is. It fails the test on a store in use, one in the middle of a
// transition, and one whose rollback was committed away.
func HandOver(t testing.TB, dbPath, to string) {
	t.Helper()
	if err := handOver(context.Background(), dbPath, to); err != nil {
		t.Fatalf("testsupport.HandOver(%s, %s): %v", dbPath, to, err)
	}
}

// Rehome republishes the mirror of a fenced store a test copied into another directory with the
// copy's physical identity and control socket; owner, epoch, phase, transition and scope stay
// as schema_meta and the copied mirror have them. A copy that lacks takeover.json gets the
// mirror its durable stamp implies, and write-gate.lock and takeover.lock are created 0600 when
// the copy lacks them. It fails the test on an unfenced store (that is Fence) and on a directory
// whose mirror belongs to another store: every store needs a directory of its own.
func Rehome(t testing.TB, dbPath string) {
	t.Helper()
	if err := rehome(context.Background(), dbPath); err != nil {
		t.Fatalf("testsupport.Rehome(%s): %v", dbPath, err)
	}
}

// Restamp is for a copy (a snapshot or a backup) of a fenced store that has never changed hands -
// owner_epoch 1, no takeover_id, rollback_allowed 1 - which a test replays in owner's runtime and
// compares whole with what its creator's runtime did to the original. It stamps the copy for owner
// exactly as owner's absent-store initializer stamps a store it creates: it rewrites the
// schema_meta owner value - the one row OwnerNeutral neutralizes - and nothing else in the
// database, then publishes the mirror that stamp implies with the copy's physical identity and
// control socket, creating write-gate.lock and takeover.lock 0600 when the copy lacks them. The
// two stores then differ where OwnerNeutral says they may and nowhere else, which a HandOver
// (owner_epoch+1, a takeover_id) cannot give. A copy already stamped for owner only gets its
// mirror, as Rehome gives it. It fails the test on a store whose mirror names this very file
// (moving an original between the runtimes is HandOver), on a store that has changed hands, on an
// unfenced store (that is Fence), on a store in use and on a directory whose mirror belongs to
// another store.
func Restamp(t testing.TB, dbPath, owner string) {
	t.Helper()
	if err := restamp(context.Background(), dbPath, owner); err != nil {
		t.Fatalf("testsupport.Restamp(%s, %s): %v", dbPath, owner, err)
	}
}

func validOwner(owner string) bool { return owner == "python" || owner == "go" }

func otherOwner(owner string) string {
	if owner == "python" {
		return "go"
	}
	return "python"
}

// fenceState is what a store's directory holds of the fence.
type fenceState struct {
	physical ownership.Database
	meta     map[string]string
	keys     int
	mirror   bool
	gate     bool
}

func (s fenceState) dir() string { return filepath.Dir(s.physical.RealPath) }

func (s fenceState) complete() bool { return s.keys == len(ownership.Keys) && s.mirror && s.gate }

func (s fenceState) String() string {
	return fmt.Sprintf("%d/%d ownership keys, mirror=%t, write gate=%t", s.keys, len(ownership.Keys), s.mirror, s.gate)
}

// inspect reads schema_meta from a disposable copy, so looking never adds a sidecar.
func inspect(ctx context.Context, dbPath string) (fenceState, error) {
	physical, err := ownership.Physical(dbPath)
	if err != nil {
		return fenceState{}, fmt.Errorf("existing store required: %w", err)
	}
	state := fenceState{physical: physical, meta: map[string]string{}}
	snapshot, cleanup, err := ownership.CopySnapshot(physical.RealPath)
	if err != nil {
		return state, err
	}
	defer func() { _ = cleanup() }()
	db, err := ownership.OpenExisting(ctx, snapshot, "ro")
	if err != nil {
		return state, err
	}
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM schema_meta")
	if err != nil {
		return state, errors.Join(fmt.Errorf("not a relay store: %w", err), db.Close())
	}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			break
		}
		state.meta[key] = value
	}
	if err = errors.Join(err, rows.Err(), rows.Close(), db.Close()); err != nil {
		return state, err
	}
	for _, key := range ownership.Keys {
		if _, ok := state.meta[key]; ok {
			state.keys++
		}
	}
	for name, present := range map[string]*bool{"takeover.json": &state.mirror, "write-gate.lock": &state.gate} {
		if _, err = os.Lstat(filepath.Join(state.dir(), name)); err == nil {
			*present = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return state, err
		}
	}
	return state, nil
}

// admitted is the fenced state both runtimes admit: a readable mirror that agrees with the
// durable stamp and with the file it sits next to.
func admitted(ctx context.Context, path string) (ownership.Record, ownership.Stamp, error) {
	record, err := ownership.ReadRecord(path)
	if err != nil {
		return record, ownership.Stamp{}, err
	}
	stamp, err := ownership.SnapshotMeta(ctx, path)
	if err != nil {
		return record, stamp, err
	}
	return record, stamp, ownership.Validate(path, record, stamp)
}

// stopped takes the controller lock and the write gate exclusively, creating either 0600 when
// absent and never replacing one that exists. It fails at once if anything holds them.
//
// The locks are flocks, which belong to the open file description, and a forked child shares
// every description its parent has open until its exec closes the close-on-exec ones. A process
// another test of this binary started while a helper held the locks, and that had not reached
// its exec by the time the helper released them (a busy host delays a new child for
// milliseconds), kept both locks held, and the next helper on that store failed at once with
// EWOULDBLOCK against a child of its own binary. So the descriptors are open only under
// syscall.ForkLock held for reading, which every process start takes for writing across its
// fork: no child is forked holding them. Nothing done while they are held may start a process,
// which would wait for this read lock forever.
func stopped(dir string, createGate bool) (release func() error, err error) {
	syscall.ForkLock.RLock()
	defer func() {
		if err != nil {
			syscall.ForkLock.RUnlock()
		}
	}()
	controller, err := ownership.Lock(filepath.Join(dir, "takeover.lock"), true, true)
	if err != nil {
		return nil, fmt.Errorf("takeover.lock: %w", err)
	}
	gate, err := ownership.Lock(filepath.Join(dir, "write-gate.lock"), true, createGate)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("write-gate.lock (is the store in use?): %w", err), controller.Close())
	}
	return func() error {
		defer syscall.ForkLock.RUnlock()
		return errors.Join(gate.Close(), controller.Close())
	}, nil
}

// initialRecord is the mirror a stopped store's durable stamp implies. An installed takeover's
// transition comes from the other runtime: there are two.
func initialRecord(physical ownership.Database, stamp ownership.Stamp) (ownership.Record, error) {
	record := ownership.Record{
		Protocol: ownership.Protocol, StoreID: stamp.StoreID, Database: physical,
		Epoch: stamp.Epoch, Owner: stamp.Owner, Phase: "active", RollbackAllowed: stamp.RollbackAllowed,
		PythonCompatibilityBuild: stamp.PythonCompatibilityBuild,
		RelayRPCSocket:           filepath.Join(filepath.Dir(physical.RealPath), "control.sock"),
	}
	if stamp.SocketPath != "" {
		socket := stamp.SocketPath
		key, err := ownership.ScopeKey(socket)
		if err != nil {
			return record, err
		}
		record.AppServerSocket, record.ScopeKey = &socket, &key
	}
	if stamp.TakeoverID != "" {
		record.Transition = &ownership.Transition{ID: stamp.TakeoverID, From: otherOwner(stamp.Owner), To: stamp.Owner, TargetEpoch: stamp.Epoch}
	}
	return record, nil
}

// publish writes the mirror and proves the result is the admitted state.
func publish(ctx context.Context, path string, record ownership.Record) error {
	if err := ownership.Publish(path, record, nil); err != nil {
		return err
	}
	if _, _, err := admitted(ctx, path); err != nil {
		return fmt.Errorf("published mirror does not validate: %w", err)
	}
	return nil
}

func fence(ctx context.Context, dbPath, owner string) (err error) {
	if !validOwner(owner) {
		return fmt.Errorf("owner %q is neither python nor go", owner)
	}
	state, err := inspect(ctx, dbPath)
	if err != nil {
		return err
	}
	path := state.physical.RealPath
	switch {
	case state.complete():
		_, stamp, e := admitted(ctx, path)
		if e != nil {
			return fmt.Errorf("fenced store does not validate (a copied store is Rehome): %w", e)
		}
		if stamp.Owner != owner {
			return fmt.Errorf("store is owned by %s at epoch %d (moving it is HandOver)", stamp.Owner, stamp.Epoch)
		}
		return nil
	case state.keys != 0 || state.mirror || state.gate:
		return fmt.Errorf("partially fenced store: %s", state)
	}
	release, err := stopped(state.dir(), true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	db, err := ownership.OpenExisting(ctx, path, "rw")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{"writer_protocol", "1"}, {"owner", owner}, {"owner_epoch", "1"}, {"takeover_id", ""}, {"rollback_allowed", "1"}, {"python_compatibility_build", ownership.PythonBuild}} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_meta VALUES (?, ?)", pair[0], pair[1]); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	stamp, err := ownership.ReadStamp(ctx, db)
	if err != nil {
		return err
	}
	record, err := initialRecord(state.physical, stamp)
	if err != nil {
		return err
	}
	return publish(ctx, path, record)
}

// takeoverID is deterministic, so a handed-over store reads the same in every run.
func takeoverID(storeID, from, to string, epoch int64) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"testsupport-handover", storeID, from, to, strconv.FormatInt(epoch, 10)}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func handOver(ctx context.Context, dbPath, to string) (err error) {
	if !validOwner(to) {
		return fmt.Errorf("owner %q is neither python nor go", to)
	}
	state, err := inspect(ctx, dbPath)
	if err != nil {
		return err
	}
	if !state.complete() {
		return fmt.Errorf("not a fenced store (%s): a fixture is Fence", state)
	}
	path := state.physical.RealPath
	record, stamp, err := admitted(ctx, path)
	if err != nil {
		return fmt.Errorf("fenced store does not validate (a copied store is Rehome): %w", err)
	}
	if stamp.Owner == to {
		return nil
	}
	if record.Phase != "active" {
		return fmt.Errorf("store is %s, in the middle of a transition", record.Phase)
	}
	if !stamp.RollbackAllowed {
		return errors.New("rollback_allowed is 0: ownership can no longer move")
	}
	release, err := stopped(state.dir(), false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	db, err := ownership.OpenExisting(ctx, path, "rw")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, e := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			err = errors.Join(err, e)
		}
	}()
	if current, e := ownership.ReadStamp(ctx, conn); e != nil || current != stamp {
		return errors.Join(errors.New("durable ownership changed while handing over"), e)
	}
	target := stamp.Epoch + 1
	transition := &ownership.Transition{ID: takeoverID(stamp.StoreID, stamp.Owner, to, target), From: stamp.Owner, To: to, TargetEpoch: target}
	for _, pair := range [][2]string{{"owner", to}, {"owner_epoch", strconv.FormatInt(target, 10)}, {"takeover_id", transition.ID}} {
		if _, err = conn.ExecContext(ctx, "UPDATE schema_meta SET value = ? WHERE key = ?", pair[1], pair[0]); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	if _, err = conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	record.Owner, record.Epoch, record.Phase = to, target, "active"
	record.Transition, record.Holder, record.Controller = transition, nil, nil
	return publish(ctx, path, record)
}

func rehome(ctx context.Context, dbPath string) (err error) {
	state, err := inspect(ctx, dbPath)
	if err != nil {
		return err
	}
	if state.keys != len(ownership.Keys) {
		return fmt.Errorf("not a fenced store (%s): a fixture is Fence", state)
	}
	path := state.physical.RealPath
	stamp, err := ownership.SnapshotMeta(ctx, path)
	if err != nil {
		return err
	}
	var record ownership.Record
	if state.mirror {
		raw, e := os.ReadFile(filepath.Join(state.dir(), "takeover.json"))
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &record); e != nil {
			return fmt.Errorf("copied mirror: %w", e)
		}
		named := record.Database.RealPath
		if named != path && filepath.Dir(named) == state.dir() {
			if _, e = os.Lstat(named); e == nil {
				return fmt.Errorf("this directory's mirror belongs to %s: one takeover.json per directory, so give each store its own", named)
			}
		}
		if record.StoreID != stamp.StoreID || record.Owner != stamp.Owner || record.Epoch != stamp.Epoch || record.RollbackAllowed != stamp.RollbackAllowed {
			return fmt.Errorf("copied mirror disagrees with durable ownership beyond physical identity: mirror %s/%s@%d, store %s/%s@%d", record.StoreID, record.Owner, record.Epoch, stamp.StoreID, stamp.Owner, stamp.Epoch)
		}
		record.Database = state.physical
		record.RelayRPCSocket = filepath.Join(state.dir(), "control.sock")
	} else if record, err = initialRecord(state.physical, stamp); err != nil {
		return err
	}
	release, err := stopped(state.dir(), true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return publish(ctx, path, record)
}

func restamp(ctx context.Context, dbPath, owner string) (err error) {
	if !validOwner(owner) {
		return fmt.Errorf("owner %q is neither python nor go", owner)
	}
	state, err := inspect(ctx, dbPath)
	if err != nil {
		return err
	}
	if state.keys != len(ownership.Keys) {
		return fmt.Errorf("not a fenced store (%s): a fixture is Fence", state)
	}
	path := state.physical.RealPath
	if state.mirror {
		raw, e := os.ReadFile(filepath.Join(state.dir(), "takeover.json"))
		if e != nil {
			return e
		}
		var record ownership.Record
		if e = json.Unmarshal(raw, &record); e != nil {
			return fmt.Errorf("copied mirror: %w", e)
		}
		if record.Database == state.physical {
			return errors.New("not a copy: the mirror names this very file, and an original store changes hands with HandOver")
		}
		if named := record.Database.RealPath; named != path && filepath.Dir(named) == state.dir() {
			if _, e = os.Lstat(named); e == nil {
				return fmt.Errorf("this directory's mirror belongs to %s: one takeover.json per directory, so give each store its own", named)
			}
		}
	}
	stamp, err := ownership.SnapshotMeta(ctx, path)
	if err != nil {
		return err
	}
	if stamp.Epoch != 1 || stamp.TakeoverID != "" || !stamp.RollbackAllowed {
		return fmt.Errorf("store has changed hands (%s at epoch %d, takeover_id %q, rollback_allowed %t): only a store its creator still owns is restamped", stamp.Owner, stamp.Epoch, stamp.TakeoverID, stamp.RollbackAllowed)
	}
	release, err := stopped(state.dir(), true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	db, err := ownership.OpenExisting(ctx, path, "rw")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, e := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			err = errors.Join(err, e)
		}
	}()
	if current, e := ownership.ReadStamp(ctx, conn); e != nil || current != stamp {
		return errors.Join(errors.New("durable ownership changed while restamping"), e)
	}
	if _, err = conn.ExecContext(ctx, "UPDATE schema_meta SET value = ? WHERE key = 'owner'", owner); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	if _, err = conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	stamp.Owner = owner
	record, err := initialRecord(state.physical, stamp)
	if err != nil {
		return err
	}
	return publish(ctx, path, record)
}

// frozenStore is the Python-produced empty store the contract freezes.
func frozenStore() ([]byte, error) {
	_, file, _, _ := runtime.Caller(0)
	return os.ReadFile(filepath.Join(filepath.Dir(file), "../../contract/fixtures/sqlite-ddl/python-store.sqlite3"))
}

// canonicalSocket is store.py canonical_socket: expanded, absolute, symlinks resolved as far as
// the path exists.
func canonicalSocket(socket string) (string, error) {
	if socket == "~" || strings.HasPrefix(socket, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		socket = filepath.Join(home, strings.TrimPrefix(socket[1:], "/"))
	}
	absolute, err := filepath.Abs(socket)
	if err != nil {
		return "", err
	}
	var resolve func(string) string
	resolve = func(path string) string {
		if real, e := filepath.EvalSymlinks(path); e == nil {
			return real
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		return filepath.Join(resolve(parent), filepath.Base(path))
	}
	return resolve(absolute), nil
}

func create(ctx context.Context, dbPath, socket, owner string) (err error) {
	if !validOwner(owner) {
		return fmt.Errorf("owner %q is neither python nor go", owner)
	}
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(absolute)
	flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY
	if info, e := os.Lstat(absolute); e == nil && info.Mode().IsRegular() && info.Size() == 0 {
		flags = os.O_WRONLY
	} else if e == nil || !errors.Is(e, os.ErrNotExist) {
		return fmt.Errorf("store is not absent: %s exists (%v)", absolute, e)
	}
	for _, name := range []string{filepath.Join(dir, "takeover.json"), filepath.Join(dir, "write-gate.lock")} {
		if _, e := os.Lstat(name); e == nil || !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("store is not absent: %s exists (%v)", name, e)
		}
	}
	raw, err := frozenStore()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(absolute, flags, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(raw)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	db, err := ownership.OpenExisting(ctx, absolute, "rw")
	if err != nil {
		return err
	}
	statements := []struct {
		query string
		args  []any
	}{
		{"UPDATE schema_meta SET value = ? WHERE key = 'store_id'", []any{hex.EncodeToString(id[:])}},
		{"UPDATE schema_meta SET value = ? WHERE key = 'store_created_at'", []any{time.Now().UTC().Format("2006-01-02T15:04:05Z")}},
		{"DELETE FROM schema_meta WHERE key = 'socket_path'", nil},
	}
	if socket != "" {
		canonical, e := canonicalSocket(socket)
		if e != nil {
			return errors.Join(e, db.Close())
		}
		statements = append(statements, struct {
			query string
			args  []any
		}{"INSERT INTO schema_meta VALUES ('socket_path', ?)", []any{canonical}})
	}
	err = execAll(ctx, db, statements...)
	if err = errors.Join(err, db.Close()); err != nil {
		return err
	}
	return fence(ctx, absolute, owner)
}

func execAll(ctx context.Context, db *sql.DB, statements ...struct {
	query string
	args  []any
}) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, s := range statements {
		if _, err = tx.ExecContext(ctx, s.query, s.args...); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
