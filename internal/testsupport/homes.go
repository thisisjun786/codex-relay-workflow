package testsupport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// AccountHomes is the set of account homes one test hands to the code under test: HOME, CODEX_HOME and
// CRW_HOME, each a temporary directory the test owns. A test that must show a run reached no account home
// observes these directories and no others. The real ~/.codex and ~/.crw are shared with every other process
// of the host (a live Codex session opens its sqlite files there at any moment), so a listing of them
// cannot tell this run's writes from theirs; the test's own directories can only change by this process.
type AccountHomes struct {
	Home, Codex, CRW string
	before           string
	watchHome        bool
	content          bool
}

// SandboxAccountHomes points HOME, CODEX_HOME and CRW_HOME at fresh temporary directories for the test (t
// restores them) and has the test fail, at its end, if anything was created or removed at the top level of
// the directories the code could take for an account home: HOME/.codex, HOME/.crw, CODEX_HOME and CRW_HOME.
// A test that puts fixtures there calls Rebase once they are in place.
func SandboxAccountHomes(t *testing.T) *AccountHomes {
	t.Helper()
	h := &AccountHomes{Home: t.TempDir(), Codex: t.TempDir(), CRW: t.TempDir()}
	t.Setenv("HOME", h.Home)
	t.Setenv("CODEX_HOME", h.Codex)
	t.Setenv("CRW_HOME", h.CRW)
	h.Rebase()
	t.Cleanup(func() {
		h.Verify(func(msg string) { t.Error(msg) })
	})
	return h
}

// WatchAccountHomes observes homes a test hands to a child process through its environment (a real shell
// started with HOME, CODEX_HOME and CRW_HOME set to these directories), which SandboxAccountHomes does not
// reach because the child never sees the process's own variables. It sets no variable. The test fails, at its
// end, if anything was created or removed at the top level of home, home/.codex, home/.crw, codex and crw; the
// top level of home is watched too, since the child's HOME is the directory its own startup would write to.
// The directories exist when it is called; a test that puts fixtures there calls Rebase once they are in place.
// An empty codex or crw is not watched: a command that writes one of its homes on purpose (a config rewrite
// with its backup) has the test check that home's contents itself.
func WatchAccountHomes(t *testing.T, home, codex, crw string) *AccountHomes {
	t.Helper()
	h := &AccountHomes{Home: home, Codex: codex, CRW: crw, watchHome: true}
	h.Rebase()
	t.Cleanup(func() {
		h.Verify(func(msg string) { t.Error(msg) })
	})
	return h
}

// Rebase takes the current state as the one later changes are measured against: the name-only listing, or the
// content snapshot once Snapshot has been called.
func (h *AccountHomes) Rebase() { h.before = h.state() }

// Snapshot switches the homes to the content snapshot and takes it as the baseline. A home that holds fixtures
// calls it once they are in place: an in-place edit of a fixture keeps its name, so the name-only listing does
// not see it. The snapshot walks each directory the code could take for an account home, recursively, and records
// every entry's relative path, mode and, for a regular file, its sha256 digest or, for a symlink, its target. A watched
// directory that is itself a symlink is recorded with its target and walked through the directory it resolves to.
// HOME itself keeps its top-level names only: a home that a command rewrites on purpose may sit below it (the retrust
// fixture keeps CODEX_HOME at HOME/codex), and a recursion into HOME would read that intended rewrite as a stray write.
func (h *AccountHomes) Snapshot() {
	h.content = true
	h.Rebase()
}

// Listing is the top-level names below each directory the code could take for an account home, sorted, with
// an absent directory marked.
func (h *AccountHomes) Listing() string {
	parts := []string{}
	for _, dir := range h.dirs() {
		parts = append(parts, h.label(dir)+": "+topLevelNames(dir))
	}
	return strings.Join(parts, " | ")
}

// topLevelNames is the sorted names directly below dir, or "absent".
func topLevelNames(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "absent"
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// Contents is the recursive snapshot of each watched directory, one line per path, with an absent directory
// marked. The recursion covers every directory the code could take for an account home except HOME, which
// contributes its top-level names as Listing lists them.
func (h *AccountHomes) Contents() string {
	parts := []string{}
	for _, dir := range h.dirs() {
		label := h.label(dir)
		if _, err := os.Stat(dir); err != nil {
			parts = append(parts, label+": absent")
			continue
		}
		if dir == h.Home {
			parts = append(parts, label+": "+topLevelNames(dir))
			continue
		}
		// A watched directory that is itself a symlink keeps its own line (mode and target) and the walk starts at
		// the directory it resolves to: WalkDir does not follow a link at its root, and the fixtures behind it
		// would go unread.
		var entries []string
		root := dir
		if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				parts = append(parts, label+": unreadable: "+err.Error())
				continue
			}
			entries = append(entries, label+" "+info.Mode().String()+" -> "+target)
			if root, err = filepath.EvalSymlinks(dir); err != nil {
				parts = append(parts, strings.Join(entries, "\n")+"\n"+label+": unreadable: "+err.Error())
				continue
			}
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			name := label
			if rel != "." {
				name += "/" + filepath.ToSlash(rel)
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			entry := name + " " + info.Mode().String()
			switch {
			case info.Mode().IsRegular():
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(raw)
				entry += " sha256:" + hex.EncodeToString(sum[:])
			case info.Mode()&os.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				entry += " -> " + target
			}
			entries = append(entries, entry)
			return nil
		})
		if err != nil {
			entries = append(entries, label+": unreadable: "+err.Error())
		}
		parts = append(parts, strings.Join(entries, "\n"))
	}
	return strings.Join(parts, "\n")
}

// Verify reports through fail when the state differs from the one Rebase took.
func (h *AccountHomes) Verify(fail func(string)) {
	if after := h.state(); after != h.before {
		fail(fmt.Sprintf("the run changed an account home it was given:\nbefore %s\nafter  %s", h.before, after))
	}
}

// state is what Rebase and Verify compare: the content snapshot of a home that holds fixtures, else the listing.
func (h *AccountHomes) state() string {
	if h.content {
		return h.Contents()
	}
	return h.Listing()
}

// dirs are the directories the code could take for an account home, HOME first when the watch covers it; an
// unnamed directory is left out.
func (h *AccountHomes) dirs() []string {
	dirs := []string{filepath.Join(h.Home, ".codex"), filepath.Join(h.Home, ".crw"), h.Codex, h.CRW}
	if h.watchHome {
		dirs = append([]string{h.Home}, dirs...)
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// label names a watched directory in the listing: HOME for the home, the path under it for a directory below it,
// and the base name for one elsewhere.
func (h *AccountHomes) label(dir string) string {
	if dir == h.Home {
		return "HOME"
	}
	if rel := strings.TrimPrefix(dir, h.Home); rel != dir {
		return rel
	}
	return filepath.Base(dir)
}
