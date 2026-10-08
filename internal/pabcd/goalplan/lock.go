package goalplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"golang.org/x/sys/unix"
)

// GoalplanWriteLockOptions supplies the oracle's delay/clock seams (:753-757).
// Nil delays use 5/10/20/40 ms; a non-nil empty list tries only once.
type GoalplanWriteLockOptions struct {
	RetryDelaysMs []int
	Sleep         func(int)
	Now           func() string
}

// GoalplanWriteLockResult is the ok/locked/unreadable union (:759-762).
// A pointer Value preserves an ok zero value, absent on refusal.
type GoalplanWriteLockResult[T any] struct {
	Kind   string `json:"kind"`
	Value  *T     `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// GoalplanLockStatus is GoalplanWriteLockStatus (:774-778); AgeMs null means absent.
type GoalplanLockStatus struct {
	Path   string   `json:"path"`
	Exists bool     `json:"exists"`
	AgeMs  *float64 `json:"ageMs"`
}

// GoalplanLockStatusOptions is the clock/stat seam of goalplanWriteLockStatus.
type GoalplanLockStatusOptions struct {
	NowMs *float64
	Stat  func(string) (float64, error)
}

// goalplanLockVanishedAfterHeldOpen is a test seam. It runs inside the acquisition path's
// held-open, between the openat and boundFile's descriptor check, so a test can order the
// holder's release into that window without sleeping. Production leaves it nil.
var goalplanLockVanishedAfterHeldOpen func()

// goalplanLockVanished reports whether a refused held-open is the lock directory the holder
// released and removed between the openat and boundFile's descriptor check. The descriptor is
// re-read while it is still open, so the answer comes from the kernel rather than from
// boundFile's message: exactly the expected path plus the Linux " (deleted)" suffix is a
// released lock. Any other path (a rename, a swap) stays refused.
func goalplanLockVanished(f *os.File, dir string) bool {
	if f == nil {
		return false
	}
	actual, err := descriptorPath(f)
	if err != nil {
		return false
	}
	return actual == dir+" (deleted)"
}

// goalplanLockVanishedOpenHeld opens the extant lock directory the way openAt does, so the
// acquisition loop keeps openAt's flag discipline and boundFile's path-swap refusal.
func goalplanLockVanishedOpenHeld(parent *os.File, dir string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), GoalplanLockDir, directoryOpenFlags()|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	f := os.NewFile(uintptr(fd), dir)
	if goalplanLockVanishedAfterHeldOpen != nil {
		goalplanLockVanishedAfterHeldOpen()
	}
	if e := boundFile(f, dir, true); e != nil {
		if goalplanLockVanished(f, dir) {
			_ = f.Close()
			return nil, nil
		}
		_ = f.Close()
		return nil, e
	}
	return f, nil
}

func sleepGoalplanLock(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// goalplanLockAcquire takes slug's write lock inside parent, the plan directory, waiting on the
// oracle's retry schedule. It returns the held directory, or a "locked" outcome when the whole
// schedule ran out with another holder's directory still there, or the acquisition error as it is.
// It is the one acquisition path, so a creator and a mutator queue on the same mkdir.
func goalplanLockAcquire(parent *os.File, real, slug string, o *GoalplanWriteLockOptions) (*os.File, *GoalplanWriteLockResult[struct{}], error) {
	delays, sleep, now := GoalplanLockRetryDelaysMs(), sleepGoalplanLock, func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	if o != nil {
		if o.RetryDelaysMs != nil {
			delays = o.RetryDelaysMs
		}
		if o.Sleep != nil {
			sleep = o.Sleep
		}
		if o.Now != nil {
			now = o.Now
		}
	}
	dir := filepath.Join(real, GoalplanLockDir)
	for attempt := 0; ; attempt++ {
		if err := boundFile(parent, real, true); err != nil {
			return nil, nil, err
		}
		err := unix.Mkdirat(int(parent.Fd()), GoalplanLockDir, 0o777)
		if err == nil {
			lock, err := openAt(parent, GoalplanLockDir, dir, unix.O_RDONLY|unix.O_DIRECTORY, true, 0)
			if err != nil {
				return nil, nil, err
			}
			writeLockOwner(lock, dir, now)
			return lock, nil, nil
		}
		if err != unix.EEXIST {
			return nil, nil, err
		}
		held, e := goalplanLockVanishedOpenHeld(parent, dir)
		if e != nil && !pathAbsent(e) {
			return nil, nil, e
		}
		if attempt >= len(delays) {
			owner := "(owner.json unavailable)"
			if held != nil {
				owner = readGoalplanLockOwnerText(held, dir)
				_ = held.Close()
			}
			reason := fmt.Sprintf("goalplan '%s' is busy. Lock directory: %s. owner=%s. Inspect %s. After verifying no writer is active, remove that lock directory with a tool for this platform.", slug, dir, owner, filepath.Join(dir, GoalplanLockOwnerFile))
			return nil, &GoalplanWriteLockResult[struct{}]{Kind: "locked", Reason: reason}, nil
		}
		if held != nil {
			_ = held.Close()
		}
		sleep(delays[attempt])
	}
}

// WithGoalplanCreationLock takes slug's goalplan write lock for a creator whose plan does not exist
// yet. WithGoalplanWriteLock reads the plan under its lock, so it refuses a slug that has none; a
// creator must instead check for absence AND publish inside one critical section, or two creators on
// one slug can both see it absent and the second rename replaces the first plan (CRW-646 c1, a
// data-loss defect). The slug directory is created through the same symlink-safe walk WriteGoalplan
// uses, so a linked state root is refused with that walk's message; a lock another holder owns comes
// back as Kind "locked" with the same reason text WithGoalplanWriteLock gives.
func WithGoalplanCreationLock(cwd, slug string, fn func() error, o *GoalplanWriteLockOptions) (GoalplanWriteLockResult[struct{}], error) {
	result := GoalplanWriteLockResult[struct{}]{}
	if _, err := ValidateGoalplanSlug(slug); err != nil {
		return result, err
	}
	checked, err := GoalplanDir(cwd, slug)
	if err != nil {
		return result, err
	}
	dir, real, err := writeOpenCheckedDir(cwd, checked)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	lock, locked, err := goalplanLockAcquire(dir, real, slug, o)
	if err != nil {
		return result, err
	}
	if locked != nil {
		return *locked, nil
	}
	defer releaseLock(dir, lock, filepath.Join(real, GoalplanLockDir))
	if err := fn(); err != nil {
		return result, err
	}
	return GoalplanWriteLockResult[struct{}]{Kind: "ok"}, nil
}

// GoalplanWriteLockDir is the lexical path (:769-771), with an added secure
// descriptor check for an extant directory. A linked or dangling lock is refused.
func GoalplanWriteLockDir(cwd, slug string) (string, error) {
	status, err := GoalplanWriteLockStatus(cwd, slug, nil)
	if err != nil {
		return "", err
	}
	return status.Path, nil
}

// GoalplanWriteLockStatus is read-only (:779-796). It never reads owner metadata
// or removes an old directory; disappearance between lookup and stat is absent.
func GoalplanWriteLockStatus(cwd, slug string, o *GoalplanLockStatusOptions) (GoalplanLockStatus, error) {
	dir, err := GoalplanDir(cwd, slug)
	if err != nil {
		return GoalplanLockStatus{}, err
	}
	status := GoalplanLockStatus{Path: filepath.Join(dir, GoalplanLockDir)}
	parent, real, err := openPlanDir(cwd, slug)
	if pathAbsent(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	defer parent.Close()
	lock, err := openAt(parent, GoalplanLockDir, filepath.Join(real, GoalplanLockDir), directoryOpenFlags(), true, 0)
	if pathAbsent(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	defer lock.Close()
	info, err := lock.Stat()
	var mtime float64
	if err == nil {
		mtime = float64(info.ModTime().UnixNano()) / 1e6
	}
	now := float64(time.Now().UnixNano()) / 1e6
	if o != nil {
		if o.NowMs != nil {
			now = *o.NowMs
		}
		if o.Stat != nil {
			mtime, err = o.Stat(status.Path)
		}
	}
	if pathAbsent(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	age := max(0, now-mtime)
	status.Exists = true
	status.AgeMs = &age
	return status, nil
}

// goalplanLockOwnerTextCap bounds how much of a held lock's owner.json the refusal text reads (CRW-982 D1).
const goalplanLockOwnerTextCap = 64 << 10

func readGoalplanLockOwnerText(lock *os.File, dir string) string {
	file, err := openAt(lock, GoalplanLockOwnerFile, filepath.Join(dir, GoalplanLockOwnerFile), unix.O_RDONLY, false, 0)
	if err != nil {
		return "(owner.json unavailable)"
	}
	defer file.Close()
	// The read is capped (CRW-982 post-evaluation D1). The cap is well above what a refusal can carry, so a busy
	// answer keeps the owner text that the harness's own trim handles; a file beyond it is not read whole.
	b, err := io.ReadAll(io.LimitReader(file, goalplanLockOwnerTextCap))
	if err != nil {
		return "(owner.json unavailable)"
	}
	s := jstext.Trim(source.DecodeUTF8(b))
	if s == "" {
		return "(empty owner.json)"
	}
	return s
}
func writeLockOwner(lock *os.File, dir string, now func() string) {
	defer func() { _ = recover() }() // options.now failure is diagnostic only, as in the oracle's inner catch.
	if boundFile(lock, dir, true) != nil {
		return
	}
	file, err := openAt(lock, GoalplanLockOwnerFile, filepath.Join(dir, GoalplanLockOwnerFile), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, false, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintf(file, "{\"pid\":%d,\"acquiredAt\":%s}\n", os.Getpid(), quote(now()))
}

// GoalplanLockHolder is what a waiter can learn about slug's goalplan write lock. The four answers
// call for different actions: Live is waited for (the holder may still publish), Dead ends the wait
// with the lock's own busy message, Gone means the lock was released and the waiter can try again, and
// Unknown (no lock directory, or owner.json absent/unreadable/undecodable) is bounded by the caller so
// a foreign or corrupt lock cannot make a waiter hang (CRW-646).
type GoalplanLockHolder int

const (
	GoalplanHolderGone    GoalplanLockHolder = iota // the lock directory is absent
	GoalplanHolderLive                              // owner.json names a running process
	GoalplanHolderDead                              // owner.json names a process that is gone
	GoalplanHolderUnknown                           // owner.json is absent, unreadable or undecodable
)

// GoalplanLockHolderState reports the holder of slug's goalplan write lock from the lock directory and
// its owner.json. The shared lock never expires a directory (a stale one is removed by hand), so this is
// how a waiter tells a live competing writer from an abandoned lock: it keeps waiting for the former
// and gives up on the latter.
func GoalplanLockHolderState(cwd, slug string) GoalplanLockHolder {
	dir, err := GoalplanDir(cwd, slug)
	if err != nil {
		return GoalplanHolderUnknown
	}
	lockDir := filepath.Join(dir, GoalplanLockDir)
	if _, err := os.Lstat(lockDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return GoalplanHolderGone
		}
		return GoalplanHolderUnknown
	}
	raw, err := readLockOwnerBytes(filepath.Join(lockDir, GoalplanLockOwnerFile))
	if err != nil {
		// The lock directory exists but its owner.json is absent or unreadable. mkdir acquires the lock
		// and owner.json is written right after, so this is the ordinary state of a holder that has just
		// started; it is also what a foreign or corrupt lock looks like. The caller treats Unknown as a
		// live holder for a bounded grace, so a just-started creator is not refused and a corrupt lock
		// cannot make the waiter hang.
		return GoalplanHolderUnknown
	}
	var owner struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(raw, &owner) != nil || owner.PID <= 0 {
		return GoalplanHolderUnknown
	}
	if processAlive(owner.PID) {
		return GoalplanHolderLive
	}
	return GoalplanHolderDead
}

// readLockOwnerBytes reads a lock's small owner/metadata file without following a symbolic link and
// without blocking on a special file: the open refuses a link (O_NOFOLLOW) and a FIFO or device
// (O_NONBLOCK with a regular-file check), so a hostile or accidental special file at the path cannot
// turn a bounded lock wait into an indefinite hang.
func readLockOwnerBytes(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrLockOwnerNotRegular, path)
	}
	// A small cap: the file this reads is a pid or one short JSON object, so a file that claims to be
	// huge is not one this call should spend memory on.
	return io.ReadAll(io.LimitReader(f, 4096))
}

// ErrLockOwnerNotRegular is what the owner probe returns for a path that opens to something other than a
// regular file (a FIFO, a device, a directory).
var ErrLockOwnerNotRegular = errors.New("lock metadata is not a regular file")

// ReadLockOwnerFile is the owner probe, exported for the session lock's holder check (CRW-982 c3): it opens
// path with O_NOFOLLOW and O_NONBLOCK, checks that the opened descriptor is a regular file, and reads at most
// a small fixed size. A link at path is refused with ELOOP, a special file with ErrLockOwnerNotRegular, and an
// absent path with fs.ErrNotExist.
func ReadLockOwnerFile(path string) ([]byte, error) {
	return readLockOwnerBytes(path)
}

// processAlive reports whether pid names a running process: signal 0 reaches it, and a permission
// refusal means it exists under another user, which is still alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// WithGoalplanWriteLock ports :806-873. The mkdir itself is the lock; owner.json
// is diagnostic only and a pre-existing lock is never expired. Callback errors
// propagate as Go errors and panic unwinds still release the acquired directory.
func WithGoalplanWriteLock[T any](cwd, slug string, fn func(*Goalplan) (T, error), o *GoalplanWriteLockOptions) (GoalplanWriteLockResult[T], error) {
	result := GoalplanWriteLockResult[T]{}
	if _, err := ValidateGoalplanSlug(slug); err != nil {
		return result, err
	}
	parent, real, err := openPlanDir(cwd, slug)
	missing := func() GoalplanWriteLockResult[T] {
		return GoalplanWriteLockResult[T]{Kind: "unreadable", Reason: "goalplan '" + slug + "' does not exist"}
	}
	if pathAbsent(err) {
		return missing(), nil
	}
	if err != nil {
		return result, err
	}
	defer parent.Close()
	var st unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), GoalplanFile, &st, unix.AT_SYMLINK_NOFOLLOW)
	if pathAbsent(err) {
		return missing(), nil
	}
	if err != nil {
		return result, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return result, fmt.Errorf("goalplan state path must not be a symlink: %s", filepath.Join(real, GoalplanFile))
	}
	lock, locked, err := goalplanLockAcquire(parent, real, slug, o)
	if err != nil {
		return result, err
	}
	if locked != nil {
		return GoalplanWriteLockResult[T]{Kind: "locked", Reason: locked.Reason}, nil
	}
	dir := filepath.Join(real, GoalplanLockDir)
	defer releaseLock(parent, lock, dir)
	read, file := revivalLossReadPlan(parent, real, filepath.Join(real, GoalplanFile), slug)
	if read.Plan == nil {
		detail := "goalplan '" + slug + "' could not be read"
		if d := read.Diagnostic; d != nil && d.Kind != "absent" {
			detail = d.Detail
		}
		return GoalplanWriteLockResult[T]{Kind: "unreadable", Reason: detail}, nil
	}
	// A plan whose revival would drop or change stored data is not handed to a writer (revivalLoss); bytes that are not UTF-8 are
	// refused first, because revival and its re-encoding would both read them as U+FFFD, and a key repeated in one object next,
	// because decoding keeps only its last value.
	if file.badByte > 0 {
		return GoalplanWriteLockResult[T]{Kind: "unreadable", Reason: revivalLossRefusal(slug, fmt.Sprintf("invalid UTF-8 at byte %d", file.badByte-1))}, nil
	}
	if dup := revivalLossDuplicate(file.text); dup != "" {
		return GoalplanWriteLockResult[T]{Kind: "unreadable", Reason: revivalLossRefusal(slug, "the repeated key "+dup)}, nil
	}
	if lost := revivalLoss(file.parsed); lost != "" {
		return GoalplanWriteLockResult[T]{Kind: "unreadable", Reason: revivalLossRefusal(slug, lost)}, nil
	}
	value, err := fn(read.Plan)
	if err != nil {
		return result, err
	}
	return GoalplanWriteLockResult[T]{Kind: "ok", Value: &value}, nil
}
