package testsupport

import (
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveTempTree removes an owned test tree, including read-only module-cache
// directories. It does not follow symlinks out of the tree.
func RemoveTempTree(root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode() | 0200
		if entry.IsDir() {
			mode |= 0700
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}
