package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

type ProbeAccess struct {
	DirectoryExists   bool
	DirectoryReadable bool
	DirectoryWritable bool
	DBExists          bool
	DBReadable        bool
	DBWritable        bool
	// Measured is whether the write probe ran: DirectoryWritable and DBWritable are then what a
	// write attempt answered. Without it they are judged, never tried (ProbeOptions).
	Measured bool
	Detail   string
}

// ProbeOptions chooses how a probe finds out what this process may write.
type ProbeOptions struct {
	// Write measures writability by writing: a temporary file is created and removed in the state
	// directory, and the write gate is taken shared for a write transaction that is begun and
	// rolled back on a read-write connection. Without it the probe only reads: writability is
	// judged from permissions, the ownership stamp and the gate's own metadata, no file is created
	// and the write gate is never opened.
	Write bool
}

type ProbeResult struct {
	Store     Location
	Access    ProbeAccess
	Selection StateSelection
}

// Probe is ProbeWith without the write probe: a read of the selected state.
func Probe(ctx context.Context, selection StateSelection) ProbeResult {
	return ProbeWith(ctx, selection, ProbeOptions{})
}

// ProbeWith is store.probe: it describes the selected state WITHOUT constructing a Store, and
// every step owns its error and becomes a note in Access.Detail instead of failing the call. Its
// reads create no SQLite sidecar wherever SQLite allows (openHeldRead).
func ProbeWith(ctx context.Context, selection StateSelection, opts ProbeOptions) (result ProbeResult) {
	result = ProbeResult{Selection: selection, Store: Location{DBPath: selection.DBPath()}}
	var notes []string
	defer func() { result.Access.Detail = strings.Join(notes, "; ") }()
	if info, err := os.Stat(selection.Path); err == nil {
		result.Access.DirectoryExists = info.IsDir()
	} else if !os.IsNotExist(err) {
		notes = append(notes, "directory stat failed: "+err.Error())
	}
	if result.Access.DirectoryExists {
		result.Access.DirectoryReadable = syscall.Access(selection.Path, 0x4|0x1) == nil
	}
	if _, err := os.Stat(selection.DBPath()); err == nil {
		result.Access.DBExists = true
		if real, err := resolvePath(selection.DBPath()); err == nil {
			result.Store.RealPath = real
		}
	} else if result.Access.DirectoryExists {
		notes = append(notes, "database stat failed: "+err.Error())
	}
	refusal, unstamped := ownershipPreflight(ctx, selection.DBPath())
	if refusal != "" {
		// A store this runtime may not write is diagnosed without a live SQLite open (even
		// mode=ro can create WAL/SHM on a copied WAL database) and without creating even a
		// temporary file beside it.
		probeForeign(ctx, selection.DBPath(), &result, &notes)
		notes = append(notes, refusal)
		return result
	}
	if result.Access.DirectoryExists {
		if opts.Write {
			result.Access.Measured = true
			// Writability is measured by writing: a privileged runner ignores mode bits.
			if temp, err := os.CreateTemp(selection.Path, ".probe-"); err == nil {
				result.Access.DirectoryWritable = true
				_ = temp.Close()
				_ = os.Remove(temp.Name())
			} else {
				notes = append(notes, "directory write failed: "+err.Error())
			}
		} else if err := syscall.Access(selection.Path, 0x2|0x1); err == nil {
			// Judged by access(2): it sees mode bits, a privileged runner's capabilities and a
			// read-only file system, not a sandbox that denies writes by another mechanism.
			result.Access.DirectoryWritable = true
		} else {
			notes = append(notes, "directory write judged unavailable: "+err.Error())
		}
	}
	if !result.Access.DBExists {
		return result
	}
	file, expected, refused := holdDatabase(ctx, selection.DBPath())
	if file == nil {
		notes = append(notes, "the database could not be held open: "+refused)
		return result
	}
	defer file.Close()
	held, ok := measureHeld(ctx, file)
	if !ok {
		notes = append(notes, "the held database could not be identified, so it was not read")
		return result
	}
	result.Store.Device, result.Store.Inode, result.Store.Links = held.device, held.inode, held.links
	result.Store.RealPath = expected
	if device, inode, name, located := heldLogLocation(file, expected); located {
		result.Store.LogDevice, result.Store.LogInode, result.Store.LogName = device, inode, name
	} else {
		notes = append(notes, "the log location of the held database could not be measured")
	}
	stamp, reached := probeRead(ctx, file, expected, !opts.Write, &result, &notes)
	// The closing question for the read, and the gate for the write probe.
	if moved := relocation(file, expected); moved != "" {
		notes = append(notes, "the database moved while it was being read: "+moved)
		result.Access.DBReadable = false
		result.Store = Location{DBPath: result.Store.DBPath, RealPath: result.Store.RealPath}
		return result
	}
	if opts.Write {
		probeWrite(ctx, file, expected, unstamped, &result, &notes)
	} else if reached {
		judgeWrite(expected, unstamped, stamp, &result, &notes)
	}
	return result
}

// ownershipPreflight is probe's check_start: "" when this runtime may be admitted to write
// the store (or it is absent or legacy, left to the probe itself), otherwise the refusal as
// Python's str(OwnershipRefused). The mirror is read as ownership.mirror reads it, before the
// database; a database that cannot be read at all is left to the probe below, which reports
// it in detail. Go's admission preflight decides whether a fenced store is refused, and
// ownership.py validate's words name why wherever validate refuses it too. unstamped is
// whether the reading found no ownership stamp at all: no mirror and no ownership key.
func ownershipPreflight(ctx context.Context, dbPath string) (refusal string, unstamped bool) {
	const refused = "store_owned_by_other: "
	raw, err := OwnershipMirror(dbPath)
	if err != nil {
		return err.Error(), false
	}
	if raw != nil {
		if why := MirrorRefusal(raw); why != "" {
			return refused + why, false
		}
	}
	meta, err := readMetadata(ctx, dbPath)
	if err != nil {
		return "", false
	}
	if !stamped(raw, meta) {
		return "", true
	}
	if raw == nil || meta["writer_protocol"] != "1" {
		return refused + "missing or unsupported writer protocol", false
	}
	if meta["owner"] != "go" {
		return refused + "the relay store belongs to another runtime", false
	}
	return "", false
}

// stamped is whether a store carries any part of an ownership stamp: its mirror, or an ownership
// key in schema_meta.
func stamped(mirror []byte, meta map[string]string) bool {
	if mirror != nil {
		return true
	}
	for _, key := range ownership.Keys {
		if _, present := meta[key]; present {
			return true
		}
	}
	return false
}

// UnstampedStoreDetail is the refusal detail for a store with no write gate and no ownership
// stamp (no mirror, no ownership key): a database no Go writer was ever admitted to, which no
// running relay holds open either, since a serving relay holds its store's write gate.
const UnstampedStoreDetail = "the store carries no ownership stamp (no write-gate.lock): no Go writer was ever bound to it; it is not the store a running relay serves"

// writeGateRefusal is store_owned_by_other for a write gate that could not be taken: in plain
// words (UnstampedStoreDetail) when the gate does not exist beside a store that carries no
// ownership stamp, else the gate's own failure as the fence words it.
func writeGateRefusal(err error, unstamped bool) error {
	refused := &ownership.Refused{Detail: fmt.Sprintf("write gate: %v", err)}
	if unstamped && errors.Is(err, os.ErrNotExist) {
		return &RefusedError{Reason: "store_owned_by_other", Detail: UnstampedStoreDetail, cause: refused}
	}
	return ownershipRefusal(refused)
}

// unstampedAt is whether the store at dbPath carries no ownership stamp, read as the probe's
// preflight reads it; a mirror or schema_meta that cannot be read is not called unstamped.
func unstampedAt(ctx context.Context, dbPath string) bool {
	raw, err := OwnershipMirror(dbPath)
	if err != nil {
		return false
	}
	meta, err := readMetadata(ctx, dbPath)
	return err == nil && !stamped(raw, meta)
}

// probeForeign is probe's foreign-store branch: identity from a disposable copy and a stat of
// the resolved path, never a connection to the database itself.
func probeForeign(ctx context.Context, dbPath string, result *ProbeResult, notes *[]string) {
	meta, err := readMetadata(ctx, dbPath)
	if err == nil {
		var resolved string
		if resolved, err = resolvePath(dbPath); err == nil {
			var info os.FileInfo
			if info, err = os.Stat(resolved); err == nil {
				result.Store.StoreID, result.Store.CreatedAt, result.Store.SchemaVersion = meta["store_id"], meta["store_created_at"], meta["version"]
				if st, ok := info.Sys().(*syscall.Stat_t); ok {
					result.Store.Device, result.Store.Inode, result.Store.Links = uint64(st.Dev), st.Ino, uint64(st.Nlink)
				}
				if dir, err := os.Stat(filepath.Dir(resolved)); err == nil {
					if st, ok := dir.Sys().(*syscall.Stat_t); ok {
						result.Store.LogDevice, result.Store.LogInode, result.Store.LogName = uint64(st.Dev), st.Ino, filepath.Base(resolved)
					}
				}
				result.Access.DBReadable = true
				return
			}
		}
	}
	*notes = append(*notes, "database read failed: "+err.Error())
}

// probeRead reads the store's identity on a read connection. With judgeStamp it also reads the
// durable ownership stamp there (one SELECT, nothing written) and answers it as stampOn would on
// the write probe's connection; judged is whether that reading was reached, which a connection
// that failed to open or to name the right file, or an identity SELECT that failed, never reaches.
func probeRead(ctx context.Context, file *os.File, expected string, judgeStamp bool, result *ProbeResult, notes *[]string) (stamp error, judged bool) {
	if moved := relocation(file, expected); moved != "" {
		*notes = append(*notes, "database read failed: "+moved)
		return nil, false
	}
	conn, err := openHeldRead(ctx, file, expected, true)
	if err != nil {
		*notes = append(*notes, "database read failed: "+err.Error())
		return nil, false
	}
	defer conn.close()
	if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
		*notes = append(*notes, "database read failed: "+elsewhere)
		return nil, false
	}
	result.Access.DBReadable = true
	for _, field := range []struct {
		key  string
		dest *string
	}{{"store_id", &result.Store.StoreID}, {"store_created_at", &result.Store.CreatedAt}, {"version", &result.Store.SchemaVersion}} {
		// A store written before identity existed has no row: absence stays absence.
		err := conn.scanRow(ctx, "SELECT value FROM schema_meta WHERE key=?", []any{field.key}, field.dest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			*notes = append(*notes, "database read failed: "+err.Error())
			return nil, false
		}
	}
	if !judgeStamp {
		return nil, false
	}
	_, stamp = stampOn(ctx, conn.conn)
	return stamp, true
}

// gateUsable is what ownership.Lock demands of write-gate.lock, judged from the file's own
// metadata so the gate is never opened: a regular file (the lock is opened with O_NOFOLLOW) owned
// by this user that either grants no group or other access or sits in an owner-only directory
// (lockFileSafe, decision D3), and that this process may read and write. A gate that is absent
// answers the Lstat error, which writeGateRefusal words as the plain unstamped store or the gate's
// own.
func gateUsable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owned := func(info os.FileInfo) bool {
		st, ok := info.Sys().(*syscall.Stat_t)
		return ok && int(st.Uid) == os.Geteuid()
	}
	if !info.Mode().IsRegular() || !owned(info) {
		return fmt.Errorf("unsafe lock file %s: not a regular file owned by this user", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		dir, err := os.Stat(ownership.LexicalDir(path))
		if err != nil || !dir.IsDir() || !owned(dir) || dir.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("unsafe lock file %s: grants group or other access and its directory is not owned by this user or is group/world writable", path)
		}
	}
	return syscall.Access(path, 0x4|0x2)
}

// judgeWrite is what the default probe says in place of the write probe: whether this runtime
// would be admitted to write the store, from what a read can see, with nothing written and no
// lock taken. The write gate must be one ownership.Lock would take (gateUsable; its failure is
// worded as the gate's own, or as the plain unstamped store when it is absent), the durable stamp
// read on the read connection must name this runtime, and the database file and its directory
// must permit writing. It does not see a gate
// another process holds, or a sandbox that denies writes without changing permissions: only the
// write probe measures those.
func judgeWrite(expected string, unstamped bool, stamp error, result *ProbeResult, notes *[]string) {
	unavailable := func(why string) { *notes = append(*notes, "database write judged unavailable: "+why) }
	if err := gateUsable(filepath.Join(filepath.Dir(expected), "write-gate.lock")); err != nil {
		unavailable(writeGateRefusal(err, unstamped).Error())
		return
	}
	if stamp != nil {
		unavailable(stamp.Error())
		return
	}
	if err := syscall.Access(expected, 0x2); err != nil {
		unavailable("the database file: " + err.Error())
		return
	}
	// The directory a writer needs is the database's own, which a link resolves elsewhere than
	// the selected one: SQLite builds the log and the index beside the file it resolved.
	if err := syscall.Access(filepath.Dir(expected), 0x2|0x1); err != nil {
		unavailable("the database's directory: " + err.Error())
		return
	}
	result.Access.DBWritable = true
}

func probeWrite(ctx context.Context, file *os.File, expected string, unstamped bool, result *ProbeResult, notes *[]string) {
	gate, err := ownership.Lock(filepath.Join(filepath.Dir(expected), "write-gate.lock"), false, false)
	if err != nil {
		*notes = append(*notes, "database write probe failed: "+writeGateRefusal(err, unstamped).Error())
		return
	}
	defer gate.Close()
	conn, err := openHeld(ctx, file, "rw")
	if err != nil {
		*notes = append(*notes, "database write probe failed: "+err.Error())
		return
	}
	defer conn.close()
	// Before the transaction, never after it: BEGIN IMMEDIATE on a moved name creates its -wal.
	if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
		*notes = append(*notes, "database write probe failed: "+elsewhere)
		return
	}
	if _, err = stampOn(ctx, conn.conn); err != nil {
		*notes = append(*notes, "database write probe failed: "+err.Error())
		return
	}
	if err := conn.exec(ctx, "BEGIN IMMEDIATE"); err != nil {
		*notes = append(*notes, "database write probe failed: "+err.Error())
		return
	}
	if err := conn.exec(ctx, "ROLLBACK"); err != nil {
		*notes = append(*notes, "database write probe failed: "+err.Error())
		return
	}
	result.Access.DBWritable = true
}
