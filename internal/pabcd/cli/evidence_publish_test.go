package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// These are the CRW-744 cases: evidence resolve must follow the state it published. When WriteState reports that the state
// reached the final path before a directory sync failed (state.Published), the tombstone is already gone, so the resolve
// clears the attempt record too and exits 0 with a warning instead of leaving the counter at the cap for good.

// evidencePublishSeed builds a session with three recorded attempts, one resolvable tombstone and a valid receipt, with the
// real-state roots pinned to a temporary home. It returns the workspace and the resolve request with the seam unset.
func evidencePublishSeed(t *testing.T) (string, EvidenceResolveArgs) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	cwd := t.TempDir()
	s := state.DefaultState("rec-s1", "keep")
	s.Phase = state.PhaseB
	s.UnverifiedSubagents = []state.UnverifiedSubagent{cliVerdict("a1", "t1")}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	cliPut(t, filepath.Join(cwd, ".crw/evidence/check.md"), "verified")
	if !evidence.WriteAttempts(cwd, "rec-s1", "a1", 3, "t1") {
		t.Fatal("seed counter failed")
	}
	return cwd, EvidenceResolveArgs{Verb: "resolve", SessionID: "rec-s1", AgentID: "a1", Receipt: ".crw/evidence/check.md", Cwd: cwd}
}

// evidencePublishRoots lists the files under a home-relative root, so a test can prove the run touched no real state.
func evidencePublishRoots(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, path)
		}
		return nil
	})
	sort.Strings(names)
	return names
}

func TestEvidenceResolvePublishedDirectorySyncFailureClearsAttempts(t *testing.T) {
	cwd, a := evidencePublishSeed(t)
	codexHome, crwHome := os.Getenv("CODEX_HOME"), os.Getenv("CRW_HOME")
	beforeCodex, beforeCrw := evidencePublishRoots(t, codexHome), evidencePublishRoots(t, crwHome)
	a.writeState = func(c string, s state.State) error {
		if err := state.WriteState(c, s); err != nil {
			t.Fatalf("the seam must publish the state first: %v", err)
		}
		return &state.PublishedError{Err: syscall.EIO}
	}
	out, code := RunEvidenceCLI(a)
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, out)
	}
	lines := strings.Split(out, "\n")
	want := "evidence resolve: agent a1 resolved against .crw/evidence/check.md"
	if len(lines) != 2 || lines[0] != want {
		t.Fatalf("output = %q", out)
	}
	if !strings.HasPrefix(lines[1], "evidence resolve: warning: the session state was published but its directory sync failed: ") {
		t.Fatalf("warning line = %q", lines[1])
	}
	if !strings.Contains(lines[1], "input/output error") {
		t.Fatalf("warning line does not carry the cause: %q", lines[1])
	}
	if evidence.HasTombstone(cwd, "rec-s1", evidence.Payload{AgentID: "a1", TurnID: "t1"}) {
		t.Fatal("tombstone was not removed from the published state")
	}
	if evidence.HasSpentBudget(cwd, "rec-s1") {
		t.Fatal("attempt record left spent after the published write")
	}
	if got := evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1"); got != 0 {
		t.Fatalf("attempts = %d, want 0", got)
	}
	if got := evidencePublishRoots(t, codexHome); strings.Join(got, "\n") != strings.Join(beforeCodex, "\n") {
		t.Fatalf("real CODEX_HOME changed: %v -> %v", beforeCodex, got)
	}
	if got := evidencePublishRoots(t, crwHome); strings.Join(got, "\n") != strings.Join(beforeCrw, "\n") {
		t.Fatalf("real CRW_HOME changed: %v -> %v", beforeCrw, got)
	}
}

func TestEvidenceResolvePrePublicationFailureKeepsAttempts(t *testing.T) {
	cwd, a := evidencePublishSeed(t)
	a.writeState = func(string, state.State) error { return syscall.EIO }
	out, code := RunEvidenceCLI(a)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.HasPrefix(out, "evidence resolve: ") || !strings.Contains(out, "input/output error") {
		t.Fatalf("output = %q", out)
	}
	if !evidence.HasTombstone(cwd, "rec-s1", evidence.Payload{AgentID: "a1", TurnID: "t1"}) {
		t.Fatal("tombstone removed on a pre-publication failure")
	}
	if got := evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1"); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if !evidence.HasSpentBudget(cwd, "rec-s1") {
		t.Fatal("budget should still be spent after a pre-publication failure")
	}
}
