package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The run lock is a file lock (flock) on <state>/flow/<run>/run.lock. The process that runs a run, and the command that appends
// a row to the journal of a run that no process holds (`crw flow answer`), take it for as long as they write; the kernel
// releases it when the holder dies, so a run whose lock is free is a run no process works on and `crw flow resume` or
// `crw flow recover` may take it from its journal. The holder writes its pid into the file while it holds the lock, so a
// reader that finds the lock held knows whom `crw flow pause` and `crw flow cancel` signal.

// ErrRunLive is the refusal of a command that needs the run lock when a process holds it.
var ErrRunLive = errors.New("a process holds the run")

// RunDir is the directory of a run below the state directory: its lock, its spec, its log and its result (the allocator of the run's
// worktrees lives below it too, <state>/flow/<run>/wt).
func RunDir(state, run string) string { return filepath.Join(state, "flow", run) }

func lockPath(state, run string) string { return filepath.Join(RunDir(state, run), "run.lock") }

// validRunID is a run id a path can carry: no separator, no dot segment.
func validRunID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// Lock is a held run lock.
type Lock struct {
	f *os.File
}

type lockRecord struct {
	PID     int    `json:"pid"`
	Since   string `json:"since"`
	Command string `json:"command,omitempty"`
}

// TakeLock takes the run lock without waiting. It returns ErrRunLive (wrapped, with the holder's pid when it can be read) when a
// process holds it. The directory is created; the lock file is never removed, because another process may hold it.
func TakeLock(state, run, command string, now time.Time) (*Lock, error) {
	if !validRunID(run) {
		return nil, fmt.Errorf("%q is not a run id", run)
	}
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(state, run), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid, ok := LockHolder(state, run); ok {
				return nil, fmt.Errorf("%w %s (pid %d)", ErrRunLive, run, pid)
			}
			return nil, fmt.Errorf("%w %s", ErrRunLive, run)
		}
		return nil, err
	}
	// the holder must be nameable: a lock whose record cannot be written would let `crw flow pause` and `cancel` find a held run with
	// no process to signal
	rec, _ := json.Marshal(lockRecord{PID: os.Getpid(), Since: now.UTC().Format(time.RFC3339), Command: command})
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("the pid of the holder of run %s cannot be recorded: %w", run, err)
	}
	if _, err := f.WriteAt(append(rec, '\n'), 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("the pid of the holder of run %s cannot be recorded: %w", run, err)
	}
	return &Lock{f: f}, nil
}

// Release releases the lock. The pid record stays in the file, and means nothing once the lock is free.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// LockHeld reports whether a process holds the run lock. A run that never had a lock file is not held.
func LockHeld(state, run string) bool {
	if !validRunID(run) {
		return false
	}
	return heldAt(lockPath(state, run))
}

// The start lock is a file lock on <state>/flow/<run>/start.lock that `crw flow start` holds from before it binds the run's owner thread
// until the run's process holds the run lock (or the start gave up on it). A start binds before its process exists (CRW-1316 §9), so
// another start that binds the same owner thread in that window takes the run as live by its start lock, not as a run whose process
// is gone (CRW-1372: two near-simultaneous starts cannot both bind).
func startLockPath(state, run string) string { return filepath.Join(RunDir(state, run), "start.lock") }

// takeStartLock takes the start lock of a new run without waiting.
func takeStartLock(state, run string) (*Lock, error) {
	if !validRunID(run) {
		return nil, fmt.Errorf("%q is not a run id", run)
	}
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(startLockPath(state, run), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("the start lock of run %s: %w", run, err)
	}
	return &Lock{f: f}, nil
}

// startLockHeld says whether a start of the run holds its start lock: the run is being started and its process may not hold the
// run lock yet.
func startLockHeld(state, run string) bool {
	if !validRunID(run) {
		return false
	}
	return heldAt(startLockPath(state, run))
}

// runLive says whether a run is live for the owner binding: its process holds the run lock, or its start holds the start lock.
func runLive(state, run string) bool { return LockHeld(state, run) || startLockHeld(state, run) }

// The process lock is a file lock a session run's process holds on a file of its own, <state>/flow/<run>/process.<token>.lock: it takes
// it before the run lock and lets it go after the run lock, and its started row names the token (`process`). A reader that finds the
// run lock held tells by it whether the newest started row is the holder's (its process lock is held) or an earlier process's (the
// kernel let that one go when the process ended), so `crw flow next` never takes an ended process's pins for the holder's (CRW-1372).
// The holder removes its file when it ends; the file of a process that died stays, and nothing holds it.
type processLock struct {
	f     *os.File
	path  string
	token string
}

// processTokenLen is the length of a process token: 16 lowercase hex digits.
const processTokenLen = 16

func processLockPath(state, run, token string) string {
	return filepath.Join(RunDir(state, run), "process."+token+".lock")
}

// validProcessToken is a token a path can carry: 16 lowercase hex digits.
func validProcessToken(token string) bool {
	if len(token) != processTokenLen {
		return false
	}
	for _, r := range token {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// takeProcessLock makes the process lock of this process of a run under a new token and holds it.
func takeProcessLock(state, run string) (*processLock, error) {
	if !validRunID(run) {
		return nil, fmt.Errorf("%q is not a run id", run)
	}
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		return nil, err
	}
	var b [processTokenLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b[:])
	path := processLockPath(state, run, token)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	// the file is new and this process's: only a reader's probe (heldAt) can hold its lock, for an instant, so the lock is waited for
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("the process lock of run %s: %w", run, err)
	}
	return &processLock{f: f, path: path, token: token}, nil
}

// Token is the token the process's started row names ("" for no lock).
func (p *processLock) Token() string {
	if p == nil {
		return ""
	}
	return p.token
}

// Release removes the file and lets the lock go.
func (p *processLock) Release() {
	if p == nil || p.f == nil {
		return
	}
	_ = os.Remove(p.path)
	_ = syscall.Flock(int(p.f.Fd()), syscall.LOCK_UN)
	_ = p.f.Close()
	p.f = nil
}

// processHeld says whether the process a started row names by its token still holds its process lock.
func processHeld(state, run, token string) bool {
	return validRunID(run) && validProcessToken(token) && heldAt(processLockPath(state, run, token))
}

// processFresh says whether the process lock a token names was made within startingWait: the process took the run a moment ago.
func processFresh(state, run, token string) bool {
	if !validRunID(run) || !validProcessToken(token) {
		return false
	}
	info, err := os.Stat(processLockPath(state, run, token))
	return err == nil && time.Since(info.ModTime()) < startingWait
}

// anyProcessHeld says whether a process of the run holds a process lock: a process of this crw holds the run, or takes it.
func anyProcessHeld(state, run string) bool {
	if !validRunID(run) {
		return false
	}
	entries, _ := os.ReadDir(RunDir(state, run))
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "process.")
		token, lock := strings.CutSuffix(rest, ".lock")
		if ok && lock && validProcessToken(token) && heldAt(processLockPath(state, run, token)) {
			return true
		}
	}
	return false
}

// heldAt says whether another open file holds the flock of path; a file that is not there is not held.
func heldAt(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// LockHolder is the pid the holder of the run lock wrote into it. It is read for a lock that is held: the holder writes its pid
// after it took the lock, so a read in that instant waits a moment for it.
func LockHolder(state, run string) (int, bool) {
	if !validRunID(run) {
		return 0, false
	}
	for i := 0; i < 20; i++ {
		raw, err := os.ReadFile(lockPath(state, run))
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false
		}
		var rec lockRecord
		if err == nil && json.Unmarshal(raw, &rec) == nil && rec.PID > 1 {
			return rec.PID, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, false
}

func itoa(n int) string { return strconv.Itoa(n) }

// killProcess sends sig to one process. A pid of 1 or less is refused: kill(2) sends to the caller's process group for 0, to
// every process the caller may signal for -1 and to a process group for any other negative number.
func killProcess(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("%d is not the pid of a run process", pid)
	}
	return syscall.Kill(pid, sig)
}
