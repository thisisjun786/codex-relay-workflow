package service

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// ErrDescriptorsUnlisted is a sweep that has no way to reach every open descriptor: close_range is
// not offered and no listing of the open descriptors is readable. No number taken from a limit
// bounds the descriptors already open (RLIMIT_NOFILE and fs.nr_open can both be lowered after a
// descriptor above them was opened, and lowering them closes nothing), so walking the numbers up to
// one proves nothing and the start is refused instead.
var ErrDescriptorsUnlisted = errors.New("no way to reach every open descriptor: close_range is not offered and no descriptor listing is readable")

// fdSweep marks every descriptor above 2 close-on-exec. Each way of doing it is tried in turn and
// the first that works wins, so the sweep never depends on one filesystem path being mounted: an
// isolated or containerised Linux can hide /dev/fd, or /proc, or both. A way that is reachable but
// fails to mark a descriptor fails the sweep, and so does having no way at all: a start that could
// hand the caller's descriptor to the daemon is refused (CRW-1057).
type fdSweep struct {
	// closeRange marks the whole range 3..~0U in one call (close_range with CLOSE_RANGE_CLOEXEC,
	// Linux 5.11 and up). An error means this kernel or sandbox policy does not offer it.
	closeRange func() error
	// dirs list the open descriptors of this process, one entry per descriptor.
	dirs []string
	// mark marks one descriptor close-on-exec; markCloseOnExec when nil.
	mark func(fd int) error
}

var defaultFDSweep = fdSweep{
	closeRange: closeRangeCloExec,
	dirs:       []string{"/proc/self/fd", "/dev/fd"},
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
	return ErrDescriptorsUnlisted
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
// open (the listing's own descriptor, one closed since the listing was read) is nothing to mark. unix.CloseOnExec does
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
