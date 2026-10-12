package source

import "time"

// CommitIdentity is the source identity of a commit as it stands in the object store: resolved, on that commit, with no
// uncommitted work. It is what Capture answers for a clean worktree of the commit, so Compare judges evidence that was made on a
// pinned commit and evidence that was made in a worktree by one rule. An empty commit names no source and has no identity
// (unavailable, which Compare never reads as the same). now supplies the capture time; nil means time.Now.
//
// The final gate of a flow run (CRW-1206) uses it: the integrated head it judges is a commit, and a receipt for that head is
// fresh when its identity compares the same with the identity of the head the gate pins now.
func CommitIdentity(commitSha string, now func() time.Time) Identity {
	if now == nil {
		now = time.Now
	}
	capturedAt := now().UTC().Format("2006-01-02T15:04:05.000Z")
	if commitSha == "" {
		return Identity{Kind: KindUnavailable, CapturedAt: capturedAt}
	}
	return Identity{Kind: KindResolved, CommitSha: commitSha, CapturedAt: capturedAt}
}
