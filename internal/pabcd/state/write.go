package state

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

// ErrNonCanonicalSessionID is the TypeError ensureState throws for an id that sanitising would rewrite.
const ErrNonCanonicalSessionID = sentinel("sessionId must be a canonical state key")

type sentinel string

func (e sentinel) Error() string { return string(e) }

// timestampLayout is Date.prototype.toISOString.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// EnsureState creates the session's file as a fresh IDLE state and reports whether it did, without resetting a resumed session
// (ensureState). The default is written beside the final path and published with an exclusive hard link, which is atomic at the
// destination, so concurrent SessionStart hooks race safely and an existing file, valid or corrupt, is never touched. Where
// hard links are unavailable (EPERM, ENOTSUP or EXDEV: FAT32, some shares) the default is written and fsynced to a second temp
// file and published by a rename that refuses to replace (publishWithoutLink), so a failed write leaves nothing at the final
// path. The oracle creates the final path and then writes into it, which leaves an empty or truncated state file when the write
// fails (a known defect, fixed here by decision).
func EnsureState(cwd, sessionID string) (bool, error) {
	return ensureState(cwd, sessionID, time.Now, os.Link)
}

func ensureState(cwd, sessionID string, now func() time.Time, link func(existing, created string) error) (bool, error) {
	return ensureStateWith(cwd, sessionID, now, link, nil)
}

// ensureStep names a step of the fallback publication, so a test can fail it.
type ensureStep int

const (
	stepWrite   ensureStep = iota // before the data is written to a file the fallback created
	stepSync                      // before that file is fsynced
	stepClose                     // before that file is closed; the error is returned after it is
	stepPublish                   // before the no-replace rename; an error stands in for the rename's own
)

// noReplaceUnsupported reports whether err says the platform cannot rename without replacing: a kernel or filesystem without the flag
// (EINVAL, ENOSYS, ENOTSUP), or a seccomp filter that blocks the call (EPERM). EXDEV is not in the set: a rename inside one directory
// cannot cross devices.
func noReplaceUnsupported(err error) bool {
	for _, errno := range [...]syscall.Errno{syscall.EINVAL, syscall.ENOSYS, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EPERM} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// failAt calls the test hook, if there is one, just before step runs on path.
func failAt(fail func(ensureStep, string) error, step ensureStep, path string) error {
	if fail == nil {
		return nil
	}
	return fail(step, path)
}

// ensureStateWith is ensureState with a hook that is called just before each fallback step and fails it by returning an error.
func ensureStateWith(cwd, sessionID string, now func() time.Time, link func(existing, created string) error, fail func(ensureStep, string) error) (created bool, err error) {
	if !IsCanonicalSessionID(sessionID) {
		return false, ErrNonCanonicalSessionID
	}
	if err = makeSessionsDir(cwd); err != nil {
		return false, err
	}
	body, err := Encode(defaultState(sessionID, "", now()))
	if err != nil {
		return false, err
	}
	finalPath := StatePath(cwd, sessionID)
	tmp := tempPath(finalPath)
	defer func() { // the oracle's finally: a removal that throws replaces the result, a value or an error
		if rmErr := removeFile(tmp); rmErr != nil {
			created, err = false, rmErr
		}
	}()
	if err = createExclusive(tmp, string(body)); err != nil {
		return false, err
	}
	switch err = link(tmp, finalPath); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	case !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EXDEV):
		return false, err // not fs.ErrPermission: it also matches EACCES, which the oracle rethrows
	}
	if _, statErr := os.Lstat(finalPath); statErr == nil {
		// anything there, a file, a directory or a symlink, answers the oracle's exclusive create with EEXIST. On a filesystem without
		// hard links this is how a resumed session arrives (link answers EPERM whether or not the file exists), so it is settled
		// before anything more is written; an Lstat error is not an answer, and the publication reports the real one
		return false, nil
	}
	// the oracle stringifies a second defaultState for the fallback file, read from the clock again
	if body, err = Encode(defaultState(sessionID, "", now())); err != nil {
		return false, err
	}
	return publishWithoutLink(finalPath, body, fail)
}

// publishWithoutLink creates finalPath from data on a filesystem without hard links, and never replaces what is there: the data is
// written and fsynced to a temp file beside finalPath, which a rename that refuses to replace then publishes, so a failed write
// leaves nothing at finalPath. EEXIST, from the rename or the create below, is the oracle's answer (false, nil): someone else
// published first. Where the platform has no such rename (noReplaceUnsupported) the file is created in place instead, the oracle's
// way, but a failed write, fsync or close removes the file this call created; that route can still expose a partial file while it is
// written, a kill part-way still leaves one, and the removal can undo another writer's rename that lands between its check and
// the unlink: it is best effort, the safest rule where nothing atomic is available. The oracle's finally semantics (a removal that
// fails replaces the result) apply to the first temp file only; the removal of this one is best effort.
func publishWithoutLink(finalPath string, data []byte, fail func(ensureStep, string) error) (bool, error) {
	tmp := tempPath(finalPath)
	if err := writeNew(tmp, data, fail); err != nil {
		return false, err
	}
	defer func() { _ = removeFile(tmp) }() // gone after a successful rename
	err := failAt(fail, stepPublish, tmp)
	if err == nil {
		err = renameNoReplace(tmp, finalPath)
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	case !noReplaceUnsupported(err):
		return false, err
	}
	switch err = writeNew(finalPath, data, fail); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	}
	return false, err
}

// WriteState publishes next as the session's state file: written to a temp file beside it and renamed over it, so a reader sees
// the old file or the new one whole (writeState). That is atomic publication, not a serialised read-modify-write: a caller that
// must not lose a concurrent update re-reads inside WithSessionLock. The tracker is capped on the way out and updatedAt stamped.
func WriteState(cwd string, next State) error {
	return writeState(cwd, next, time.Now(), crwdir.Rename)
}

func writeState(cwd string, next State, now time.Time, rename func(tmp, finalPath string) error) (err error) {
	if err = makeSessionsDir(cwd); err != nil {
		return err
	}
	finalPath := StatePath(cwd, next.SessionID)
	tmp := tempPath(finalPath)
	defer func() {
		if err != nil {
			_ = removeFile(tmp) // best effort; the first error is the one returned
		}
	}()
	next.Interview, next.UpdatedAt = interview.Normalize(next.Interview), now.UTC().Format(timestampLayout)
	body, err := Encode(next)
	if err != nil {
		return err
	}
	if err = os.WriteFile(tmp, body, 0o666); err != nil {
		return err
	}
	return rename(tmp, finalPath)
}

// makeSessionsDir is ensureCodexclawDir(cwd) then mkdirSync(sessionsDir, { recursive: true }), in that order.
func makeSessionsDir(cwd string) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(cwd, crwdir.DirName, SessionsSubdir), 0o777)
}

// createExclusive is writeFileSync(path, data, { flag: "wx" }): the file exists before it is written, and a failed write
// leaves it behind.
func createExclusive(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	return errors.Join(err, f.Close())
}

// writeNew creates path, which must not exist (fs.ErrExist otherwise, nothing touched), writes data, fsyncs and closes it. On any
// failure, a close error included, the file this call created is removed again, but only while the path still names that file;
// a removal that fails is joined into the error, so a file left behind is never silent.
func writeNew(path string, data []byte, fail func(ensureStep, string) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	made, _ := f.Stat() // the file this call created
	err = failAt(fail, stepWrite, path)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = failAt(fail, stepSync, path)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = failAt(fail, stepClose, path)
	}
	if err = errors.Join(err, f.Close()); err == nil {
		return nil
	}
	now, statErr := os.Lstat(path)
	switch {
	case statErr == nil && os.SameFile(made, now):
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		err = errors.Join(err, statErr) // the file cannot even be looked at, so it cannot be removed, and the failure says so
	}
	return err
}

// removeFile is rmSync(path, { force: true }): a missing file is fine, a directory is refused whether empty or not (os.Remove
// would delete an empty one).
func removeFile(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.IsDir():
		return &fs.PathError{Op: "rm", Path: path, Err: syscall.EISDIR}
	}
	if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// tempPath names a sibling of finalPath by pid and a random UUID. The oracle's writeState uses Date.now(), unique only because
// Node runs one writer at a time per process; goroutines of one Go process must not share a name.
func tempPath(finalPath string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // cannot fail: the runtime stops the program if it cannot read
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%s.%d.%x-%x-%x-%x-%x.tmp", finalPath, os.Getpid(), b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
