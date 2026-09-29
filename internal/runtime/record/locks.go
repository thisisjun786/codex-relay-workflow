package record

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// LockSuffix names the read-modify-write sidecar beside a target (hostrecord.Locked).
const LockSuffix = ".crw-lock"

// StaleLock is the age after which a .crw-lock left by a dead process is removed.
const StaleLock = 300 * time.Second

// LockTimeout is how long a run waits for another run's .crw-lock.
var LockTimeout = 10 * time.Second

// PromotionLockSuffix names the host-wide promotion lock beside the host record.
const PromotionLockSuffix = ".promotion-lock"

// PromotionTimeout is how long a promotion waits for another one.
var PromotionTimeout = 60 * time.Second

// Busy is hostrecord.Busy: a lock in this package is held by another run, and nothing else.
type Busy struct{ Message string }

func (b *Busy) Error() string { return b.Message }

// Locked is hostrecord.Locked: an exclusive lock FILE around a read-modify-write.
//
// It keeps Python's protocol exactly - O_CREAT|O_EXCL, the pid written into it, a file older
// than 300 s unlinked as stale, and the file unlinked on release - because the Python
// installer and plugin_transition take the same files until todo 44 retires them, and a flock
// on this path would not exclude an O_EXCL holder. It is the documented exception to the rule
// that no lock file is ever unlinked (docs/port/decisions.md 33). What it does not cover is an
// editor that ignores it.
type Locked struct {
	Path   string
	handle *os.File
}

// Lock takes target's .crw-lock, waiting up to timeout (LockTimeout when zero).
func Lock(target string, timeout time.Duration) (*Locked, error) {
	return LockContext(context.Background(), target, timeout)
}

// wait sleeps one polling interval, or answers the context's error once it is done: a
// cancelled caller stops waiting for a lock at once, and never takes it afterwards.
func wait(ctx context.Context) error {
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// LockContext is Lock that stops waiting, taking nothing, once ctx is done.
func LockContext(ctx context.Context, target string, timeout time.Duration) (*Locked, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = LockTimeout
	}
	path := target + LockSuffix
	deadline := time.Now().Add(timeout)
	for {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return nil, err
		}
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if err == nil {
			if _, err := handle.WriteString(strconv.Itoa(os.Getpid())); err != nil {
				_ = handle.Close()
				_ = os.Remove(path)
				return nil, err
			}
			return &Locked{Path: path, handle: handle}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// A lock left by a process that died would otherwise block for ever.
		age := time.Duration(0)
		if info, statErr := os.Stat(path); statErr == nil {
			age = time.Since(info.ModTime())
		}
		if age > StaleLock {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, &Busy{Message: "another run holds " + path}
		}
		if err := wait(ctx); err != nil {
			return nil, err
		}
	}
}

// Release closes and unlinks the lock file, on every path.
func (l *Locked) Release() {
	if l == nil {
		return
	}
	if l.handle != nil {
		_ = l.handle.Close()
		l.handle = nil
	}
	_ = os.Remove(l.Path)
}

// Exclusive is hostrecord.Exclusive: the host-wide promotion lock, an flock(2) on one file
// beside the host record that is created once and never replaced or removed. flock is what
// Python's fcntl.flock takes, so a Go and a Python promotion exclude each other.
type Exclusive struct {
	Path   string
	handle *os.File
}

// Promote takes the promotion lock beside recordPath, waiting up to timeout (PromotionTimeout
// when zero).
func Promote(recordPath string, timeout time.Duration) (*Exclusive, error) {
	return PromoteContext(context.Background(), recordPath, timeout)
}

// PromoteContext is Promote that stops waiting, taking nothing, once ctx is done.
func PromoteContext(ctx context.Context, recordPath string, timeout time.Duration) (*Exclusive, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = PromotionTimeout
	}
	path := recordPath + PromotionLockSuffix
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(handle.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &Exclusive{Path: path, handle: handle}, nil
		}
		if !contended(err) {
			_ = handle.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = handle.Close()
			return nil, &Busy{Message: "another run holds the promotion lock at " + path}
		}
		if err := wait(ctx); err != nil {
			_ = handle.Close()
			return nil, err
		}
	}
}

// Release unlocks and closes; the file is never unlinked.
func (e *Exclusive) Release() {
	if e == nil || e.handle == nil {
		return
	}
	_ = unix.Flock(int(e.handle.Fd()), unix.LOCK_UN)
	_ = e.handle.Close()
	e.handle = nil
}

func contended(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES)
}

// Liveness answers whether somebody holds an flock on a lock file.
const (
	Held    = "HELD"
	Free    = "FREE"
	NoFile  = "NO_FILE"
	Unknown = "UNKNOWN"
)

// Probe asks whether an flock is held on path by taking it without blocking and releasing it
// at once. It never creates the file: a missing lock file is NoFile, and a lock that could not
// be tested is Unknown rather than Free.
func Probe(path string) (string, string) {
	handle, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return NoFile, "no lock file exists at " + path
	}
	if err != nil {
		return Unknown, "the lock file could not be opened to test it: " + store.PythonOSError(err)
	}
	defer handle.Close()
	if err := unix.Flock(int(handle.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if contended(err) {
			return Held, "another process holds the advisory lock on " + path
		}
		return Unknown, fmt.Sprintf("the advisory lock could not be tested: %v", err)
	}
	_ = unix.Flock(int(handle.Fd()), unix.LOCK_UN)
	return Free, "nothing holds the advisory lock on " + path
}
