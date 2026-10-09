//go:build linux

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// CRW-1052: attach confirms the file the connection itself holds, not the file the path names after
// the connection opened. The tests use only temporary state and never touch the operational store.

// seedDecoy creates a second store in a directory of its own below dir (a store file is always named
// relay.sqlite3) and returns its path.
func seedDecoy(t *testing.T, dir string) string {
	t.Helper()
	other := filepath.Join(dir, "decoy")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(other, "relay.sqlite3")
	seedDatabase(t, decoy)
	return decoy
}

// swapPathAroundConnect arranges that SQLite opens decoy instead of the database at path: before the
// connector opens the file the original is moved aside and the decoy put at the path, and as soon as
// the connection has opened the file the original is put back. After the connect the path names the
// original file again, which is all a check by path can see.
func swapPathAroundConnect(t *testing.T, path, decoy string) {
	t.Helper()
	dir := filepath.Dir(path)
	away := filepath.Join(dir, "original.away")
	parked := filepath.Join(filepath.Dir(decoy), "parked.sqlite3")
	var before, after sync.Once
	storeBeforeConnectHook = func(string) {
		before.Do(func() {
			if err := os.Rename(path, away); err != nil {
				t.Errorf("move the original away: %v", err)
				return
			}
			if err := os.Rename(decoy, path); err != nil {
				t.Errorf("put the decoy at the path: %v", err)
			}
		})
	}
	storeAfterConnectHook = func(string) {
		after.Do(func() {
			if err := os.Rename(path, parked); err != nil {
				t.Errorf("move the decoy off the path: %v", err)
				return
			}
			if err := os.Rename(away, path); err != nil {
				t.Errorf("put the original back: %v", err)
			}
		})
	}
	t.Cleanup(func() { storeBeforeConnectHook, storeAfterConnectHook = nil, nil })
}

// TestStoreFileAttach_aConnectionOnAnotherFileIsRefusedWhenThePathCameBack: the path is renamed to a
// decoy for the moment SQLite opens it and the original is restored before attach runs. The path then
// names the file the reference was taken on, but the connection holds the decoy, so the open must fail.
func TestStoreFileAttach_aConnectionOnAnotherFileIsRefusedWhenThePathCameBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seedDatabase(t, path)
	decoy := seedDecoy(t, dir)
	before := identityTableSize()
	swapPathAroundConnect(t, path, decoy)

	s, err := open(context.Background(), path, "", OpenOptions{BusyTimeout: 5 * time.Second})
	if err == nil {
		_ = s.Close()
		t.Fatal("open accepted a connection that holds a different file than the one its reference was taken on")
	}
	if after := identityTableSize(); after != before {
		t.Fatalf("the refused open left the identity table at %d entries, want %d", after, before)
	}
}

// TestStoreFileAttach_readOnlyOpenersRefuseAConnectionOnAnotherFile is the same window for the
// read-only openers and the in-place opener, which attach through the same check.
func TestStoreFileAttach_readOnlyOpenersRefuseAConnectionOnAnotherFile(t *testing.T) {
	ctx := context.Background()
	openers := map[string]func(string) error{
		"OpenReadOnlyStore": func(path string) error {
			s, err := OpenReadOnlyStore(ctx, path)
			if err == nil {
				_ = s.Close()
			}
			return err
		},
		"OpenReadOnly": func(path string) error {
			r, err := OpenReadOnly(ctx, path, time.Second)
			if err == nil {
				_ = r.Close()
			}
			return err
		},
		"OpenInPlace": func(path string) error {
			r, err := OpenInPlace(ctx, path, time.Second)
			if err == nil {
				_ = r.Close()
			}
			return err
		},
	}
	for name, opener := range openers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "relay.sqlite3")
			seedDatabase(t, path)
			decoy := seedDecoy(t, dir)
			before := identityTableSize()
			swapPathAroundConnect(t, path, decoy)
			if err := opener(path); err == nil {
				t.Fatalf("%s accepted a connection that holds a different file than the one its reference was taken on", name)
			}
			if after := identityTableSize(); after != before {
				t.Fatalf("the refused open left the identity table at %d entries, want %d", after, before)
			}
		})
	}
}

// TestConnectionFileKey_isTheFileTheConnectionOpenedWhereverThePathGoes pins the descriptor accessor
// against the driver this module builds: the key it reads is the inode of the database file for a
// writable store, a read-only one and an immutable in-place read, and it keeps naming that inode when
// the path is moved away and another file takes the name.
func TestConnectionFileKey_isTheFileTheConnectionOpenedWhereverThePathGoes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seedDatabase(t, path)
	want := identityKeyOf(t, path)

	s, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ro, err := OpenInPlace(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	dbs := map[string]*sql.DB{"writable store": s.DB, "in-place read": ro.db}
	for name, db := range dbs {
		got, err := poolConnectionKeyOf(ctx, db)
		if err != nil || got != want {
			t.Fatalf("%s: the connection's file is %+v (%v), want %+v", name, got, err, want)
		}
	}

	// Move the file away and put another one at its name: the connection still holds the original.
	moved := filepath.Join(dir, "moved.sqlite3")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("another file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if now := identityKeyOf(t, path); now == want {
		t.Fatal("the replacement file has the original's inode")
	}
	for name, db := range dbs {
		got, err := poolConnectionKeyOf(ctx, db)
		if err != nil || got != want {
			t.Fatalf("%s after the path was replaced: the connection's file is %+v (%v), want %+v", name, got, err, want)
		}
	}
}

// TestConnectionFileKey_refusesAConnectionItCannotRead: a connection of another kind is an error,
// never an identity.
func TestConnectionFileKey_refusesAConnectionItCannotRead(t *testing.T) {
	if _, err := connectionFileKey("not a connection"); err == nil {
		t.Fatal("connectionFileKey named a file for a value that is no driver connection")
	}
	if _, err := connectionFileKey(guardedConn{}); err == nil {
		t.Fatal("connectionFileKey named a file for a connection wrapper with no driver connection")
	}
}

// TestStoreFileDiagnosticRead_otherStoreClosingKeepsTheHolderLock: a diagnostic read of a store file
// the process holds a connection on opens no descriptor that escapes the registry's keep-or-close
// decision. holdDatabase borrows the registry's own handle (holdStoreFile), so the inode is in the
// registry for the life of the process before the read connects; the read connects through
// /proc/self/fd, which SQLite closes itself. The read below is held open while a second store on the
// same file closes (its table reference goes) and a descriptor-closing reader (HashArtifact, the
// artifact reader) is pointed at the file: the reader is refused rather than closing a descriptor,
// the holder keeps its POSIX lock on the main inode, and its connection still writes (CRW-1052,
// CRW-846 I-563).
func TestStoreFileDiagnosticRead_otherStoreClosingKeepsTheHolderLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	holder, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	insert := func(nonce string) error {
		_, err := holder.Querier(ctx).ExecContext(ctx, "INSERT INTO store_challenge(nonce,written_by,written_at) VALUES(?,?,?)", nonce, "holder", "2026-01-01T00:00:00Z")
		return err
	}
	if err := insert("before"); err != nil {
		t.Fatal(err)
	}
	inode := identityKeyOf(t, path).inode
	pid := os.Getpid()
	if held, lines := storeFileLocks(pid, inode); held < 1 {
		t.Skipf("the holder's POSIX lock on the main inode is not visible before the read (locks=%d, %v); a loss after the read cannot be told from this", held, lines)
	}
	other, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	var closeErr, hashErr error
	var duringRead int
	read := ReadOnlyRows(ctx, StateSelection{Path: dir}, "SELECT nonce FROM store_challenge", nil, func(RowScanner) error {
		if closeErr = other.Close(); closeErr != nil {
			return nil
		}
		_, _, _, hashErr = HashArtifact(ctx, path, []string{dir}, false)
		duringRead, _ = storeFileLocks(pid, inode)
		return nil
	})
	if !read.Readable || read.Detail != "" {
		t.Fatalf("the diagnostic read failed: %+v", read)
	}
	if closeErr != nil {
		t.Fatalf("closing the second store: %v", closeErr)
	}
	if hashErr == nil {
		t.Fatal("the artifact reader read and closed a store file this process holds")
	}
	if duringRead < 1 {
		t.Fatalf("the holder's POSIX lock on the main inode was gone while the diagnostic read was open (locks=%d)", duringRead)
	}
	if after, lines := storeFileLocks(pid, inode); after < 1 {
		t.Fatalf("the holder's POSIX lock on the main inode was gone after the diagnostic read (locks=%d, %v)", after, lines)
	}
	if err := insert("after"); err != nil {
		t.Fatalf("the holder's connection stopped writing after the diagnostic read: %v", err)
	}
}
