package state

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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
// (ensureState). The default is written and fsynced beside the final path and published with an exclusive hard link, which is atomic at the
// destination, so concurrent SessionStart hooks race safely and an existing file, valid or corrupt, is never touched. Where
// hard links are unavailable (EPERM, ENOTSUP or EXDEV: FAT32, some shares) the default is written and fsynced to a second temp
// file and published by a rename that refuses to replace (publishWithoutLink), so a failed write leaves nothing at the final
// path. The oracle creates the final path and then writes into it, which leaves an empty or truncated state file when the write
// fails (a known defect, fixed here by decision). After a successful hard link the sessions directory is fsynced; a directory
// sync error is returned with created true and the published file retained, unless the temp cleanup overrides the result.
func EnsureState(cwd, sessionID string) (bool, error) {
	return ensureState(cwd, sessionID, time.Now, os.Link)
}

func ensureState(cwd, sessionID string, now func() time.Time, link func(existing, created string) error) (bool, error) {
	return ensureStateWith(cwd, sessionID, now, link, nil)
}

// ensureStep names a step of the fallback publication, so a test can fail it.
type ensureStep int

const (
	stepStat    ensureStep = iota // before the identity of a file the fallback created is read
	stepWrite                     // before the data is written to that file
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

// ensureStateWith has a fallback-step hook and an optional sync operation for the primary file and directory.
func ensureStateWith(cwd, sessionID string, now func() time.Time, link func(existing, created string) error, fail func(ensureStep, string) error, syncFile ...func(*os.File) error) (created bool, err error) {
	sync := (*os.File).Sync
	if len(syncFile) > 0 {
		sync = syncFile[0]
	}
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
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return false, err
	}
	if _, err = f.Write(body); err == nil {
		err = sync(f)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return false, err
	}
	switch err = link(tmp, finalPath); {
	case err == nil:
		dir, err := os.Open(filepath.Dir(finalPath))
		if err != nil {
			return true, err
		}
		return true, errors.Join(sync(dir), dir.Close())
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
	_ = removeFile(tmp) // the staged copy is of no use now, and the in-place file needs the room it holds: near a quota three copies would not fit where the oracle's two did
	switch err = writeNew(finalPath, data, fail); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	}
	return false, err
}

// PublishedError reports that a state write published the new state at the final path and then failed a step that runs after the
// rename: opening or fsyncing the directory that holds it. The state is visible to every reader, so the error is not a reason to
// roll it back, and a caller that has a reconciling step after a failed write (clearing an attempt counter, for example) must run
// it anyway. Err is the underlying failure and Unwrap exposes it, so errors.Is keeps answering for the cause.
type PublishedError struct{ Err error }

func (e *PublishedError) Error() string { return e.Err.Error() }

// Unwrap exposes the failure the publication wrapped, so errors.Is and errors.As reach it.
func (e *PublishedError) Unwrap() error { return e.Err }

// Published reports whether err is, or wraps, a PublishedError: the state at the final path was published before the write failed.
// It is false for an error a write returned before the rename, where nothing was published.
func Published(err error) bool {
	var target *PublishedError
	return errors.As(err, &target)
}

// WriteState publishes next as the session's state file: written to a temp file beside it and renamed over it, so a reader sees
// the old file or the new one whole (writeState). The temp file is fsynced before the rename, and the directory afterward;
// a directory sync error is returned as a PublishedError after publication, without removing the new state. This is not a
// serialised read-modify-write: a caller that must not lose a concurrent update re-reads inside WithSessionLock. The tracker is
// capped on the way out and updatedAt stamped.
func WriteState(cwd string, next State) error {
	return writeState(cwd, next, time.Now(), crwdir.Rename)
}

func writeState(cwd string, next State, now time.Time, rename func(tmp, finalPath string) error, syncFile ...func(*os.File) error) (err error) {
	sync := (*os.File).Sync
	if len(syncFile) > 0 {
		sync = syncFile[0]
	}
	if err = makeSessionsDir(cwd); err != nil {
		return err
	}
	finalPath := StatePath(cwd, next.SessionID)
	// CRW-1005 (decision D1): a file that holds an unpaired surrogate escape is refused, not rewritten. The reader reads the
	// escape as U+FFFD, so a rewrite would replace stored text the CXC original keeps; the refusal leaves the file as it is.
	// A file that cannot be read cannot be shown clean, so only a missing file is a new write; any other read error returns
	// before a temp file is staged. A directory at the path (EISDIR) holds no escape and the rename onto it fails by itself,
	// so it falls through to that failure, which the oracle's write also reports.
	switch raw, readErr := readExisting(finalPath); {
	case readErr == nil:
		if rewriteLosslessUnpaired(raw) {
			return fmt.Errorf("refusing to rewrite %s: it holds an unpaired surrogate escape that a rewrite would replace with U+FFFD", finalPath)
		}
	case errors.Is(readErr, errNotRegular):
		return fmt.Errorf("refusing to rewrite %s: %w", finalPath, readErr)
	case !errors.Is(readErr, fs.ErrNotExist) && !errors.Is(readErr, syscall.EISDIR):
		return fmt.Errorf("refusing to rewrite %s: it cannot be checked for an unpaired surrogate escape: %w", finalPath, readErr)
	}
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
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err = f.Write(body); err == nil {
		err = sync(f)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = rename(tmp, finalPath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(finalPath))
	if err != nil {
		return &PublishedError{Err: err}
	}
	if err := errors.Join(sync(dir), dir.Close()); err != nil {
		return &PublishedError{Err: err}
	}
	return nil
}

// errNotRegular is the answer for a state path that holds something other than a regular file (a FIFO, a device, a socket, or a
// link to one).
var errNotRegular = errors.New("the path is not a regular file")

// classifyBeforeOpen is the check that runs before the open: lstat the path and, for a symbolic link, stat what it points at, so
// a FIFO, device or socket (or a link to one) is refused without ever being opened. Opening a device has effects of its own and
// opening a FIFO releases a peer that waits on it. A dangling link reports fs.ErrNotExist, as the open would.
func classifyBeforeOpen(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if fi, err = os.Stat(path); err != nil {
			return err
		}
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return errNotRegular
	}
	return nil
}

// readExisting reads the file at path for the lone-surrogate check without ever blocking on it. The path is classified before it
// is opened (classifyBeforeOpen); the open is then non-blocking, so a FIFO swapped in after the classification returns at once
// instead of waiting for a writer, and the descriptor is inspected again before any read: only a regular file is read. A
// directory returns EISDIR (the rename onto it fails by itself later), a missing path fs.ErrNotExist, and a FIFO, device or
// socket (opening a socket fails with ENXIO) errNotRegular. A symbolic link to a regular file is followed, as the rename's
// replacement of it was never refused.
func readExisting(path string) ([]byte, error) {
	if err := classifyBeforeOpen(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return nil, errNotRegular
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case fi.IsDir():
		return nil, syscall.EISDIR
	case !fi.Mode().IsRegular():
		return nil, errNotRegular
	}
	return io.ReadAll(f)
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
	made, err := f.Stat() // the file this call created
	if err == nil {
		err = failAt(fail, stepStat, path)
	}
	if err != nil { // its identity is unknown, so only the instant since the create vouches for the path: take it back and say so
		err = errors.Join(err, f.Close())
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return err
	}
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
