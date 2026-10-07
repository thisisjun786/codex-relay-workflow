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
	"golang.org/x/sys/unix"
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

// makeSessionsDir is ensureCodexclawDir(cwd) then mkdirSync(sessionsDir, { recursive: true }), in that
// order, with one addition: the two directories are created through a descriptor walk that refuses a
// symbolic link at any step (CRW-646). Without it a linked .crw would send the sessions directory and
// the lock file outside the workspace, because the pathname creates follow the link; the plan write
// path already refuses such a root through its own walk, and this is the same judgement for the
// session side, taken at the moment of creation rather than from an earlier observation.
func makeSessionsDir(cwd string) error {
	dir, err := openSessionsDir(cwd)
	if err != nil {
		return err
	}
	return dir.Close()
}

// openSessionsDir returns cwd/.crw/sessions as an open descriptor, creating .crw and the sessions
// directory when they are missing. The state root is opened with O_NOFOLLOW|O_DIRECTORY, so a symbolic
// link (or a file) in its place is refused as ErrStateRootSymlink instead of followed, and both the
// sessions directory and the root's .gitignore are created RELATIVE to the held descriptors with
// Mkdirat/Openat. A caller that writes through the returned descriptor therefore cannot be redirected
// outside the workspace by a rename of a path component (CRW-646).
//
// Only the two components the session state owns are opened this way; cwd and its ancestors keep the
// ordinary pathname resolution every other caller uses, so a search-only ancestor (mode 0111) is still
// traversable and no new read permission is required. A .crw this call created publishes its .gitignore;
// nothing is ever removed by pathname, so a concurrent writer's replacement is never destroyed.
func openSessionsDir(cwd string) (*os.File, error) {
	rootPath := filepath.Join(cwd, crwdir.DirName)
	root, created, err := openOrCreateRootDir(cwd, rootPath)
	if err != nil {
		return nil, err
	}
	if created {
		// A .gitignore that could not be published is not a reason to delete a file: rmdir removes an
		// EMPTY DIRECTORY only and fails with ENOTDIR for a file or a symbolic link, so a concurrent
		// writer's replacement is never destroyed (this is exactly crwdir.ensureDir's own cleanup).
		if err := writeIgnoreAt(root, rootPath); err != nil {
			_ = root.Close()
			_ = syscall.Rmdir(rootPath)
			return nil, err
		}
	}
	sessionsPath := filepath.Join(rootPath, SessionsSubdir)
	sessions, _, err := ensureDirNoFollow(root, SessionsSubdir, sessionsPath)
	_ = root.Close()
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// openOrCreateRootDir opens cwd/.crw as a directory, creating it when it is absent. It reports whether
// this call created it. The create is a pathname Mkdir (which answers EEXIST for a file, a directory or
// a symbolic link in the way) and the open then refuses a link with O_NOFOLLOW, so a linked root is
// never followed. Resolving cwd by pathname needs only search permission on its ancestors, so a
// search-only ancestor keeps working exactly as it did before this change.
func openOrCreateRootDir(cwd, rootPath string) (*os.File, bool, error) {
	created := false
	if err := os.Mkdir(rootPath, 0o777); err == nil {
		created = true
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, false, err
	}
	dir, err := openDirNoFollow(nil, rootPath, rootPath)
	if err != nil {
		return nil, false, err
	}
	return dir, created, nil
}

// ensureDirNoFollow opens name under parent as a directory, creating it with Mkdirat relative to parent
// when it is absent. It reports whether this call created the directory. A symbolic link is refused as
// ErrStateRootSymlink and any other non-directory entry as an ordinary error.
func ensureDirNoFollow(parent *os.File, name, expected string) (*os.File, bool, error) {
	next, err := openDirNoFollow(parent, name, expected)
	if err == nil {
		return next, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	// created is true only when THIS call's Mkdirat made the directory: an EEXIST means another writer
	// made it, and the caller must not treat that directory as its own to clean up.
	created := false
	if mkErr := unix.Mkdirat(int(parent.Fd()), name, 0o777); mkErr == nil {
		created = true
	} else if !errors.Is(mkErr, fs.ErrExist) {
		return nil, false, &os.PathError{Op: "mkdir", Path: expected, Err: mkErr}
	}
	next, err = openDirNoFollow(parent, name, expected)
	if err != nil {
		return nil, false, err
	}
	return next, created, nil
}

// writeIgnoreAt publishes .crw/.gitignore through the descriptor of the .crw this call created, so the
// write cannot be redirected by a rename of the root path. A .gitignore a concurrent creator wrote first
// answers EEXIST and is kept.
func writeIgnoreAt(dir *os.File, dirPath string) error {
	fd, err := unix.Openat(int(dir.Fd()), ".gitignore", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: filepath.Join(dirPath, ".gitignore"), Err: err}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dirPath, ".gitignore"))
	_, err = f.WriteString(crwdir.GitignoreText)
	return errors.Join(err, f.Close())
}

// ErrStateRootSymlink reports a state root (cwd/.crw) or its sessions directory that is a symbolic
// link. A caller can tell this refusal apart from an ordinary IO failure and answer it as its own
// refusal rather than a generic error.
var ErrStateRootSymlink = errors.New("state path must not be a symlink")

// openDirNoFollow opens name under parent as a directory, refusing a symbolic link at that step. A
// link is reported as ErrStateRootSymlink so the caller can name the refusal.
func openDirNoFollow(parent *os.File, name, expected string) (*os.File, error) {
	var fd int
	var err error
	if parent == nil {
		fd, err = unix.Open(name, sessionsDirOpenFlags(), 0)
	} else {
		fd, err = unix.Openat(int(parent.Fd()), name, sessionsDirOpenFlags(), 0)
	}
	if err != nil {
		// The open answers ENOTDIR (or ELOOP) for a symbolic link, because O_NOFOLLOW stops at the link
		// and a link is not a directory. Ask the kernel whether the entry IS a link, so the refusal can
		// name it, and let any other failure stand as it is.
		var st unix.Stat_t
		if e := lstatAt(parent, name, &st); e == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, fmt.Errorf("%w: %s", ErrStateRootSymlink, expected)
		}
		return nil, &os.PathError{Op: "open", Path: expected, Err: err}
	}
	f := os.NewFile(uintptr(fd), expected)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// With O_PATH the open can succeed on a symbolic link (the descriptor names the link itself), so the
	// mode is checked here as well, not only on the error path above.
	if info.Mode()&os.ModeSymlink != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrStateRootSymlink, expected)
	}
	if !info.IsDir() {
		_ = f.Close()
		return nil, errors.New("state path is not a directory: " + expected)
	}
	return f, nil
}

// lstatAt is Fstatat for a named entry under parent, or Lstat for a pathname when parent is nil.
func lstatAt(parent *os.File, name string, st *unix.Stat_t) error {
	if parent == nil {
		return unix.Lstat(name, st)
	}
	return unix.Fstatat(int(parent.Fd()), name, st, unix.AT_SYMLINK_NOFOLLOW)
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
