package role

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// dispatchProcessStart is the start time of process pid as the owner record keeps it: the p_starttime of its
// kinfo_proc as "<seconds>.<microseconds>". It is compared for equality and never read as a time.
func dispatchProcessStart(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
