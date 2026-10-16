// Package workdirs is the shared recognition of a run's disposable working directories: the roots a
// run holds under its state directory that can be rebuilt from the repository (land-wt, node-wt, wt
// and each call's wt or wt-N worktree). Backup records them without their bytes only while a current
// cleanup ref proves the checkout is preserved and pristine — changed, unpreserved or unproven data
// is copied — and cleanup removes them when the run is over. Neither follows links.
package workdirs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

// IsRoot reports whether rel — slash-separated, relative to the state directory — is a disposable
// worktree root of a run: flow/<run>/land-wt, node-wt or wt, or flow/<run>/calls/<call>/<wt|wt-N>.
func IsRoot(rel string) bool {
	if !fs.ValidPath(rel) || path.Clean(rel) != rel {
		return false
	}
	p := strings.Split(rel, "/")
	if len(p) == 3 && p[0] == "flow" {
		return p[2] == "land-wt" || p[2] == "node-wt" || p[2] == "wt"
	}
	return len(p) == 5 && p[0] == "flow" && p[2] == "calls" && IsCallRoot(p[4])
}

// IsCallRoot reports whether name is a call-private worktree directory: wt or a positive decimal
// allocation wt-N (wt-1, wt-2, …; wt-0 and wt-old are not).
func IsCallRoot(name string) bool {
	if name == "wt" {
		return true
	}
	n, ok := strings.CutPrefix(name, "wt-")
	if !ok {
		return false
	}
	value, err := strconv.Atoi(n)
	return err == nil && value > 0 && strconv.Itoa(value) == n
}

// Roots lists the disposable worktree roots present under run — a root opened on a run's state
// directory (flow/<run>): the three top-level names and each real calls/<call> container's wt/wt-N
// children. Directories and symlinks count; a symlinked calls container is never descended into and
// is an error (a complete listing cannot be claimed), while an ordinary file bearing the name is
// skipped. A symlinked worktree root is listed as an entry to unlink, never a target to traverse.
func Roots(run *os.Root) ([]string, error) {
	var out []string
	for _, name := range []string{"land-wt", "node-wt", "wt"} {
		info, err := run.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			out = append(out, name)
		}
	}
	calls, err := run.Lstat("calls")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case err != nil:
		return nil, err
	case calls.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("calls is a link, not a directory")
	case !calls.IsDir():
		return out, nil
	}
	dirs, err := fs.ReadDir(run.FS(), "calls")
	if err != nil {
		return nil, err
	}
	for _, call := range dirs {
		if call.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if !call.IsDir() {
			continue
		}
		inner, err := fs.ReadDir(run.FS(), path.Join("calls", call.Name()))
		if err != nil {
			return nil, err
		}
		for _, child := range inner {
			if IsCallRoot(child.Name()) && (child.IsDir() || child.Type()&fs.ModeSymlink != 0) {
				out = append(out, path.Join("calls", call.Name(), child.Name()))
			}
		}
	}
	return out, nil
}
