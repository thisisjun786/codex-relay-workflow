package service

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// sweepCap is the most descriptor numbers the last-resort walk covers (about a second of fcntl
// calls).
const sweepCap = 1 << 24

// ErrDescriptorsUnbounded is a last-resort walk that cannot cover every descriptor number a process
// can hold, so the descriptors the caller left open cannot all be accounted for.
var ErrDescriptorsUnbounded = errors.New("the number of descriptors a process may hold cannot be bounded")

// fdSweep marks every descriptor above 2 close-on-exec. Each way of doing it is tried in turn and
// the first that works wins, so the sweep never depends on one filesystem path being mounted: an
// isolated or containerised Linux can hide /dev/fd, or /proc, or both. A way that is reachable but
// fails to mark a descriptor fails the sweep: a start that would hand the caller's descriptor to the
// daemon is refused (CRW-1057).
type fdSweep struct {
	// closeRange marks the whole range in one call (close_range with CLOSE_RANGE_CLOEXEC, Linux 5.11
	// and up). An error means this kernel or sandbox policy does not offer it.
	closeRange func() error
	// dirs list the open descriptors of this process, one entry per descriptor.
	dirs []string
	// limit is the number of descriptor numbers the last resort walks: a bound that no open
	// descriptor reaches, or the error that says no such bound is known.
	limit func() (int, error)
	// mark marks one descriptor close-on-exec; markCloseOnExec when nil.
	mark func(fd int) error
}

var defaultFDSweep = fdSweep{
	closeRange: closeRangeCloExec,
	dirs:       []string{"/proc/self/fd", "/dev/fd"},
	limit:      descriptorLimit,
}

// closeOnExecInherited marks every descriptor above 2 close-on-exec before a daemon is spawned, or
// fails when it cannot, so that the start is refused.
//
// The caller of `relay service start` may hold descriptors without close-on-exec: a shell's
// exec 9>lock keeps the integration lock on fd 9 and passes it to every command it runs. os/exec
// forks the parent's descriptors into the child unless they are close-on-exec, and only the
// ExtraFiles it lists are set up for the child on purpose, so the supervisor (and through it the
// worker) would keep the caller's lock for as long as it lives (CRW-1057). The ExtraFiles of the
// launch are set up at 3 and up by the fork itself, which clears close-on-exec on those copies, so
// marking the parent's descriptors first does not change what the daemon receives on purpose.
func closeOnExecInherited() error {
	if err := defaultFDSweep.apply(); err != nil {
		return fmt.Errorf("start refused, the caller's open descriptors cannot all be kept out of the daemon: %w", err)
	}
	return nil
}

func (s fdSweep) apply() error {
	if s.closeRange != nil && s.closeRange() == nil {
		return nil
	}
	mark := s.mark
	if mark == nil {
		mark = markCloseOnExec
	}
	for _, dir := range s.dirs {
		if listed, err := sweepListing(dir, mark); listed {
			return err
		}
	}
	if s.limit == nil {
		return ErrDescriptorsUnbounded
	}
	n, err := s.limit()
	if err != nil {
		return err
	}
	for fd := 3; fd < n; fd++ {
		if err := mark(fd); err != nil {
			return err
		}
	}
	return nil
}

// sweepListing marks the descriptors that dir lists. It reports whether the listing was readable,
// and the first descriptor that could not be marked.
func sweepListing(dir string, mark func(int) error) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, nil
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		if err := mark(fd); err != nil {
			return true, err
		}
	}
	return true, nil
}

// markCloseOnExec makes descriptor fd close-on-exec and checks that it is. A number that is not
// open (the listing's own descriptor, a gap in the walk) is nothing to mark. unix.CloseOnExec does
// not do: it returns no error, and a sandbox that refuses F_SETFD would go unnoticed.
func markCloseOnExec(fd int) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if errors.Is(err, unix.EBADF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("descriptor %d: %w", fd, err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		return nil
	}
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("descriptor %d cannot be marked close-on-exec: %w", fd, err)
	}
	flags, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("descriptor %d is still inherited after marking it close-on-exec: %v", fd, err)
	}
	return nil
}

// descriptorLimit is how far the last-resort walk goes, and it is a bound: a descriptor opened
// before a limit was lowered (by this process or by a caller before the exec) stays open above the
// limit, so the soft and hard limits do not bound the open descriptors, but nothing is ever opened
// at or above the kernel's own maximum (fs.nr_open on Linux), which is therefore walked too. When
// that maximum is unknown, or more than sweepCap numbers, the walk could miss a descriptor and the
// sweep fails (CRW-1057 re-evaluation of 9c0015af).
func descriptorLimit() (int, error) { return descriptorBound(kernelMaxDescriptors) }

func descriptorBound(kernelMax func() (uint64, error)) (int, error) {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return 0, err
	}
	kernel, err := kernelMax()
	if err != nil {
		return 0, fmt.Errorf("%w: the kernel's maximum is unknown: %v", ErrDescriptorsUnbounded, err)
	}
	n := max(limit.Cur, limit.Max, kernel)
	if n > sweepCap {
		return 0, fmt.Errorf("%w: up to %d, the walk covers %d", ErrDescriptorsUnbounded, n, sweepCap)
	}
	return int(n), nil
}
