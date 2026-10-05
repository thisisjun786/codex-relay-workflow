package role

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Every directory lock of the dispatch ledger records its owner, and dispatch-lock-clear removes a lock a dead process left,
// but only once it has confirmed that the owner is gone. The oracle (fallback-dispatch.ts:137-139) has no way to recover a
// lock at all. Nothing here steals a lock: a held lock is still refused, and so is one whose owner cannot be confirmed gone.
const (
	dispatchLockOwnerFile   = "owner.json"
	dispatchLockClearLog    = "lock-clears.jsonl"
	dispatchLockClearWait   = 5 * time.Second
	dispatchLockClearHelp   = "usage: crw role helper dispatch-lock-clear --session <id> (--dispatch <id> | --session-lock) --reason <text> [--cwd <dir>]"
	dispatchLockUnconfirmed = "cannot confirm its owner is gone: "
)

// dispatchLockOwner is owner.json: the process that took the lock. processStart is empty when the platform could not read it.
type dispatchLockOwner struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"processStart"`
	Host         string `json:"host"`
	CreatedAt    int64  `json:"createdAt"`
}

// dispatchLockCleared is one line of lock-clears.jsonl, and what the command prints. Times are milliseconds.
type dispatchLockCleared struct {
	ClearedAt    int64             `json:"clearedAt"`
	Lock         string            `json:"lock"`
	Owner        dispatchLockOwner `json:"owner"`
	Reason       string            `json:"reason"`
	ClearedByPid int               `json:"clearedByPid"`
}

// writeOwner creates owner.json in the lock lockWith just made. The lock is pinned with the identity lockWith read, so a lock
// that was swapped for a link meanwhile is refused and nothing is written through it.
func (d *dispatchPinnedDir) writeOwner(lock string, created fs.FileInfo) error {
	name, err := os.Hostname()
	if err != nil {
		return err
	}
	pid := os.Getpid()
	start, _ := dispatchProcessStart(pid) // unreadable: recorded empty, and a clear then refuses while the pid exists
	data, err := json.Marshal(dispatchLockOwner{pid, start, name, time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	sub, err := dispatchPinnedOpen(d.root, lock, created)
	if err != nil {
		return err
	}
	defer sub.Close()
	f, err := sub.OpenFile(dispatchLockOwnerFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return dispatchPinnedFail("open", filepath.Join(d.display(lock), dispatchLockOwnerFile), err)
	}
	_, err = f.Write(append(data, '\n'))
	return errors.Join(err, f.Close())
}

// lockAbandon undoes a lock whose owner could not be written. It removes the directory lockWith created and only that one: an
// entry that took its name meanwhile is not ours to delete.
func (d *dispatchPinnedDir) lockAbandon(lock string, created fs.FileInfo, cause error) error {
	if created != nil {
		now, err := d.root.Lstat(lock)
		if err == nil && os.SameFile(created, now) {
			if err = d.root.RemoveAll(lock); err == nil {
				return cause
			}
			return errors.Join(cause, fmt.Errorf("lock %s left behind: %w", d.display(lock), err))
		}
	}
	return errors.Join(cause, fmt.Errorf("lock %s is not the directory that was created and is left in place", d.display(lock)))
}

// dispatchLockOwnerRead returns the bytes of owner.json of the lock directory name of dir. The entry must be a real directory
// (a link is refused) that is pinned while owner.json, a regular file, is read through that handle.
func dispatchLockOwnerRead(dir *dispatchPinnedDir, name string) ([]byte, error) {
	info, err := dir.root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("no lock to clear: %s", dir.display(name))
	case err != nil:
		return nil, dir.fail("lstat", name, err)
	case !info.IsDir():
		return nil, fmt.Errorf("%s is not a real directory; nothing was removed", dir.display(name))
	}
	sub, err := dispatchPinnedOpen(dir.root, name, info)
	if err != nil {
		return nil, errors.New(dispatchLockUnconfirmed + err.Error())
	}
	defer sub.Close()
	data, err := (&dispatchPinnedDir{root: sub, path: dir.display(name)}).readFile(dispatchLockOwnerFile)
	if err != nil {
		return nil, errors.New(dispatchLockUnconfirmed + err.Error())
	}
	return data, nil
}

// dispatchLockJudge returns the owner of lock when it is confirmed gone, with the bytes of owner.json it judged, and an error
// that says why not otherwise. The lock must be a real directory holding a regular owner.json of a process on this host that
// no longer exists (kill 0 answers ESRCH) or whose start time differs from the recorded one, which means the pid was taken
// over. A pid that exists with the recorded start time is alive, EPERM included, and a pid whose start time cannot be
// compared is not confirmed either way.
func dispatchLockJudge(dir *dispatchPinnedDir, lock string) (owner dispatchLockOwner, data []byte, err error) {
	if data, err = dispatchLockOwnerRead(dir, lock); err != nil {
		return owner, nil, err
	}
	err = json.Unmarshal(data, &owner)
	if err == nil && (owner.PID <= 0 || owner.PID > 1<<31-1 || owner.Host == "") {
		err = errors.New("owner.json has no valid pid and host")
	}
	if err != nil {
		return owner, data, errors.New(dispatchLockUnconfirmed + err.Error())
	}
	if name, err := os.Hostname(); err != nil || name != owner.Host {
		return owner, data, fmt.Errorf("owner host differs from this host (%s); nothing was removed", owner.Host)
	}
	if err := syscall.Kill(owner.PID, 0); errors.Is(err, syscall.ESRCH) {
		return owner, data, nil
	} else if err != nil && !errors.Is(err, syscall.EPERM) {
		return owner, data, errors.New(dispatchLockUnconfirmed + err.Error())
	}
	start, err := dispatchProcessStart(owner.PID)
	switch {
	case owner.ProcessStart == "" || err != nil:
		return owner, data, errors.New(dispatchLockUnconfirmed + "the pid exists and its start time cannot be compared")
	case start == owner.ProcessStart:
		return owner, data, fmt.Errorf("owner process is alive (pid %d); nothing was removed", owner.PID)
	}
	return owner, data, nil
}

// dispatchLockClearExclusive takes an exclusive advisory lock on f, polling for dispatchLockClearWait. It belongs to the open
// file description, so it serialises clears (those of one process too) and the kernel takes it back at close or death.
func dispatchLockClearExclusive(f *os.File) error {
	deadline := time.Now().Add(dispatchLockClearWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		case time.Now().After(deadline):
			return errors.New("another clear of this session is running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// dispatchLockClear removes lock from dir once its owner is confirmed gone and appends what it did to lock-clears.jsonl; it
// returns that line. Clears of one session take turns. The log is checked before anything is touched and opened for writing
// before the claim. The owner is judged by name, then the lock is claimed by renaming it to a tombstone, whose owner.json must
// still be the bytes that were judged: a lock that took the place of the judged one in between is put back (restoreLock),
// never removed. After the claim only the write of the line and the removal of the tombstone can fail, and the line is then
// returned with the error. os.Root follows a link that stays inside the root even with O_NOFOLLOW, so a link or any other entry that is not a regular file
// is refused by its Lstat, and an existing log must still be that file once it is open.
func dispatchLockClear(dir *dispatchPinnedDir, lock, reason string) ([]byte, error) {
	guard, err := dir.root.Open(".")
	if err != nil {
		return nil, dir.fail("open", ".", err)
	}
	defer guard.Close()
	if err = dispatchLockClearExclusive(guard); err != nil {
		return nil, err
	}
	logInfo, err := dir.root.Lstat(dispatchLockClearLog)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		logInfo = nil
	case err != nil:
		return nil, dir.fail("lstat", dispatchLockClearLog, err)
	case !logInfo.Mode().IsRegular():
		return nil, fmt.Errorf("%s must be a regular file; nothing was removed", dir.display(dispatchLockClearLog))
	}
	owner, judged, err := dispatchLockJudge(dir, lock)
	if err != nil {
		return nil, err
	}
	flags := os.O_WRONLY | os.O_APPEND | syscall.O_NONBLOCK
	if logInfo == nil {
		flags |= os.O_CREATE | os.O_EXCL
	}
	log, err := dir.root.OpenFile(dispatchLockClearLog, flags, 0o600)
	if err != nil {
		return nil, dir.fail("open", dispatchLockClearLog, err)
	}
	defer log.Close()
	if opened, err := log.Stat(); logInfo != nil && (err != nil || !os.SameFile(logInfo, opened)) {
		return nil, fmt.Errorf("%s changed while it was opened; nothing was removed", dir.display(dispatchLockClearLog))
	}
	line, err := json.Marshal(dispatchLockCleared{time.Now().UnixMilli(), lock, owner, reason, os.Getpid()})
	if err != nil {
		return nil, err
	}
	dir.point("judged")
	tomb := lock + ".clearing-" + dispatchUUID()
	if err = dir.root.Rename(lock, tomb); err != nil {
		return nil, err
	}
	dir.point("claimed")
	if now, err := dispatchLockOwnerRead(dir, tomb); err != nil || !bytes.Equal(now, judged) {
		return nil, dir.restoreLock(lock, tomb)
	}
	// The lock is gone from its name, so the removal is written down first; removing the tombstone is only tidying up.
	_, err = log.Write(append(line, '\n'))
	if rmErr := dir.root.RemoveAll(tomb); rmErr != nil {
		err = errors.Join(err, fmt.Errorf("the lock is cleared, but its tombstone %s could not be removed: %w", dir.display(tomb), rmErr))
	}
	return line, err
}

// restoreLock answers a clear that did not claim the lock it judged: the lock that was there changed after the judgment, so what
// the rename moved is another process's lock. It goes back under its name when the name is still free; when the name was taken
// again it stays at the tombstone name, where the error says to find it. It is never removed.
func (d *dispatchPinnedDir) restoreLock(lock, tomb string) error {
	if _, err := d.root.Lstat(lock); errors.Is(err, fs.ErrNotExist) && d.root.Rename(tomb, lock) == nil {
		return fmt.Errorf("%s was replaced while it was being cleared and has been put back; nothing was removed", d.display(lock))
	}
	return fmt.Errorf("%s was replaced while it was being cleared and is left at %s; nothing was removed", d.display(lock), d.display(tomb))
}

// DispatchLockClearCommand is 'crw role helper dispatch-lock-clear'. Every outcome is one JSON line on stdout: the line that was
// logged (exit 0), {"error": ...} for a refusal (exit 1) or for usage (exit 2). A refusal that has a "cleared" member comes after
// the lock was claimed: that is its line, whose write or whose tombstone removal then failed.
func DispatchLockClearCommand(args []string, _ io.Reader, out io.Writer, _ host.LookupEnv) int {
	answer, code := dispatchLockClearRun(args)
	encoded, err := json.Marshal(answer)
	if err != nil {
		return 1
	}
	if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
		return 1
	}
	return code
}

func dispatchLockClearRun(args []string) (any, int) {
	var session, dispatch, reason, cwd string
	sessionLock := false
	usage := func() (any, int) { return map[string]string{"error": dispatchLockClearHelp}, 2 }
	for i := 0; i < len(args); i++ {
		var target *string
		switch args[i] {
		case "--session":
			target = &session
		case "--dispatch":
			target = &dispatch
		case "--reason":
			target = &reason
		case "--cwd":
			target = &cwd
		case "--session-lock":
			sessionLock = true
			continue
		default:
			return usage()
		}
		if i++; i >= len(args) || *target != "" {
			return usage()
		}
		*target = args[i]
	}
	if _, err := dispatchID(session, "sessionId"); err != nil || sessionLock == (dispatch != "") || text.Trim(reason) == "" {
		return usage()
	}
	name := dispatchSessionLock
	if dispatch != "" {
		if _, err := dispatchID(dispatch, "dispatchId"); err != nil {
			return usage()
		}
		name = dispatch + ".json"
	}
	refuse := func(err error, cleared []byte) (any, int) {
		answer := map[string]any{"error": err.Error()}
		if cleared != nil {
			answer["cleared"] = json.RawMessage(cleared)
		}
		return answer, 1
	}
	var err error
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return refuse(err, nil)
		}
	}
	root, err := dispatchRoot(cwd)
	if err != nil {
		return refuse(err, nil)
	}
	// A refusal creates nothing: dispatchDirectory below would make a session directory that does not exist.
	path := filepath.Join(root, crwdir.DirName, "dispatches", session)
	if _, err = os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("session directory does not exist: %s", path)
		}
		return refuse(err, nil)
	}
	dir, err := dispatchDirectory(root, session, nil)
	if err != nil {
		return refuse(err, nil)
	}
	defer dir.Close()
	line, err := dispatchLockClear(dir, name+".lock", reason)
	if err != nil {
		return refuse(err, line)
	}
	return json.RawMessage(line), 0
}
