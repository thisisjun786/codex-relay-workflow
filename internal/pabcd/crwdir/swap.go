package crwdir

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// This file is CRW-844's publication: the swap-then-verify step crw doctor retrust replaces
// config.toml with. Publish (atomic.go) keeps its own contract and callers; this step is separate
// because it must keep the file it displaced, which a plain rename destroys.

// crwdirSwapLockSuffix names the advisory sidecar every CRW writer of config.toml locks. It is
// created once and never unlinked (docs/port/decisions.md 7).
const crwdirSwapLockSuffix = ".crw-lock"

// crwdirSwapLockPoll is how long LockConfig sleeps between attempts.
const crwdirSwapLockPoll = 25 * time.Millisecond

// PublishedError reports that a swap publication exchanged the new content into place and then
// failed a step that runs after the exchange. The new content is visible to every reader, so the
// failure is not a reason to undo it; the caller counts the publication as done and reports the
// failure. Err is the underlying failure and Unwrap exposes it, so errors.Is keeps answering for the
// cause. It is the crwdir form of state.PublishedError.
//
// DisplacedAt names the file that holds the content the exchange displaced when the backup could not
// be filled: the move to backupPath failed, or backupPath could not be read back. It is empty when
// backupPath holds the displaced content, which is every other case. A caller that reports the file
// roles must use DisplacedAt rather than assume the backup path.
type PublishedError struct {
	Err         error
	DisplacedAt string
}

func (e *PublishedError) Error() string { return e.Err.Error() }

func (e *PublishedError) Unwrap() error { return e.Err }

// Published reports whether err is, or wraps, a PublishedError: the target was exchanged before the
// failure. It is false for an error returned before the exchange, where nothing was published.
func Published(err error) bool {
	var target *PublishedError
	return errors.As(err, &target)
}

// crwdirSwapStep names an action of the swap publication, so a test can fail it.
type crwdirSwapStep int

const (
	crwdirSwapStepCreate   crwdirSwapStep = iota // the exclusive create of the temp file
	crwdirSwapStepMode                           // right after the create, before the temp file takes the target's mode
	crwdirSwapStepWrite                          // before the data is written
	crwdirSwapStepSync                           // before the temp file is fsynced
	crwdirSwapStepReserve                        // before the backup path is reserved
	crwdirSwapStepReread                         // before the last check, the target read again
	crwdirSwapStepCompare                        // right after the last check passed, before the exchange
	crwdirSwapStepExchange                       // before the atomic exchange
	crwdirSwapStepMove                           // before the displaced file is moved to the backup path
	crwdirSwapStepSyncDir                        // before the directory is fsynced
)

// PublishSwap replaces target with next, keeping what it displaced in backupPath, and answers the
// bytes the exchange moved there. The steps, in order:
//
//   - the target is resolved (a symlink is followed to the file it names) and a temp file is written
//     beside it, fsynced and given the target's permission bits;
//   - the target is read again and the publication is refused when it no longer holds expected: the
//     last check before the exchange, so a writer that saved in between is reported and never
//     replaced;
//   - the temp file and the target are exchanged atomically (linux renameat2 RENAME_EXCHANGE,
//     darwin renamex_np RENAME_SWAP). A filesystem without the exchange is refused before anything
//     is written; there is no plain-rename fallback;
//   - backupPath is reserved before the exchange, with the same exclusive create the oracle's
//     copyFileSync(target, backup, COPYFILE_EXCL) used, so an occupied backup path refuses the whole
//     publication before anything is exchanged and a colliding backup name can never leave the
//     target replaced with the displaced content only at a temporary path;
//   - the file the exchange displaced is moved to backupPath with a rename that refuses to replace,
//     so the backup is the very file the publication displaced and nothing the caller did not create
//     is deleted;
//   - the directory is fsynced. Every failure after the exchange - the move, reading the backup back,
//     or the sync - is a *PublishedError: the exchange happened, so the caller counts the publication
//     as done and reports the failure, and PublishedError.DisplacedAt names where the displaced
//     content is when the backup could not be filled.
//
// The answer is the displaced file's bytes, read from backupPath after the move, so the caller can
// tell a cooperative writer (they equal expected) from one that saved between the last check and the
// exchange (they do not: that content is in the backup, the target holds next, and this function
// never exchanges back).
func PublishSwap(target string, expected, next []byte, backupPath string) ([]byte, error) {
	return crwdirSwapPublish(target, expected, next, backupPath, nil, nil)
}

// crwdirSwapPublish runs PublishSwap with a step hook a test can fail and an optional directory sync.
func crwdirSwapPublish(target string, expected, next []byte, backupPath string, fail func(crwdirSwapStep) error, syncDir func(string) error) (displaced []byte, err error) {
	at := func(step crwdirSwapStep) error {
		if fail == nil {
			return nil
		}
		return fail(step)
	}
	resolved, info, err := resolveTarget(target)
	if err != nil {
		return nil, err
	}
	if syncDir == nil {
		syncDir = crwdirSwapSyncDir
	}
	create := os.FileMode(0o666)
	if info != nil {
		create = 0o600
	}
	tmp := filepath.Join(filepath.Dir(resolved), "."+filepath.Base(resolved)+"."+rand.Text()+".crwswap")
	if err = at(crwdirSwapStepCreate); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, create)
	if err != nil {
		return nil, err
	}
	// keep is set once the exchange has run: the temp path then holds the file the exchange
	// displaced, which is never deleted. Before that the temp file is this call's own and is removed.
	keep := false
	defer func() {
		if keep {
			return
		}
		_ = f.Close() // a second close after the one below only reports that it is closed
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()
	if err = at(crwdirSwapStepMode); err != nil {
		return nil, err
	}
	if info != nil {
		if err = f.Chmod(info.Mode().Perm()); err != nil {
			return nil, err
		}
	}
	if err = at(crwdirSwapStepWrite); err != nil {
		return nil, err
	}
	if _, err = f.Write(next); err != nil {
		return nil, err
	}
	if err = at(crwdirSwapStepSync); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	// An occupied backup path is refused before the exchange, exactly as the oracle's exclusive
	// copyFileSync(target, backup, COPYFILE_EXCL) refused one before its write: the common collision
	// (a second run within the same timestamped name) then leaves the target untouched instead of
	// replaced with the displaced content only at a temporary path. The no-replace move below is the
	// guarantee for a name taken after this check; its failure is a PublishedError naming where the
	// displaced content is.
	if err = at(crwdirSwapStepReserve); err == nil {
		if _, statErr := os.Lstat(backupPath); statErr == nil {
			err = &os.LinkError{Op: "rename", Old: tmp, New: backupPath, Err: fs.ErrExist}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			err = statErr
		}
	}
	if err != nil {
		return nil, err
	}
	if err = at(crwdirSwapStepReread); err != nil {
		return nil, err
	}
	current, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(current, expected) {
		return nil, fmt.Errorf("refusing to exchange: %s changed after it was read", resolved)
	}
	if err = at(crwdirSwapStepCompare); err == nil {
		if err = at(crwdirSwapStepExchange); err == nil {
			err = crwdirSwapExchange(tmp, resolved)
		}
	}
	if err != nil {
		if crwdirSwapExchangeUnsupported(err) {
			return nil, fmt.Errorf("this filesystem does not support an atomic exchange: %w", err)
		}
		return nil, err
	}
	keep = true
	if err = at(crwdirSwapStepMove); err == nil {
		err = crwdirSwapNoReplace(tmp, backupPath)
	}
	if err != nil {
		return nil, &PublishedError{Err: fmt.Errorf("%w (the content the exchange displaced is kept at %s)", err, tmp), DisplacedAt: tmp}
	}
	if displaced, err = os.ReadFile(backupPath); err != nil {
		return nil, &PublishedError{Err: err, DisplacedAt: backupPath}
	}
	if err = at(crwdirSwapStepSyncDir); err == nil {
		err = syncDir(filepath.Dir(resolved))
	}
	if err != nil {
		return displaced, &PublishedError{Err: err}
	}
	return displaced, nil
}

// ConfigLock is an exclusive advisory lock on a target's sidecar: every CRW writer of config.toml
// takes it around its read-modify-write, so their publications are serialized. It is an flock(2) on
// the sidecar file, which is created once and never unlinked (docs/port/decisions.md 7); release
// unlocks and closes and leaves the file in place.
type ConfigLock struct {
	// Target is the resolved file the lock guards: the caller's path with a symlink followed, so two
	// writers reaching one file through different spellings share one lock. It is the lock's key and
	// the identity a caller compares against, not a mandatory content path. A writer whose content
	// path can change under an external runner reads and publishes through the caller's path instead
	// (the rule CRW-891 gave the multi-agent repair and CRW-899 the activation), because the runner
	// may atomically replace the caller's pathname while the lock still names the file the link
	// pointed at when it was taken; the caller records that window as a limitation. Every other
	// writer reads and publishes Target, which is the file the lock actually guards.
	Target string
	// Path is the sidecar the flock is held on.
	Path string
	file *os.File
}

// ConfigLockBusy is the refusal every CRW writer of config.toml answers when another holds the lock.
const ConfigLockBusy = "config.toml is busy: another CRW writer holds its lock"

// LockConfig resolves target (a symlink is followed to the file it names), takes an exclusive flock
// on that file's sidecar, and waits up to wait for another holder. On contention after the wait it
// refuses with ConfigLockBusy. An error that is not contention means this filesystem's advisory
// locks are unavailable, and the caller must not write. A wait of zero refuses at the first
// contention.
func LockConfig(target string, wait time.Duration) (*ConfigLock, error) {
	resolved, err := crwdirSwapResolvePath(target)
	if err != nil {
		return nil, err
	}
	path := resolved + crwdirSwapLockSuffix
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &ConfigLock{Target: resolved, Path: path, file: file}, nil
		}
		if !crwdirSwapLockContended(err) {
			_ = file.Close()
			return nil, fmt.Errorf("advisory locking is unavailable at %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, errors.New(ConfigLockBusy)
		}
		time.Sleep(crwdirSwapLockPoll)
	}
}

// crwdirSwapResolvePath answers the file a writer must lock and publish: target with a symlink
// followed to the file it names. Unlike resolveTarget it does not probe the file for write
// permission, because taking the lock and reading the file are legal on a file this process may not
// write (a read-only config.toml an activation has nothing to change in is a normal case), and the
// publication's own probe still refuses the write.
func crwdirSwapResolvePath(target string) (string, error) {
	if link, err := os.Lstat(target); err == nil && link.Mode()&fs.ModeSymlink != 0 {
		return filepath.EvalSymlinks(target)
	}
	return target, nil
}

// Release unlocks and closes the sidecar. The file is never unlinked.
func (l *ConfigLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

// crwdirSwapLockContended reports whether err says somebody else holds the lock, rather than that
// this platform or filesystem cannot lock at all.
func crwdirSwapLockContended(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES)
}

// crwdirSwapSyncDir fsyncs the directory that holds a published file, so the exchange and the backup
// rename survive a power failure.
func crwdirSwapSyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
