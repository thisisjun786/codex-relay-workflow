package dagsched

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// checkIntegrationCheckout refuses to move a branch beneath any registered worktree. A worktree that has the
// integration branch checked out would keep its files and index on the old head while its branch moved under it, so
// the batch refuses instead of leaving a worktree staged against a moved HEAD (CRW-1595). The check runs before a
// batch's verification and again inside the fenced move transaction, and in UpdateIntegrationRef itself so a direct
// caller gets the same safety.
func checkIntegrationCheckout(ctx context.Context, checkout, ref string) error {
	out, err := runGit(ctx, checkout, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return refuse(contract.RefusalMergeTargetUnreadable, "cannot check worktrees before moving %s: %v", ref, err)
	}
	path := ""
	for _, field := range strings.Split(out, "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			path = strings.TrimPrefix(field, "worktree ")
		}
		if field == "branch refs/heads/"+ref {
			return refuse(contract.RefusalDispositionConflict, "integration branch %s is checked out at %s: switch that worktree to another branch or a detached HEAD before integrating; its files and index were not changed", ref, path)
		}
	}
	return nil
}
