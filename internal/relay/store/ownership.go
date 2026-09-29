package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// RefuseLiveState is the live-state guard every Go writable opener applies; the
// takeover controller checks it before a transition whose Go candidate must open D.
func RefuseLiveState(path string) (string, error) { return refuseLiveState(path) }

func admitWrite(ctx context.Context, path string) (*ownership.Admission, error) {
	admission, err := ownership.Admit(ctx, path)
	if err != nil {
		return nil, &RefusedError{Reason: "store_owned_by_other", Detail: OwnershipRefusalDetail(err), cause: err}
	}
	return admission, nil
}

// StartPreflight is ownership.check_start as cli.py main runs it before a command that opens
// its own admitted connection (service, daemon, and the marker commands that record or
// confirm the selected store): an absent store passes, because the writable opener creates
// it; anything else must be Go-owned and active, or starting for the designated candidate
// ctx carries, or it is refused before any lock, record, marker or child exists.
// socketPath is the command's --socket as given, so the opener that completes a torn socket
// binding is let through (cutover.md Record); one that cannot be canonicalized binds
// nothing, and the opener reports it.
func StartPreflight(ctx context.Context, dbPath, socketPath string) error {
	socket := ""
	if socketPath != "" {
		socket, _ = CanonicalSocket(socketPath)
	}
	if err := ownership.CheckStart(ctx, dbPath, socket); err != nil {
		return &RefusedError{Reason: "store_owned_by_other", Detail: OwnershipRefusalDetail(err)}
	}
	return nil
}

// OwnershipRefusalDetail is the detail a refused Go admission or start preflight answers
// with. Where the fence refuses for the same reason (ownership.py validate: another owner, a
// draining store, a starting store without the candidate's permit) it is the fence's
// OwnershipRefused detail, so a refused write form answers Python's bytes; every other
// refusal keeps Go's own words.
func OwnershipRefusalDetail(err error) string {
	var refused *ownership.Refused
	if errors.As(err, &refused) {
		if words := fenceWords(refused.Detail); words != "" {
			return words
		}
	}
	return err.Error()
}

// AsOwnershipRefusal answers a bare ownership refusal as a refused admission answers it
// (reason store_owned_by_other, in the fence's words where it has them), keeping it reachable
// through errors.As; any other error is returned unchanged.
func AsOwnershipRefusal(err error) error {
	var refused *ownership.Refused
	if RefusalReason(err) == "" && errors.As(err, &refused) {
		return &RefusedError{Reason: "store_owned_by_other", Detail: OwnershipRefusalDetail(err), cause: err}
	}
	return err
}

// fenceWords is the fence's wording of the three ownership decisions Go's judge words its
// own way, or "" for any other refusal.
func fenceWords(detail string) string {
	switch {
	case strings.HasPrefix(detail, "store belongs to "):
		return "the relay store belongs to another runtime"
	case detail == "only designated candidate may enter starting":
		return "only the designated candidate may enter starting"
	case detail == "store is draining":
		return "the relay store is draining"
	}
	return ""
}

func openFenced(ctx context.Context, path, socket string, options OpenOptions) (s *Store, err error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	if err = createAbsent(ctx, resolved, socket, options); err != nil {
		return nil, err
	}
	if err = readGateless(ctx, resolved); err != nil {
		return nil, err
	}
	// A binding holds the gate SH from its own EX until admission holds it SH too.
	bound, err := bindSocket(ctx, resolved, socket)
	if err != nil {
		return nil, err
	}
	admission, err := admitWrite(ctx, resolved)
	if bound != nil {
		err = errors.Join(err, bound.Close())
	}
	if err != nil {
		return nil, errors.Join(err, admission.Close())
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, admission.Close())
		}
	}()
	r, err := ownership.ReadRecord(resolved)
	if err != nil {
		return nil, err
	}
	if socket != "" {
		canonical, e := canonicalSocket(socket)
		if e != nil {
			return nil, e
		}
		if r.AppServerSocket == nil || *r.AppServerSocket != canonical {
			// A domain refusal (exit 2), like every other admission refusal, in the fence's
			// words: a bound store is opened only for the socket its record names.
			refused := &ownership.Refused{Detail: "requested socket disagrees with ownership record"}
			return nil, &RefusedError{Reason: "store_owned_by_other", Detail: refused.Detail, cause: refused}
		}
	}
	// The exact schema is checked before the legacy initializer can run any DDL.
	if err = validateSchemaSnapshot(ctx, resolved); err != nil {
		return nil, err
	}
	options.admission = admission
	s, err = open(ctx, path, socket, options)
	if err != nil {
		return nil, err
	}
	s.admission = admission
	return s, nil
}

// readGateless is the fence's first look at an existing store with no write gate
// (ownership.py Admission for a Store() opener, initialize=True): it reads D's schema_meta from
// a disposable copy before anything decides what the store is. A D that cannot be copied or
// read as a database fails with that error, in Python's f"{type(error).__name__}: {error}"
// (Store() raises it: the host envelope, and declarations._open's store_unopenable), rather
// than as an unfenced store. A readable D goes on to admission, which still refuses a legacy or
// partial store: Go never initializes one (decision 30). The takeover candidate, which Python
// admits without initialize, only ever opens a store whose gate its controller holds.
func readGateless(ctx context.Context, resolved string) error {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(resolved), "write-gate.lock")); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := readMetadata(ctx, resolved); err != nil {
		return &pythonHostError{cause: err}
	}
	return nil
}

// ValidateOwnershipSchema requires every table and column of the frozen v1 DDL.
// It does not repair a store or accept version=1 as proof of compatibility.
func ValidateOwnershipSchema(ctx context.Context, db ownership.Queryer) error {
	raw, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		return err
	}
	ddl := strings.SplitN(string(raw), guardMarker, 2)[0]
	var lines []string
	for _, line := range strings.Split(ddl, "\n") {
		code, _, _ := strings.Cut(line, "--")
		lines = append(lines, code)
	}
	ddl = strings.Join(lines, "\n")
	columnPattern := regexp.MustCompile(`(?:^|,)\s*([A-Za-z_][A-Za-z_0-9]*)\s+(?:TEXT|INTEGER|REAL|BLOB)\b`)
	for _, statement := range strings.Split(ddl, ";") {
		start := strings.Index(statement, "CREATE TABLE IF NOT EXISTS ")
		if start < 0 {
			continue
		}
		definition := strings.TrimSpace(statement[start+len("CREATE TABLE IF NOT EXISTS "):])
		name, body, ok := strings.Cut(definition, "(")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		rows, e := db.QueryContext(ctx, `PRAGMA table_info("`+name+`")`)
		if e != nil {
			return e
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var column, typ string
			var defaultValue any
			if e = rows.Scan(&cid, &column, &typ, &notnull, &defaultValue, &pk); e != nil {
				break
			}
			columns[column] = true
		}
		e = errors.Join(e, rows.Err(), rows.Close())
		if e != nil {
			return e
		}
		if len(columns) == 0 {
			return &ownership.Refused{Detail: "required table missing: " + name}
		}
		for _, match := range columnPattern.FindAllStringSubmatch(body, -1) {
			column := match[1]
			if !columns[column] {
				return &ownership.Refused{Detail: "required column missing: " + name + "." + column}
			}
		}
	}
	return nil
}

// validateSchemaSnapshot requires the frozen v1 schema of the store at resolved, read from
// a disposable copy so no byte beside the database changes; it never repairs the store.
func validateSchemaSnapshot(ctx context.Context, resolved string) error {
	snapshot, cleanup, err := ownership.CopySnapshot(resolved)
	if err != nil {
		return err
	}
	defer cleanup()
	db, err := ownership.OpenExisting(ctx, snapshot, "ro")
	if err != nil {
		return err
	}
	return errors.Join(ValidateOwnershipSchema(ctx, db), db.Close())
}

// bindSocket is the socket binding (cutover.md Record; Python ownership.unbound and
// Admission.initialize). A writable open that passes an App Server socket to an unbound store
// this runtime owns, active and with no transition, binds it: under write-gate EX, waited for
// at most ownership.LockWait (other admitted connections hold SH for their lifetime), the rest
// of the record is revalidated, schema_meta.socket_path is committed, then the mirror is
// published with appServerSocket and scopeKey, every other field unchanged but updatedAt. The
// torn state a crash between the commit and the publication leaves (socket_path set under a
// null mirror) is completed by the next opener passing the same socket. It returns the gate
// downgraded to SH, which the caller holds until its admission holds SH too, or nil when this
// open binds nothing. Read-only commands and the takeover candidate never bind; anything the
// binding does not start from is left to admission, which refuses it in its own words.
func bindSocket(ctx context.Context, resolved, socket string) (held *os.File, err error) {
	if socket == "" || ReadOnlyCommand(ctx) {
		return nil, nil
	}
	canonical, err := canonicalSocket(socket)
	if err != nil {
		return nil, err
	}
	// The lock-free preflight: only a record that binds takes the exclusive gate.
	if r, s, e := readOwnership(ctx, resolved); e != nil || !ownership.Unbound(ctx, r, s, canonical) {
		return nil, nil
	}
	gate, err := ownership.LockWithin(ctx, filepath.Join(filepath.Dir(resolved), "write-gate.lock"), true, "write-gate EX for the socket binding")
	var expired *ownership.LockWaitExpired
	if errors.As(err, &expired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	defer func() {
		if held == nil {
			err = errors.Join(err, gate.Close())
		}
	}()
	r, s, err := readOwnership(ctx, resolved)
	if err != nil || !ownership.Unbound(ctx, r, s, canonical) {
		return nil, nil // changed while this opener waited: admission decides
	}
	// Every other part of the record is validated before anything is written.
	start := s
	start.SocketPath = ""
	if err = ownership.Validate(resolved, r, start); err != nil {
		return nil, &RefusedError{Reason: "store_owned_by_other", Detail: OwnershipRefusalDetail(err), cause: err}
	}
	if err = validateSchemaSnapshot(ctx, resolved); err != nil {
		return nil, err
	}
	key, err := ownership.ScopeKey(canonical)
	if err != nil {
		return nil, err
	}
	if err = commitSocket(ctx, r.Database, canonical, s); err != nil {
		return nil, err
	}
	if err = bindFault("committed"); err != nil {
		return nil, err
	}
	r.AppServerSocket, r.ScopeKey = &canonical, &key
	if err = ownership.Publish(r.Database.RealPath, r, nil); err != nil {
		return nil, err
	}
	// Downgrade in place. A holder that slipped in between keeps the gate; admission decides.
	if unix.Flock(int(gate.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil {
		return nil, nil
	}
	return gate, nil
}

// bindFault is a deterministic crash seam for tests; production leaves it inert.
var bindFault = func(string) error { return nil }

// readOwnership is the record and the durable stamp as a lock-free preflight reads them.
func readOwnership(ctx context.Context, resolved string) (ownership.Record, ownership.Stamp, error) {
	r, err := ownership.ReadRecord(resolved)
	if err != nil {
		return r, ownership.Stamp{}, err
	}
	s, err := ownership.SnapshotMeta(ctx, resolved)
	return r, s, err
}

// commitSocket is the binding's DB half: under the caller's write-gate EX, on the database
// whose physical identity and stamp were validated, schema_meta.socket_path is recorded with
// INSERT OR IGNORE (the first recording wins, as store.py _open records it) and must then be
// the socket this opener binds.
func commitSocket(ctx context.Context, database ownership.Database, socket string, validated ownership.Stamp) (err error) {
	db, err := boundedDB(database.RealPath, "rw", ownership.LockWait, "PRAGMA synchronous=FULL")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if physical, e := ownership.Physical(database.RealPath); e != nil || physical != database {
		return errors.Join(&ownership.Refused{Detail: "physical store changed before the socket binding"}, e)
	}
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
	current, err := ownership.ReadStamp(ctx, conn)
	if err != nil {
		return err
	}
	if current.StoreID != validated.StoreID || current.Owner != validated.Owner || current.Epoch != validated.Epoch || current.TakeoverID != validated.TakeoverID {
		return &ownership.Refused{Detail: "admitted ownership changed"}
	}
	if _, err = conn.ExecContext(ctx, "INSERT OR IGNORE INTO schema_meta VALUES ('socket_path', ?)", socket); err != nil {
		return fmt.Errorf("record socket_path: %w", err)
	}
	var recorded string
	if err = conn.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='socket_path'").Scan(&recorded); err != nil {
		return err
	}
	if recorded != socket {
		refused := &ownership.Refused{Detail: "requested socket disagrees with the recorded store socket"}
		return &RefusedError{Reason: "store_owned_by_other", Detail: refused.Detail, cause: refused}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// createAbsent initializes a store that does not exist at all - no database, no
// ownership mirror and no write gate - as the fence release does for Python
// (ownership.py Admission(initialize=True)), with this runtime as the owner:
// owner=go, owner_epoch=1, writer_protocol=1, rollback_allowed=1 and the matching
// takeover.json, all under an exclusive maintenance gate (decision 30; IS-1: a host
// with no Python interpreter still needs a store). Anything partially present is
// left to admission, which refuses it; an existing unfenced store is never adopted
// here, because only the retained Python fence may initialize it (cutover Step 0).
//
// The gate is placed already held EX (placeGate), as Python places it, so a concurrent
// first opener of either runtime that finds it waits for the creation (awaitCreation)
// instead of seeing a gate without a database. The database is built and stamped under a
// temporary name in S and linked into place with link(2), which never replaces: D never
// exists without its six ownership keys, so no other opener can take it for a legacy store.
func createAbsent(ctx context.Context, path, socket string, options OpenOptions) (err error) {
	dir := filepath.Dir(path)
	if !storeAbsent(path) {
		return awaitCreation(ctx, path, options)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	gate, err := placeGate(dir)
	if err != nil {
		return err
	}
	if gate == nil {
		// Another first opener placed the gate first, already held EX.
		return awaitCreation(ctx, path, options)
	}
	defer func() { err = errors.Join(err, gate.Close()) }()
	// Held EX since before any other opener could find it.
	if _, e := os.Lstat(path); e == nil {
		return nil // not created here; admission decides
	}
	if _, e := os.Lstat(filepath.Join(dir, "takeover.json")); e == nil {
		return nil
	}
	if err = createFault("gate-placed"); err != nil {
		return err
	}
	suffix, err := randomBytes(8)
	if err != nil {
		return err
	}
	// The owner-only file mode is the one Go has always created (0600).
	temp := filepath.Join(dir, ".relay-create-"+hex.EncodeToString(suffix)+".sqlite3")
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() {
		for _, name := range []string{temp, temp + "-wal", temp + "-shm"} {
			if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
				err = errors.Join(err, e)
			}
		}
	}()
	if err = file.Close(); err != nil {
		return err
	}
	if err = buildAbsent(ctx, temp, socket, options); err != nil {
		return err
	}
	if err = syncFile(temp); err != nil {
		return err
	}
	if err = createFault("built"); err != nil {
		return err
	}
	if err = os.Link(temp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // another creator won; admission decides
		}
		return err
	}
	if err = syncFile(dir); err != nil {
		return err
	}
	if err = createFault("linked"); err != nil {
		return err
	}
	return publishAbsent(ctx, path)
}

// placeGate creates S/write-gate.lock already held EX (Python ownership._create_gate): a
// 0600 file under a temporary S/.write-gate-* name, locked while no other process knows its
// name, then link(2)ed into place, so no opener ever finds the gate unlocked before its
// creator has created the store. It returns nil when another opener placed the gate first.
func placeGate(dir string) (gate *os.File, err error) {
	suffix, err := randomBytes(8)
	if err != nil {
		return nil, err
	}
	temp := filepath.Join(dir, ".write-gate-"+hex.EncodeToString(suffix))
	fd, err := unix.Open(temp, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: temp, Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dir, "write-gate.lock"))
	defer func() {
		if e := os.Remove(temp); e != nil {
			err = errors.Join(err, e)
		}
		if gate == nil {
			err = errors.Join(err, file.Close())
		}
	}()
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	if err = os.Link(temp, filepath.Join(dir, "write-gate.lock")); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, nil
		}
		return nil, err
	}
	return file, nil
}

// awaitCreation is Python's blocking LOCK_SH on a gate it found, for the one case Go's
// admission, which never waits for a held gate, would refuse wrongly: a gate another opener
// holds EX while it creates or initializes the store (the database or the mirror is still
// missing) or binds it to its socket (the record is active). Such an opener waits, bounded
// by the busy timeout, until the gate can be shared, then leaves the store to admission,
// which refuses anything still partial. A read-only form never waits (its read goes
// mode=ro instead), and neither does an opener that meets a transfer barrier, taken only
// after the record left phase active.
func awaitCreation(ctx context.Context, path string, options OpenOptions) error {
	dir := filepath.Dir(path)
	gatePath := filepath.Join(dir, "write-gate.lock")
	if _, err := os.Lstat(gatePath); err != nil || ReadOnlyCommand(ctx) {
		return nil
	}
	deadline := time.Now().Add(options.BusyTimeout)
	for waited := false; ; waited = true {
		gate, err := ownership.Lock(gatePath, false, false)
		if err == nil {
			return gate.Close()
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil // an untrusted or unreadable gate: admission refuses it in its own words
		}
		if !waited && !creatingOrBinding(path) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return &RefusedError{Reason: "store_owned_by_other", Detail: "write gate: " + err.Error(), cause: err}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// creatingOrBinding reads, without a lock, whether an exclusive holder of the gate is
// creating, initializing or binding the store rather than transferring it: the database or
// the mirror is missing, or the mirror is in phase active.
func creatingOrBinding(path string) bool {
	for _, name := range []string{path, filepath.Join(filepath.Dir(path), "takeover.json")} {
		if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	r, err := ownership.ReadRecord(path)
	return err == nil && r.Phase == "active"
}

// createFault is a deterministic crash seam for tests; production leaves it inert.
var createFault = func(string) error { return nil }

// buildAbsent runs the frozen DDL, seeds and the six ownership keys on the unpublished
// temporary database, then closes it so no WAL outlives the connection.
func buildAbsent(ctx context.Context, temp, socket string, options OpenOptions) (err error) {
	s, err := open(ctx, temp, socket, OpenOptions{BusyTimeout: options.BusyTimeout})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.DB.Close()) }()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{"writer_protocol", "1"}, {"owner", "go"}, {"owner_epoch", "1"}, {"takeover_id", ""}, {"rollback_allowed", "1"}, {"python_compatibility_build", ownership.PythonBuild}} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_meta VALUES (?, ?)", pair[0], pair[1]); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	return tx.Commit()
}

// publishAbsent writes the mirror of the linked store from its durable stamp.
func publishAbsent(ctx context.Context, path string) error {
	stamp, err := ownership.SnapshotMeta(ctx, path)
	if err != nil {
		return err
	}
	physical, err := ownership.Physical(path)
	if err != nil {
		return err
	}
	record := ownership.Record{Protocol: ownership.Protocol, StoreID: stamp.StoreID, Database: physical, Epoch: 1, Owner: "go", Phase: "active", RollbackAllowed: true, PythonCompatibilityBuild: ownership.PythonBuild, RelayRPCSocket: filepath.Join(filepath.Dir(physical.RealPath), "control.sock")}
	if stamp.SocketPath != "" {
		key, e := ownership.ScopeKey(stamp.SocketPath)
		if e != nil {
			return e
		}
		socketPath := stamp.SocketPath
		record.AppServerSocket, record.ScopeKey = &socketPath, &key
	}
	return ownership.Publish(physical.RealPath, record, nil)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
