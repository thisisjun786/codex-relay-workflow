package trace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ErrLiveStore is the refusal of a path that is a live relay store: a file under a live state directory, or any file that
// is the same file as one of the live store's files (a hard link, or a symbolic link that resolves to one). The tools of this
// package work on copies and traces only; none of them opens, reads or writes the store a relay is serving.
var ErrLiveStore = errors.New("refused: the path is a live relay store; work on a copy of it")

const (
	// liveWalkDepth bounds how far below a live state directory the identity check looks for the live store's files.
	liveWalkDepth = 3
	// liveWalkFiles bounds how many files of a live state directory the identity check compares.
	liveWalkFiles = 4096
)

// RefuseLive returns ErrLiveStore (wrapped with the reason) when path is a live relay store, nil when it is not. The path may
// not exist yet (an output file). The live stores are the ones the relay itself would use in this process's environment:
// the selection the relay makes with no flags (--state, CODEX_SESSION_RELAY_STATE, the configured relay_state, the XDG or
// home base scoped by the default socket) and the codex-session-relay directory beside it, and the XDG and home
// directories whichever of them the environment selects. A path is live when it lies under one of them, or when it is the
// same file (device and inode) as a regular file in one of them: the identity check is what catches a hard link or a
// symbolic link placed outside the directory. A store id cannot be the test: a faithful copy of a store carries its id.
func RefuseLive(path string) error {
	if path == "" {
		return errors.New("a path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	resolved, err := store.Realpath(abs)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	roots := liveRoots()
	for _, root := range roots {
		if within(root, resolved) || within(root, abs) {
			return fmt.Errorf("%w (%s is under the live state directory %s)", ErrLiveStore, path, root)
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("examine %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	for _, root := range roots {
		if live, ok := sameAsLiveFile(root, info); ok {
			return fmt.Errorf("%w (%s is the same file as %s)", ErrLiveStore, path, live)
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// liveRoots are the directories whose files are live relay state, resolved and without repeats.
func liveRoots() []string {
	var candidates []string
	if sel, err := store.ResolveStateDir("", ""); err == nil {
		candidates = append(candidates, sel.Path)
		if sel.Source != "env" && sel.Source != "flag" {
			candidates = append(candidates, filepath.Dir(sel.Path))
		}
	}
	if v := os.Getenv("CODEX_SESSION_RELAY_STATE"); v != "" {
		candidates = append(candidates, v)
	}
	if configured, err := crwconfig.ConfiguredRelayState(os.Getenv); err == nil && configured != "" {
		candidates = append(candidates, configured)
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		candidates = append(candidates, filepath.Join(base, "codex-session-relay"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "state", "codex-session-relay"))
	}
	var roots []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		real, err := store.Realpath(abs)
		if err != nil {
			real = abs
		}
		if !seen[real] {
			seen[real] = true
			roots = append(roots, real)
		}
	}
	return roots
}

// sameAsLiveFile reports whether info is the identity of a regular file below root, and which one.
func sameAsLiveFile(root string, info os.FileInfo) (string, bool) {
	var found string
	files := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rel, rerr := filepath.Rel(root, p); rerr == nil && rel != "." && strings.Count(rel, string(os.PathSeparator)) >= liveWalkDepth {
				return fs.SkipDir
			}
			return nil
		}
		if files++; files > liveWalkFiles {
			return fs.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		other, err := d.Info()
		if err == nil && os.SameFile(info, other) {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	return found, found != ""
}
