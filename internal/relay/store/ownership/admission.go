package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// LockWait is the one bound on a fence lock wait that another writer can hold for an
// unbounded time (Python ownership.LOCK_WAIT_SECONDS, cutover.md Lock order): the socket
// binding's write-gate EX, which every admitted connection's lifetime SH excludes. It is
// the store's SQLite busy timeout and Go's default OpenOptions.BusyTimeout.
var LockWait = 30 * time.Second

// LockWaitExpired is Python's LockWaitExpired: a bounded fence lock wait ran out. It is a
// retryable host error that changed nothing, and its text is the host detail Python prints.
type LockWaitExpired struct {
	What  string
	Bound time.Duration
}

func (e *LockWaitExpired) Error() string {
	// Python formats the bound with {bound:g}: 30.0 is "30", 0.3 is "0.3".
	return "LockWaitExpired: " + e.What + " was not acquired within " + strconv.FormatFloat(e.Bound.Seconds(), 'g', -1, 64) + "s; retry"
}

// LockWithin takes the existing lock at path, waiting at most LockWait for a holder to
// release it (Python ownership.flock_within); a lock that cannot be opened or is not
// trusted fails at once, as Lock does.
func LockWithin(ctx context.Context, path string, exclusive bool, what string) (*os.File, error) {
	bound := LockWait
	deadline := time.Now().Add(bound)
	for {
		f, err := Lock(path, exclusive, false)
		if err == nil || !errors.Is(err, unix.EWOULDBLOCK) {
			return f, err
		}
		if !time.Now().Before(deadline) {
			return nil, &LockWaitExpired{What: what, Bound: bound}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Lock always uses the existing rendezvous inode. Contention bounded-fails
// immediately; callers can explicitly resume, never bypass a holder on age/PID.
func Lock(path string, exclusive, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = lockFileSafe(fd, path); err != nil {
		err = errors.Join(refuse("unsafe lock file %s", path), err)
	}
	if err == nil {
		kind := unix.LOCK_SH
		if exclusive {
			kind = unix.LOCK_EX
		}
		err = unix.Flock(fd, kind|unix.LOCK_NB)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// lockFileSafe trusts a lock inode that is a regular file owned by this user and
// either grants no group or other access (the 0600 mode Go creates, trusted in any
// directory as Python and every earlier Go build trust it) or sits in an owner-only
// directory: a lock another runtime created under umask 002 is 0664 inside a 0700 S,
// and no other user can reach it there (decision D3). flock does not depend on the
// file mode, and a lock is never chmodded, replaced or unlinked (cutover.md: every
// lock file keeps its inode). A lock Lock or serviceLock creates is 0600, so neither
// ever leaves behind a lock file that it then refuses.
func lockFileSafe(fd int, path string) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	euid := uint32(os.Geteuid())
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != euid {
		return refuse("lock is not a regular file owned by this user")
	}
	if info.Mode&0077 == 0 {
		return nil
	}
	var dir unix.Stat_t
	if err := unix.Stat(LexicalDir(path), &dir); err != nil {
		return err
	}
	if dir.Mode&unix.S_IFMT != unix.S_IFDIR || dir.Uid != euid || dir.Mode&0022 != 0 {
		return refuse("lock grants group or other access and its directory is not owned by this user or is group/world writable")
	}
	return nil
}

// LexicalDir is the directory the kernel resolved when it opened path: everything
// before the last slash, never cleaned, so a '..' after a symlink names the same
// directory in the stat as in the open (filepath.Dir would clean it lexically).
func LexicalDir(path string) string {
	switch i := strings.LastIndex(path, "/"); {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	default:
		return path[:i]
	}
}

type Admission struct {
	gate  *os.File
	Path  string
	Stamp Stamp
}

func Admit(ctx context.Context, path string) (_ *Admission, err error) {
	physical, err := Physical(path)
	if err != nil {
		return nil, refuse("existing database required: %v", err)
	}
	path = physical.RealPath
	gate, err := Lock(filepath.Join(filepath.Dir(path), "write-gate.lock"), false, false)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			// The fence runs its lock-free check_start before its blocking admission, so a
			// store whose gate a transfer barrier holds is refused as that check refuses it
			// (draining: queueable, decision 25), never for the contention itself.
			if e := CheckStart(ctx, path, ""); e != nil {
				return nil, e
			}
		}
		return nil, refuse("write gate: %v", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, gate.Close())
		}
	}()
	r, err := ReadRecord(path)
	if err != nil {
		return nil, err
	}
	s, err := SnapshotMeta(ctx, path)
	if err != nil {
		return nil, err
	}
	if err = judge(path, r, s); err != nil {
		return nil, err
	}
	return &Admission{gate: gate, Path: path, Stamp: s}, nil
}

// judge is the ownership decision shared by admission and the start preflight: a valid
// record, Go as the durable owner, and an active phase. A store starting under a takeover is
// refused to every opener: no candidate exists any more (decision 54), and the refusal is
// the one a process without the candidate's permit always got.
func judge(path string, r Record, s Stamp) error {
	if err := Validate(path, r, s); err != nil {
		return err
	}
	if s.Owner != "go" {
		return refuse("store belongs to %s", s.Owner)
	}
	if r.Phase == "starting" {
		return refuse("only designated candidate may enter starting")
	} else if r.Phase != "active" {
		return refuse("store is draining")
	}
	return nil
}

// Unbound is Python's ownership.unbound for this runtime: whether a writable open that passes
// the canonical App Server socket binds this store to it (cutover.md Record, Socket binding).
// The mirror names no socket and no scope, the durable socket_path is absent or already this
// socket (the torn binding a crash between its commit and its publication leaves), Go owns
// the store in phase active with no transition.
func Unbound(r Record, s Stamp, socket string) bool {
	if socket == "" {
		return false
	}
	return r.AppServerSocket == nil && r.ScopeKey == nil && (s.SocketPath == "" || s.SocketPath == socket) &&
		s.Owner == "go" && r.Owner == "go" && r.Phase == "active" && r.Transition == nil
}

// CheckStart is the read-only preflight of a service or daemon command (Python
// ownership.check_start). A truly absent store passes, because the writable opener
// creates it; anything else is judged as Admit judges it, without taking the gate
// and without creating a byte beside the database. socket is the canonical App Server
// socket the command's writable open passes ("" for none), so a binding that open would
// complete (Unbound) is judged as the binding judges it, not refused here first.
func CheckStart(ctx context.Context, path, socket string) error {
	return checkStart(ctx, path, socket, SnapshotMeta)
}

// CheckStop is CheckStart for the read-only Stop path (Python ownership.check_stop): the verdict
// of a candidate-less, socketless start, with the durable stamp read by stamp from the resolved
// database (in place, without the copy SnapshotMeta makes) and no SQLite sidecar created.
func CheckStop(ctx context.Context, path string, stamp func(context.Context, string) (Stamp, error)) error {
	return checkStart(ctx, path, "", stamp)
}

func checkStart(ctx context.Context, path, socket string, stamp func(context.Context, string) (Stamp, error)) error {
	dir := filepath.Dir(path)
	absent := true
	for _, name := range []string{path, filepath.Join(dir, "takeover.json"), filepath.Join(dir, "write-gate.lock")} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			absent = false
		}
	}
	if absent {
		return nil
	}
	physical, err := Physical(path)
	if err != nil {
		return refuse("existing database required: %v", err)
	}
	path = physical.RealPath
	r, err := ReadRecord(path)
	if err != nil {
		return err
	}
	s, err := stamp(ctx, path)
	if err != nil {
		return err
	}
	if Unbound(r, s, socket) {
		s.SocketPath = ""
	}
	return judge(path, r, s)
}

// Check runs before a connection's first write-capable PRAGMA, including pool
// replacements; opening mode=rw alone must not turn a swapped pathname into a writer.
func (a *Admission) Check(ctx context.Context) error {
	if a == nil {
		return nil
	}
	r, err := ReadRecord(a.Path)
	if err != nil {
		return err
	}
	s, err := SnapshotMeta(ctx, a.Path)
	if err != nil {
		return err
	}
	if err = Validate(a.Path, r, s); err != nil {
		return err
	}
	if s.Owner != a.Stamp.Owner || s.Epoch != a.Stamp.Epoch || s.TakeoverID != a.Stamp.TakeoverID {
		return refuse("admitted ownership changed")
	}
	return nil
}
func (a *Admission) Revalidate(ctx context.Context, db Queryer) error {
	if a == nil {
		return nil
	}
	r, err := ReadRecord(a.Path)
	if err != nil {
		return err
	}
	s, err := ReadStamp(ctx, db)
	if err != nil {
		return err
	}
	if err = Validate(a.Path, r, s); err != nil {
		return err
	}
	if s.Owner != a.Stamp.Owner || s.Epoch != a.Stamp.Epoch || s.TakeoverID != a.Stamp.TakeoverID {
		return refuse("admitted ownership changed")
	}
	// Already admitted work may finish in draining, with its original SH held; no candidate
	// exists to write in starting.
	if r.Phase == "starting" {
		return refuse("candidate changed")
	}
	return nil
}
func (a *Admission) Close() error {
	if a == nil || a.gate == nil {
		return nil
	}
	err := a.gate.Close()
	a.gate = nil
	return err
}
