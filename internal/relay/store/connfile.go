package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"syscall"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/libc/sys/types"
	sqlite3 "modernc.org/sqlite/lib"
)

// The file a SQLite connection holds (CRW-1052).
//
// The store-file identity table (storefile.go, CRW-967) is keyed by the (device, inode) of the
// database file. A path names a file only at the moment it is looked up: a path moved aside while
// SQLite opens it and put back before the check names the original again, though the connection
// holds the file that was there at the open. So the identity a connection really holds is read from
// the connection, not from the path.
//
// The driver offers no accessor for it. database/sql's Conn.Raw hands the driver connection over,
// and SQLite itself will give the unix VFS's file object for the main database
// (sqlite3_file_control with SQLITE_FCNTL_FILE_POINTER); the first members of that object, which
// sqlite3's os_unix.c declares as
//
//	struct unixFile { const sqlite3_io_methods *pMethod; sqlite3_vfs *pVfs; unixInodeInfo *pInode; int h; ... }
//
// end in the descriptor h the connection reads and writes the database through. The accessor below
// reads h and asks the kernel which file it names (fstat). It needs the connection's SQLite handle
// and its libc thread state, which the driver keeps in unexported fields, so it reads those through
// reflect and refuses, naming what it could not find, whenever the driver is not the one it was
// written against: an open that cannot prove its file does not succeed. TestConnectionFileKey_*
// pins the layout against the driver this module builds.

// unixFileDescriptorOffset is where os_unix.c's unixFile keeps the descriptor: after three
// pointers.
const unixFileDescriptorOffset = 3 * unsafe.Sizeof(uintptr(0))

// rawSQLiteConn unwraps the store's connection wrapper (textguard.go) to the driver's connection.
func rawSQLiteConn(raw any) (reflect.Value, error) {
	guarded, ok := raw.(guardedConn)
	if !ok {
		return reflect.Value{}, fmt.Errorf("the SQLite connection is a %T, not the driver connection the store opens", raw)
	}
	driverConn := reflect.ValueOf(guarded.guardedDriverConn)
	if driverConn.Kind() != reflect.Pointer || driverConn.IsNil() || driverConn.Elem().Kind() != reflect.Struct {
		return reflect.Value{}, errors.New("the SQLite driver connection is not a pointer to a struct")
	}
	return driverConn.Elem(), nil
}

// connectionFileKey is the (device, inode) of the file the SQLite connection raw holds open as its
// main database. raw is what database/sql's Conn.Raw passes to its callback, so the connection is
// checked out and no statement runs on it meanwhile.
func connectionFileKey(raw any) (storeFileKey, error) {
	conn, err := rawSQLiteConn(raw)
	if err != nil {
		return storeFileKey{}, err
	}
	dbField, tlsField := conn.FieldByName("db"), conn.FieldByName("tls")
	if !dbField.IsValid() || dbField.Kind() != reflect.Uintptr || !tlsField.IsValid() || tlsField.Kind() != reflect.Pointer || tlsField.IsNil() {
		return storeFileKey{}, errors.New("the SQLite driver connection no longer has the database handle and thread state the descriptor accessor reads")
	}
	db := uintptr(dbField.Uint())
	tls := (*libc.TLS)(unsafe.Pointer(tlsField.Pointer()))
	if db == 0 {
		return storeFileKey{}, errors.New("the SQLite connection has no database handle")
	}

	// One allocation: the out parameter for the file pointer, then the schema name "main".
	const pointer = unsafe.Sizeof(uintptr(0))
	const size = int(pointer) + 8
	scratch := tls.Alloc(size)
	defer tls.Free(size)
	var zero uintptr
	libc.Xmemcpy(tls, scratch, uintptr(unsafe.Pointer(&zero)), types.Size_t(pointer))
	name := [5]byte{'m', 'a', 'i', 'n', 0}
	libc.Xmemcpy(tls, scratch+pointer, uintptr(unsafe.Pointer(&name[0])), types.Size_t(len(name)))
	if rc := sqlite3.Xsqlite3_file_control(tls, db, scratch+pointer, sqlite3.SQLITE_FCNTL_FILE_POINTER, scratch); rc != sqlite3.SQLITE_OK {
		return storeFileKey{}, fmt.Errorf("SQLite would not name the connection's database file (result code %d)", rc)
	}
	var file uintptr
	libc.Xmemcpy(tls, uintptr(unsafe.Pointer(&file)), scratch, types.Size_t(pointer))
	if file == 0 {
		return storeFileKey{}, errors.New("SQLite returned no file object for the connection's database")
	}
	var methods uintptr
	libc.Xmemcpy(tls, uintptr(unsafe.Pointer(&methods)), file, types.Size_t(pointer))
	if methods == 0 {
		return storeFileKey{}, errors.New("the connection's database file object is not open")
	}
	var fd int32
	libc.Xmemcpy(tls, uintptr(unsafe.Pointer(&fd)), file+unixFileDescriptorOffset, types.Size_t(unsafe.Sizeof(fd)))
	if fd < 0 {
		return storeFileKey{}, fmt.Errorf("the connection's database file object holds descriptor %d", fd)
	}
	var info syscall.Stat_t
	if err := syscall.Fstat(int(fd), &info); err != nil {
		return storeFileKey{}, fmt.Errorf("the connection's database descriptor cannot be identified: %w", err)
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return storeFileKey{}, errors.New("the connection's database descriptor does not name a regular file")
	}
	return storeFileKey{uint64(info.Dev), uint64(info.Ino)}, nil
}

// rawConner is the part of *sql.Conn the check needs.
type rawConner interface {
	Raw(func(driverConn any) error) error
}

// connectionKeyOf reads the identity of the file the checked-out connection c holds.
func connectionKeyOf(c rawConner) (key storeFileKey, err error) {
	err = c.Raw(func(driverConn any) error {
		var e error
		key, e = connectionFileKey(driverConn)
		return e
	})
	return key, err
}

// poolConnectionKeyOf reads the identity of the file the connection of db holds. The pools of this
// package open one connection (SetMaxOpenConns(1)) and it is idle when a store has just connected,
// so the connection checked out here is the store's own. Where the pool had to open another
// connection the answer is that connection's file, which is the file the store would use next.
func poolConnectionKeyOf(ctx context.Context, db *sql.DB) (storeFileKey, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return storeFileKey{}, err
	}
	key, keyErr := connectionKeyOf(conn)
	return key, errors.Join(keyErr, conn.Close())
}
