package configguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// ResolveCodexHome answers the physical directory a CODEX_HOME spelling names, resolved once through the kernel so that the
// Codex CLI the commands run, the managed-key edits, the backup, the install manifest and the config lock all use one
// identity (CRW-1144). filepath.Join folds "link/.." lexically, to the directory that holds the link, while the CLI reaches
// the link's parent; the two then edited different config files. An ordinary symlinked home is followed. A home that does
// not exist yet is named through its nearest existing ancestor; a ".." after a missing directory has no physical meaning
// and is refused before anything is written.
func ResolveCodexHome(home string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		if abs, ok := configLockPathsAbsolute(resolved); ok {
			return abs, nil
		}
		return "", fmt.Errorf("the codex home %s cannot be made absolute", home)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the codex home %s cannot be resolved: %w", home, err)
	}
	// Walk up the spelling without cleaning it, so a ".." is never folded against a component that was not resolved.
	sep := string(filepath.Separator)
	dir, rest := strings.TrimRight(home, sep), []string(nil)
	for {
		i := strings.LastIndex(dir, sep)
		base := dir[i+1:]
		if base == ".." {
			return "", fmt.Errorf("the codex home %s goes through '..' after a directory that does not exist; name it without '..'", home)
		}
		if base != "." && base != "" {
			rest = append([]string{base}, rest...)
		}
		switch {
		case i < 0:
			dir = "."
		case i == 0:
			dir = sep
		default:
			dir = strings.TrimRight(dir[:i], sep)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			abs, ok := configLockPathsAbsolute(resolved)
			if !ok {
				return "", fmt.Errorf("the codex home %s cannot be made absolute", home)
			}
			return filepath.Join(append([]string{abs}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || dir == "." || dir == sep {
			return "", fmt.Errorf("the codex home %s cannot be resolved: %w", home, err)
		}
	}
}

// configLinkTarget is the file a config path names now: a final symlink followed, as crwdir.LockConfig resolves it.
func configLinkTarget(path string) string {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
	}
	return path
}

// configLocks is the set of config locks one command holds: the one it took before it read anything, and the ones it took
// later on files the Codex CLI put in place of the caller's config.toml (CRW-1144).
type configLocks struct {
	main  *crwdir.ConfigLock
	extra []*crwdir.ConfigLock
}

// recheck re-proves, after the Codex CLI ran, that the config file and its directory are the ones the command pinned before
// it (CRW-1144). The directory must be the same directory. When the CLI replaced the caller's config.toml, so that the path
// now names a file no held lock guards, the command takes that file's lock as well and re-checks under it: another CRW writer
// that reached the new file first makes it wait and then refuse, and nothing more is published. A command that runs the CLI
// more than once calls it after every run, before the next effect.
func (c *configLocks) recheck(path string, dir os.FileInfo) error {
	extra, err := c.acquire(path, dir)
	if extra != nil {
		c.extra = append(c.extra, extra)
	}
	return err
}

// releaseExtra releases the locks taken after the first; the first is the caller's to release.
func (c *configLocks) releaseExtra() {
	for _, l := range c.extra {
		l.Release()
	}
	c.extra = nil
}

func (c *configLocks) acquire(path string, dir os.FileInfo) (*crwdir.ConfigLock, error) {
	if dir != nil {
		if live, err := os.Stat(filepath.Dir(path)); err != nil || !os.SameFile(live, dir) {
			return nil, fmt.Errorf("the directory of %s changed while codex ran; nothing more was published", path)
		}
	}
	target := configLinkTarget(path)
	if c.main.HoldsSidecar(target) {
		return nil, nil
	}
	for _, l := range c.extra {
		if l.HoldsSidecar(target) {
			return nil, nil
		}
	}
	extra, err := crwdir.LockConfig(path, activationLockWait)
	if err != nil {
		return nil, fmt.Errorf("codex replaced %s while it ran, and the file it names now could not be locked (%w); nothing more was published", path, err)
	}
	if !extra.HoldsSidecar(configLinkTarget(path)) {
		extra.Release()
		return nil, fmt.Errorf("%s changed again while its new lock was taken; nothing more was published", path)
	}
	return extra, nil
}

// configIdentityAfterRunner is recheck for a command that runs the CLI once: it answers the extra lock to release, nil when
// the held one still guards the path.
func configIdentityAfterRunner(lock *crwdir.ConfigLock, path string, dir os.FileInfo) (*crwdir.ConfigLock, error) {
	return (&configLocks{main: lock}).acquire(path, dir)
}
