package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gateSession = "019a0000-0000-7000-8000-000000000001"

// #133: the predicate is "the SOURCE identity is unavailable", not "there is no .git"; written the other way it would reject
// the split-cwd case at the door.
func TestANonGitWorkspaceHasNoResolvableSourceIdentity(t *testing.T) {
	base := hermetic(t)
	cwd := filepath.Join(base, "plain")
	must(t, os.Mkdir(cwd, 0o755))
	got := CheckBound(cwd, gateSession)
	if got.OK || got.Cause != CauseUnavailable {
		t.Fatalf("a non-git workspace without a binding: %+v", got)
	}
	// The message must offer both exits, or the user is told to stop without being told how.
	for _, want := range []string{"crw relay session source", "unbound", "CHECK-BINDING-01"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, got.Reason)
		}
	}
}

func TestACleanGitWorkspaceResolves(t *testing.T) {
	cwd := newRepo(t, hermetic(t), "clean")
	if got := CheckBound(cwd, gateSession); !got.OK || got.Reason != "" || got.Cause != CauseNone {
		t.Fatalf("%+v", got)
	}
}

// Dirty is a resolved identity: confusing it with unavailable would refuse every real working session.
func TestADirtyGitWorkspaceStillResolves(t *testing.T) {
	cwd := newRepo(t, hermetic(t), "dirty")
	must(t, os.WriteFile(filepath.Join(cwd, "tracked.txt"), []byte("changed\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(cwd, "untracked.txt"), []byte("new\n"), 0o644))
	if got := CheckBound(cwd, gateSession); !got.OK {
		t.Fatalf("%+v", got)
	}
}

// CRW-145: a corrupt binding is a binding error, not an unavailable source: binding a tree cannot clear it (Bind refuses the
// same file), so the cause tells a caller not to offer the bind command, and the reason is the binding's own refusal.
func TestACorruptBindingIsABindingError(t *testing.T) {
	base := hermetic(t)
	cwd := filepath.Join(base, "plain")
	must(t, os.MkdirAll(filepath.Join(cwd, ".crw", SourcesSubdir), 0o755))
	must(t, os.WriteFile(filepath.Join(cwd, ".crw", SourcesSubdir, gateSession+".json"), []byte("{not json"), 0o644))
	got := CheckBound(cwd, gateSession)
	if got.OK || got.Cause != CauseBindingError || got.Reason != "Source binding is unreadable or corrupt; existing bytes were preserved." {
		t.Fatalf("%+v", got)
	}
}
