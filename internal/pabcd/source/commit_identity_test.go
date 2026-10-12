package source

import "testing"

// CRW-1206: the final gate of a flow run judges a commit it pinned, not a working tree. The identity of that commit has to compare
// with Compare exactly as the identity Capture takes of a clean worktree of the same commit does, so evidence made in one form is
// judged against the other by the same rule as the native final-gate hook uses.
func TestCommitIdentityComparesAsACleanCapture(t *testing.T) {
	base := hermetic(t)
	root := newRepo(t, base)
	first := gitIn(t, root, "rev-parse", "HEAD")[:40]
	if got := Compare(CommitIdentity(first, nil), Capture(root, Options{})); got.Kind != ComparisonSame {
		t.Fatalf("the identity of the commit and the capture of its clean worktree: %+v", got)
	}
	writeFile(t, root, "tracked.ts", "changed\n")
	gitIn(t, root, "commit", "-qam", "second")
	second := gitIn(t, root, "rev-parse", "HEAD")[:40]
	if got := Compare(CommitIdentity(first, nil), CommitIdentity(second, nil)); got.Kind != ComparisonDifferent {
		t.Fatalf("two commits: %+v", got)
	}
	if got := Compare(CommitIdentity(second, nil), Capture(root, Options{})); got.Kind != ComparisonSame {
		t.Fatalf("the second commit against its worktree: %+v", got)
	}
	// a worktree with uncommitted work is another identity than the commit it stands on
	writeFile(t, root, "tracked.ts", "dirty\n")
	if got := Compare(CommitIdentity(second, nil), Capture(root, Options{})); got.Kind != ComparisonDifferent {
		t.Fatalf("the commit against a dirty worktree: %+v", got)
	}
}

// A commit that is not named has no identity: it is unavailable, and Compare never reads an unavailable side as the same.
func TestCommitIdentityOfNothingIsUnavailable(t *testing.T) {
	id := CommitIdentity("", nil)
	if id.Kind != KindUnavailable || id.CommitSha != "" {
		t.Fatalf("%+v", id)
	}
	if got := Compare(id, CommitIdentity("a1b2c3d", nil)); got.Kind != ComparisonUnavailable {
		t.Fatalf("%+v", got)
	}
}
