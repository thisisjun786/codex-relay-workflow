package goalplan

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func sleepGoalplanLock(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

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

func readGoalplanLockOwnerText(lock *os.File, dir string) string {
	file, err := openAt(lock, GoalplanLockOwnerFile, filepath.Join(dir, GoalplanLockOwnerFile), unix.O_RDONLY, false, 0)
	if err != nil {
		return "(owner.json unavailable)"
	}
	defer file.Close()
	b, err := io.ReadAll(file)
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
	var lock *os.File
	for attempt := 0; ; attempt++ {
		if err = boundFile(parent, real, true); err != nil {
			return result, err
		}
		err = unix.Mkdirat(int(parent.Fd()), GoalplanLockDir, 0o777)
		if err == nil {
			lock, err = openAt(parent, GoalplanLockDir, dir, unix.O_RDONLY|unix.O_DIRECTORY, true, 0)
			if err != nil {
				return result, err
			}
			break
		}
		if err != unix.EEXIST {
			return result, err
		}
		held, e := openAt(parent, GoalplanLockDir, dir, directoryOpenFlags(), true, 0)
		if e != nil && !pathAbsent(e) {
			return result, e
		}
		if attempt >= len(delays) {
			owner := "(owner.json unavailable)"
			if held != nil {
				owner = readGoalplanLockOwnerText(held, dir)
				_ = held.Close()
			}
			reason := fmt.Sprintf("goalplan '%s' is busy. Lock directory: %s. owner=%s. Inspect %s. After verifying no writer is active, remove that lock directory with a tool for this platform.", slug, dir, owner, filepath.Join(dir, GoalplanLockOwnerFile))
			return GoalplanWriteLockResult[T]{Kind: "locked", Reason: reason}, nil
		}
		if held != nil {
			_ = held.Close()
		}
		sleep(delays[attempt])
	}
	defer releaseLock(parent, lock, dir)
	writeLockOwner(lock, dir, now)
	read := readPlanAt(parent, real, filepath.Join(real, GoalplanFile), slug)
	if read.Plan == nil {
		detail := "goalplan '" + slug + "' could not be read"
		if d := read.Diagnostic; d != nil && d.Kind != "absent" {
			detail = d.Detail
		}
		return GoalplanWriteLockResult[T]{Kind: "unreadable", Reason: detail}, nil
	}
	value, err := fn(read.Plan)
	if err != nil {
		return result, err
	}
	return GoalplanWriteLockResult[T]{Kind: "ok", Value: &value}, nil
}
