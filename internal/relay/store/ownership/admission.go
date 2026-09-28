package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

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
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err == nil && (info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0) {
		err = refuse("unsafe lock file %s", path)
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

// Candidate identifies the single controller-designated starting daemon. A
// context permit is passed only by the inherited activation-channel entry point.
type Candidate struct {
	TransitionID string
	Epoch        int64
	Controller   Identity
}
type candidateKey struct{}

func WithCandidate(ctx context.Context, c Candidate) context.Context {
	return context.WithValue(ctx, candidateKey{}, c)
}

type Admission struct {
	gate      *os.File
	Path      string
	Stamp     Stamp
	candidate *Candidate
}

func Admit(ctx context.Context, path string) (_ *Admission, err error) {
	physical, err := Physical(path)
	if err != nil {
		return nil, refuse("existing database required: %v", err)
	}
	path = physical.RealPath
	gate, err := Lock(filepath.Join(filepath.Dir(path), "write-gate.lock"), false, false)
	if err != nil {
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
	if err = Validate(path, r, s); err != nil {
		return nil, err
	}
	if s.Owner != "go" {
		return nil, refuse("store belongs to %s", s.Owner)
	}
	a := &Admission{gate: gate, Path: path, Stamp: s}
	if r.Phase == "starting" {
		c, ok := ctx.Value(candidateKey{}).(Candidate)
		if !ok || r.Transition == nil || r.Controller == nil || c.TransitionID != s.TakeoverID || c.Epoch != s.Epoch || c.Controller != *r.Controller {
			return nil, refuse("only designated candidate may enter starting")
		}
		a.candidate = &c
	} else if r.Phase != "active" {
		return nil, refuse("store is draining")
	}
	return a, nil
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
	// Already admitted work may finish in draining, with its original SH held.
	if r.Phase == "starting" && (a.candidate == nil || r.Controller == nil || *r.Controller != a.candidate.Controller) {
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
