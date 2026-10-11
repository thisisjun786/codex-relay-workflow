// Package integrator is the product port of the shell integration queue runner
// (cc-queue-runner.sh with integrate_git.sh): the plain-head path that merges gated heads
// onto the remote integration branch, verifies the merged tree and fast-forwards the branch.
//
// A head is a 40-hex commit named by one file of the queue's pending directory. One pass
// takes the integration lock, re-checks the pre-merge gate of every pending head, merges the
// passing heads in issue-number order with merge commits in a private detached worktree
// (resolving only a conflict that is nothing but the plugin manifest's version line, then
// re-deriving that line), runs the verification command on the merged tree and pushes it
// fast-forward only. It never forces a push and never touches the relay.
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
)
