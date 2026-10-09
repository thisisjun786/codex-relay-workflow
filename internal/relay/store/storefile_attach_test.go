//go:build linux

package store

import (
	"context"
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
