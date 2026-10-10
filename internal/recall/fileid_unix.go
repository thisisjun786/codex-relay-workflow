//go:build unix

package recall

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity names the file a path reached when it was stat'ed: device and inode. A rollout that
// is replaced by another file (rename over it, restore from backup) changes it even when the new
// file has the same size and modification time.
func fileIdentity(st os.FileInfo) string {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(sys.Dev), uint64(sys.Ino))
}
