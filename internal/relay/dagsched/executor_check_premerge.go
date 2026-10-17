package dagsched

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// executorPremerge keeps the existing record/criteria judgment and bounded head
// reuse rules, but asks Git about merge trees in the scheduler's isolated object
// repository. Candidate attributes cannot select the controller's merge drivers.
func executorPremerge(ctx context.Context, raw []byte, node string, n dag.SnapNode, c *executor.CollectedCandidate) error {
	rec, err := premerge.Decode(raw)
	if err != nil {
		return premergeRefusal(err)
	}
	// Validate the record at the head it evaluated, with no Git merge operation.
	if _, err = judgePremerge(ctx, raw, node, n, rec.Head, nil); err != nil {
		return err
	}
	after, err := executorPremergeHeadRelation(ctx, c.Checkout, c.Base, rec.Head, c.Head)
	if err != nil {
		return err
	}
	return premergeRefusal(premerge.Judge(rec, premerge.Options{AfterEvaluation: after}))
}

// This read adapter mirrors premergeHeadRelation's bounds and first-parent
// rules. Every Git read, including merge-tree, uses isolate's explicit environment
// instead of the controller's global/system configuration.
func executorPremergeHeadRelation(ctx context.Context, checkout, base, evaluated, accepted string) (bool, error) {
	mismatch := func(format string, args ...any) error {
		return refuse(contract.RefusalPremergeHeadMismatch, format, args...)
	}
	if strings.EqualFold(evaluated, accepted) {
		return false, nil
	}
	g, err := isolate(ctx, checkout)
	if err != nil {
		return false, err
	}
	defer g.close()
	cur, plain, merges := accepted, false, 0
	for step := 0; step < maxPremergeHeadWalk; step++ {
		if cur == evaluated {
			return plain, nil
		}
		_, line, err := g.run(ctx, "rev-list", "--parents", "-n", "1", cur)
		if err != nil {
			return false, mismatch("git could not read predecessor commits of %s: %v", cur, err)
		}
		fields := strings.Fields(line)
		switch len(fields) - 1 {
		case 1:
			plain, cur = true, fields[1]
		case 2:
			first, second := fields[1], fields[2]
			if merges >= maxPremergeDevMerges {
				return false, mismatch("record head %s is more than %d merges behind %s", evaluated, maxPremergeDevMerges, accepted)
			}
			if code, _, err := g.run(ctx, "merge-base", "--is-ancestor", second, base); err != nil || code != 0 {
				return false, mismatch("second parent %s of %s is not in base %s", second, cur, base)
			}
			_, tree, err := g.run(ctx, "rev-parse", cur+"^{tree}")
			if err != nil {
				return false, mismatch("git could not read tree of %s: %v", cur, err)
			}
			code, merged, err := g.runWith(ctx, []string{"GIT_ATTR_SOURCE=" + first}, "merge-tree", "--write-tree", first, second)
			if err != nil || code != 0 || len(strings.Fields(merged)) == 0 || strings.Fields(merged)[0] != strings.TrimSpace(tree) {
				return false, mismatch("merge %s is not the isolated automatic merge of its input commits", cur)
			}
			merges++
			cur = first
		default:
			return false, mismatch("%s has %d parents; only a merge of two commits can keep the evaluation", cur, len(fields)-1)
		}
	}
	return false, mismatch("record head %s is not an ancestor of %s within %d commits", evaluated, accepted, maxPremergeHeadWalk)
}
