package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// record is the installed record crw writes beside the executable: the tool's name, its version,
// the archive's sha256 and when the archive was fetched. It is what makes a directory an install.
type record struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	FetchedAt string `json:"fetched_at"`
}

// installedPath answers the installed executable's path, or not_installed. A directory without a
// record that names this pin, or without the executable, is not an install.
func installedPath(pin Pin, toolsRoot string) (string, error) {
	path := pin.ExecutablePath(toolsRoot)
	raw, err := os.ReadFile(pin.RecordPath(toolsRoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s", pin.Name, pin.Version, toolsRoot)
		}
		return "", hostFail("the record %s could not be read: %v", pin.RecordPath(toolsRoot), err)
	}
	var found record
	if err := json.Unmarshal(raw, &found); err != nil {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: %s is not readable", pin.Name, pin.Version, toolsRoot, pin.RecordPath(toolsRoot))
	}
	if found.Name != pin.Name || found.Version != pin.Version || found.SHA256 != pin.SHA256 {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: the record does not name this pin", pin.Name, pin.Version, toolsRoot)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: the executable is missing", pin.Name, pin.Version, toolsRoot)
	}
	// An executable that lost its execute bits is not usable as the pinned tool, so it is not an
	// install: install repairs it rather than handing back a path that cannot be run.
	if info.Mode().Perm()&0o111 == 0 {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: the executable is not executable", pin.Name, pin.Version, toolsRoot)
	}
	return path, nil
}

// install makes the pin installed under toolsRoot, fetching nothing when it already is. The archive
// is downloaded to a file under tempRoot, verified and unpacked from that one file, and only then is
// anything written under the tools root, so a digest that is not the pin's leaves the tools root
// untouched.
func install(ctx context.Context, pin Pin, seams *Seams, toolsRoot, tempRoot string) (string, error) {
	// The platform rule is checked before the installed-path fast path, so a tools root shared or
	// restored from another host never hands back a binary this host cannot run.
	if err := platformRefusal(pin, seams); err != nil {
		return "", err
	}
	if path, err := installedPath(pin, toolsRoot); err == nil {
		return path, nil
	}
	// fetch removes the download directory and every directory it created under tempRoot before it
	// returns, and answers the executable's body or the refusal; nothing under the tools root is
	// written before the digest matches.
	body, err := fetch(ctx, pin, seams, tempRoot)
	if err != nil {
		return "", err
	}
	// The first interrupt must be honoured before anything durable happens: the member is in memory
	// and nothing has been written under the tools root yet.
	if err := ctx.Err(); err != nil {
		return "", hostFail("the install was cancelled: %v", err)
	}
	return stage(ctx, pin, seams, toolsRoot, body)
}

// fetch downloads the pin's archive into a fresh directory under tempRoot, verifies its digest and returns the
// executable member read out of it. Each directory this call makes under tempRoot is made and removed while the
// lock on the directory that holds it is held, so an install that shares a missing root cannot remove a directory
// another install is using. The download runs with no lock held. Every directory this call creates is removed
// before fetch returns, whatever the answer. A parent that another install still uses stays in place with the
// directories inside it, which is accepted: this call removes only the directories it made, and only when empty.
func fetch(ctx context.Context, pin Pin, seams *Seams, tempRoot string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, hostFail("the install was cancelled: %v", err)
	}
	created, dir, err := makeDownloadDir(pin, seams, tempRoot)
	if err != nil {
		return nil, err
	}
	body, downloadErr := verifyDownload(ctx, pin, seams, dir)
	// The download directory is removed before the directories this call made, so the directory that holds it is
	// empty when it is tried. A download directory that cannot be removed keeps its parent, which is then left.
	_ = os.RemoveAll(dir)
	removeCreated(created)
	if downloadErr != nil {
		return nil, downloadErr
	}
	return body, nil
}

// makeDownloadDir makes the download root under tempRoot and a fresh download directory in it. It answers the
// directories this call made and the download directory. A failure removes what this call made.
func makeDownloadDir(pin Pin, seams *Seams, tempRoot string) ([]createRootRecord, string, error) {
	created, err := createRoot(tempRoot)
	if err != nil {
		return nil, "", hostFail("the download root %s could not be made: %v", tempRoot, err)
	}
	dir, err := tempDirUnderLock(pin, seams, tempRoot)
	if errors.Is(err, fs.ErrNotExist) {
		// A concurrent install removed the download root between this call making it and here. The root is made
		// again, recording only what this call makes, and the directory is tried once more.
		again, mkErr := createRoot(tempRoot)
		if mkErr != nil {
			removeCreated(created)
			return nil, "", hostFail("the download root %s could not be made: %v", tempRoot, mkErr)
		}
		created = append(created, again...)
		dir, err = tempDirUnderLock(pin, seams, tempRoot)
	}
	if err != nil {
		removeCreated(created)
		return nil, "", hostFail("the download directory under %s could not be made: %v", tempRoot, err)
	}
	return created, dir, nil
}

// verifyDownload downloads the pin's archive into dir, checks its digest and returns the executable member. The
// archive is read exactly once, so the bytes that reach the hasher are the bytes the unpack reads.
func verifyDownload(ctx context.Context, pin Pin, seams *Seams, dir string) ([]byte, error) {
	archive, err := download(ctx, pin, seams, dir)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(archive)
	if err != nil {
		return nil, hostFail("the downloaded archive %s could not be read: %v", archive, err)
	}
	defer file.Close()
	hasher := sha256.New()
	tee := io.TeeReader(file, hasher)
	body, memberErr := member(tee, pin.Executable)
	// The rest of the file is drained into the hash even when the member could not be read, so the
	// digest always covers the whole archive. A malformed body is usually the wrong archive as
	// well, and the pinned refusal names what went wrong more precisely than the extraction error,
	// so the digest is compared first and the extraction error is reported only for a matching
	// digest. The file is still read exactly once.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, hostFail("the downloaded archive %s could not be read: %v", archive, err)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest != pin.SHA256 {
		return nil, refuse("digest_mismatch", digestMismatchExit, "the archive %s has sha256 %s, not the pinned %s", pin.Archive, digest, pin.SHA256)
	}
	if memberErr != nil {
		return nil, memberErr
	}
	return body, nil
}

// tempRootLockAttempts bounds how often a parent is opened and locked again when the path that names it changes
// while the process waits for the lock.
const tempRootLockAttempts = 8

// ancestorOf answers the existing directory a component is made in: its parent, or the root or the current
// directory when the spelling has no parent part.
func ancestorOf(component string) string {
	if parent := componentParent(component); parent != "" {
		return parent
	}
	if strings.HasPrefix(component, string(os.PathSeparator)) {
		return string(os.PathSeparator)
	}
	return "."
}

// lockOpenDir takes the exclusive flock on the open directory dir and answers whether the lock is held. A
// filesystem that refuses flock answers false and no error, and the caller proceeds unlocked, which is how the
// walk ran before the lock existed. Any other error is a host failure.
func lockOpenDir(dir *os.File) (bool, error) {
	for {
		err := unix.Flock(int(dir.Fd()), unix.LOCK_EX)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case tempRootLockUnsupported(err):
			return false, nil
		default:
			return false, hostFail("the lock on %s could not be taken: %v", dir.Name(), err)
		}
	}
}

// unlockOpenDir releases the lock lockOpenDir took. The descriptor stays open for the caller that keeps it.
func unlockOpenDir(dir *os.File) {
	_ = unix.Flock(int(dir.Fd()), unix.LOCK_UN)
}

// openLockedDir opens the directory that path names and takes its lock, answering the descriptor and whether the
// lock is held. The descriptor is compared with the path after the lock is granted, because a path that changed
// while the process waited no longer names the directory that was locked; that descriptor is closed and the path is
// opened again, at most tempRootLockAttempts times. A directory this process cannot open answers no descriptor and
// no error, and the caller proceeds without a parent handle, as before the lock existed.
func openLockedDir(path string) (*os.File, bool, error) {
	for attempt := 0; attempt < tempRootLockAttempts; attempt++ {
		dir, err := os.Open(path)
		if err != nil {
			return nil, false, nil
		}
		locked, err := lockOpenDir(dir)
		if err != nil {
			_ = dir.Close()
			return nil, false, err
		}
		held, heldErr := dir.Stat()
		named, namedErr := os.Stat(path)
		if heldErr == nil && namedErr == nil && os.SameFile(held, named) {
			return dir, locked, nil
		}
		_ = dir.Close()
	}
	return nil, false, hostFail("%s changed on each of %d attempts to take its lock", path, tempRootLockAttempts)
}

// underParentLock runs fn while the directory that holds path is locked, so an entry fn makes or removes at path is
// not changed by another install that holds the same lock. Closing the directory when fn returns releases the lock.
// A parent this process cannot open runs fn without a lock, as before.
func underParentLock(path string, fn func() error) error {
	dir, _, err := openLockedDir(ancestorOf(path))
	if err != nil {
		return err
	}
	if dir == nil {
		return fn()
	}
	defer dir.Close()
	return fn()
}

// tempDirUnderLock makes a download directory under tempRoot while the lock on the directory that holds tempRoot is
// held, so the removal of tempRoot by an install that made it cannot run between this call's check and its mkdir.
func tempDirUnderLock(pin Pin, seams *Seams, tempRoot string) (string, error) {
	var dir string
	err := underParentLock(tempRoot, func() error {
		var mkErr error
		dir, mkErr = seams.mkdirTemp(tempRoot, pin.Name+"-")
		return mkErr
	})
	return dir, err
}

// tempRootLockUnsupported reports whether a flock error means the filesystem cannot take the lock at all, and not
// that another holder has it.
func tempRootLockUnsupported(err error) bool {
	return errors.Is(err, unix.ENOLCK) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL)
}

// download streams the pin's archive into a file named pin.Archive inside dir and answers that
// file's path. The file is created 0600 and a body larger than maxArchiveBytes is a host failure
// that removes it, so a download that is refused leaves nothing behind.
func download(ctx context.Context, pin Pin, seams *Seams, dir string) (string, error) {
	run, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	url := seams.urlBase() + "/v" + pin.Version + "/" + pin.Archive
	request, err := http.NewRequestWithContext(run, http.MethodGet, url, nil)
	if err != nil {
		return "", hostFail("%s could not be requested: %v", url, err)
	}
	response, err := seams.client().Do(request)
	if err != nil {
		return "", hostFail("%s could not be fetched: %v", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", hostFail("%s answered %s", url, response.Status)
	}
	path := crwconfig.JoinRoot(dir, pin.Archive)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", hostFail("the archive %s could not be written: %v", path, err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, maxArchiveBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", hostFail("%s could not be read: %v", url, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", hostFail("the archive %s could not be written: %v", path, closeErr)
	}
	if written > maxArchiveBytes {
		_ = os.Remove(path)
		return "", hostFail("%s is larger than %d bytes", url, maxArchiveBytes)
	}
	return path, nil
}

// createRootRecord is one directory createRoot made. Its identity is the device and inode read when the
// directory was made, and every later check compares identities, never names alone. A directory made through
// an open parent handle is identified and removed through that handle with fstatat and unlinkat, so the cleanup
// never opens the directory and works when the mode of the directory denies reading it. The directory is also
// held open where the mode lets this process open it, so its inode cannot be reused while the record stands.
type createRootRecord struct {
	path   string
	parent string
	name   string
	id     createRootID
	dir    *os.File
	pdir   *os.File
}

// close releases the handles a record holds. A record is closed once, when it leaves the walk's list.
func (made createRootRecord) close() {
	if made.dir != nil {
		_ = made.dir.Close()
	}
	if made.pdir != nil {
		_ = made.pdir.Close()
	}
}

// componentParent is component's parent as text: everything before the last separator, with nothing
// cleaned. filepath.Dir cannot be used here, because it cleans the result and a clean would collapse
// a ".." that has to stay in the path: the parent of "base/link/../p/q" is "base/link/../p" and not
// the different directory "base/p" a clean names, and resolving the cleaned form would answer a
// location that is not the one the mkdir acts on.
func componentParent(component string) string {
	trimmed := strings.TrimRight(component, string(os.PathSeparator))
	if cut := strings.LastIndex(trimmed, string(os.PathSeparator)); cut > 0 {
		return trimmed[:cut]
	}
	return ""
}

// parentLocation answers the location the kernel resolves component's parent to, or "" when it
// cannot be resolved. It is read before the component's mkdir, while the spelling is known to reach
// the parent, so the location keeps reaching the component after a segment above the parent -- a
// "..", for instance -- vanishes, which is the race this walk handles.
func parentLocation(component string) string {
	parent := componentParent(component)
	if parent == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return ""
	}
	return resolved
}

// componentName answers the leaf name the component's mkdir uses under its parent: the text after the
// last separator, with a trailing separator dropped. It is "" for a component with no separator.
func componentName(component string) string {
	trimmed := strings.TrimRight(component, string(os.PathSeparator))
	cut := strings.LastIndex(trimmed, string(os.PathSeparator))
	if cut < 0 {
		return ""
	}
	return trimmed[cut+1:]
}

// createRootID is the identity of a directory on this host: its device and inode.
type createRootID struct {
	dev, ino uint64
}

// known reports whether an identity was read. An identity that was not read is the zero value.
func (id createRootID) known() bool {
	return id.ino != 0
}

// idOfStat answers the identity of a stat result.
func idOfStat(st *unix.Stat_t) createRootID {
	return createRootID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// lstatID answers the identity of the entry at path, without following a symbolic link.
func lstatID(path string) (createRootID, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return createRootID{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	return idOfStat(&st), nil
}

// entryID answers the identity of the directory that name holds under the open parent pdir. The entry is read
// through the parent handle and not opened, so a directory whose mode denies reading is still identified. A
// symbolic link is not followed, and an entry that is not a directory is an error.
func entryID(pdir *os.File, name string) (createRootID, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(pdir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return createRootID{}, &fs.PathError{Op: "fstatat", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return createRootID{}, &fs.PathError{Op: "fstatat", Path: name, Err: unix.ENOTDIR}
	}
	return idOfStat(&st), nil
}

// openPinned opens the directory that name holds under pdir and returns the handle, which keeps the inode
// allocated while a record stands. It answers nil when the mode does not let this process open the directory;
// the record then holds the identity alone.
func openPinned(pdir *os.File, name string) *os.File {
	fd, err := unix.Openat(int(pdir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(fd), name)
}

// createRootReach answers the names a record made without a parent handle can be reached by, the caller's
// spelling first and the resolved parent location second.
func createRootReach(made createRootRecord) []string {
	if made.parent == "" || made.name == "" {
		return []string{made.path}
	}
	return []string{made.path, crwconfig.JoinRoot(made.parent, made.name)}
}

// createRootMatch answers the name at which a record's directory is found, when the record reached it by a
// name, together with the identity read there, and whether that is still the directory the record made. A
// record held through a parent handle answers "" for the name, because the handle, not a name, reaches it.
// Identity, not the name, decides: a directory another install made at the same place is a different
// directory.
func createRootMatch(made createRootRecord) (string, createRootID, bool) {
	if made.pdir != nil {
		id, err := entryID(made.pdir, made.name)
		return "", id, err == nil && id == made.id
	}
	for _, reach := range createRootReach(made) {
		if id, err := lstatID(reach); err == nil && id == made.id {
			return reach, id, true
		}
	}
	return "", createRootID{}, false
}

// createRootComponent makes component's directory and answers the record of the directory the mkdir made. The
// parent is opened and locked by openLockedDir, and the mkdir runs through that open handle on the parent
// location the kernel resolved, so the entry it acts on is the one in the directory that handle pins. When no
// parent handle can be opened -- a relative root, a parent that no longer resolves, or a parent this process may
// not read -- the mkdir runs through the caller's spelling without a lock, and the record keeps the identity read
// by name.
//
// On EEXIST the record describes the directory standing there, which is not this call's to make unless the
// caller finds it is the one recorded. When no identity could be read the record has none, and the caller
// keeps the record it already holds rather than deciding ownership from nothing.
func createRootComponent(component string) (createRootRecord, error) {
	name := componentName(component)
	parent := parentLocation(component)
	if parent == "" || name == "" || name == "." || name == ".." {
		return createRootSpelled(component, "", "")
	}
	pdir, locked, err := openLockedDir(parent)
	if err != nil {
		return createRootRecord{}, err
	}
	if pdir == nil {
		return createRootSpelled(component, parent, name)
	}
	return createRootUnderParent(component, parent, name, pdir, locked)
}

// createRootSpelled makes component through the caller's spelling and reads the identity of what stands
// there by name.
func createRootSpelled(component, parent, name string) (createRootRecord, error) {
	made := createRootRecord{path: component, parent: parent, name: name}
	err := os.Mkdir(component, 0o755)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return createRootRecord{}, err
	}
	id, readErr := lstatID(component)
	if readErr != nil {
		if err != nil {
			return createRootRecord{}, err
		}
		return createRootRecord{}, readErr
	}
	made.id = id
	return made, err
}

// createRootUnderParent makes name under the open parent handle pdir and records the directory it finds there.
// The handle is taken ownership of: it is closed here on every path that does not return it in the record. When
// locked is set the lock taken on pdir is held for the mkdir and released on return. The identity is read through
// the handle, so a directory whose mode denies reading is still recorded. Before the record is returned the parent
// is checked against the name the caller spelled: when the spelling no longer reaches the directory the handle
// pins, the entry is in a place the caller did not ask for, so it is removed when this call made it and the walk
// recomputes.
func createRootUnderParent(component, parent, name string, pdir *os.File, locked bool) (createRootRecord, error) {
	if locked {
		defer unlockOpenDir(pdir)
	}
	if createRootAfterParentOpened != nil {
		createRootAfterParentOpened(component)
	}
	mkErr := unix.Mkdirat(int(pdir.Fd()), name, 0o755)
	if mkErr != nil && !errors.Is(mkErr, fs.ErrExist) {
		_ = pdir.Close()
		return createRootRecord{}, &fs.PathError{Op: "mkdirat", Path: component, Err: mkErr}
	}
	if createRootAfterMkdir != nil {
		createRootAfterMkdir(component)
	}
	id, err := entryID(pdir, name)
	if err != nil {
		if mkErr == nil {
			// The entry this call made is not a directory that can be identified. unlinkat with AT_REMOVEDIR
			// removes it only while it is an empty directory, so a file a peer put in its place stays, and the
			// walk recomputes. The handle is used before it is closed.
			_ = unix.Unlinkat(int(pdir.Fd()), name, unix.AT_REMOVEDIR)
			_ = pdir.Close()
			return createRootRecord{}, fs.ErrNotExist
		}
		_ = pdir.Close()
		return createRootRecord{path: component, parent: parent, name: name}, mkErr
	}
	if !parentStillNamed(component, pdir) {
		if mkErr == nil {
			unlinkHeldDir(pdir, name, id)
		}
		_ = pdir.Close()
		return createRootRecord{}, fs.ErrNotExist
	}
	return createRootRecord{path: component, parent: parent, name: name, id: id, dir: openPinned(pdir, name), pdir: pdir}, mkErr
}

// parentStillNamed reports whether the caller's spelling of component's parent still reaches the directory
// the parent handle pins. It is checked after the entry was made, so an entry made in a directory the
// spelling no longer names is caught before it is recorded.
func parentStillNamed(component string, pdir *os.File) bool {
	held, err := pdir.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(componentParent(component))
	return err == nil && os.SameFile(held, named)
}

// unlinkHeldDir removes the entry name under a parent handle when it is still the directory with identity id. The
// parent is locked for the check and the removal, so an install that holds the same lock cannot make or remove
// entries under it meanwhile. The identity is read through the handle and the removal is unlinkat with
// AT_REMOVEDIR, which refuses a file and a directory with entries, so only an empty directory can be removed here.
// No child is opened, so a directory whose mode denies reading or searching is removed too. When the lock cannot be
// taken the directory is left in place.
func unlinkHeldDir(pdir *os.File, name string, id createRootID) {
	locked, err := lockOpenDir(pdir)
	if err != nil {
		return
	}
	if locked {
		defer unlockOpenDir(pdir)
	}
	if current, err := entryID(pdir, name); err == nil && current == id {
		_ = unix.Unlinkat(int(pdir.Fd()), name, unix.AT_REMOVEDIR)
	}
}

// dropGone drops the record of a component the scan found missing when the component is really gone. A
// failed read of the component is evidence of absence only while its parent is reachable: a component
// below a segment that has vanished is unreachable rather than absent, and its record is kept so a
// directory this call made is still removed.
func dropGone(created []createRootRecord, component string) []createRootRecord {
	for _, made := range created {
		if made.path != component {
			continue
		}
		if _, _, ok := createRootMatch(made); ok {
			return created
		}
	}
	if parentLocation(component) == "" {
		return created
	}
	if _, err := os.Lstat(component); err == nil {
		return created
	}
	return dropCreated(created, component)
}

// dropGoneComponents drops the record of every component the scan found missing that is really gone. The
// scan reports the components missing outermost first, and each is decided on its own.
func dropGoneComponents(created []createRootRecord, components []string) []createRootRecord {
	for _, component := range components {
		created = dropGone(created, component)
	}
	return created
}

// createRoot makes dir and every missing ancestor of it, recording only the components this call created: a
// mkdir that succeeds is this call's and is recorded with the identity of the directory that mkdir made, and
// one that answers EEXIST is this call's only when the identity standing there is the one this call recorded
// at that path. The list is outermost first, the order removeCreated walks backwards. The path is walked by
// its raw spelling, the way os.MkdirAll walks it, so a configured root that mixes a symbolic link and ".." is
// not rewritten. Lstat is used so a dangling symbolic link counts as existing rather than as a directory this
// call may make and remove.
//
// A concurrent install sharing this root removes the directories it made as it finishes, and it can remove an
// ancestor this call's scan found present. The mkdir of a component then answers ENOENT; the missing
// components are recomputed and the walk continues, at most createRootMkdirRounds times, so a normal overlap
// does not become a host failure. What this call made stays recorded either way.
func createRoot(dir string) ([]createRootRecord, error) {
	// A root that keeps vanishing under this call must not spin: after this many recomputes the
	// error is returned rather than the walk being tried again.
	const createRootMkdirRounds = 3
	var created []createRootRecord
	for round := 0; ; round++ {
		// absent carries this round's not-exist error out of the component walk, so the walk is
		// left and the components are recomputed instead of the error being returned at once.
		var absent error
		components := rootComponents(dir)
		if len(components) > 0 {
			created = dropGoneComponents(created, components)
		}
		for _, component := range components {
			if createRootBeforeMkdir != nil {
				createRootBeforeMkdir(component)
			}
			made, err := createRootComponent(component)
			switch {
			case err == nil:
				// The mkdir made the component, so the identity read for it is this call's to record.
				created = recordCreated(created, made)
			case errors.Is(err, fs.ErrExist):
				// The path is there but the scan found it missing. When the identity is the one this call
				// recorded for that path, the directory is still the one this call made; otherwise another
				// install made its own there, which is not this call's to remove.
				switch {
				case !made.id.known():
					// No identity could be read, so nothing proves the directory changed hands. The record
					// this call already holds is left alone, and removeCreated verifies it again before
					// removing.
				case sameRecordedDirectory(created, component, made.id):
					created = recordCreated(created, made)
				default:
					made.close()
					created = dropCreated(created, component)
				}
			case errors.Is(err, fs.ErrNotExist):
				// An ancestor the scan found was removed before this mkdir; the missing components
				// are recomputed and the walk continues.
				absent = err
			default:
				removeCreated(created)
				return nil, err
			}
			if absent != nil {
				break
			}
		}
		if absent == nil {
			return created, nil
		}
		if round >= createRootMkdirRounds {
			// The root keeps vanishing; what this call made is not left for the next run to trip
			// over, and the answer is the error the last mkdir gave.
			removeCreated(created)
			return nil, absent
		}
	}
}

// recordCreated records the directory a component's mkdir made as this call's, with the identity read for
// it. The record is keyed by identity and not by the spelling alone: one spelled component can name two
// directories over the life of a walk when a parent is retargeted between rounds, and both are this call's
// while they stand. A record for the path is kept beside the new one while its directory is still standing,
// and closed once neither name reaches it. The list is kept outermost first, the order removeCreated walks
// backwards.
func recordCreated(created []createRootRecord, made createRootRecord) []createRootRecord {
	kept := make([]createRootRecord, 0, len(created)+1)
	recorded := false
	for _, held := range created {
		if held.path != made.path {
			kept = append(kept, held)
			continue
		}
		if held.id == made.id {
			// The same directory is recorded again: the new record takes its place, and the handles
			// it held are released.
			held.close()
			if !recorded {
				kept = append(kept, made)
				recorded = true
			}
			continue
		}
		if _, _, ok := createRootMatch(held); ok {
			kept = append(kept, held)
		} else {
			held.close()
		}
	}
	if !recorded {
		kept = append(kept, made)
	}
	slices.SortStableFunc(kept, func(a, b createRootRecord) int {
		return strings.Count(a.path, string(os.PathSeparator)) - strings.Count(b.path, string(os.PathSeparator))
	})
	return kept
}

// sameRecordedDirectory reports whether the directory standing at path is one this call recorded there.
// Identity, not the name, decides: a directory another install made at the same path is a different
// directory. One spelling can hold more than one record, so every record for the path is compared.
func sameRecordedDirectory(created []createRootRecord, path string, id createRootID) bool {
	for _, made := range created {
		if made.path == path && made.id == id {
			return true
		}
	}
	return false
}

// dropCreated forgets the records for path whose directory no longer stands where it was made, because a
// directory that is not there is not this call's to remove. A record whose directory is still standing is
// kept, and the handles of a forgotten record are released.
func dropCreated(created []createRootRecord, path string) []createRootRecord {
	var kept []createRootRecord
	for _, made := range created {
		if made.path != path {
			kept = append(kept, made)
			continue
		}
		if _, _, ok := createRootMatch(made); ok {
			kept = append(kept, made)
			continue
		}
		made.close()
	}
	return kept
}

// createRootBeforeMkdir is a test seam called immediately before each mkdir createRoot performs, so a test
// can remove an ancestor at exactly the moment a concurrent install would instead of relying on timing. nil
// is the production value.
var createRootBeforeMkdir func(path string)

// createRootAfterParentOpened is a test seam called after a component's parent location has been opened and
// before its entry is made, so a test can move that parent at exactly the moment a concurrent install would.
// nil is the production value.
var createRootAfterParentOpened func(component string)

// createRootAfterMkdir is a test seam called after an entry is made through a parent handle and before the
// entry is opened, so a test can replace it at exactly the moment a concurrent install would. nil is the
// production value.
var createRootAfterMkdir func(component string)

// rootComponents lists the components of dir that do not exist yet, outermost first, stopping at
// the first ancestor that does exist. A dangling symbolic link counts as existing, so it is never
// recorded as a directory this call made.
func rootComponents(dir string) []string {
	var missing []string
	for path := dir; path != ""; {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		missing = append(missing, path)
		trimmed := strings.TrimRight(path, string(os.PathSeparator))
		cut := strings.LastIndex(trimmed, string(os.PathSeparator))
		if cut < 0 {
			break
		}
		path = trimmed[:cut]
	}
	// missing holds the innermost component first; reverse it so the outermost is created first.
	for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
		missing[i], missing[j] = missing[j], missing[i]
	}
	return missing
}

// removeCreated removes the directories this call created, innermost outward, and only while each one is still
// the directory this call made. A directory held through a parent handle is identified through that handle and
// removed with unlinkat(AT_REMOVEDIR), which never removes a file and never a directory with entries; a directory
// made by name is removed by name only after its identity matches. Every record's handles are released here,
// since removeCreated is the last use of a record.
func removeCreated(created []createRootRecord) {
	for i := len(created) - 1; i >= 0; i-- {
		made := created[i]
		if made.pdir != nil {
			unlinkHeldDir(made.pdir, made.name, made.id)
		} else if reach, _, ok := createRootMatch(made); ok {
			_ = unix.Rmdir(reach)
		}
		made.close()
	}
}

// member reads one regular file out of a tar.gz stream. A member the archive does not carry, a name
// that is not the one asked for, or an entry that is not a regular file is refused.
func member(reader io.Reader, name string) ([]byte, error) {
	compressed, err := gzip.NewReader(reader)
	if err != nil {
		return nil, hostFail("the archive is not a gzip stream: %v", err)
	}
	defer compressed.Close()
	// The decompressed volume is bounded as well as the compressed one: an archive whose expansion
	// is far larger than any pinned release is refused rather than decompressed in full, so a
	// mismatched or hostile body cannot turn a small download into unbounded work.
	entries := tar.NewReader(io.LimitReader(compressed, maxArchiveBytes+1))
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, hostFail("the archive could not be read: %v", err)
		}
		if filepath.Base(header.Name) != name {
			continue
		}
		if !header.FileInfo().Mode().IsRegular() {
			return nil, hostFail("the archive's %s is not a regular file", name)
		}
		body, err := io.ReadAll(io.LimitReader(entries, maxArchiveBytes+1))
		if err != nil {
			return nil, hostFail("the archive's %s could not be read: %v", name, err)
		}
		if int64(len(body)) > maxArchiveBytes {
			return nil, hostFail("the archive's %s is larger than %d bytes", name, maxArchiveBytes)
		}
		return body, nil
	}
	return nil, hostFail("the archive does not carry %s", name)
}

// stage writes the executable and its record into a fresh directory under toolsRoot and renames it
// into place, so the destination never holds a partial install: the rename is on one filesystem
// because the staging directory is inside the tools root. The check-and-replace that changes the
// install directory runs under the pin's lock, so two installs of one pin cannot interleave there. A
// failure after the staging directory exists removes it.
func stage(ctx context.Context, pin Pin, seams *Seams, toolsRoot string, body []byte) (string, error) {
	if err := os.MkdirAll(toolsRoot, 0o755); err != nil {
		return "", hostFail("the tools root %s could not be made: %v", toolsRoot, err)
	}
	dir, err := os.MkdirTemp(toolsRoot, stagingPrefix+pin.Name+"-")
	if err != nil {
		return "", hostFail("a staging directory under %s could not be made: %v", toolsRoot, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	executable := crwconfig.JoinRoot(dir, pin.Executable)
	if err := os.WriteFile(executable, body, executableMode); err != nil {
		return "", hostFail("the executable could not be written: %v", err)
	}
	// A restrictive umask can mask the execute bits os.WriteFile asked for, so the mode is set
	// explicitly: a tool the host cannot run is not installed.
	if err := os.Chmod(executable, executableMode); err != nil {
		return "", hostFail("the executable's mode could not be set: %v", err)
	}
	// The record is written after the executable, so a directory that holds a record holds a
	// complete install.
	record := record{
		Name: pin.Name, Version: pin.Version, SHA256: pin.SHA256,
		FetchedAt: seams.now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", hostFail("the record could not be written: %v", err)
	}
	if err := os.WriteFile(crwconfig.JoinRoot(dir, recordFile), append(data, '\n'), recordMode); err != nil {
		return "", hostFail("the record could not be written: %v", err)
	}
	// The prepared copy is complete; a test seam may order this install against another here,
	// before either takes the lock.
	if seams.BeforeRepair != nil {
		seams.BeforeRepair()
	}
	// keep is set only when this call's own directory was renamed into place. When another install
	// won the race, the deferred cleanup removes the staged copy rather than leaking it.
	moved, err := withLock(ctx, pin, toolsRoot, func() (bool, error) {
		// The install state is read again inside the lock: an install that reached the destination
		// between the fast path and here answers success, and this call's copy is dropped.
		if _, err := installedPath(pin, toolsRoot); err == nil {
			return false, nil
		}
		return replace(pin, toolsRoot, dir, pin.InstallDir(toolsRoot))
	})
	if err != nil {
		return "", err
	}
	keep = moved
	return pin.ExecutablePath(toolsRoot), nil
}

// withLock runs fn while this process holds the pin's exclusive lock, so the install state cannot
// change under fn's check-and-replace. The lock is taken on a file beside the install directory,
// after the tools root exists, and the file is left in place: removing it would let two callers lock
// different inodes. A download is never done under the lock. The wait is a blocking flock with no
// context of its own, so a first interrupt while another holder has the lock is honoured once the
// lock is acquired rather than interrupting the wait itself.
func withLock(ctx context.Context, pin Pin, toolsRoot string, fn func() (bool, error)) (bool, error) {
	path := pin.LockPath(toolsRoot)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, hostFail("the lock file %s could not be opened: %v", path, err)
	}
	defer file.Close()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return false, hostFail("the lock %s could not be taken: %v", path, err)
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	// The lock may have been taken after the caller's context was cancelled, so the durable effect
	// is refused here rather than performed and then abandoned.
	if err := ctx.Err(); err != nil {
		return false, hostFail("the install was cancelled: %v", err)
	}
	return fn()
}

// replace renames a staged directory into place. A destination that is already this pin's install
// is kept, because another install reached it first and its bytes are the pinned ones. A destination
// that is not (a stale record, a partial install) is removed and the rename retried once, so an
// install repairs what it finds without ever leaving a partial directory under the final name. A
// final rename that fails is followed by one more read of the install state, because a competing
// install may have completed the destination while this one was removing the stale copy.
func replace(pin Pin, toolsRoot, dir, target string) (bool, error) {
	if err := os.Rename(dir, target); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrExist) && !isNotEmpty(err) {
		return false, hostFail("the install directory %s could not be made: %v", target, err)
	}
	if _, err := installedPath(pin, toolsRoot); err == nil {
		// Another install reached the destination first with a complete, matching install; keep
		// it and let the caller's defer remove the staged copy.
		return false, nil
	}
	if err := os.RemoveAll(target); err != nil {
		return false, hostFail("the incomplete install at %s could not be removed: %v", target, err)
	}
	if err := os.Rename(dir, target); err != nil {
		// The rename can still fail if a competing install completed the destination after the
		// stale copy was removed; a complete install answers success either way.
		if _, readErr := installedPath(pin, toolsRoot); readErr == nil {
			return false, nil
		}
		return false, hostFail("the install directory %s could not be made: %v", target, err)
	}
	return true, nil
}

// isNotEmpty reports whether an error is a rename onto a non-empty directory.
func isNotEmpty(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return strings.Contains(linkErr.Err.Error(), "not empty")
	}
	return false
}
