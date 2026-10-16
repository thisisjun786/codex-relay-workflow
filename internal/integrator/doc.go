// Package integrator is the product port of the shell integration queue runner
// (cc-queue-runner.sh with integrate_git.sh): the plain-head path that merges gated heads
// onto the remote integration branch, verifies the merged tree and fast-forwards the branch.
//
// A head is a 40-hex commit named by one file of the queue's pending directory. One pass
// takes the integration lock, re-checks the pre-merge gate of every pending head, merges the
// passing heads in issue-number order with merge commits in a private detached worktree
// (a conflict on the plugin manifest's version line is an ordinary conflict, left to the queue),
// runs the verification command on the merged tree and pushes it
// fast-forward only. It never forces a push and never touches the relay.
//
// A pull request is the other kind of pending entry (PR-unit landing, CRW-1355): a file pr-<N> holds the head of pull request N
// of the configured repository, put there by EnqueuePR. A pass lands one pull request alone, before the issue heads: it reads
// the pull request (open, toward the branch, still at that head), merges its head, verifies the merged tree, runs the configured
// review command and moves the branch only on a verification that passed and a review that did not ask for changes. The
// landing is recorded against the pull request in <tag>.pr.json and as a comment on it.
//
// Every host path and command comes from the configuration (Config); the package fixes no
// path of its own. The shell scripts stay the live runner until an explicit post-cutover
// switch; this package only reads their queue directories and lock.
package integrator

// Exit statuses of a pass and of one integration run, the shell scripts' own.
const (
	// ExitPushed means the merged heads were pushed (or there was nothing to do).
	ExitPushed = 0
	// ExitUsage is a usage, configuration, fetch or lock failure; nothing was changed.
	ExitUsage = 2
	// ExitVerifyFailed means the verification of the merged tree failed; nothing was pushed.
	ExitVerifyFailed = 3
	// ExitMoved means the remote branch moved during the run or the push failed; nothing was pushed.
	ExitMoved = 4
	// ExitNothingMerged means no head could be merged.
	ExitNothingMerged = 5
	// ExitReviewRefused means the review of a pull request's merged tree asked for changes; nothing was pushed.
	ExitReviewRefused = 6
)
