package manage

import (
	"path/filepath"
	"strings"
)

// rootDir is the directory part of path, taken from the text and cleaned of nothing. A path built
// below a configured or environment root keeps the spelling the filesystem resolves: filepath.Dir
// cleans, and cleaning a root that mixes a symbolic link and ".." names a different directory than
// the kernel gives that spelling. Use crwconfig.JoinRoot to build such a path and rootDir to
// take its directory; filepath.Base of the last element is safe to use as it is.
//
// For a path without "." or ".." elements and with no repeated separators it returns what
// filepath.Dir returns.
func rootDir(path string) string {
	separator := string(filepath.Separator)
	i := strings.LastIndex(path, separator)
	if i < 0 {
		return "."
	}
	dir := strings.TrimRight(path[:i], separator)
	if dir == "" {
		return separator
	}
	return dir
}
