package dagsched

import (
	"context"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The push of the integration branch to a remote branch (CRW-965, criteria c6 and c7). It moves the remote branch to the
// integration commit only by a fast-forward. The remote branch is read first: an equal remote is up to date; a remote that
// holds a commit the integration branch contains is advanced; a remote that holds a commit the branch does not contain is
// refused with merge_base_mismatch and nothing is pushed. There is no forced push. A remote that cannot be reached (a
// GitHub outage) defers the push, and the integration the batch already recorded stands.

// SchemaIntegrationPush names the document dag-integrate-push prints.
const SchemaIntegrationPush = "dag-integrate-push/1"

// Outcomes of one push. pushed moved the remote branch to the integration head; up_to_date needed nothing; deferred
// means the remote could not be reached, so the push waits and the same command can be run again later.
const (
	PushPushed   = "pushed"
	PushUpToDate = "up_to_date"
	PushDeferred = "deferred"
)

// PushResult is the answer of one push attempt.
type PushResult struct {
	Remote, RemoteRef string
	LocalHead         string
	RemoteHead        string
	Outcome           string
	Detail            string
}

// PushIntegration moves a remote branch to a local integration branch's commit by a fast-forward only.
func PushIntegration(ctx context.Context, checkout, remote, remoteRef, integrationRef string) (PushResult, error) {
	out := PushResult{Remote: remote, RemoteRef: remoteRef}
	// a name that starts with a dash would be read by git as an option (CRW-965 review): refused before git runs
	if strings.HasPrefix(remote, "-") || strings.HasPrefix(remoteRef, "-") {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "the remote %q and the remote ref %q must not start with a dash", remote, remoteRef)
	}
	local, found, err := integrationBranchTip(ctx, checkout, integrationRef)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s in %s: %v", integrationRef, checkout, err)
	}
	if !found {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s names no branch in %s", integrationRef, checkout)
	}
	out.LocalHead = local
	pushTarget := remoteURLForPush(ctx, checkout, remote)
	remoteHead, unreachableRemote, err := lsRemoteHead(ctx, checkout, pushTarget, remoteRef)
	if err != nil {
		if unreachableRemote {
			out.Outcome, out.Detail = PushDeferred, err.Error()
			return out, nil
		}
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s of %s: %v", remoteRef, remote, err)
	}
	out.RemoteHead = remoteHead
	if remoteHead == local {
		out.Outcome = PushUpToDate
		return out, nil
	}
	if remoteHead != "" {
		if !hasCommitIn(ctx, checkout, remoteHead) {
			return out, refuse(contract.RefusalMergeBaseMismatch, "%s on %s points at %s, which %s does not hold: nothing is pushed", remoteRef, remote, remoteHead, checkout)
		}
		ancestor, err := isAncestorIn(ctx, checkout, remoteHead, local)
		if err != nil {
			return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not compare %s with %s: %v", remoteHead, local, err)
		}
		if !ancestor {
			return out, refuse(contract.RefusalMergeBaseMismatch, "%s on %s points at %s, which %s does not contain: nothing is pushed", remoteRef, remote, remoteHead, integrationRef)
		}
	}
	if err := pushCommit(ctx, checkout, pushTarget, local, remoteRef); err != nil {
		if _, ok := err.(*pushUnreachable); ok {
			out.Outcome, out.Detail = PushDeferred, err.Error()
			return out, nil
		}
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not push %s to %s: %v", local, remote, err)
	}
	out.Outcome, out.RemoteHead = PushPushed, local
	return out, nil
}

// fullCommitPattern is the shape of a full object name this package accepts: 40 hex digits, or 64 for a sha256 repository.
var fullCommitPattern = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

// lsRemoteHead reads where a remote branch points. unreachable reports a failure that means "the remote could not be
// reached", which defers the push; any other failure is a refusal.
func lsRemoteHead(ctx context.Context, checkout, remote, branch string) (string, bool, error) {
	code, out, err := runGitExit(ctx, checkout, nil, "ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return "", code < 0 || unreachableGitMessage(err.Error()), err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch && fullCommitPattern.MatchString(fields[0]) {
			return fields[0], false, nil
		}
	}
	return "", false, nil
}

// unreachableGitMessage is whether git's message means the remote could not be reached rather than that it answered and
// refused.
func unreachableGitMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, needle := range []string{"could not resolve host", "connection refused", "connection timed out", "network is unreachable",
		"could not read from remote repository", "no route to host", "operation timed out", "temporary failure in name resolution",
		"could not connect to server", "failed to connect to"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// pushUnreachable is the error a push returns when the remote could not be reached.
type pushUnreachable struct{ detail string }

func (e *pushUnreachable) Error() string { return e.detail }

// pushCommit pushes a local commit to a remote branch with no force and no leading plus, so the remote refuses the update
// unless it is a fast-forward of what it holds.
func pushCommit(ctx context.Context, checkout, remote, commit, branch string) error {
	code, _, err := runGitExit(ctx, checkout, nil, "push", remote, commit+":refs/heads/"+branch)
	if err == nil {
		return nil
	}
	if code < 0 || unreachableGitMessage(err.Error()) {
		return &pushUnreachable{detail: err.Error()}
	}
	return err
}

// hasCommitIn reports whether a checkout holds a commit object.
func hasCommitIn(ctx context.Context, checkout, commit string) bool {
	code, _, _ := runGitExit(ctx, checkout, nil, "cat-file", "-e", commit+"^{commit}")
	return code == 0
}

// isAncestorIn reports whether one commit is contained in another in a checkout (merge-base --is-ancestor).
func isAncestorIn(ctx context.Context, checkout, ancestor, descendant string) (bool, error) {
	code, _, err := runGitExit(ctx, checkout, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if code == 0 {
		return true, nil
	}
	if code == 1 {
		return false, nil
	}
	return false, err
}
