//go:build darwin

package hook

import (
	"os"
	"syscall"
)

// automationCtime is the inode change time of a stat result in nanoseconds (the oracle's ctimeMs,
// automation-store.ts:124). darwin names the field Ctimespec; linux's file names it Ctim (CRW-804).
func automationCtime(info os.FileInfo) (int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Ctimespec.Nano(), true
}
