//go:build linux

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// heldCreationGate is a second reference to the creating open's write-gate description, taken at
// the createFault("gate-placed") seam and held until the test releases it. A child forked during
// the creation that has not exec'd yet holds exactly this reference: flock belongs to the open
// file description, so the creator's own close does not release the EX while this reference
// survives.
type heldCreationGate struct {
	fd   int
	held bool
}

// holdCreationGateThroughOpen makes the next Open of path dup the creation gate's description as
// soon as the creator has linked it into place, and holds it until the test releases it. The dup
// names the gate by its /proc/self/fd entry, which exists only in the window between placeGate's
// link(2) and the creator's own close.
func holdCreationGateThroughOpen(t *testing.T, path string) *heldCreationGate {
	t.Helper()
	held := &heldCreationGate{fd: -1}
	previous := createFault
	createFault = func(point string) error {
		if err := previous(point); err != nil {
			return err
		}
		if point != "gate-placed" {
			return nil
		}
		fd, err := dupCreationGate(filepath.Join(filepath.Dir(path), "write-gate.lock"))
		if err != nil {
			return err
		}
		held.fd, held.held = fd, true
		return nil
	}
	t.Cleanup(func() { createFault = previous })
	t.Cleanup(func() { held.release() })
	return held
}

// dupCreationGate duplicates the open file description of the creation gate at gatePath, found
// through /proc/self/fd and identified by the gate's inode and device, so the path the creator
// spelled and the kernel's resolved one need not agree.
func dupCreationGate(gatePath string) (int, error) {
	var want unix.Stat_t
	if err := unix.Stat(gatePath, &want); err != nil {
		return -1, err
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1, err
	}
	for _, entry := range entries {
		number, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil || !strings.HasPrefix(filepath.Base(target), ".write-gate-") {
			continue
		}
		var got unix.Stat_t
		if err = unix.Fstat(number, &got); err != nil {
			continue
		}
		if got.Ino != want.Ino || got.Dev != want.Dev {
			continue
		}
		fd, err := unix.Dup(number)
		if err != nil {
			continue
		}
		return fd, nil
	}
	return -1, errors.New("no open .write-gate- description found")
}

func (h *heldCreationGate) release() {
	if h.held {
		_ = unix.Close(h.fd)
		h.held = false
	}
}

// gateLockFree reports whether a lock of the store's write gate can be taken right now from a
// fresh description, closing the probe's own description either way. A shared probe is the
// discriminating one: flock refuses a second description's LOCK_SH while any description holds
// the gate LOCK_EX, so a shared probe that succeeds proves the store holds the gate SH and not
// EX (the exclusive probe alone cannot tell the two apart).
func gateLockFree(t *testing.T, path string, exclusive bool) (bool, error) {
	t.Helper()
	gate, err := ownership.Lock(filepath.Join(filepath.Dir(path), "write-gate.lock"), exclusive, false)
	if err == nil {
		return true, gate.Close()
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

// requireGateHeldShared pins that the store holds the gate SH right now: an exclusive lock from a
// fresh description must fail and a shared one must succeed.
func requireGateHeldShared(t *testing.T, path string) {
	t.Helper()
	if free, err := gateLockFree(t, path, true); err != nil {
		t.Fatal(err)
	} else if free {
		t.Fatal("an exclusive lock of the gate succeeded while the store was open")
	}
	if free, err := gateLockFree(t, path, false); err != nil {
		t.Fatal(err)
	} else if !free {
		t.Fatal("a shared lock of the gate failed while the store was open: the store holds it EX, not SH")
	}
}

// CRW-853: a creating open keeps the write gate's EX open file description and downgrades that
// same description to SH in place. Before the fix the creator closed its EX and took SH on a
// fresh description, which a surviving reference to the first (a child forked during the
// creation, before it execs) still held EX: the opening then refused its own store with
// store_owned_by_other 'write gate: resource temporarily unavailable'.
//
// The test drives that interleaving by holding a dup of the creation gate through Open:
//
//	(a) Open succeeds where it used to be refused;
//	(b) while the store is open the gate is held SH, so an exclusive lock answers EWOULDBLOCK;
//	(c) after Close and releasing the dup an exclusive lock succeeds;
//	(d) the same holds for a fresh store opened with a socket.
func Test853CreatingOpenKeepsItsGateAndDowngradesItInPlace(t *testing.T) {
	// Serial: assigns the package-level createFault seam, which a running sibling test would read.
	for _, tc := range []struct {
		name   string
		socket func(dir string) string
	}{
		{"socketless", func(string) string { return "" }},
		{"socket", func(dir string) string { return filepath.Join(dir, "app.sock") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stateDir(t)
			path, socket := filepath.Join(dir, "relay.sqlite3"), tc.socket(dir)
			held := holdCreationGateThroughOpen(t, path)
			db, err := Open(t.Context(), path, socket)
			if err != nil {
				t.Fatalf("a creating open refused its own store: %v", err)
			}
			if !held.held {
				t.Fatal("the creation gate was not reached: no dup was taken")
			}
			if socket != "" {
				stamp, err := ownership.SnapshotMeta(t.Context(), path)
				must(t, err)
				if stamp.SocketPath != socket {
					t.Fatalf("socketed creation recorded %q, want %q", stamp.SocketPath, socket)
				}
			}
			// (b) the store holds the gate SH for its lifetime: a fresh shared description is
			// admitted, a fresh exclusive one is not.
			requireGateHeldShared(t, path)
			must(t, db.Close())
			// (c) the description the store kept is the one the creator placed, and the dup still
			// holds it: an exclusive lock must keep failing until the dup is released.
			if free, err := gateLockFree(t, path, true); err != nil {
				t.Fatal(err)
			} else if free {
				t.Fatal("the dup no longer holds the creation description after Close")
			}
			held.release()
			if free, err := gateLockFree(t, path, true); err != nil {
				t.Fatal(err)
			} else if !free {
				t.Fatal("an exclusive lock still fails after every reference was closed")
			}
		})
	}
}

// Test853CreationRefusalWordsAreUnchanged pins what a genuine EX holder still meets: the fix
// must not have turned the refusal into a wait or a success, and its reason and words stay the
// fence's.
func Test853CreationRefusalWordsAreUnchanged(t *testing.T) {
	// Serial: it holds the gate the creation path looks for.
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	gate, err := ownership.Lock(filepath.Join(dir, "write-gate.lock"), true, true)
	must(t, err)
	defer func() { must(t, gate.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err = OpenWith(ctx, path, "", OpenOptions{BusyTimeout: 300 * time.Millisecond})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" {
		t.Fatalf("a gate another opener holds: %#v", err)
	}
	if !strings.Contains(refused.Detail, "write gate: resource temporarily unavailable") {
		t.Fatalf("refusal detail changed: %q", refused.Detail)
	}
}

// Test853CreateAbsentNeverHandsOnAGateWithAnError pins the hand-over contract at its boundary: a
// creation that fails anywhere, including in the deferred removal of its temporary database that
// runs after the store was published, returns no gate and leaves no description holding the write
// gate EX. A gate handed back together with an error is dropped by the caller before it can be
// downgraded, so the description would keep the store's own gate locked until the process exits
// and a retry in the same process would refuse the store it had just created.
func Test853CreateAbsentNeverHandsOnAGateWithAnError(t *testing.T) {
	// Serial: assigns the package-level createFault seam.
	dir := stateDir(t)
	path := filepath.Join(dir, "relay.sqlite3")
	previous := createFault
	createFault = func(point string) error {
		if err := previous(point); err != nil {
			return err
		}
		if point != "linked" {
			return nil
		}
		// The store is published next and the temporary database is removed after that. Leaving a
		// non-empty directory where the removal will look makes that deferred removal fail, so the
		// creation ends with an error after it has already published the store.
		names, err := filepath.Glob(filepath.Join(dir, ".relay-create-*.sqlite3"))
		if err != nil {
			return err
		}
		if len(names) != 1 {
			return fmt.Errorf("temporary databases: %v", names)
		}
		if err = os.Remove(names[0]); err != nil {
			return err
		}
		if err = os.Mkdir(names[0], 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(names[0], "keep"), []byte("x"), 0600)
	}
	t.Cleanup(func() { createFault = previous })

	gate, err := createAbsent(t.Context(), path, "", OpenOptions{BusyTimeout: time.Second})
	if err == nil {
		if gate != nil {
			must(t, gate.Close())
		}
		t.Fatal("a failed temporary cleanup was reported as a success")
	}
	if gate != nil {
		must(t, gate.Close())
		t.Fatalf("createAbsent handed a gate on together with an error: %v", err)
	}
	// Nothing holds the gate EX behind the failed creation.
	lock, err := ownership.Lock(filepath.Join(dir, "write-gate.lock"), true, false)
	if err != nil {
		t.Fatalf("the failed creation left the write gate held: %v", err)
	}
	must(t, lock.Close())
}
