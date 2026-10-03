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
// hard links are unavailable (EPERM, ENOTSUP or EXDEV: FAT32, some shares) an exclusive create of the final path stands in.
func EnsureState(cwd, sessionID string) (bool, error) {
	return ensureState(cwd, sessionID, time.Now(), os.Link)
}

func ensureState(cwd, sessionID string, now time.Time, link func(existing, created string) error) (created bool, err error) {
	if !IsCanonicalSessionID(sessionID) {
		return false, ErrNonCanonicalSessionID
	}
	if err = makeSessionsDir(cwd); err != nil {
		return false, err
	}
	body, err := Encode(defaultState(sessionID, "", now))
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
	switch err = createExclusive(finalPath, string(body)); {
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
