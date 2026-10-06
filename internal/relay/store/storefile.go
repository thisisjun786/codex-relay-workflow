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

// storeFileSuffixes is the suffix set the rule names: the database and its SQLite sidecars. The
// guard test (storefile_guard_test.go) reads the same set, so a new sidecar suffix is declared
// once.
var storeFileSuffixes = []string{"", "-wal", "-shm", "-journal"}

// storeFileKey identifies one store file by the identity the kernel gives it. Two paths naming
// one inode share a key, which is what makes the registry hold each file once.
type storeFileKey struct{ device, inode uint64 }

// heldRegistry is the process-wide registry. byPath answers the common case without an open;
// byKey answers a second path that names an already-held inode. Neither map ever loses an entry
// and nothing in this package closes a stored handle.
type heldRegistry struct {
	sync.Mutex
	byPath map[string]*os.File
	byKey  map[storeFileKey]*os.File
}

var heldStoreFiles = &heldRegistry{byPath: map[string]*os.File{}, byKey: map[storeFileKey]*os.File{}}

func init() { ownership.HoldStoreFile = holdStoreFile }

// holdStoreFile returns the descriptor this process holds for the store file at path, opening it
// the first time and never closing it. The returned handle is a borrow: the caller must not close
// it. A file whose descriptor cannot be identified is still returned and kept open, but it is not
// registered, so nothing can reuse it by identity.
func holdStoreFile(path string) (*os.File, error) {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	// The path may name a different file than the handle held under it: a store can be replaced,
	// and a test reuses a directory. A handle is only reused when it still names the file the path
	// resolves to now, so a read is never answered from a file that is no longer there. The
	// previous handle is never closed - it keeps its descriptor, and with it whatever locks the
	// process holds on that inode.
	if file, ok := heldStoreFiles.byPath[path]; ok && namesTheFileAt(file, path) {
		return file, nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	identity, measured := fstatIdentity(int(file.Fd()))
	if !measured {
		return file, nil
	}
	key := storeFileKey{identity.device, identity.inode}
	if existing, ok := heldStoreFiles.byKey[key]; ok {
		// Another path already holds this inode. Both descriptors stay open: closing this one
		// would drop the other's POSIX locks, which is exactly the defect this registry exists
		// to prevent. The first handle answers for the inode.
		heldStoreFiles.byPath[path] = existing
		return existing, nil
	}
	heldStoreFiles.byKey[key] = file
	heldStoreFiles.byPath[path] = file
	return file, nil
}

// namesTheFileAt reports whether a held handle still names the file the path resolves to. A path
// that cannot be stat'ed (it is gone) or a handle whose descriptor cannot be identified is not
// reused: the caller opens again and the new file is held in addition to this one.
func namesTheFileAt(file *os.File, path string) bool {
	held, measured := fstatIdentity(int(file.Fd()))
	if !measured {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uint64(stat.Dev) == held.device && uint64(stat.Ino) == held.inode
}

// heldStoreFileCount is the number of distinct store files this process holds, for tests.
func heldStoreFileCount() int {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	return len(heldStoreFiles.byKey)
}
