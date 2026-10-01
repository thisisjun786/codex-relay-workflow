package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// StartPreflight is the start check cli.py main ran before a command that opens its own
// admitted connection (service, daemon, and the marker commands that record or confirm the
// selected store): an absent store passes, because the writable opener creates it; a partial one
// (a gate or a mirror without D) is refused in the words the fence's writer meets it with
// (partialStore); a fenced store whose stamp names another runtime is refused
// (CheckStartLikeFence), before any lock, record, marker or child exists.
// The store is named as every opener names it, beside Path.resolve()'s D: a dangling D link
// alone is an absent store, which the writable opener creates through the link. A gate that
// another opener holds EX beside no D and no mirror is a first opener still creating the store
// (creating), which is neither a partial store nor one to act on yet. The preflight waits,
// polling without blocking, until no opener holds the gate EX (awaitCreator), for at most
// CreationWait, and then judges the store again from the start: a creator that gave up (the gate
// let go, no D) leaves a partial store, and a creator still holding the gate at the bound is
// refused as a creation in progress (creationRefused).
func StartPreflight(ctx context.Context, dbPath string) error {
	resolved := resolveLoosely(dbPath)
	deadline := time.Now().Add(CreationWait)
	for creating(resolved) {
		if err := awaitCreator(ctx, resolved, deadline); err != nil {
			return err
		}
	}
	if !storeAbsent(resolved) {
		if err := partialStore(resolved); err != nil {
			return err
		}
	}
	return CheckStartLikeFence(ctx, resolved)
}

// CreationWait bounds how long a start preflight waits for a first opener still creating the
// store (StartPreflight; ownership.py CREATION_WAIT_SECONDS, the same 30 s). It is the bound the
// admitted open already waits for a creation within (awaitCreation, the default busy timeout)
// and ownership.LockWait. A variable so tests can shorten it.
var CreationWait = 30 * time.Second

// awaitCreator waits until no opener holds the gate beside resolved EX, polling every 10 ms
// without blocking (gateHeld), as awaitCreation polls. Past deadline it refuses the command as
// a creation still in progress; a cancelled ctx ends the wait with its error.
func awaitCreator(ctx context.Context, resolved string, deadline time.Time) error {
	for gateHeld(resolved) {
		if !time.Now().Before(deadline) {
			return creationRefused()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}

// creationRefused is the refusal of a start preflight whose creator still holds the gate at
// CreationWait: non-queueable, reason store_owned_by_other, exit 2, in the words ownership.py
// refuse_partial gives it, the bound formatted as Python's {bound:g} formats it.
func creationRefused() error {
	return fenceRefused("store creation in progress: write-gate.lock still held after " +
		strconv.FormatFloat(CreationWait.Seconds(), 'g', -1, 64) + "s; retry")
}

// CheckStartLikeFence is the lock-free preflight cli.py main ran for the commands that answer
// with the selected store ahead of anything else they check (decision 31): the mirror's bytes, if
// any, must read as a JSON object, and a fenced store whose durable stamp, read in place
// (stampInPlace), names another owner or none is refused, before the selection refusal,
// --kind-module and the handler. An absent store and one with neither an ownership key nor a
// mirror pass, as does a store whose write-ahead log an in-place read cannot use: the writable
// open decides them (decision 56).
func CheckStartLikeFence(ctx context.Context, dbPath string) error {
	return checkStamp(ctx, dbPath, false)
}

// CheckStop is the read-only Stop path's check (guard-evaluate's local evaluation): as
// CheckStartLikeFence, except that a store an in-place read cannot read at all is an error, which
// the Stop answers as a host error rather than trusting D.
func CheckStop(ctx context.Context, dbPath string) error {
	return checkStamp(ctx, dbPath, true)
}

// checkStamp is the preflights' reading, in the fence's order: the mirror's bytes (absent is
// none; unreadable, or not a JSON object, is refused in the fence's words), then the stamp read
// in place (an OS or SQLite failure is the command's host error), then the stamp's judgement.
func checkStamp(ctx context.Context, dbPath string, strict bool) error {
	resolved := resolveLoosely(dbPath)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(resolved), "takeover.json"))
	switch {
	case errors.Is(err, unix.ENOENT):
		raw = nil
	case err != nil:
		return fenceRefused("takeover record unreadable: " + err.Error())
	default:
		if why := MirrorRefusal(raw); why != "" {
			return fenceRefused(why)
		}
	}
	// The Stop path never waits on a lock (its hook has a budget); a command's preflight waits
	// for a writer as its own open would.
	wait := ownership.LockWait
	if strict {
		wait = 0
	}
	meta, err := inPlaceMetadata(ctx, dbPath, wait)
	if err != nil {
		if !strict && errors.Is(err, ErrWALWithoutIndex) {
			return nil
		}
		return &hostError{cause: err}
	}
	fenced := raw != nil
	for _, key := range ownership.Keys {
		_, stamped := meta[key]
		fenced = fenced || stamped
	}
	if !fenced {
		return nil
	}
	return stampRefusal(meta)
}

func fenceRefused(detail string) error {
	refused := &ownership.Refused{Detail: detail}
	return &RefusedError{Reason: "store_owned_by_other", Detail: detail, cause: refused}
}

// PythonHostDetail is the host envelope detail of an OS or SQLite failure the store raises
// unhandled out of an open or an ownership read (CheckStartLikeFence), in Go's words, or of the
// exception it raises for a frozen copy it could not read as a manifest, and whether err is one
// of them.
func PythonHostDetail(err error) (string, bool) {
	if encode := EncodeError(err); encode != nil {
		return encode.HostDetail(), true
	}
	var host *hostError
	if errors.As(err, &host) {
		return host.Error(), true
	}
	// A frozen copy that is not a manifest leaves the fence's intake as this exception.
	var frozen *ManifestException
	if errors.As(err, &frozen) {
		return frozen.PythonText(), true
	}
	return "", false
}

// OwnershipRefusalDetail is the detail a refused writable open or start preflight answers with:
// for another runtime's store the fence's words, so a refused write form answers what the fence
// answered; every other refusal keeps Go's own words.
func OwnershipRefusalDetail(err error) string {
	var refused *ownership.Refused
	if errors.As(err, &refused) && strings.HasPrefix(refused.Detail, "store belongs to ") {
		return "the relay store belongs to another runtime"
	}
	return err.Error()
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
	// Whatever a creator racing this opener left has settled (awaitCreation): a gate or a
	// mirror still without D is refused in the fence writer's words, before any lock.
	if err = partialStore(resolved); err != nil {
		return nil, err
	}
	options.verify = func(ctx context.Context, db *sql.DB) (*os.File, error) {
		return verifyWritable(ctx, db, resolved, socket)
	}
	return open(ctx, path, socket, options)
}

// readGateless is the first look at an existing store with no write gate (a store no runtime
// ever fenced, or one whose gate was removed): D's schema_meta is read from a disposable copy
// before anything decides what the store is, so a D that cannot be read as a database fails
// with that error as the command's host error, rather than as an unfenced store. A readable D goes on to the writable open, which refuses a store
// with no gate: Go never initializes one (decision 30). A store with a gate, every store a
// runtime created, is never copied.
func readGateless(ctx context.Context, resolved string) error {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(resolved), "write-gate.lock")); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := readMetadata(ctx, resolved); err != nil {
		return &hostError{cause: err}
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

// createAbsent initializes a store that does not exist at all - no database, no
// ownership mirror and no write gate - as the fence release does for Python
// (ownership.py Admission(initialize=True)), with this runtime as the owner:
// owner=go, owner_epoch=1, writer_protocol=1, rollback_allowed=1 and the matching
// takeover.json, all under an exclusive maintenance gate (decision 30; IS-1: a host
// with no Python interpreter still needs a store). Anything partially present is
// left to the writable open, which refuses it; an existing unfenced store is never adopted
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
		return nil // not created here; the writable open decides
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
	stamp, err := buildAbsent(ctx, temp, socket, options)
	if err != nil {
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
			return nil // another creator won; the writable open decides
		}
		return err
	}
	if err = syncFile(dir); err != nil {
		return err
	}
	if err = createFault("linked"); err != nil {
		return err
	}
	return publishAbsent(path, stamp)
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
			return nil // an untrusted or unreadable gate: the writable open refuses it in its own words
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
// temporary database, then closes it so no WAL outlives the connection. It returns the stamp it
// wrote, which the mirror publishAbsent writes is derived from.
func buildAbsent(ctx context.Context, temp, socket string, options OpenOptions) (stamp ownership.Stamp, err error) {
	s, err := open(ctx, temp, socket, OpenOptions{BusyTimeout: options.BusyTimeout})
	if err != nil {
		return stamp, err
	}
	defer func() { err = errors.Join(err, s.DB.Close()) }()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return stamp, err
	}
	for _, pair := range [][2]string{{"writer_protocol", "1"}, {"owner", "go"}, {"owner_epoch", "1"}, {"takeover_id", ""}, {"rollback_allowed", "1"}, {"python_compatibility_build", ownership.CompatibilityBuild}} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_meta VALUES (?, ?)", pair[0], pair[1]); err != nil {
			return stamp, errors.Join(err, tx.Rollback())
		}
	}
	if err = tx.Commit(); err != nil {
		return stamp, err
	}
	return ownership.ReadStamp(ctx, s.DB)
}

// publishAbsent writes the mirror of the linked store from the stamp it was built with.
func publishAbsent(path string, stamp ownership.Stamp) error {
	record, err := ownership.InitialRecord(path, stamp)
	if err != nil {
		return err
	}
	return ownership.Publish(record.Database.RealPath, record, nil)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
