package state

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// WithSessionLock runs fn holding the session's exclusive lock, the file <state file>.lock (withSessionLock). The session id must
// be canonical: an id that sanitising would rewrite, or an empty one, is refused with ErrNonCanonicalSessionID before anything is
// created (CRW-1108).
//
// Ownership is the kernel's (CRW-1094). A holder keeps an flock(2) on the lock file for as long as fn runs and records its pid in
// it ("<pid> flock"); the kernel drops that lock when the holder's descriptor closes, at the release or when the process dies, so
// a lock left by a killed hook no longer silences the session. An acquirer that finds the file takes the kernel lock without
// waiting and checks that the path still names the file it locked; it then takes the file over when its owner is known to be gone:
// a record of this protocol (its writer held the kernel lock until it died), an empty record (a record write that failed), or the
// oracle's bare pid of a process that no longer exists. A live holder, the oracle's pid of a live process, and a record or a path it
// cannot judge (a directory, a link, a FIFO, other text) are never taken over: there is no time-based breaker. Acquisition keeps the
// oracle's schedule and gives up after about 250 ms with the busy error (a *fs.PathError on the lock path that errors.Is
// fs.ErrExist). A record that cannot be written removes the file this call holds again and returns the write's error, and the release,
// which also runs when fn panics, removes the lock file only while the path still names the file this holder locked (known-defects.md
// :76 and :77, port: fixed).
func WithSessionLock(cwd, sessionID string, fn func() error) error {
	return WithSessionLockContext(context.Background(), cwd, sessionID, fn)
}

// SessionLockPath is the session's lock file, <state file>.lock.
func SessionLockPath(cwd, sessionID string) string { return StatePath(cwd, sessionID) + ".lock" }

// sessionLockBeforeGiveUp is a test seam: it runs inside orchestrateInterruptLockContext immediately
// before an acquisition failure is returned, so a test can cancel the invocation's context in that
// window without a sleep. Production leaves it nil.
var sessionLockBeforeGiveUp func()

func withSessionLock(cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	return orchestrateInterruptLockContext(context.Background(), cwd, sessionID, fn, sleep, nil)
}

// WithSessionLockContext is WithSessionLock for a caller that can be interrupted (the orchestrate row under cmd/crw
// serve, whose invocation context the first SIGINT cancels). The retry schedule is the same; a context cancelled
// before or during the wait creates no lock file, does not run fn and returns the context's own error. A caller with
// no context passes context.Background(), which is what WithSessionLock does: its behaviour and its sleep seam are
// unchanged.
func WithSessionLockContext(ctx context.Context, cwd, sessionID string, fn func() error) error {
	return orchestrateInterruptLockContext(ctx, cwd, sessionID, fn, time.Sleep, nil)
}

// orchestrateInterruptLockContext is the acquisition both entries share. retryDelays is a test seam: a
// caller that has to reach the give-up passes a short schedule so the case does not burn the oracle's
// real waits (CRW-922); nil means LOCK_RETRY_DELAYS_MS, which is what both entries above pass.
func orchestrateInterruptLockContext(ctx context.Context, cwd, sessionID string, fn func() error, sleep func(time.Duration), retryDelays []time.Duration) error {
	return orchestrateInterruptLockWait(ctx, cwd, sessionID, fn, sleep, retryDelays, nil)
}

// WithSessionLockObserved is WithSessionLock for a test that has to know the caller is waiting. onBusy runs once, on the first
// attempt that finds the lock held, immediately before the caller sleeps for the first time; retryDelays replaces the oracle's
// schedule (milliseconds, as LOCK_RETRY_DELAYS_MS) so the test, not the wall clock, decides when the wait gives up. A nil onBusy
// and a nil retryDelays are exactly WithSessionLock. Production never passes either (CRW-564).
func WithSessionLockObserved(cwd, sessionID string, fn func() error, retryDelays []time.Duration, onBusy func()) error {
	return orchestrateInterruptLockWait(context.Background(), cwd, sessionID, fn, time.Sleep, retryDelays, onBusy)
}

func orchestrateInterruptLockWait(ctx context.Context, cwd, sessionID string, fn func() error, sleep func(time.Duration), retryDelays []time.Duration, onBusy func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !IsCanonicalSessionID(sessionID) { // CRW-1108: a/b no longer shares a-b's lock, nor "" the lock of missing
		return ErrNonCanonicalSessionID
	}
	if err := makeSessionsDir(cwd); err != nil {
		return err
	}
	lockPath := SessionLockPath(cwd, sessionID)
	delays := [...]time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35} // milliseconds: LOCK_RETRY_DELAYS_MS
	schedule := delays[:]
	if retryDelays != nil {
		schedule = retryDelays
	}
	var held *sessionLock
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		held, err = trySessionLock(lockPath)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= len(schedule) {
			if sessionLockBeforeGiveUp != nil {
				sessionLockBeforeGiveUp()
			}
			// CRW-922 (c1-3): the give-up is reported after the invocation's context is read once more, so
			// a cancellation that landed as the wait ended answers the context's own error (130) rather
			// than the busy error with code 1 - the same rule the D close's goalplan locks follow. A
			// caller with no context (context.Background, which WithSessionLock passes) is unchanged: its
			// ctx.Err() is always nil.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		if attempt == 0 && onBusy != nil {
			onBusy()
		}
		delay := schedule[attempt] * time.Millisecond
		if ctx.Done() == nil {
			sleep(delay) // a context that can never end keeps the caller's seam
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer held.release()
	return fn()
}

// sessionLock is a held session lock: the descriptor that carries the kernel lock and the identity of the file it locked.
type sessionLock struct {
	path string
	file *os.File
	info os.FileInfo
}

// sessionLockRecord is the record a holder writes: its pid and the protocol word, which tells a later acquirer that the
// writer held the kernel lock, where the oracle's record is the bare pid.
func sessionLockRecord(pid int) string { return strconv.Itoa(pid) + " flock\n" }

// sessionLockWriteRecord writes the holder's record over whatever the file held. It is a test seam; production keeps it.
var sessionLockWriteRecord = func(f *os.File, record string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(record), 0)
	return err
}

// trySessionLock is one attempt. It returns the held lock, the busy error (a *fs.PathError that errors.Is fs.ErrExist) when
// the lock is held or its owner cannot be judged gone, or any other error, which ends the wait.
func trySessionLock(path string) (*sessionLock, error) {
	busy := &fs.PathError{Op: "open", Path: path, Err: syscall.EEXIST}
	// A file that leaves the path between the open and the kernel lock was released by its holder: that is progress, so the
	// attempt opens again rather than waiting, a bounded number of times.
	for range 3 {
		created := true
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o666)
		if errors.Is(err, fs.ErrExist) {
			created = false
			// O_NONBLOCK: a FIFO at the path must not block the open; it is refused below as not a regular file.
			f, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil: // a link, a directory, a socket, a file this process may not write: nothing it can judge
				return nil, busy
			}
		}
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, busy
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
				return nil, busy
			}
			return nil, &fs.PathError{Op: "flock", Path: path, Err: err}
		}
		// A holder releases by removing its file while it still holds the kernel lock, so a lock taken on a file the path no
		// longer names is a lock on a released file.
		if now, err := os.Lstat(path); err != nil || !os.SameFile(info, now) {
			_ = f.Close()
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		held := &sessionLock{path: path, file: f, info: info}
		if !created && !sessionLockOwnerGone(f) {
			_ = f.Close()
			return nil, busy
		}
		if err := sessionLockWriteRecord(f, sessionLockRecord(os.Getpid())); err != nil {
			held.release()
			return nil, &fs.PathError{Op: "write", Path: path, Err: err}
		}
		return held, nil
	}
	return nil, busy
}

// sessionLockOwnerGone judges the record of a lock file whose kernel lock this process now holds. A record of this protocol, or
// an empty one, has no live owner: a holder of this protocol keeps the kernel lock until it is gone. The oracle's record is the
// bare pid of a holder that took no kernel lock, so it is gone only when no such process exists. Anything else cannot be judged.
func sessionLockOwnerGone(f *os.File) bool {
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	record := string(buf[:n])
	if record == "" {
		return true
	}
	if pid, ok := strings.CutSuffix(record, " flock\n"); ok && sessionLockDigits(pid) {
		return true
	}
	if !sessionLockDigits(record) {
		return false
	}
	pid, err := strconv.Atoi(record)
	return err == nil && ProcessGone(pid)
}

func sessionLockDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// ProcessGone reports whether no process with this pid exists on this host (kill(pid, 0) answers ESRCH). A pid that cannot name
// a process, zero or negative or beyond the kernel's range, is not judged gone: kill(0) and kill(-n) address process groups.
func ProcessGone(pid int) bool {
	if pid <= 0 || pid > math.MaxInt32 {
		return false
	}
	return errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}

// release removes the lock file while the path still names the file this holder locked, then closes the descriptor, which drops
// the kernel lock. A file another process put at the path (after this one's was removed by hand) is left alone.
func (l *sessionLock) release() {
	if now, err := os.Lstat(l.path); err == nil && os.SameFile(l.info, now) {
		_ = os.Remove(l.path)
	}
	_ = l.file.Close()
}
