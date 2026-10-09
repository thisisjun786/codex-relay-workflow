package command

import (
	"path/filepath"
	"slices"
)

// The identity of an output path is the physical path: a symlink that stands for the output directory (/alias -> /real) and the directory it points to name one file, and the ledger keeps one history for
// it. canonicalDir resolves the symlinks of a directory; a directory that does not exist yet is resolved through its nearest existing ancestor, so the path a first review will create is the path a later
// one finds. The file name is never resolved: a symlink at the final path is followed when the file is written, which belongs to the operator.
func canonicalDir(dir string) string {
	dir = filepath.Clean(dir)
	var missing []string
	for cur := dir; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			slices.Reverse(missing)
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// canonicalPath is path with its directory resolved by canonicalDir.
func canonicalPath(path string) string {
	return filepath.Join(canonicalDir(filepath.Dir(path)), filepath.Base(path))
}

// samePath reports whether two output paths name the same file: the same string, or the same resolved directory and file name. It matches the paths a ledger recorded before output directories were
// recorded resolved as well as the ones it records now.
func samePath(a, b string) bool {
	return a == b || canonicalPath(a) == canonicalPath(b)
}
