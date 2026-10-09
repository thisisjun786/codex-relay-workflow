package crwdir

import (
	"context"
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
	stepDirSync // after the rename, the fsync of the directory that holds the new entry
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
// After the rename the directory that holds the file is fsynced (SyncDir), so that once Publish returns the entry survives a
// power failure and not only the file's data. A failure of that sync is returned as a *PublishedError although the rename has
// happened: the file is in place and must not be undone, but it is not known to be durable. Published(err) tells that error from
// one returned before the rename, where nothing was published (CRW-802). That includes a directory the process can write into
// but not open for reading (mode 0300): the sync did not run, so the publication is reported as published and unsynced, and a
// caller whose file does not depend on the sync (the receipt) decides that for itself.
//
// The owner of an existing file and its hard links are not kept, and there is no lock against a concurrent writer (the last
// rename wins).
func Publish(finalPath string, data []byte) error { return publish(finalPath, data, nil) }

// PublishChecked is Publish with check run at the last step: after the temp file is written and synced and
// immediately before the rename. A non-nil check error is returned in place of the rename, so the temp file
// is removed and finalPath is untouched. A nil check is Publish.
func PublishChecked(finalPath string, data []byte, check func() error) error {
	return publish(finalPath, data, func(at publishStep) error {
		if at == stepRename && check != nil {
			return check()
		}
		return nil
	})
}

// PublishContext is Publish with the caller's cancellation: a context cancelled at any point before the rename, the last
// step, is returned from the rename step instead of moving the temp file over finalPath, so the deferred removal leaves no
// temp file and finalPath as it was. The context is checked once, at that step: a cancellation that lands between the check
// and the rename still publishes, which is why the caller checks its context again after this returns. ctx must not be nil;
// pass context.Background() for an uncancellable publish.
func PublishContext(ctx context.Context, finalPath string, data []byte) error {
	return publishContext(ctx, finalPath, data, nil)
}

// publishContext runs publish with the hook PublishContext installs; a test's fail hook runs first, so it can trigger a
// cancellation at any step while the production path only consults the context at the rename step.
func publishContext(ctx context.Context, finalPath string, data []byte, fail func(publishStep) error) error {
	return publish(finalPath, data, func(at publishStep) error {
		if fail != nil {
			if err := fail(at); err != nil {
				return err
			}
		}
		if at == stepRename {
			return ctx.Err()
		}
		return nil
	})
}

// PublishDurable is Publish: every publication syncs the directory after the rename. The name stays for the callers that
// state, by using it, that a record written afterwards depends on the file.
func PublishDurable(finalPath string, data []byte) error { return publish(finalPath, data, nil) }

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
	if err = Rename(tmp, target); err != nil {
		return err
	}
	// The rename has happened: a failure from here on is a PublishedError, which the deferred cleanup above treats like any
	// error (the temp file no longer exists) and the caller must not read as an unpublished file.
	if err = at(stepDirSync); err != nil {
		return &PublishedError{Err: publishedUnsynced(err)}
	}
	if err = SyncDir(filepath.Dir(target)); err != nil {
		return &PublishedError{Err: publishedUnsynced(err)}
	}
	return nil
}

// publishedUnsynced words a failed directory sync so that a caller that prints the error says the file is in place.
func publishedUnsynced(err error) error {
	return fmt.Errorf("the new file is in place but its directory could not be synced: %w", err)
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

// SyncDir fsyncs the directory dir, which makes the entries created, renamed or removed in it durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
