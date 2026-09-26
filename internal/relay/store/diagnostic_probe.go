package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"syscall"
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
		notes = append(notes, "directory stat failed: "+PythonOSError(err))
	}
	if result.Access.DirectoryExists {
		result.Access.DirectoryReadable = syscall.Access(selection.Path, 0x4|0x1) == nil
		// Writability is measured by writing: a privileged runner ignores mode bits.
		if temp, err := os.CreateTemp(selection.Path, ".probe-"); err == nil {
			result.Access.DirectoryWritable = true
			_ = temp.Close()
			_ = os.Remove(temp.Name())
		} else {
			notes = append(notes, "directory write failed: "+PythonOSError(err))
		}
	}
	if _, err := os.Stat(selection.DBPath()); err != nil {
		if result.Access.DirectoryExists {
			notes = append(notes, "database stat failed: "+PythonOSError(err))
		}
		return result
	}
	result.Access.DBExists = true
	if real, err := resolvePath(selection.DBPath()); err == nil {
		result.Store.RealPath = real
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

func probeRead(ctx context.Context, file *os.File, expected string, result *ProbeResult, notes *[]string) {
	if moved := relocation(file, expected); moved != "" {
		*notes = append(*notes, "database read failed: "+moved)
		return
	}
	conn, err := openHeld(ctx, file, "ro")
	if err != nil {
		*notes = append(*notes, "database read failed: "+PythonSQLiteError(err))
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
			*notes = append(*notes, "database read failed: "+PythonSQLiteError(err))
			return
		}
	}
}

func probeWrite(ctx context.Context, file *os.File, expected string, result *ProbeResult, notes *[]string) {
	conn, err := openHeld(ctx, file, "rw")
	if err != nil {
		*notes = append(*notes, "database write probe failed: "+PythonSQLiteError(err))
		return
	}
	defer conn.close()
	// Before the transaction, never after it: BEGIN IMMEDIATE on a moved name creates its -wal.
	if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
		*notes = append(*notes, "database write probe failed: "+elsewhere)
		return
	}
	if err := conn.exec(ctx, "BEGIN IMMEDIATE"); err != nil {
		*notes = append(*notes, "database write probe failed: "+PythonSQLiteError(err))
		return
	}
	if err := conn.exec(ctx, "ROLLBACK"); err != nil {
		*notes = append(*notes, "database write probe failed: "+PythonSQLiteError(err))
		return
	}
	result.Access.DBWritable = true
}
