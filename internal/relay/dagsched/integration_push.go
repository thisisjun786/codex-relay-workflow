package dagsched

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// VerifiedMoveOnto is whether the relay moved integrationRef onto commit after verifying it (CRW-965, parent decision D6). The
// one source is the move itself: a ref_moved stage row names the new head, and the planned intent of its batch carries the
// verified head and a verification digest for the same ref. A batch row is not consulted, so a move the reconciliation
// recorded after a crash counts the same as a move the batch recorded.
func (s *Scheduler) VerifiedMoveOnto(ctx context.Context, integrationRef, commit string) (bool, error) {
	return s.plannedVerifiedMove(ctx, integrationRef, commit)
}

// plannedVerifiedMove is whether a move the reconciliation recorded (a ref_moved stage with no batch row) names commit, and its
// planned intent carries the verified head and a verification digest for integrationRef (CRW-965, parent decision D6).
func (s *Scheduler) plannedVerifiedMove(ctx context.Context, integrationRef, commit string) (bool, error) {
	rows, err := s.Store.Q(ctx).QueryContext(ctx, "SELECT i.detail FROM dag_integration_stages m JOIN dag_integration_stages i ON i.batch_id = m.batch_id AND i.stage = 'intent' AND i.node_id = '' WHERE m.stage = 'ref_moved' AND m.detail = ?", commit)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var intent map[string]string
		if json.Unmarshal([]byte(raw), &intent) == nil && intent["verified_head"] == commit && intent["integration_ref"] == integrationRef && intent["verification_digest"] != "" {
			return true, nil
		}
	}
	return false, rows.Err()
}

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

// MovedHeadCheck answers whether the relay verified a commit and moved an integration ref onto it.
type MovedHeadCheck func(ctx context.Context, integrationRef, commit string) (bool, error)

// PushIntegration moves a remote branch to a local integration branch's commit by a fast-forward only. The branch's tip
// must be a head the relay verified and moved the branch onto; any other tip is refused before the remote is read (CRW-965,
// parent decision D6).
func PushIntegration(ctx context.Context, checkout, remote, remoteRef, integrationRef string, moved MovedHeadCheck) (PushResult, error) {
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
	verified, err := moved(ctx, integrationRef, local)
	if err != nil {
		return out, err
	}
	if !verified {
		return out, refuse(contract.RefusalMergeBaseMismatch, "%s in %s points at %s, which the relay did not verify and move the branch onto: nothing is pushed", integrationRef, checkout, local)
	}
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
		"could not connect to server", "failed to connect to", "returned error: 5"} {
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
