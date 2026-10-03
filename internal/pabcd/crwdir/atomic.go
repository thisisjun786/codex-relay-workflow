package crwdir

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Rename publishes tmp as finalPath (atomic-write.ts renameWithRetry on a POSIX platform).
// rename(2) replaces the destination unconditionally, so the win32-only retry is not ported: a
// failed rename is returned after one attempt.
func Rename(tmp, finalPath string) error { return renameWith(os.Rename, tmp, finalPath) }

// renameWith takes the rename call as an argument so a test can count the attempts.
func renameWith(rename func(oldpath, newpath string) error, tmp, finalPath string) error {
	return rename(tmp, finalPath)
}

// publishStep names an action of publish, so a test can fail it.
type publishStep int

const (
	stepCreate publishStep = iota // the exclusive create of the temp file
	stepMode                      // right after the create, before the temp file takes the final file's mode
	stepWrite
	stepSync
	stepRename
)

// Publish writes data as finalPath through a temp file in the same directory: the data is written, fsynced and renamed over
// finalPath (Rename), so a concurrent reader, or a process that dies part-way, leaves the previous file or the new one whole and
// never a truncated one. On any failure the temp file is removed (a removal that fails is reported with the failure) and
// finalPath is untouched. This is what the oracle's in-place writeFileSync is not (CRW-427), and it keeps what that write gave:
//
//   - a new file gets mode 0666 under the umask, an existing file keeps its permission bits;
//   - a symlink at finalPath is followed, so the link stays and its target changes;
//   - a file that cannot be opened for writing (one the owner made read-only), a directory and any other file that is not
//     a regular file are refused, and so is a link that leads nowhere.
//
// The owner of an existing file and its hard links are not kept, there is no lock against a concurrent writer (the last
// rename wins), and the directory is not fsynced: after a power failure the rename may not have happened, and then the previous
// file is still there.
func Publish(finalPath string, data []byte) error { return publish(finalPath, data, nil) }

// publish takes a hook that is called just before each step and fails it by returning an error.
func publish(finalPath string, data []byte, fail func(publishStep) error) (err error) {
	at := func(step publishStep) error {
		if fail == nil {
			return nil
		}
		return fail(step)
	}
	target, existing, err := resolveTarget(finalPath)
	if err != nil {
		return err
	}
	// The temp file of an existing file starts private, so the new content is never readable through a broader mode than
	// the file it replaces had; it is widened to that mode while it is still empty.
	create := os.FileMode(0o666)
	if existing != nil {
		create = 0o600
	}
	tmp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+"."+rand.Text()+".tmp")
	if err = at(stepCreate); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, create)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close() // a second close after the one below only reports that it is closed
			if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr) // the temp file stays behind, and the failure says so
			}
		}
	}()
	if err = at(stepMode); err != nil {
		return err
	}
	if existing != nil {
		if err = f.Chmod(existing.Mode().Perm()); err != nil {
			return err
		}
	}
	if err = at(stepWrite); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = at(stepSync); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = at(stepRename); err != nil {
		return err
	}
	return Rename(tmp, target)
}

// resolveTarget is the path Publish replaces and what is there now: a nil info means a new file. A symlink is followed to
// the file it names. An existing file must be a regular file that this process can open for writing, which is where the
// oracle's in-place write would have failed; the open does not create or truncate.
func resolveTarget(path string) (target string, info fs.FileInfo, err error) {
	target = path
	if link, lerr := os.Lstat(path); lerr == nil && link.Mode()&fs.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(path); err != nil {
			return "", nil, err
		}
	}
	info, err = os.Stat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return target, nil, nil
	case err != nil:
		return "", nil, err
	case !info.Mode().IsRegular():
		return "", nil, fmt.Errorf("%s is not a regular file", target)
	}
	probe, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return "", nil, err
	}
	return target, info, probe.Close()
}
