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
	Detail            string
}

type ProbeResult struct {
	Store     Location
	Access    ProbeAccess
	Selection StateSelection
}

// Probe is store.probe: it describes the selected state WITHOUT constructing a Store, and
// every step owns its error and becomes a note in Access.Detail instead of failing the call.
func Probe(ctx context.Context, selection StateSelection) (result ProbeResult) {
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
	if refusal := ownershipPreflight(ctx, selection.DBPath()); refusal != "" {
		// A store this runtime may not write is diagnosed without a live SQLite open (even
		// mode=ro can create WAL/SHM on a copied WAL database) and without creating even a
		// temporary file beside it.
		probeForeign(ctx, selection.DBPath(), &result, &notes)
		notes = append(notes, refusal)
		return result
	}
	if result.Access.DirectoryExists {
		// Writability is measured by writing: a privileged runner ignores mode bits.
		if temp, err := os.CreateTemp(selection.Path, ".probe-"); err == nil {
			result.Access.DirectoryWritable = true
			_ = temp.Close()
			_ = os.Remove(temp.Name())
		} else {
			notes = append(notes, "directory write failed: "+err.Error())
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
	probeRead(ctx, file, expected, &result, &notes)
	// The closing question for the read, and the gate for the write probe.
	if moved := relocation(file, expected); moved != "" {
		notes = append(notes, "the database moved while it was being read: "+moved)
		result.Access.DBReadable = false
		result.Store = Location{DBPath: result.Store.DBPath, RealPath: result.Store.RealPath}
		return result
	}
	probeWrite(ctx, file, expected, &result, &notes)
	return result
}

// ownershipPreflight is probe's check_start: "" when this runtime may be admitted to write
// the store (or it is absent or legacy, left to the probe itself), otherwise the refusal as
// Python's str(OwnershipRefused). The mirror is read as ownership.mirror reads it, before the
// database; a database that cannot be read at all is left to the probe below, which reports
// it in detail. Go's admission preflight decides whether a fenced store is refused, and
// ownership.py validate's words name why wherever validate refuses it too.
func ownershipPreflight(ctx context.Context, dbPath string) string {
	const refused = "store_owned_by_other: "
	raw, err := OwnershipMirror(dbPath)
	if err != nil {
		return err.Error()
	}
	if raw != nil {
		if why := MirrorRefusal(raw); why != "" {
			return refused + why
		}
	}
	meta, err := readMetadata(ctx, dbPath)
	if err != nil {
		return ""
	}
	fenced := raw != nil
	for _, key := range ownership.Keys {
		_, present := meta[key]
		fenced = fenced || present
	}
	if !fenced {
		return ""
	}
	if raw == nil || meta["writer_protocol"] != "1" {
		return refused + "missing or unsupported writer protocol"
	}
	if meta["owner"] != "go" {
		return refused + "the relay store belongs to another runtime"
	}
	return ""
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

func probeRead(ctx context.Context, file *os.File, expected string, result *ProbeResult, notes *[]string) {
	if moved := relocation(file, expected); moved != "" {
		*notes = append(*notes, "database read failed: "+moved)
		return
	}
	conn, err := openHeld(ctx, file, "ro")
	if err != nil {
		*notes = append(*notes, "database read failed: "+err.Error())
		return
	}
	defer conn.close()
	if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
		*notes = append(*notes, "database read failed: "+elsewhere)
		return
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
			return
		}
	}
}

func probeWrite(ctx context.Context, file *os.File, expected string, result *ProbeResult, notes *[]string) {
	gate, err := ownership.Lock(filepath.Join(filepath.Dir(expected), "write-gate.lock"), false, false)
	if err != nil {
		*notes = append(*notes, "database write probe failed: "+ownershipRefusal(&ownership.Refused{Detail: fmt.Sprintf("write gate: %v", err)}).Error())
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
