package store

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Modelled on packages/codex-session-relay/tests/test_manifest_scope.py (PinnedTraversal,
// StabilityBoundary), but every read is reached through receipt intake, so removing a guard
// from HashArtifact changes what AcceptChildReceipt answers.

// requireUnverifiedBecause asserts the receipt was refused as manifest_unverified and that
// the per-artifact problem it carries names the scope reason.
func requireUnverifiedBecause(t *testing.T, err error, reason string) {
	t.Helper()
	requireReason(t, err, ReasonManifestUnverified)
	if !strings.Contains(err.Error(), ": "+reason+": ") {
		t.Fatalf("expected the artifact problem to be %s, got %v", reason, err)
	}
}

// duringRead installs the between-passes seam for one read only.
func duringRead(t *testing.T, act func(fd int, before *statSnapshot)) {
	t.Helper()
	fired := false
	betweenPasses = func(fd int, before *statSnapshot) {
		if !fired {
			fired = true
			act(fd, before)
		}
	}
	t.Cleanup(func() { betweenPasses = nil })
}

func TestAcceptReceipt_refuses_symlink_component_when_artifact_is_swapped_for_a_link(t *testing.T) {
	// Given: a receipt built from a real artifact whose declared path is then a symlink to
	// identical bytes, so only the pinned walk can tell the difference.
	f := newIntakeFixture(t)
	path := f.artifact("out.txt", "payload")
	payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
	real := filepath.Join(f.root, "real.txt")
	if err := os.Rename(path, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	// When: the receipt is accepted.
	_, err := f.acceptPayload(payload)
	// Then: the leaf symlink is refused by name.
	requireUnverifiedBecause(t, err, ReasonSymlinkComponent)
}

func TestAcceptReceipt_refuses_symlink_component_when_an_ancestor_is_a_link(t *testing.T) {
	// Given: the artifact's directory replaced by a symlink to a directory with the same bytes.
	f := newIntakeFixture(t)
	if err := os.Mkdir(filepath.Join(f.root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := f.artifact("sub/out.txt", "payload")
	payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
	if err := os.Rename(filepath.Join(f.root, "sub"), filepath.Join(f.root, "real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, "real"), filepath.Join(f.root, "sub")); err != nil {
		t.Fatal(err)
	}
	// When: the receipt is accepted.
	_, err := f.acceptPayload(payload)
	// Then: the intermediate symlink is refused.
	requireUnverifiedBecause(t, err, ReasonSymlinkComponent)
}

func TestAcceptReceipt_refuses_path_relocated_when_ancestor_moves_during_the_read(t *testing.T) {
	// Given: an authorized artifact whose ancestor is renamed after the descriptor is pinned.
	f := newIntakeFixture(t)
	if err := os.Mkdir(filepath.Join(f.root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := f.artifact("sub/out.txt", "payload")
	payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
	duringRead(t, func(int, *statSnapshot) {
		if err := os.Rename(filepath.Join(f.root, "sub"), filepath.Join(f.tmp, "moved")); err != nil {
			t.Fatal(err)
		}
	})
	// When: the receipt is accepted.
	_, err := f.acceptPayload(payload)
	// Then: the descriptor no longer at the declared path is refused.
	requireUnverifiedBecause(t, err, ReasonPathRelocated)
}

func TestAcceptReceipt_refuses_artifact_mutated_when_bytes_change_between_passes(t *testing.T) {
	// Given: a same-size rewrite between the passes with the metadata snapshot re-baselined,
	// the mapped-writer case where only the second hash can notice.
	f := newIntakeFixture(t)
	path := f.artifact("out.txt", "AAAA")
	payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
	duringRead(t, func(fd int, before *statSnapshot) {
		if err := os.WriteFile(path, []byte("BBBB"), 0o600); err != nil {
			t.Fatal(err)
		}
		rebaselined, _, err := snapshotOf(fd)
		if err != nil {
			t.Fatal(err)
		}
		*before = rebaselined
	})
	// When: the receipt is accepted.
	_, err := f.acceptPayload(payload)
	// Then: the two passes disagree and the read is refused.
	requireUnverifiedBecause(t, err, ReasonArtifactMutated)
}

func TestAcceptReceipt_refuses_artifact_mutated_when_metadata_changes_during_the_read(t *testing.T) {
	// Given: identical bytes on both passes, but the timestamps change between them.
	f := newIntakeFixture(t)
	path := f.artifact("out.txt", "payload")
	payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
	duringRead(t, func(fd int, before *statSnapshot) {
		later := time.Unix(0, before.mtime).Add(time.Hour)
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
		var info syscall.Stat_t
		if err := syscall.Fstat(fd, &info); err != nil || info.Mtim.Nano() == before.mtime {
			t.Fatalf("mtime did not move: %v", err)
		}
	})
	// When: the receipt is accepted.
	_, err := f.acceptPayload(payload)
	// Then: the stat comparison refuses the read.
	requireUnverifiedBecause(t, err, ReasonArtifactMutated)
}
