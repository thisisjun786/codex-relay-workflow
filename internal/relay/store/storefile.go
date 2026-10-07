package store

import (
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

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
// Three consequences shape the code below. First, a descriptor that becomes unreachable would be
// closed by os.File's finalizer at the next garbage collection, which is a close of the same file
// and drops the same locks, so nothing opened here is left for the collector to find - neither a
// handle whose identity could not be measured nor the second handle a same-inode race produced
// (CRW-880). Second, a second name for a file this process already holds must reuse that handle
// rather than open another descriptor, so the lookup by identity happens before the open and
// again after it. Third, the open itself must not wait: the registry mutex is process-wide, so a
// path that is not a regular file is refused without a blocking open (CRW-880).

// storeFileSuffixes is the suffix set the rule names: the database and its SQLite sidecars. The
// guard test (storefile_guard_test.go) reads the same set, so a new sidecar suffix is declared
// once.
var storeFileSuffixes = []string{"", "-wal", "-shm", "-journal"}

// storeFileAfterStatHook is a deterministic seam for tests, called between the identity lookup
// and the open, where a test can move the path away and back (CRW-880). Production leaves it nil.
var storeFileAfterStatHook func(path string)

// storeFileKey identifies one store file by the identity the kernel gives it. Two paths naming
// one inode share a key, which is what makes the registry hold each file once.
type storeFileKey struct{ device, inode uint64 }

// heldRegistry is the process-wide registry. byPath answers the common case without an open;
// byKey answers a second path that names an already-held inode. Neither map ever loses an entry
// and nothing in this package closes a stored handle. neverClosed keeps every descriptor that
// must stay reachable but is not the registry's answer for its inode: one whose identity could
// not be measured, one that lost the same-inode race after the open (CRW-880), and one the
// artifact reader refused and handed over rather than closing (CRW-880). recorded holds the
// resolved database paths this process opened through store.open, without opening anything.
type heldRegistry struct {
	sync.Mutex
	byPath      map[string]*os.File
	byKey       map[storeFileKey]*os.File
	neverClosed []*os.File
	recorded    map[string]bool
}

var heldStoreFiles = &heldRegistry{byPath: map[string]*os.File{}, byKey: map[storeFileKey]*os.File{}, recorded: map[string]bool{}}

func init() { ownership.HoldStoreFile = holdStoreFile }

// holdStoreFile returns the descriptor this process holds for the store file at path, opening it
// the first time and never closing it. The returned handle is a borrow: the caller must not close
// it. A path that does not name a regular file is refused, with the descriptor closed before the
// refusal, because a directory or a device is not a store file and cannot carry SQLite's locks.
// The refusal happens before the open where a stat can already see it, so a writerless FIFO never
// reaches open(2) while this mutex is held (CRW-880).
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
	// Refuse a path that is not a regular file BEFORE opening it, and look the file up by
	// identity before opening it too. A second name for an inode this process already holds (a
	// hard link, a bind mount, another spelling of the path) must reuse that handle: opening
	// another descriptor and dropping it would let the collector close it and drop this
	// process's locks on the file.
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, storeFileRefusal(path, info)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			if file, ok := heldStoreFiles.byKey[storeFileKey{uint64(stat.Dev), uint64(stat.Ino)}]; ok {
				heldStoreFiles.byPath[path] = file
				return file, nil
			}
		}
	}
	if storeFileAfterStatHook != nil {
		storeFileAfterStatHook(path)
	}
	// O_NONBLOCK closes the window between the stat above and this open: a path that became a
	// FIFO in it returns at once instead of waiting for a writer that may never come. The flag is
	// cleared again before the handle is registered or handed out.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		// The descriptor is open on a store file, so it is kept reachable rather than closed:
		// closing it would drop this process's POSIX locks on that inode, which is the defect
		// this file exists to prevent. The caller gets the error and uses nothing.
		heldStoreFiles.neverClosed = append(heldStoreFiles.neverClosed, file)
		return nil, err
	}
	if !info.Mode().IsRegular() {
		// Not a store file: the path became a FIFO, a device or a directory between the stat
		// above and this open. Such a file carries no SQLite lock, so closing it is safe and
		// refusing it keeps it from consuming a descriptor for the life of the process. The
		// refusal is the one ownership.CopySnapshot documents for a directory source.
		_ = file.Close()
		return nil, storeFileRefusal(path, info)
	}
	clearStoreFileNonblock(int(file.Fd()))
	identity, measured := fstatIdentity(int(file.Fd()))
	if !measured {
		// Keep the descriptor reachable rather than let the collector close it: it may be the
		// only descriptor this process holds on a file it will hold a lock on.
		heldStoreFiles.neverClosed = append(heldStoreFiles.neverClosed, file)
		return file, nil
	}
	key := storeFileKey{identity.device, identity.inode}
	if held, ok := heldStoreFiles.byKey[key]; ok {
		// The path came back to an inode this process already holds between the stat above and
		// this open (CRW-880). The handle the registry already has is the answer and byKey keeps
		// it: overwriting byKey here would drop the old handle's last reference, and the
		// collector's close of it would drop this process's POSIX locks on the file. The
		// descriptor just opened is kept reachable for the same reason.
		heldStoreFiles.neverClosed = append(heldStoreFiles.neverClosed, file)
		// A handle this path used to answer with is displaced here. It stays reachable through
		// byKey today (every value byPath holds is one byKey holds), and it is put on the
		// never-closed list as well so the rule holds even if that ever stops being true.
		if displaced, ok := heldStoreFiles.byPath[path]; ok && displaced != held {
			heldStoreFiles.neverClosed = append(heldStoreFiles.neverClosed, displaced)
		}
		heldStoreFiles.byPath[path] = held
		return held, nil
	}
	heldStoreFiles.byKey[key] = file
	heldStoreFiles.byPath[path] = file
	return file, nil
}

// storeFileRefusal is the refusal of a store-file path that is not a regular file, in the shape
// the directory case has always used: an os.PathError whose Op is "open". A directory keeps
// EISDIR; every other special file (a FIFO, a device, a socket) gets ENXIO, which no SQLite lock
// can be taken on and which is not a directory. No refusal reason is added.
func storeFileRefusal(path string, info os.FileInfo) error {
	errno := syscall.ENXIO
	if info.IsDir() {
		errno = syscall.EISDIR
	}
	return &os.PathError{Op: "open", Path: path, Err: errno}
}

// clearStoreFileNonblock clears the O_NONBLOCK holdStoreFile adds to its open. The flag lives on
// the open file description, so a descriptor this process keeps must not leave it set: a read
// through it, or through any descriptor that shares the description, would see EAGAIN.
func clearStoreFileNonblock(fd int) {
	_ = unix.SetNonblock(fd, false)
}

// recordStoreFilePath records a resolved database path this process opened through store.open,
// without opening anything. The artifact reader asks whether an artifact is one of these paths or
// one of their sidecars (holdsStoreFileIdentity).
func recordStoreFilePath(resolved string) {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	heldStoreFiles.recorded[resolved] = true
}

// holdsStoreFileIdentity reports whether (device, inode) is a store file this process holds or
// opened: an inode the registry holds, or a recorded database path or one of its sidecars as the
// kernel resolves it now. It stats and never opens, and it is asked about the identity of an
// already-open descriptor, so a pathname race cannot move the answer (CRW-880). The registry
// mutex is held for the stats and the maps only; the caller hashes outside it, and the two
// functions never nest, so nothing here waits on a read.
func holdsStoreFileIdentity(device, inode uint64) bool {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	return holdsStoreFileIdentityLocked(device, inode)
}

// holdsStoreFileIdentityLocked is holdsStoreFileIdentity with the registry lock already held. It
// stats and never opens. The caller holds the lock.
func holdsStoreFileIdentityLocked(device, inode uint64) bool {
	key := storeFileKey{device, inode}
	if _, ok := heldStoreFiles.byKey[key]; ok {
		return true
	}
	for path := range heldStoreFiles.recorded {
		for _, suffix := range storeFileSuffixes {
			info, err := os.Stat(path + suffix)
			if err != nil {
				continue
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			if uint64(stat.Dev) == device && uint64(stat.Ino) == inode {
				return true
			}
		}
	}
	return false
}

// closeOrKeepStoreFileDescriptor closes fd unless its identity is a store file this process holds
// or opened, in which case the descriptor is kept reachable instead: closing any descriptor of
// such a file drops this process's POSIX locks on it (CRW-880, I-563). The decision and the close
// are made under the registry lock, and store.open takes that same lock to record a path before it
// connects, so a store opened while the caller was reading is still recognised here.
func closeOrKeepStoreFileDescriptor(fd int, name string) {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	if identity, measured := fstatIdentity(fd); measured && holdsStoreFileIdentityLocked(identity.device, identity.inode) {
		clearStoreFileNonblock(fd)
		heldStoreFiles.neverClosed = append(heldStoreFiles.neverClosed, os.NewFile(uintptr(fd), name))
		return
	}
	_ = syscall.Close(fd)
}

// knownStoreFileIdentityAt is the identity of the path itself, without following a symbolic link
// and without opening anything. The artifact reader asks it before it opens, so a path this
// process already knows as a store file is refused without producing a descriptor that would have
// to be kept unclosed for the life of the process (CRW-880).
func knownStoreFileIdentityAt(path string) (storeFileKey, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return storeFileKey{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return storeFileKey{}, false
	}
	return storeFileKey{uint64(stat.Dev), uint64(stat.Ino)}, true
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
