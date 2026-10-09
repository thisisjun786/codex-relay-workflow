package service

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// sweepCeiling bounds the last-resort descriptor sweep when the soft limit is unlimited or huge.
const sweepCeiling = 1 << 20

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

// descriptorLimit is the soft descriptor limit, which no open descriptor of this process can reach.
func descriptorLimit() int {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil || limit.Cur > sweepCeiling {
		return sweepCeiling
	}
	return int(limit.Cur)
}
