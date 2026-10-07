package manage

import (
	"bytes"
	"context"
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

// TestImproveReview799DraftEntryUnderALinkedDirectoryIsProtected covers the draft child the reader
// opens: the drafts directory is spelled through a link and "..", so the reader enumerates one
// place and opens the joined spelling. The destination names the file a draft entry links to, so
// only the recorded child identity refuses it.
func TestImproveReview799DraftEntryUnderALinkedDirectoryIsProtected(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	base := filepath.Join(s.root, "base")
	real := filepath.Join(s.root, "real")
	listed := filepath.Join(s.root, "drafts")
	opened := filepath.Join(base, "drafts")
	for _, dir := range []string{base, real, listed, opened} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real, filepath.Join(base, "link")); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	// The configured spelling reaches root/drafts, so the enumeration sees that directory, but the
	// reader opens each entry at the path filepath.Join builds from the spelling, which cleans the
	// ".." away and lands in root/base/drafts. The file the reader opens is therefore the one under
	// root/base/drafts, and that is the file the guard has to record.
	improveTestWrite(t, filepath.Join(listed, "one.json"), "{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":\"enumerated\",\"project\":\"p\",\"title\":\"t\"}\n")
	target := filepath.Join(opened, "one.json")
	improveTestWrite(t, target, "{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":\"opened\",\"project\":\"p\",\"title\":\"t\"}\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": base + "/link/../drafts"},
		},
	}}})
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := improveTestRun(t, s, "--out", target)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("an output naming the draft the reader opens: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the refused run removed the draft: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused run replaced the draft it read: %q -> %q", before, after)
	}
}

// TestImproveReview799OutputParentSwapIsRefused covers the directory the destination reaches: the
// output's parent is replaced with a link into a configured source directory while the bundle is
// written, so the destination must be resolved again before the last comparison rather than
// trusting the plan's parent.
func TestImproveReview799OutputParentSwapIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	out := improveReview799OutDir(t, s)
	improveInputRelayConfig(t, s)
	swapped := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		swapped = true
		// The output directory now leads into the configured state directory.
		if err := os.RemoveAll(out); err != nil {
			t.Errorf("removing the output directory: %v", err)
		}
		if err := os.Symlink(s.stateDir, out); err != nil {
			t.Errorf("relinking the output directory: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(out, "bundle.json"))
	if !swapped {
		t.Fatalf("the pre-rename comparison ran without the seam: exit %d, stderr %q", code, stderr)
	}
	// The destination is resolved again before the last comparison, so the moved parent is caught
	// either by the parent identity check or by the containment check.
	if code != 1 || !(strings.Contains(stderr, improveReasonOutputParent) || strings.Contains(stderr, improveReasonOutputIsInput)) {
		t.Fatalf("an output whose parent moved into a source: exit %d, stderr %q, want a named refusal", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "bundle.json")); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote the bundle into the source directory (stat err %v)", err)
	}
}

// TestImproveReview799CheckpointedSidecarDoesNotAbort covers a routine WAL checkpoint: SQLite
// unlinks a store's -wal and -shm when the last connection closes, which leaves the database's
// committed state alone, so the disappearance of a recorded sidecar is not a changed input.
func TestImproveReview799CheckpointedSidecarDoesNotAbort(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	// The sidecars exist before the run, so they are recorded as inputs, and are then unlinked the
	// way a clean checkpoint unlinks them.
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm"} {
		if err := os.WriteFile(sidecar, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	improveInputRelayConfig(t, s)
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm"} {
			if err := os.Remove(sidecar); err != nil {
				t.Errorf("removing the sidecar %s: %v", sidecar, err)
			}
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 0 {
		t.Fatalf("a checkpointed sidecar aborted the run: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the run wrote no bundle: %v", err)
	}
}

// TestImproveReview799UnpinnableInputFailsClosed covers a failure to record an identity: an input
// that exists but cannot be opened is refused rather than recorded without an identity, because an
// input the collection cannot pin is one it cannot prove it will not overwrite. root ignores the
// permission bits this relies on, so the test skips there.
func TestImproveReview799UnpinnableInputFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	ledger := filepath.Join(s.root, "ledger.jsonl")
	improveTestWrite(t, ledger, "{\"mode\":\"pr\",\"issue\":\"CRW-1\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T01:00:00Z\"}\n")
	if err := os.Chmod(ledger, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ledger, 0o600) })
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": ledger},
		},
	}}})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(improveReview799OutDir(t, s), "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("an input that could not be pinned: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonInputChanged)
	}
}

// TestImproveReview799RetargetedSourceAliasIsRefused covers two configured sources that resolve to
// one file at first: each spelling is its own input, so a link at one of them that is retargeted
// after recording is refused rather than left unpinned while its reader opens the new file.
func TestImproveReview799RetargetedSourceAliasIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	data := filepath.Join(s.root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(data, "a.jsonl")
	improveTestWrite(t, ledger, "{\"mode\":\"pr\",\"issue\":\"CRW-1\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T01:00:00Z\"}\n")
	current := filepath.Join(data, "current.jsonl")
	if err := os.Symlink(ledger, current); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	out := filepath.Join(data, "b.jsonl")
	improveTestWrite(t, out, "{\"mode\":\"pr\",\"issue\":\"CRW-2\",\"status\":\"ok\",\"graded_at\":\"2026-10-06T02:00:00Z\"}\n")
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay":        map[string]any{"path": s.stateDir},
			"audit":        map[string]any{"path": ledger},
			"intervention": map[string]any{"path": current},
		},
	}}})
	retargeted := false
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		retargeted = true
		if err := os.Remove(current); err != nil {
			t.Errorf("removing the alias: %v", err)
		}
		if err := os.Symlink(out, current); err != nil {
			t.Errorf("retargeting the alias: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !retargeted {
		t.Fatalf("the pre-rename check ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("a source alias retargeted after recording: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonInputChanged)
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the refused run removed the destination: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused run replaced the file the alias was retargeted to: %q -> %q", before, after)
	}
}

// TestImproveReview799DraftAppearingAfterThePlanIsPinned covers the refresh the collection runs
// after each reader: a draft that appears while a reader runs is recorded again, so the guard still
// refuses a destination that names the file it links to. The window between two enumerations has no
// seam of its own, so the refresh step is driven directly, as the collection drives it.
func TestImproveReview799DraftAppearingAfterThePlanIsPinned(t *testing.T) {
	s := improveTestSetup(t)
	drafts := filepath.Join(s.root, "drafts")
	archive := filepath.Join(s.root, "archive")
	for _, dir := range []string{drafts, archive} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(archive, "new.json")
	improveTestWrite(t, target, "{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":\"new\",\"project\":\"p\",\"title\":\"t\"}\n")
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindDraft: {Path: drafts}}}
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		t.Fatalf("the first recording: %v", err)
	}
	if err := ids.improveIdentityRefuse(target, nil); err != nil {
		t.Fatalf("the destination was refused before the draft appeared: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(drafts, "new.json")); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	if err := ids.improveIdentityRefresh(section); err != nil {
		t.Fatalf("the refresh after the reader: %v", err)
	}
	if err := ids.improveIdentityRefuse(target, nil); err == nil {
		t.Errorf("a draft that appeared after the first recording was not refused")
	}
}

// TestImproveReview799CollectHoldsTheDraftTheReaderOpened covers the wiring: the identity set the
// collection returns holds the draft its reader opened, so the guard refuses a destination that
// names that file.
func TestImproveReview799CollectHoldsTheDraftTheReaderOpened(t *testing.T) {
	s := improveTestSetup(t)
	drafts := filepath.Join(s.root, "drafts")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(drafts, "one.json")
	improveTestWrite(t, draft, "{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":\"one\",\"project\":\"p\",\"title\":\"t\"}\n")
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindDraft: {Path: drafts}}}
	_, ids, err := improveCollect(context.Background(), section, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRefuse(draft, nil); err == nil {
		t.Errorf("the identity set does not hold the draft the reader opened")
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
