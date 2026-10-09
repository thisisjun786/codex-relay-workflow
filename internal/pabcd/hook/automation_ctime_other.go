//go:build !linux && !darwin

package hook

import "os"

// automationCtime has no portable source on this platform; the comparison then rests on the device and
// inode, size and ModTime alone (CRW-804).
func automationCtime(os.FileInfo) (int64, bool) { return 0, false }
