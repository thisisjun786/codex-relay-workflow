package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// A boundary that ends with a pending tree P is a worktree at C whose files are P: made with a two-tree read-tree in a
// temporary index, so the worktree's own index stays at C - the files show as uncommitted changes and a commit of the snapshot
// commits exactly P. The snapshot is checked against the boundary before it is returned.
func TestBoundaryPendingTreeMaterialized(t *testing.T) {
	repo, c := newRepo(t)
	// the pending tree P: the files of a worktree after an executor turn - an edit, a deletion, a mode change and a new file
	src := addWorktree(t, repo, "src", c)
	write(t, src, "a.txt", "alpha two\n", 0o644)
	if err := os.Remove(filepath.Join(src, "src", "c.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, src, "b.sh", "#!/bin/sh\n", 0o755)
	write(t, src, "new/d.txt", "delta\n", 0o644)
	p, err := Snapshot(ctx, ExecGit("git"), src)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "worktree", "remove", "--force", src)

	dest := filepath.Join(t.TempDir(), "wt", "n1", "a2-l1")
	digest, err := Restore(ctx, ExecGit("git"), repo, dest, c, p)
	if err != nil {
		t.Fatal(err)
	}
	if digest != p {
		t.Errorf("the verified digest is %s, the pending tree %s", digest, p)
	}
	if got := gitIn(t, dest, "rev-parse", "HEAD"); got != c {
		t.Errorf("HEAD is %s, want C %s", got, c)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(got) != "alpha two\n" {
		t.Errorf("a.txt: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "src", "c.txt")); err == nil {
		t.Errorf("a deletion of P is not in the files")
	}
	if got := gitIn(t, dest, "ls-files", "-s", "b.sh"); !strings.HasPrefix(got, "100644") {
		t.Errorf("the real index must stay at C: %s", got)
	}
	if info, err := os.Stat(filepath.Join(dest, "b.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the mode change of P is not in the files: %v %v", info, err)
	}
	if got := gitIn(t, dest, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("the real index differs from C: %q", got)
	}
	if got := gitIn(t, dest, "status", "--porcelain"); !strings.Contains(got, "a.txt") || !strings.Contains(got, "new/") {
		t.Errorf("the pending changes must show as uncommitted: %q", got)
	}
	if got, err := Snapshot(ctx, ExecGit("git"), dest); err != nil || got != p {
		t.Errorf("snapshot %s %v, want %s", got, err, p)
	}
	// a boundary without a pending tree is the files of C
	clean := filepath.Join(t.TempDir(), "wt", "n1", "a3-l1")
	digest, err = Restore(ctx, ExecGit("git"), repo, clean, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := gitIn(t, repo, "rev-parse", c+"^{tree}"); digest != want {
		t.Errorf("the digest of a clean boundary is %s, want the tree of C %s", digest, want)
	}
	if got := gitIn(t, clean, "status", "--porcelain"); got != "" {
		t.Errorf("a clean boundary has changes: %q", got)
	}
	// a path belongs to one allocation, and a pending tree that is no tree is refused before anything is made
	if _, err := Restore(ctx, ExecGit("git"), repo, clean, c, ""); err == nil {
		t.Errorf("a restore over an existing path was accepted")
	}
	missing := filepath.Join(t.TempDir(), "wt", "n1", "a4-l1")
	if _, err := Restore(ctx, ExecGit("git"), repo, missing, c, c); err == nil {
		t.Errorf("a commit was accepted as the pending tree")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a refused restore made a worktree")
	}
}

// The HEAD and the working snapshot of a superseded worktree are kept as one commit on a ref (the snapshot's tree on the HEAD
// commit); the worktree is only read: its files, its index and its HEAD are what they were.
func TestSupersededSuffixPreservedOnRecoveryRef(t *testing.T) {
	repo, c := newRepo(t)
	wt := addWorktree(t, repo, "old", c)
	write(t, wt, "a.txt", "edited by the superseded suffix\n", 0o644)
	write(t, wt, "untracked.txt", "untracked\n", 0o644)
	before, err := Snapshot(ctx, ExecGit("git"), wt)
	if err != nil {
		t.Fatal(err)
	}
	index := indexBytes(t, wt)
	ref := engine.SupersededRef("run-7", 2, "root/1:a%3Ab", "n1")
	if !strings.HasPrefix(ref, "refs/crw/flow/run-7/superseded/2/") || strings.Contains(ref[len("refs/crw/flow/run-7/superseded/2/"):], "/") {
		t.Fatalf("the ref %s is not one component under the lineage", ref)
	}
	if out, err := ExecGit("git")(ctx, repo, nil, "check-ref-format", ref); err != nil {
		t.Fatalf("%s is not a valid ref: %v %s", ref, err, out)
	}
	kept, err := Preserve(ctx, ExecGit("git"), repo, wt, ref, "superseded n1")
	if err != nil {
		t.Fatal(err)
	}
	if kept.Head != c || kept.Snapshot != before {
		t.Errorf("preserved %+v, want HEAD %s snapshot %s", kept, c, before)
	}
	if got := gitIn(t, repo, "rev-parse", ref); got != kept.Commit {
		t.Errorf("the ref is at %s, want %s", got, kept.Commit)
	}
	if got := gitIn(t, repo, "rev-parse", ref+"^{tree}"); got != before {
		t.Errorf("the ref's tree is %s, want the working snapshot %s", got, before)
	}
	if got := gitIn(t, repo, "rev-parse", ref+"^"); got != c {
		t.Errorf("the ref's parent is %s, want the worktree's HEAD %s", got, c)
	}
	if got := gitIn(t, repo, "show", ref+":untracked.txt"); got != "untracked" {
		t.Errorf("the untracked file is not kept: %q", got)
	}
	// nothing was written to the worktree
	if after, err := Snapshot(ctx, ExecGit("git"), wt); err != nil || after != before {
		t.Errorf("the snapshot changed: %s %v", after, err)
	}
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != c {
		t.Errorf("HEAD moved to %s", got)
	}
	if string(indexBytes(t, wt)) != string(index) {
		t.Errorf("the worktree's index was written")
	}
}

// The idempotent request id of an op is the one the ops package has always sent: the engine and ops agree on it.
func TestRequestIDIsTheEnginesOne(t *testing.T) {
	if RequestID("run-1", "c_abc") != engine.RequestID("run-1", "c_abc") || RequestID("run-1", "c_abc") == RequestID("run-1", "c_abd") {
		t.Errorf("%s %s", RequestID("run-1", "c_abc"), engine.RequestID("run-1", "c_abc"))
	}
}
