package worktrees

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolve is Python's Path.resolve() (posixpath.realpath, strict=False): symlinks are followed,
// and a component that does not exist, or sits under something that is not a directory, is kept
// as written rather than refused. That is what lets validation name the real problem ("absent
// with an existing parent", "registered worktree") instead of failing on path resolution.
func resolve(path string) string {
	resolved, _ := joinRealpath("/", strings.TrimPrefix(filepath.Clean(path), "/"), map[string]*string{})
	return resolved
}

func joinRealpath(path, rest string, seen map[string]*string) (string, bool) {
	if strings.HasPrefix(rest, "/") {
		path, rest = "/", strings.TrimLeft(rest, "/")
	}
	for rest != "" {
		name, remainder, _ := strings.Cut(rest, "/")
		rest = remainder
		switch name {
		case "", ".":
			continue
		case "..":
			path = filepath.Dir(path)
			continue
		}
		next := filepath.Join(path, name)
		info, err := os.Lstat(next)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			path = next
			continue
		}
		if known, visited := seen[next]; visited {
			if known != nil {
				path = *known
				continue
			}
			// A symlink loop: Python keeps the rest of the path as written.
			return filepath.Join(next, rest), false
		}
		seen[next] = nil
		target, err := os.Readlink(next)
		if err != nil {
			path = next
			continue
		}
		var ok bool
		if path, ok = joinRealpath(path, target, seen); !ok {
			return filepath.Join(path, rest), false
		}
		settled := path
		seen[next] = &settled
	}
	return path, true
}

// exists is Python's Path.exists(): a missing path, or one under a file, is simply absent.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENAMETOOLONG):
		return false, nil
	default:
		return false, err
	}
}

// isSymlink is Python's Path.is_symlink(), which answers false whenever lstat fails.
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

// isDir is Python's Path.is_dir(), which answers false whenever stat fails.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
