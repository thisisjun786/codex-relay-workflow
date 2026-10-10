package testsupport

import (
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

// Rebase takes the current listing as the one later changes are measured against.
func (h *AccountHomes) Rebase() { h.before = h.Listing() }

// Listing is the top-level names below each directory the code could take for an account home, sorted, with
// an absent directory marked.
func (h *AccountHomes) Listing() string {
	parts := []string{}
	dirs := []string{filepath.Join(h.Home, ".codex"), filepath.Join(h.Home, ".crw"), h.Codex, h.CRW}
	if h.watchHome {
		dirs = append([]string{h.Home}, dirs...)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		label := strings.TrimPrefix(dir, h.Home)
		if dir == h.Home {
			label = "HOME"
		} else if label == dir {
			label = filepath.Base(dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			parts = append(parts, label+": absent")
			continue
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		slices.Sort(names)
		parts = append(parts, label+": "+strings.Join(names, ","))
	}
	return strings.Join(parts, " | ")
}

// Verify reports through fail when the listing differs from the one Rebase took.
func (h *AccountHomes) Verify(fail func(string)) {
	if after := h.Listing(); after != h.before {
		fail(fmt.Sprintf("the run changed an account home it was given:\nbefore %s\nafter  %s", h.before, after))
	}
}
