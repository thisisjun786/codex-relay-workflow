package service

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	// sweepFloor is the least the last-resort walk covers: 1<<20 is the kernel's default
	// fs.nr_open, which no descriptor number reaches unless root raised it.
	sweepFloor = 1 << 20
	// sweepCap bounds the walk when a descriptor limit is unlimited or huge (about a second of
	// fcntl calls).
	sweepCap = 1 << 24
)

// fdSweep marks every descriptor above 2 close-on-exec. Each way of doing it is tried in turn and
// the first that works wins, so the sweep never depends on one filesystem path being mounted: an
// isolated or containerised Linux can hide /dev/fd, or /proc, or both.
type fdSweep struct {
	// closeRange marks the whole range in one call (close_range with CLOSE_RANGE_CLOEXEC, Linux 5.11
	// and up). An error means this kernel or sandbox policy does not offer it.
	closeRange func() error
	// dirs list the open descriptors of this process, one entry per descriptor.
	dirs []string
	// limit is the number of descriptors the last resort walks, by number, when no way above worked.
	limit func() int
}

var defaultFDSweep = fdSweep{
	closeRange: closeRangeCloExec,
	dirs:       []string{"/proc/self/fd", "/dev/fd"},
	limit:      descriptorLimit,
}

// closeOnExecInherited marks every descriptor above 2 close-on-exec before a daemon is spawned.
//
// The caller of `relay service start` may hold descriptors without close-on-exec: a shell's
// exec 9>lock keeps the integration lock on fd 9 and passes it to every command it runs. os/exec
// forks the parent's descriptors into the child unless they are close-on-exec, and only the
// ExtraFiles it lists are set up for the child on purpose, so the supervisor (and through it the
// worker) would keep the caller's lock for as long as it lives (CRW-1057). The ExtraFiles of the
// launch are set up at 3 and up by the fork itself, which clears close-on-exec on those copies, so
// marking the parent's descriptors first does not change what the daemon receives on purpose.
func closeOnExecInherited() error { return defaultFDSweep.apply() }

func (s fdSweep) apply() error {
	if s.closeRange != nil && s.closeRange() == nil {
		return nil
	}
	for _, dir := range s.dirs {
		if sweepListing(dir) {
			return nil
		}
	}
	n := 0
	if s.limit != nil {
		n = s.limit()
	}
	for fd := 3; fd < n; fd++ {
		unix.CloseOnExec(fd)
	}
	return nil
}

// sweepListing marks the descriptors that dir lists and reports whether the listing was readable.
func sweepListing(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		unix.CloseOnExec(fd)
	}
	return true
}

// descriptorLimit is how far the last-resort walk goes. The soft limit alone is not a bound: it
// only stops new descriptors, and one opened before the limit was lowered (by this process or by a
// caller before the exec) stays open above it (CRW-1057 verification of 794b8188). The walk covers
// the larger of the soft limit, the hard limit and the default fs.nr_open, up to sweepCap.
func descriptorLimit() int {
	n := uint64(sweepFloor)
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err == nil {
		n = max(n, limit.Cur, limit.Max)
	}
	return int(min(n, sweepCap))
}
