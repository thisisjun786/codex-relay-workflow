package store

import (
	"os"
	"sync"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// The store-file handle registry (CRW-846, docs/relay/invariants.md I-563).
//
// A POSIX (fcntl) lock is held per process and per file, not per descriptor: a relay process that
// holds a SQLite WAL connection on relay.sqlite3 loses that connection's lock the moment the same
// process opens and closes any other descriptor of the same file. The process that then closes
// last deletes -wal and -shm, the connection that lost its lock keeps writing to a now-unlinked
// log, and the store reads malformed. The rule this file implements is therefore absolute: a
// descriptor this process opens on a store file outside SQLite is kept for the life of the
// process and never closed.
//
// Two consequences shape the code below. First, a descriptor that becomes unreachable would be
// closed by os.File's finalizer at the next garbage collection, which is a close of the same file
// and drops the same locks, so nothing opened here is left for the collector to find. Second, a
// second name for a file this process already holds must reuse that handle rather than open
// another descriptor, so the lookup by identity happens before the open.

// storeFileSuffixes is the suffix set the rule names: the database and its SQLite sidecars. The
// guard test (storefile_guard_test.go) reads the same set, so a new sidecar suffix is declared
// once.
var storeFileSuffixes = []string{"", "-wal", "-shm", "-journal"}

// storeFileKey identifies one store file by the identity the kernel gives it. Two paths naming
// one inode share a key, which is what makes the registry hold each file once.
type storeFileKey struct{ device, inode uint64 }

// heldRegistry is the process-wide registry. byPath answers the common case without an open;
// byKey answers a second path that names an already-held inode. Neither map ever loses an entry
// and nothing in this package closes a stored handle. unidentified keeps a descriptor whose
// identity could not be measured reachable, so the collector cannot close it either.
type heldRegistry struct {
	sync.Mutex
	byPath       map[string]*os.File
	byKey        map[storeFileKey]*os.File
	unidentified []*os.File
}

var heldStoreFiles = &heldRegistry{byPath: map[string]*os.File{}, byKey: map[storeFileKey]*os.File{}}

func init() { ownership.HoldStoreFile = holdStoreFile }

// holdStoreFile returns the descriptor this process holds for the store file at path, opening it
// the first time and never closing it. The returned handle is a borrow: the caller must not close
// it. A path that does not name a regular file is refused, with the descriptor closed before the
// refusal, because a directory or a device is not a store file and cannot carry SQLite's locks.
func holdStoreFile(path string) (*os.File, error) {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	// A handle is only reused while it still names the file the path resolves to: a store can be
	// replaced at the pathname, and a read must then be answered from the file that is there now.
	// The previous handle is never closed - it keeps its descriptor, and with it whatever locks
	// this process holds on that inode.
	if file, ok := heldStoreFiles.byPath[path]; ok && namesTheFileAt(file, path) {
		return file, nil
	}
	// Look the file up by identity BEFORE opening it. A second name for an inode this process
	// already holds (a hard link, a bind mount, another spelling of the path) must reuse that
	// handle: opening another descriptor and dropping it would let the collector close it and
	// drop this process's locks on the file.
	if key, ok := storeFileKeyOf(path); ok {
		if file, ok := heldStoreFiles.byKey[key]; ok {
			heldStoreFiles.byPath[path] = file
			return file, nil
		}
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		// The descriptor is open on a store file, so it is kept reachable rather than closed:
		// closing it would drop this process's POSIX locks on that inode, which is the defect
		// this file exists to prevent. The caller gets the error and uses nothing.
		heldStoreFiles.unidentified = append(heldStoreFiles.unidentified, file)
		return nil, err
	}
	if !info.Mode().IsRegular() {
		// Not a store file: a directory at relay.sqlite3 is the case sibling discovery and a
		// directory-shaped source reach. Closing it is safe (it carries no SQLite lock) and
		// refusing it keeps malformed sibling directories from consuming descriptors for the
		// life of the process. The refusal is the one ownership.CopySnapshot documents for a
		// directory source.
		_ = file.Close()
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EISDIR}
	}
	identity, measured := fstatIdentity(int(file.Fd()))
	if !measured {
		// Keep the descriptor reachable rather than let the collector close it: it may be the
		// only descriptor this process holds on a file it will hold a lock on.
		heldStoreFiles.unidentified = append(heldStoreFiles.unidentified, file)
		return file, nil
	}
	key := storeFileKey{identity.device, identity.inode}
	heldStoreFiles.byKey[key] = file
	heldStoreFiles.byPath[path] = file
	return file, nil
}

// storeFileKeyOf is the identity of the file a path names, without opening it. ok is false when
// the path cannot be stat'ed or is not a regular file, in which case the caller opens it to
// produce the right error.
func storeFileKeyOf(path string) (storeFileKey, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return storeFileKey{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return storeFileKey{}, false
	}
	return storeFileKey{uint64(stat.Dev), uint64(stat.Ino)}, true
}

// namesTheFileAt reports whether a held handle still names the file the path resolves to. A path
// that cannot be stat'ed (it is gone) or a handle whose descriptor cannot be identified is not
// reused: the caller opens again and the new file is held in addition to this one.
func namesTheFileAt(file *os.File, path string) bool {
	held, measured := fstatIdentity(int(file.Fd()))
	if !measured {
		return false
	}
	key, ok := storeFileKeyOf(path)
	return ok && key == storeFileKey{held.device, held.inode}
}

// heldStoreFileCount is the number of distinct store files this process holds, for tests.
func heldStoreFileCount() int {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	return len(heldStoreFiles.byKey)
}
