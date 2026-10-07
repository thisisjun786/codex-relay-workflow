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

// fetch downloads the pin's archive into a fresh directory under tempRoot, verifies its digest and
// returns the executable member read out of it. The archive is streamed to a file and then read once
// through: the bytes that reach the hasher are the bytes the unpack reads, so nothing can change
// between the digest and the unpack. Every directory this call creates under tempRoot is removed
// before it returns, whatever the answer, and a directory that was already there is left alone.
func fetch(ctx context.Context, pin Pin, seams *Seams, tempRoot string) ([]byte, error) {
	// Only the components this call's own os.Mkdir creates are recorded, so a directory another
	// install made is never this call's to remove.
	created, err := createRoot(tempRoot)
	if err != nil {
		return nil, hostFail("the download root %s could not be made: %v", tempRoot, err)
	}
	// removeCreated is a closure so it reads the list after the retry below may extend it.
	defer func() { removeCreated(created) }()
	dir, err := seams.mkdirTemp(tempRoot, pin.Name+"-")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, hostFail("the download directory under %s could not be made: %v", tempRoot, err)
		}
		// A concurrent install removed the shared download root between this call creating it and
		// here. The root is made again, recording only what this call makes, and the directory is
		// tried once more, so the overlap does not turn a valid install into a host failure.
		again, mkErr := createRoot(tempRoot)
		if mkErr != nil {
			return nil, hostFail("the download root %s could not be made: %v", tempRoot, mkErr)
		}
		created = append(created, again...)
		if dir, err = seams.mkdirTemp(tempRoot, pin.Name+"-"); err != nil {
			return nil, hostFail("the download directory under %s could not be made: %v", tempRoot, err)
		}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	archive, err := download(ctx, pin, seams, dir)
	if err != nil {
		return nil, err
	}
	// The file is read exactly once. The bytes that reach the hasher are the bytes the unpack reads,
	// so the digest describes what is unpacked, and the archive never sits in memory whole.
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

// createRoot makes dir and every missing ancestor of it, recording only the components this call
// created: an os.Mkdir that succeeds is this call's, and one that answers EEXIST was already there
// or was made by another call in the same instant and is not this call's to remove. The list is
// outermost first, the order removeCreated walks backwards. The path is walked by its raw spelling,
// the way os.MkdirAll walks it, so a configured root that mixes a symbolic link and ".." is not
// rewritten. Lstat is used so a dangling symbolic link counts as existing rather than as a
// directory this call may make and remove.
//
// A concurrent install sharing this root removes the directories it made as it finishes, and it can
// remove an ancestor this call's scan found present. The mkdir of a component then answers ENOENT;
// the missing components are recomputed and the walk continues, at most createRootMkdirRounds
// times, so a normal overlap does not become a host failure. What this call made stays recorded
// either way, and a component that was made, removed and made again is the same directory, so it is
// recorded once.
func createRoot(dir string) ([]string, error) {
	// A root that keeps vanishing under this call must not spin: after this many recomputes the
	// error is returned rather than the walk being tried again.
	const createRootMkdirRounds = 3
	var created []string
	for round := 0; ; round++ {
		// absent carries this round's not-exist error out of the component walk, so the walk is
		// left and the components are recomputed instead of the error being returned at once.
		var absent error
		for _, component := range rootComponents(dir) {
			if createRootBeforeMkdir != nil {
				createRootBeforeMkdir(component)
			}
			err := os.Mkdir(component, 0o755)
			switch {
			case err == nil:
				// A component made, removed and made again is the same directory, so the list stays
				// the set of paths this call created, outermost first.
				if !slices.Contains(created, component) {
					created = append(created, component)
				}
			case errors.Is(err, fs.ErrExist):
				// Already there, or another call made it in the same instant; either way it is not
				// this call's to remove.
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

// createRootBeforeMkdir is a test seam called immediately before each os.Mkdir createRoot performs,
// so a test can remove an ancestor at exactly the moment a concurrent install would instead of
// relying on timing. nil is the production value.
var createRootBeforeMkdir func(path string)

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

// removeCreated removes the directories this call created, innermost outward. os.Remove refuses a
// directory that is not empty, which is exactly the wanted behaviour: a directory another call has
// since filled is left alone and is not this call's error to report.
func removeCreated(created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = os.Remove(created[i])
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
