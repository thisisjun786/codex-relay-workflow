package service

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// closeOnExecInherited marks every descriptor above 2 close-on-exec before a daemon is spawned.
//
// The caller of `relay service start` may hold descriptors without close-on-exec: a shell's
// exec 9>lock keeps the integration lock on fd 9 and passes it to every command it runs. os/exec
// forks the parent's descriptors into the child unless they are close-on-exec, and only the
// ExtraFiles it lists are set up for the child on purpose, so the supervisor (and through it the
// worker) would keep the caller's lock for as long as it lives (CRW-1057). The ExtraFiles of the
// launch are set up at 3 and up by the fork itself, which clears close-on-exec on those copies, so
// marking the parent's descriptors first does not change what the daemon receives on purpose.
func closeOnExecInherited() error {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return fmt.Errorf("list open descriptors: %w", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		unix.CloseOnExec(fd)
	}
	return nil
}
