package gitprobe

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// RepositorySearch tunes RepositoryAbove: Ceilings are directories excluded from the search, along with
// their ancestors (a ceiling itself is not checked), and IncludeBare lets a directory that is itself a Git
// directory (a bare repository's HEAD and objects) answer where only a .git marker otherwise would.
type RepositorySearch struct {
	Ceilings    []string
	IncludeBare bool
}

// RepositoryAbove is the nearest directory at or above dir whose .git resolves to a Git directory
// (ResolveGitDir), or "" when there is none below a ceiling. A .git entry that holds no repository — the
// empty directory a sandbox leaves behind, a file that is no gitfile — is no marker and the walk goes on;
// an I/O error reading one is returned. With IncludeBare a directory that is itself a Git directory answers
// too.
func RepositoryAbove(dir string, opts RepositorySearch) (string, error) {
	for dir = filepath.Clean(dir); !slices.Contains(opts.Ceilings, dir); dir = filepath.Dir(dir) {
		found, err := ResolveGitDir(filepath.Join(dir, ".git"))
		if err != nil {
			return "", err
		}
		if found != "" {
			return dir, nil
		}
		if opts.IncludeBare {
			ok, err := gitDirectory(dir)
			if err != nil {
				return "", err
			}
			if ok {
				return dir, nil
			}
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return "", nil
}

// ResolveGitDir returns the directory named by a Git marker, or an empty string
// for a missing or invalid marker. I/O errors are returned, never called repositories.
func ResolveGitDir(marker string) (string, error) {
	info, err := os.Stat(marker)
	if err != nil {
		return "", missingIsNil(err)
	}
	dir := marker
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return "", nil
		}
		raw, err := os.ReadFile(marker)
		if err != nil {
			return "", err
		}
		target, ok := strings.CutPrefix(strings.TrimRight(string(raw), "\r\n"), "gitdir: ")
		if !ok || target == "" {
			return "", nil
		}
		dir = relativeGitPath(filepath.Dir(marker), target)
	}
	ok, err := gitDirectory(dir)
	if !ok || err != nil {
		return "", err
	}
	return dir, nil
}

// gitDirectory reports whether dir is a Git directory: a regular HEAD and an objects directory. A
// commondir file (a linked worktree's) redirects the objects directory — read once, never recursively,
// and the per-worktree HEAD is still the one required; the file must hold a target. Anything else named
// commondir, or a target it names that has no objects directory, is not a Git directory.
func gitDirectory(dir string) (bool, error) {
	head, err := os.Stat(dir + string(filepath.Separator) + "HEAD")
	if err != nil {
		return false, missingIsNil(err)
	}
	if !head.Mode().IsRegular() {
		return false, nil
	}
	base := dir
	common, err := os.Stat(dir + string(filepath.Separator) + "commondir")
	switch {
	case err != nil:
		if missing := missingIsNil(err); missing != nil {
			return false, missing
		}
	case !common.Mode().IsRegular():
		return false, nil
	default:
		raw, err := os.ReadFile(dir + string(filepath.Separator) + "commondir")
		if err != nil {
			return false, err
		}
		target := strings.TrimRight(string(raw), "\r\n")
		if target == "" {
			return false, nil
		}
		base = relativeGitPath(dir, target)
	}
	objects, err := os.Stat(base + string(filepath.Separator) + "objects")
	if err != nil {
		return false, missingIsNil(err)
	}
	return objects.IsDir(), nil
}

// relativeGitPath joins a gitfile's or commondir's relative target onto base the way Git means it: by
// concatenation, so a target holding a link or a ".." is resolved by the kernel against what the link
// leads to, not against the lexical path (filepath.Join would clean the ".." away first).
func relativeGitPath(base, target string) string {
	if filepath.IsAbs(target) {
		return target
	}
	return base + string(filepath.Separator) + target
}

// missingIsNil answers nil when err names an absent entry — a path that does not exist, or one a
// non-directory in the way hides (ENOTDIR) — and err otherwise.
func missingIsNil(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	return err
}
