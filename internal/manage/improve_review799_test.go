package manage

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests here are the two P0s of the review of the pull request that added the input
// identity guard: the guard looked at a different file than the reader opens, and the check
// immediately before the rename forgot the identity of the input it had already read.
//
// Every path is temporary and the store is a synthetic SQLite file this package builds, so
// nothing reaches a real store, configuration or home.

// improveReview799Store is a synthetic relay store holding one refusal row.
func improveReview799Store(t *testing.T, s *improveTestState) {
	t.Helper()
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) {
		improveTestInsert(t, db, "INSERT INTO refusals (at, relationship_id, event_id, reason, detail) VALUES ('2026-10-06T01:00:00Z','rel-a','ev-a','manifest_forbidden','detail')")
	})
}

// improveReview799OutDir is a directory beside the state directory, so an output there is
// under none of the configured sources.
func improveReview799OutDir(t *testing.T, s *improveTestState) string {
	t.Helper()
	dir := filepath.Join(s.root, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestImproveReview799LinkDotDotStoreIsProtected covers the first P0: the DAG source is spelled
// through a link and then "..", so a lexical clean names one file while the reader resolves the
// components from the left and opens another. The output names the file the reader opens, so
// the guard has to resolve the same way or the final rename replaces the store with the bundle.
func TestImproveReview799LinkDotDotStoreIsProtected(t *testing.T) {
	s := improveTestSetup(t)
	real := filepath.Join(s.root, "real")
	subdir := filepath.Join(real, "subdir")
	base := filepath.Join(s.root, "base")
	for _, dir := range []string{subdir, base} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(subdir, filepath.Join(base, "link")); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	storePath := filepath.Join(real, improveStoreFile)
	improveInputSeedPlanAt(t, storePath, "p1", "a")
	// The spelling is built by hand: filepath.Join would clean the ".." away, which is exactly
	// the difference this test is about.
	spelled := base + "/link/../" + improveStoreFile
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{"dag": map[string]any{"path": spelled, "pattern": "p1"}},
	}}})
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := improveTestRun(t, s, "--out", storePath)
	if code != 1 {
		t.Fatalf("a link-and-dotdot DAG source with the store as --out: exit %d, stderr %q, want a refusal", code, stderr)
	}
	after, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("the refused run removed the store: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused run replaced the store the reader opens (%d bytes -> %d bytes)", len(before), len(after))
	}
}

// TestImproveReview799InputMovedToOutputIsRefused covers the second P0: the store the collection
// already read is moved onto the output's place between the read and the rename. The check there
// has to compare against the identity recorded when the input was opened, not a name list rebuilt
// at that moment, because the old name no longer names anything.
func TestImproveReview799InputMovedToOutputIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	dir := improveReview799OutDir(t, s)
	out := filepath.Join(dir, "bundle.json")
	improveInputRelayConfig(t, s)
	before, err := os.ReadFile(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		moved = true
		if err := os.Rename(s.dbPath, out); err != nil {
			t.Errorf("moving the store onto the output: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !moved {
		t.Fatalf("the pre-rename comparison ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an input moved onto the output: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the refused run removed the moved store: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused run replaced the moved store: %d bytes -> %d bytes", len(before), len(after))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bundle.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the output directory holds %v, want the moved store only (no temporary file)", names)
	}
}

// TestImproveReview799InputChangedDuringReadIsRefused covers the identity the open descriptor
// pins: a path that names a different file after the read is not the file the bundle was built
// from, so the collection refuses instead of writing it.
func TestImproveReview799InputChangedDuringReadIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	dir := improveReview799OutDir(t, s)
	out := filepath.Join(dir, "bundle.json")
	improveInputRelayConfig(t, s)
	replaced := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		replaced = true
		if err := os.Remove(s.dbPath); err != nil {
			t.Errorf("removing the store: %v", err)
		}
		if err := os.WriteFile(s.dbPath, []byte("replaced"), 0o600); err != nil {
			t.Errorf("replacing the store: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !replaced {
		t.Fatalf("the pre-rename check ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("an input replaced after the read: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonInputChanged)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote a bundle (stat err %v)", err)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Errorf("the refused run left %d entries in the output directory, want none", len(entries))
	}
}

// TestImproveReview799StatFailureFailsClosed covers the comparison the guard cannot make: a
// destination that cannot be examined for a reason other than its absence is a refusal, never a
// pass. root ignores the permission bits this relies on, so the test skips there.
func TestImproveReview799StatFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveInputRelayConfig(t, s)
	locked := filepath.Join(s.root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(locked, "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonOutputUnreadable) {
		t.Fatalf("an output that cannot be examined: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputUnreadable)
	}
}

// TestImproveReview799OrdinaryCollectStillWrites is the control: a link-free path with its own
// output directory still writes the bundle.
func TestImproveReview799OrdinaryCollectStillWrites(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveInputRelayConfig(t, s)
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", out); code != 0 {
		t.Fatalf("an ordinary link-free collect: exit %d, stderr %s", code, stderr)
	}
	if bundle := improveTestReadBundle(t, out); bundle.Schema != improveBundleSchema {
		t.Errorf("the ordinary output was not written: %+v", bundle)
	}
}
