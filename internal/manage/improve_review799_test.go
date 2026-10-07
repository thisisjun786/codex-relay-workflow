package manage

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	// The configured spelling reaches root/drafts, so a directory enumeration sees that directory,
	// but the reader opens each entry at the path filepath.Join builds from the spelling, which
	// cleans the ".." away and lands in root/base/drafts. Both are ordinary files holding different
	// drafts, so the file the reader opens is the one under root/base/drafts, and that is the file
	// the guard has to record.
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
	// The retargeted alias reaches the destination, so the refusal names either the changed input or
	// the output that is now an input: both are correct, and what matters is that the run refuses
	// and the file is untouched.
	if code != 1 || !(strings.Contains(stderr, improveReasonInputChanged) || strings.Contains(stderr, improveReasonOutputIsInput)) {
		t.Fatalf("a source alias retargeted after recording: exit %d, stderr %q, want a named refusal", code, stderr)
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the refused run removed the destination: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused run replaced the file the alias was retargeted to: %q -> %q", before, after)
	}
}

// TestImproveReview799DraftAppearingDuringTheReadIsPinned covers the refresh the collection runs
// after each reader: a draft that appears while the drafts reader runs is recorded again, so the
// guard still refuses a destination that names the file it links to. The whole collection runs, so
// the reader itself consumes the draft and the refresh the collection performs is what pins it.
func TestImproveReview799DraftAppearingDuringTheReadIsPinned(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	drafts := filepath.Join(s.root, "drafts")
	archive := filepath.Join(s.root, "archive")
	for _, dir := range []string{drafts, archive} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(archive, "new.json")
	improveTestWrite(t, target, "{\"schema\":\"crw-issue-draft/1\",\"fingerprint\":\"new\",\"project\":\"p\",\"title\":\"t\"}\n")
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"draft": map[string]any{"path": drafts},
		},
	}}})
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	// The draft appears exactly where the collection re-examines the inputs after the first reader
	// returns, so the drafts reader that follows takes it, and the refresh has to pin it. The
	// destination names the file that draft links to, which is the file the bundle must not
	// replace.
	appeared := false
	previous := improveInputAfterRead
	improveInputAfterRead = func() {
		if appeared {
			return
		}
		appeared = true
		if err := os.Symlink(target, filepath.Join(drafts, "new.json")); err != nil {
			t.Errorf("creating the draft: %v", err)
		}
	}
	t.Cleanup(func() { improveInputAfterRead = previous })
	code, _, stderr := improveTestRun(t, s, "--out", target)
	if !appeared {
		t.Fatalf("the post-read check ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
		t.Fatalf("a draft that appeared during the read: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputIsInput)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the refused run removed the draft: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused run replaced the draft the reader took: %q -> %q", before, after)
	}
}

// TestImproveReview799ConfiguredDirectoryIsPinned covers the configured directories the issue names
// as inputs: each is opened and held like any other input, so a destination that is a file inside
// one of them, or that replaces one, is refused by identity rather than by spelling alone.
func TestImproveReview799ConfiguredDirectoryIsPinned(t *testing.T) {
	s := improveTestSetup(t)
	drafts := filepath.Join(s.root, "drafts")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindDraft: {Path: drafts}}}
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		t.Fatalf("the recording: %v", err)
	}
	held := false
	for _, entry := range ids.entries {
		if entry.path == drafts && entry.info != nil && entry.info.IsDir() {
			held = entry.file != nil
		}
	}
	if !held {
		t.Errorf("the configured drafts directory was not opened and held as an input")
	}
	// A destination inside the directory is refused, and so is one that shares the directory's
	// identity.
	if err := ids.improveIdentityRefuse(filepath.Join(drafts, "bundle.json"), nil); err == nil {
		t.Errorf("a destination inside the configured directory was not refused")
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

// TestImproveReview799SwappedParentLeavesNoTemporaryFile covers the cleanup a refusal owes: the
// output directory is replaced with a link into a configured source directory after the temporary
// file was written into it, so the spelling the file was created under no longer reaches it. The
// refusal must still remove the temporary file, which it does through the directory descriptor it
// holds rather than through that spelling.
func TestImproveReview799SwappedParentLeavesNoTemporaryFile(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	out := improveReview799OutDir(t, s)
	moved := filepath.Join(s.root, "out-moved")
	improveInputRelayConfig(t, s)
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		if err := os.Rename(out, moved); err != nil {
			t.Errorf("moving the output directory aside: %v", err)
		}
		if err := os.Symlink(s.stateDir, out); err != nil {
			t.Errorf("relinking the output directory: %v", err)
		}
	}
	t.Cleanup(func() { improveInputBeforeRename = previous })
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(out, "bundle.json"))
	if code != 1 {
		t.Fatalf("an output whose directory moved aside: exit %d, stderr %q, want a refusal", code, stderr)
	}
	entries, err := os.ReadDir(moved)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "improve-bundle-") {
			t.Errorf("the refusal left the temporary file %s in the directory it was written to", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "bundle.json")); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote the bundle into the source directory (stat err %v)", err)
	}
}

// TestImproveReview799AbsentInputThatAppearsIsRefused covers d1: an input that was absent when the
// collection recorded it is a name the bundle must not be written to. The name is examined again at
// every comparison, so once a link at that name reaches the output's database the run refuses at
// the comparison and at the destination check, and the database is never the bundle.
//
// The window has no seam of its own: every reader reports an absent configured source before the
// bundle would be written, so an input that appears during a read is driven here through the same
// steps the collection runs — record, then examine again — as the refresh after each reader does.
func TestImproveReview799AbsentInputThatAppearsIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	improveTestWrite(t, out, "{\"schema\":\"the database the bundle must not replace\"}\n")
	absent := filepath.Join(s.root, "dag-absent")
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindAudit: {Path: absent}}}
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		t.Fatalf("the recording: %v", err)
	}
	if err := ids.improveIdentityRefuse(out, nil); err != nil {
		t.Fatalf("the destination was refused before the input appeared: %v", err)
	}
	if err := os.Symlink(out, absent); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	if err := ids.improveIdentityVerify(); err == nil {
		t.Errorf("an input that was absent and appeared was not refused by the comparison")
	} else if !strings.Contains(err.Error(), improveReasonInputChanged) {
		t.Errorf("the comparison named %v, want %s", err, improveReasonInputChanged)
	}
	if err := ids.improveIdentityRefuse(out, nil); err == nil {
		t.Errorf("an absent input that came to reach the destination was not refused by the destination check")
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "the database the bundle must not replace") {
		t.Errorf("the destination the absent input came to name was replaced: %q", before)
	}
}

// TestImproveReview799AbsentSidecarNameIsRefusedAsOutput covers d2: the sidecar names of a store are
// refused as outputs whether or not they exist, because SQLite creates one beside the store while
// the store is read. The store here has no sidecar, so only the name check can refuse.
func TestImproveReview799AbsentSidecarNameIsRefusedAsOutput(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm", s.dbPath + "-journal"} {
		if _, err := os.Lstat(sidecar); !os.IsNotExist(err) {
			t.Fatalf("the fixture left the sidecar %s behind (stat err %v)", sidecar, err)
		}
	}
	improveInputRelayConfig(t, s)
	for _, sidecar := range []string{s.dbPath + "-wal", s.dbPath + "-shm", s.dbPath + "-journal"} {
		code, _, stderr := improveTestRun(t, s, "--out", sidecar)
		if code != 1 || !strings.Contains(stderr, improveReasonOutputIsInput) {
			t.Fatalf("an output naming the absent sidecar %s: exit %d, stderr %q, want the named refusal %s", sidecar, code, stderr, improveReasonOutputIsInput)
		}
		if _, err := os.Lstat(sidecar); !os.IsNotExist(err) {
			t.Errorf("the refused run created %s (stat err %v)", sidecar, err)
		}
	}
}

// TestImproveReview799ParentSwappedBeforeCreationIsRefused covers d3: the destination's directory
// is replaced with a link into a configured source directory just before the temporary file is
// created. The file is created on the descriptor of the directory that was checked, so the refusal
// leaves no temporary file in either directory.
func TestImproveReview799ParentSwappedBeforeCreationIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	out := improveReview799OutDir(t, s)
	moved := filepath.Join(s.root, "out-moved")
	improveInputRelayConfig(t, s)
	swapped := false
	previous := improveOutputBeforeCreate
	improveOutputBeforeCreate = func(improveOutputPlan) {
		swapped = true
		if err := os.Rename(out, moved); err != nil {
			t.Errorf("moving the output directory aside: %v", err)
		}
		if err := os.Symlink(s.stateDir, out); err != nil {
			t.Errorf("relinking the output directory: %v", err)
		}
	}
	t.Cleanup(func() { improveOutputBeforeCreate = previous })
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(out, "bundle.json"))
	if !swapped {
		t.Fatalf("the pre-creation check ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 {
		t.Fatalf("an output whose directory was replaced before creation: exit %d, stderr %q, want a refusal", code, stderr)
	}
	for _, dir := range []string{moved, s.stateDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "improve-bundle-") {
				t.Errorf("the refusal left the temporary file %s in %s", entry.Name(), dir)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "bundle.json")); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote the bundle into the source directory (stat err %v)", err)
	}
}

// TestImproveReview799InputReplacedRightAfterTheReadIsRefused covers the check C2 promises after
// each reader returns: an input whose path reaches a different file in that window is refused
// there, before the bundle is written, not only at the rename. The seam runs exactly where the
// collection re-examines the inputs, so a build without that examination fails this test even
// though the pre-rename comparison would still be there.
func TestImproveReview799InputReplacedRightAfterTheReadIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	improveInputRelayConfig(t, s)
	replaced := false
	previous := improveInputAfterRead
	improveInputAfterRead = func() {
		replaced = true
		if err := os.Remove(s.dbPath); err != nil {
			t.Errorf("removing the store: %v", err)
		}
		if err := os.WriteFile(s.dbPath, []byte("replaced"), 0o600); err != nil {
			t.Errorf("replacing the store: %v", err)
		}
	}
	t.Cleanup(func() { improveInputAfterRead = previous })
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if !replaced {
		t.Fatalf("the post-read check ran without the seam: exit %d, stderr %q", code, stderr)
	}
	if code != 1 || !strings.Contains(stderr, improveReasonInputChanged) {
		t.Fatalf("an input replaced right after the read: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonInputChanged)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote a bundle (stat err %v)", err)
	}
}

// TestImproveReview799ParentReplacedBeforeTheWriteIsRefused covers the directory the write actually
// lands in: the plan is taken, the destination's spelling is then made to reach a configured source
// directory, and the write refuses because the directory it opened is not the one the plan named.
func TestImproveReview799ParentReplacedBeforeTheWriteIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	out := improveReview799OutDir(t, s)
	improveInputRelayConfig(t, s)
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindRelay: {Path: s.stateDir}}}
	plan, err := improvePlanOutput(filepath.Join(out, "bundle.json"))
	if err != nil {
		t.Fatalf("the plan: %v", err)
	}
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		t.Fatalf("the recording: %v", err)
	}
	// The spelling now reaches the configured source directory, while the plan named the real one.
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.stateDir, out); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	err = improveWriteFile(plan, ids, []byte("{}\n"))
	if err == nil {
		t.Fatalf("a parent replaced before the write was not refused")
	}
	if !strings.Contains(err.Error(), improveReasonOutputParent) {
		t.Errorf("the refusal named %v, want %s", err, improveReasonOutputParent)
	}
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "improve-bundle-") || entry.Name() == "bundle.json" {
			t.Errorf("the refused write left %s in the source directory", entry.Name())
		}
	}
}

// TestImproveReview799UnreadableOutputSpellingFailsClosed covers the destination's own examination:
// a spelling whose inspection fails for a reason other than absence is refused, so a loose
// resolution cannot step over the part it could not reach and write somewhere else.
func TestImproveReview799UnreadableOutputSpellingFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	locked := filepath.Join(s.root, "locked")
	reachable := filepath.Join(s.root, "reachable")
	for _, dir := range []string{locked, reachable} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	improveInputRelayConfig(t, s)
	// The spelled path steps out of the directory that cannot be examined; a loose resolution
	// would drop that component and land on the reachable directory instead.
	out := locked + "/../reachable/bundle.json"
	code, _, stderr := improveTestRun(t, s, "--out", out)
	if code != 1 || !strings.Contains(stderr, improveReasonOutputUnreadable) {
		t.Fatalf("an output whose spelling cannot be examined: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputUnreadable)
	}
	if _, err := os.Stat(filepath.Join(reachable, "bundle.json")); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote the bundle through the loose resolution (stat err %v)", err)
	}
}

// TestImproveReview799FifoInputDoesNotHangTheRecording covers a special file: recording a
// configured source that is a FIFO must not block, which would hang the collection before any
// reader or refusal is reached. The identity is taken without waiting for a writer. (The reader
// still blocks on a FIFO, as it always did; this pins only the recording.)
func TestImproveReview799FifoInputDoesNotHangTheRecording(t *testing.T) {
	s := improveTestSetup(t)
	fifo := filepath.Join(s.root, "audit.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("FIFOs are unavailable here: %v", err)
	}
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindAudit: {Path: fifo}}}
	done := make(chan error, 1)
	go func() {
		ids := improveIdentityNew(true)
		defer ids.improveIdentityClose()
		done <- ids.improveIdentityRecord(section)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recording a FIFO source: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("recording a FIFO source blocked instead of reaching a decision")
	}
}

// TestImproveReview799SearchOnlyDirectoriesAreAccepted covers the permission each step actually
// needs. A configured store directory may be searched but not read, because the reader opens the
// store file inside it; an output directory needs write and search, because the temporary file is
// created in it. Neither needs the directory to be readable, so neither is refused. root ignores
// the bits, so the test skips there.
func TestImproveReview799SearchOnlyDirectoriesAreAccepted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	state := filepath.Join(s.root, "search-only-state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	improveInputSeedPlanAt(t, filepath.Join(state, improveStoreFile), "p1", "a")
	out := filepath.Join(s.root, "write-only-out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(state, 0o755)
		_ = os.Chmod(out, 0o755)
	})
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"dag":   map[string]any{"path": state, "pattern": "p1"},
		},
	}}})
	// The store directory may be searched but not read; the output directory may be written and
	// searched but not read.
	if err := os.Chmod(state, 0o111); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o333); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(out, "bundle.json")
	if code, _, stderr := improveTestRun(t, s, "--out", bundle); code != 0 {
		t.Fatalf("a search-only store directory and a write-only output directory: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(bundle); err != nil {
		t.Errorf("the bundle was not written: %v", err)
	}
}

// TestImproveReview799OutputUnderARepointedInputDirectoryIsRefused covers the containment rule when
// the input's own spelling is a link that is re-pointed: the destination is judged against the
// directory the input is in now, not against the directory the recording saw.
func TestImproveReview799OutputUnderARepointedInputDirectoryIsRefused(t *testing.T) {
	s := improveTestSetup(t)
	first := filepath.Join(s.root, "first")
	second := filepath.Join(s.root, "second")
	sub := filepath.Join(second, "sub")
	for _, dir := range []string{first, sub} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(s.root, "current")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	// A configured directory input: the guard records the directory the spelling reaches.
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindDraft: {Path: link}}}
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		t.Fatalf("the recording: %v", err)
	}
	// The link now reaches the other directory, so a destination under it is inside the input.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(sub, "bundle.json")
	if err := ids.improveIdentityRefuse(dest, nil); err == nil {
		t.Errorf("a destination under the directory the input now reaches was not refused")
	}
}

// TestImproveReview799LinkedSearchOnlyDirectoryIsAccepted covers a configured store directory whose
// last component is a link to a search-only directory: the type is read from the file the path
// reaches, so it is opened with the search-only bit rather than read-only. root ignores the bits,
// so the test skips there.
func TestImproveReview799LinkedSearchOnlyDirectoryIsAccepted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	root := t.TempDir()
	real := filepath.Join(root, "private-state")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(real, 0o755) })
	// The directory may be searched but not read. Its type is read from the file the spelling
	// reaches, so it is opened with the search-only bit rather than refused for want of a read
	// permission the collection never needs.
	if err := os.Chmod(real, 0o111); err != nil {
		t.Fatal(err)
	}
	section := improveSection{Sources: map[string]improveSourceConfig{improveKindDraft: {Path: link}}}
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		t.Fatalf("recording a directory reached through a link to a search-only directory: %v", err)
	}
	held := false
	for _, entry := range ids.entries {
		if entry.path == link && entry.info != nil && entry.info.IsDir() {
			held = entry.file != nil
		}
	}
	if !held {
		t.Errorf("the linked search-only directory was not opened and held as an input")
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

// TestImproveReview799FifoInputStillReachesTheReader covers what the identity recording must not do
// to a named pipe: opening one for reading, even with the non-blocking flag, pairs with a writer
// waiting in open, and the bytes that writer then writes are dropped when the descriptor closes, so
// the reader that runs afterwards waits for a writer that has already finished. The recording opens
// a pipe the way it opens anything that is not a regular file, without attaching to it, so the one
// writer's line still reaches the reader that reads the ledger.
func TestImproveReview799FifoInputStillReachesTheReader(t *testing.T) {
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	fifo := filepath.Join(s.root, "audit.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("named pipes are unavailable here: %v", err)
	}
	improveTestConfig(t, s, map[string]any{"manage": map[string]any{"improve": map[string]any{
		"sources": map[string]any{
			"relay": map[string]any{"path": s.stateDir},
			"audit": map[string]any{"path": fifo},
		},
	}}})
	out := filepath.Join(improveReview799OutDir(t, s), "bundle.json")
	// One writer writes one graded line and closes. It blocks in open until a reader arrives, so a
	// recording that opens the pipe for reading wakes it and then throws its line away.
	written := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			written <- err
			return
		}
		_, err = f.WriteString("{\"schema\":\"crw-audit/1\",\"issue\":\"CRW-1\",\"grade\":\"A\"}\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		written <- err
	}()
	// A recording that reads the pipe leaves the reader waiting for a writer that has gone, so the
	// run is bounded: a run that has not finished is the defect, not a slow host.
	done := make(chan struct{})
	var code int
	var stderr string
	go func() {
		defer close(done)
		code, _, stderr = improveTestRun(t, s, "--out", out)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the collection did not finish: the writer's line was taken by the identity recording, so the reader waits for a writer that has closed")
	}
	if err := <-written; err != nil {
		t.Fatalf("the writer of the pipe: %v", err)
	}
	if code != 0 {
		t.Fatalf("a pipe input: exit %d, stderr %q", code, stderr)
	}
	if rows := improveTestRecordsOf(improveTestReadBundle(t, out), improveKindAudit); len(rows) != 1 {
		t.Errorf("the reader took %d audit records from the pipe, want the one the writer wrote", len(rows))
	}
}

// TestImproveReview799UnremovableTemporaryFileIsReported covers the refusal that cannot clean up
// after itself: removing the temporary file needs write permission on the output directory at the
// moment of the removal, and a directory whose permission was taken away after the file was created
// refuses it. The refusal is still named, and the file it could not remove is named with it, so a
// leftover is reported rather than dropped. root ignores the permission bits this relies on, so the
// test skips there.
func TestImproveReview799UnremovableTemporaryFileIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	s := improveTestSetup(t)
	improveReview799Store(t, s)
	improveInputRelayConfig(t, s)
	out := improveReview799OutDir(t, s)
	aside := out + "-held"
	previous := improveInputBeforeRename
	improveInputBeforeRename = func(improveOutputPlan) {
		// The directory the bundle was written in is moved aside and the spelling now reaches a
		// configured source directory, so the last comparison refuses; the directory that holds
		// the temporary file loses the write permission the removal needs.
		if err := os.Rename(out, aside); err != nil {
			t.Errorf("moving the output directory aside: %v", err)
			return
		}
		if err := os.Symlink(s.stateDir, out); err != nil {
			t.Errorf("relinking the output directory: %v", err)
			return
		}
		if err := os.Chmod(aside, 0o555); err != nil {
			t.Errorf("taking the write permission off the output directory: %v", err)
		}
	}
	t.Cleanup(func() {
		improveInputBeforeRename = previous
		_ = os.Chmod(aside, 0o755)
	})
	code, _, stderr := improveTestRun(t, s, "--out", filepath.Join(out, "bundle.json"))
	if code != 1 || !strings.Contains(stderr, improveReasonOutputParent) {
		t.Fatalf("a swapped output directory: exit %d, stderr %q, want the named refusal %s", code, stderr, improveReasonOutputParent)
	}
	if !strings.Contains(stderr, "could not be removed") {
		t.Errorf("the refusal did not report the temporary file it could not remove: %q", stderr)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("the directory the bundle was written in is gone: %v", err)
	}
}
