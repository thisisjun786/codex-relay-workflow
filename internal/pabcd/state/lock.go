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
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sessionLockWaitScale multiplies every delay of the wait WithSessionLock schedules (a caller with a context keeps its timer); 0 and 1 leave the
// oracle's schedule as it is, which is all production ever runs. A test that races callers it cannot reach through the sleep seam
// (a hook, a command) widens the wait with WidenSessionLockWait so the lock, not the time a holder takes on a loaded host, decides
// who is inside (CRW-1181).
var sessionLockWaitScale atomic.Int64

// WidenSessionLockWait makes every later wait for a session lock sleep scale times as long, and returns the call that puts the
// previous scale back. It is for tests; production never calls it.
func WidenSessionLockWait(scale int64) (restore func()) {
	previous := sessionLockWaitScale.Swap(scale)
	return func() { sessionLockWaitScale.Store(previous) }
}

// sessionLockSleep is the sleep a caller without a seam of its own uses.
func sessionLockSleep(d time.Duration) {
	if scale := sessionLockWaitScale.Load(); scale > 1 {
		d *= time.Duration(scale)
	}
	time.Sleep(d)
}

// WithSessionLock runs fn holding the session's exclusive lock, the file <state file>.lock (withSessionLock). The session id must
// be canonical: an id that sanitising would rewrite, or an empty one, is refused with ErrNonCanonicalSessionID before anything is
// created (CRW-1108).
//
// Ownership is the kernel's (CRW-1094). A holder keeps an flock(2) on the lock file for as long as fn runs and records its pid in
// it ("<pid> flock"); the kernel drops that lock when the holder's descriptor closes, at the release or when the process dies, so
// a lock left by a killed hook no longer silences the session. A holder's file reaches the path with its kernel lock and record
// already in place (trySessionLock), so the path never names an empty record of this protocol. An acquirer that finds the file
// takes the kernel lock without waiting and checks that the path still names the file it locked; it then takes the file over when
// its owner is known to be gone: a record of this protocol (its writer held the kernel lock until it died) or the oracle's bare pid
// of a process that no longer exists. A live holder, the oracle's pid of a live process, an empty record (the oracle's holder between
// its create and its pid write, alive or not) and a record or a path it cannot judge (a directory, a link, a FIFO, other text) are
// never taken over: there is no time-based breaker. Acquisition keeps the oracle's schedule and gives up after about 250 ms with the
// busy error (a *fs.PathError on the lock path that errors.Is fs.ErrExist). A record that cannot be written leaves no lock file and
// returns the write's error, and the release, which also runs when fn panics, removes the lock file only while the path still names
// the file this holder locked (known-defects.md :76 and :77, port: fixed).
// Once held, the pending ledger events of the session are judged first (JudgeLedgerOutbox); a verdict that cannot be kept is
// answered as ErrLedgerJudgment and fn does not run (CRW-1097).
func WithSessionLock(cwd, sessionID string, fn func() error) error {
	return WithSessionLockContext(context.Background(), cwd, sessionID, fn)
}

// SessionLockPath is the session's lock file, <state file>.lock.
func SessionLockPath(cwd, sessionID string) string { return StatePath(cwd, sessionID) + ".lock" }

// sessionLockBeforeGiveUp is a test seam: it runs inside orchestrateInterruptLockContext immediately
// before an acquisition failure is returned, so a test can cancel the invocation's context in that
// window without a sleep. Production leaves it nil.
var sessionLockBeforeGiveUp func()

// sessionLockOutcome is a test seam: it reports the outcome of every acquisition once it is known and before
// the caller's fn runs or the failure is returned. err is nil when the lock was acquired and otherwise the
// error that ended the acquisition (errors.Is(err, fs.ErrExist) for a busy lock whose wait ran out; any other
// error is a failure that is not contention). A test that has to tell a busy-lock give-up from a failure
// reads the order of these reports. Production leaves it nil (CRW-1172).
var sessionLockOutcome func(err error)

func withSessionLock(cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	return orchestrateInterruptLockContext(context.Background(), cwd, sessionID, fn, sleep, nil)
}

// WithSessionLockContext is WithSessionLock for a caller that can be interrupted (the orchestrate row under cmd/crw
// serve, whose invocation context the first SIGINT cancels). The retry schedule is the same; a context cancelled
// before or during the wait creates no lock file, does not run fn and returns the context's own error. A caller with
// no context passes context.Background(), which is what WithSessionLock does: its behaviour and its sleep seam are
// unchanged.
func WithSessionLockContext(ctx context.Context, cwd, sessionID string, fn func() error) error {
	retryDelays, onBusy := lockWaitProbe()
	return orchestrateInterruptLockWait(ctx, cwd, sessionID, fn, sessionLockSleep, retryDelays, onBusy)
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
		if sessionLockOutcome != nil {
			sessionLockOutcome(err)
		}
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
			if sessionLockOutcome != nil {
				sessionLockOutcome(err)
			}
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
	if sessionLockOutcome != nil {
		sessionLockOutcome(nil)
	}
	defer held.release()
	// CRW-1097: an event an earlier writer of the session left pending is judged now, from the state that writer left, before this
	// holder changes anything, so no later drain mistakes this holder's write for the event's transition. A verdict that could not be
	// kept refuses the holder: fn does not run, and the next holder judges again.
	if err := JudgeLedgerOutbox(cwd, sessionID); err != nil {
		return err
	}
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

// sessionLockRenameNoReplace is the no-replace rename a fresh lock is published with. It is a test seam (a seccomp filter that blocks
// renameat2 answers EPERM); production keeps renameNoReplace.
var sessionLockRenameNoReplace = renameNoReplace

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
//
// The path never names an empty record of this protocol: a fresh lock is a file whose kernel lock and record are in place before
// link(2) puts it at the path, which fails when anything is there already, and a takeover renames such a file over the gone
// owner's file while it holds that file's kernel lock. An empty file at the path is therefore the oracle's holder between its
// exclusive create and its pid write, alive or not, which no record can tell apart, so it is never taken over.
func trySessionLock(path string) (*sessionLock, error) {
	busy := &fs.PathError{Op: "open", Path: path, Err: syscall.EEXIST}
	// A file that leaves the path between the open and the kernel lock was released by its holder, and a file that appears
	// between a failed open and the link was placed by another acquirer: both are progress, so the attempt opens again rather
	// than waiting, a bounded number of times.
	for range 3 {
		// O_NONBLOCK: a FIFO at the path must not block the open; it is refused below as not a regular file.
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrNotExist) {
			held, err := placeSessionLock(path, false)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return held, err
		}
		if err != nil { // a link, a directory, a socket, a file this process may not write: nothing it can judge
			return nil, busy
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
		if !sessionLockOwnerGone(f) {
			_ = f.Close()
			return nil, busy
		}
		// The gone owner's file stays locked by this process until its replacement is at the path, so an acquirer that opened
		// it meanwhile finds, once it gets that kernel lock, that the path names another file.
		held, err := placeSessionLock(path, true)
		_ = f.Close()
		return held, err
	}
	return nil, busy
}

// placeSessionLock stages a lock file beside the path (<lock>.<pid>.<uuid>.tmp), takes its kernel lock, writes this holder's
// record into it and only then puts it at the path: for a fresh lock by a rename that refuses to replace (renameNoReplace; link(2)
// where the platform has no such rename or a policy blocks it, noReplaceUnsupported, EPERM included), which answers fs.ErrExist
// when another acquirer was first, and for a takeover by rename(2) over the gone owner's file. The staged name is removed again on every path; a record that cannot be
// written returns the write's error with no lock file at the path. A process killed while it stages leaves only the staged name,
// which `crw pabcd reset --state` removes once its pid is gone (OrphanStateTemp).
func placeSessionLock(path string, replace bool) (*sessionLock, error) {
	staged := tempPath(path)
	f, err := os.OpenFile(staged, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sessionLock, error) {
		_ = os.Remove(staged)
		_ = f.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(&fs.PathError{Op: "flock", Path: staged, Err: err})
	}
	if err := sessionLockWriteRecord(f, sessionLockRecord(os.Getpid())); err != nil {
		return fail(&fs.PathError{Op: "write", Path: path, Err: err})
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	switch {
	case replace:
		err = os.Rename(staged, path)
	default:
		err = sessionLockRenameNoReplace(staged, path)
		if noReplaceUnsupported(err) {
			err = os.Link(staged, path)
		}
	}
	_ = os.Remove(staged) // gone already after a rename; the second name after a link
	if err != nil {
		_ = f.Close()
		if errors.Is(err, fs.ErrExist) {
			return nil, fs.ErrExist
		}
		return nil, err
	}
	return &sessionLock{path: path, file: f, info: info}, nil
}

// sessionLockOwnerGone judges the record of a lock file whose kernel lock this process now holds. A record of this protocol has no
// live owner: a holder of this protocol keeps the kernel lock until it is gone. The oracle's record is the bare pid of a holder that
// took no kernel lock, so it is gone only when no such process exists. Anything else cannot be judged, the empty record included:
// it is what the oracle's live holder shows between its exclusive create and its pid write.
func sessionLockOwnerGone(f *os.File) bool {
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	record := string(buf[:n])
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
