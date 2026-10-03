package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The four tests of session-split.test.ts (issue #48: the same --session id resolves to different state depending on where the
// process started, so the split is made visible by detection only), over state.FindForeignSessionCopies.

func treeWithSession(t *testing.T, id string) string {
	t.Helper()
	root := t.TempDir()
	path := state.StatePath(root, id)
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := state.Encode(state.DefaultState(id, ""))
	must(t, err)
	must(t, os.WriteFile(path, data, 0o644))
	return root
}

func TestASessionIdPresentInASiblingTreeIsReported(t *testing.T) {
	mine, other := treeWithSession(t, "s-dup"), treeWithSession(t, "s-dup")
	found := state.FindForeignSessionCopies(mine, "s-dup", []string{other})
	if len(found) != 1 || !strings.HasSuffix(found[0], "s-dup.json") || !strings.HasPrefix(found[0], other) {
		t.Fatalf("must name the other tree, not this one: %v", found)
	}
}

func TestTheCallersOwnTreeIsNeverReportedAsForeign(t *testing.T) {
	mine := treeWithSession(t, "s-self")
	if found := state.FindForeignSessionCopies(mine, "s-self", []string{mine}); len(found) != 0 {
		t.Fatalf("a self-warning: %v", found)
	}
}

func TestACandidateWithoutThatSessionIsNotReported(t *testing.T) {
	mine := treeWithSession(t, "s-only")
	if found := state.FindForeignSessionCopies(mine, "s-only", []string{t.TempDir()}); len(found) != 0 {
		t.Fatalf("%v", found)
	}
}

func TestUnreadableOrMissingCandidatesAreSkippedNotThrown(t *testing.T) {
	mine := treeWithSession(t, "s-safe")
	if found := state.FindForeignSessionCopies(mine, "s-safe", []string{filepath.Join(t.TempDir(), "does-not-exist"), ""}); len(found) != 0 {
		t.Fatalf("%v", found)
	}
}
