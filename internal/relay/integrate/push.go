package integrate

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// SchemaIntegrationPush names the document dag-integrate-push prints.
const SchemaIntegrationPush = "dag-integrate-push/1"

// Outcomes of one push. pushed moved the remote branch to the integration head; up_to_date needed nothing;
// deferred means the remote could not be reached (a GitHub outage), so the push waits and the command can be
// run again later. The batch's integration and the parent's merged mark do not depend on it.
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

// PushIntegration moves the remote branch to the integration branch's commit, and only by a fast-forward. The
// remote branch is read first: when it already holds the local commit nothing is pushed; when it holds a commit
// the integration branch contains, the push advances it; when it holds a commit the integration branch does not
// contain, nothing is pushed and the command refuses with merge_base_mismatch. There is no forced push. A remote
// that cannot be reached defers the push with its detail, and no other effect is recorded.
func PushIntegration(ctx context.Context, checkout, remote, remoteRef, integrationRef string) (PushResult, error) {
	out := PushResult{Remote: remote, RemoteRef: remoteRef}
	local, found, err := refTip(ctx, checkout, integrationRef)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s in %s: %v", integrationRef, checkout, err)
	}
	if !found {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s names no branch in %s", integrationRef, checkout)
	}
	out.LocalHead = local
	remoteHead, unreachableRemote, err := lsRemote(ctx, checkout, remote, remoteRef)
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
		if !hasCommit(ctx, checkout, remoteHead) {
			return out, refuse(contract.RefusalMergeBaseMismatch, "%s on %s points at %s, which %s does not hold: nothing is pushed", remoteRef, remote, remoteHead, checkout)
		}
		ancestor, err := isAncestor(ctx, checkout, remoteHead, local)
		if err != nil {
			return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not compare %s with %s: %v", remoteHead, local, err)
		}
		if !ancestor {
			return out, refuse(contract.RefusalMergeBaseMismatch, "%s on %s points at %s, which %s does not contain: nothing is pushed", remoteRef, remote, remoteHead, integrationRef)
		}
	}
	if _, err := pushRefspec(ctx, checkout, remote, local, remoteRef); err != nil {
		if _, ok := err.(*unreachable); ok {
			out.Outcome, out.Detail = PushDeferred, err.Error()
			return out, nil
		}
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not push %s to %s: %v", local, remote, err)
	}
	out.Outcome, out.RemoteHead = PushPushed, local
	return out, nil
}
