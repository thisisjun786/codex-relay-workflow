package skill

import (
	"context"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

func init() { dagsched.RegisterRefreshMechanical(settleRelayRefresh) }

// settleRelayRefresh shares the full check's rule evaluator, but checks only the
// mechanical conflict files. The relay proves the other files separately.
func settleRelayRefresh(ctx context.Context, checkout string, st dagsched.RefreshStep, regions []dagsched.Region, paths []string) (*dagsched.RefreshMechanicalRefusal, error) {
	cov := coverage{regionSet{regions: regions}}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout+time.Duration(2*len(cov.regenerateCommands()))*defaultRegenerateTimeout)
	defer cancel()
	g, err := openRefreshGit(ctx, checkout)
	if err != nil {
		return nil, err
	}
	defer g.close()
	merged, err := g.mergeConflicts(ctx, st.Previous, st.BaseParent)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		if _, ok := merged.conflicts[p]; !ok {
			return nil, fmt.Errorf("%s is not a conflict in the mechanical check's reading", p)
		}
	}
	facts := refreshFacts{parents: []string{st.Previous, st.BaseParent}, mergeTree: merged.tree, headTree: st.Tree}
	_, why, err := g.settleMechanical(ctx, facts, st.Previous, st.Head, st.BaseParent, cov, paths, merged, defaultRegenerateTimeout)
	if err != nil {
		return nil, err
	}
	if why != nil {
		return &dagsched.RefreshMechanicalRefusal{Detail: why.code + ": " + why.detail}, nil
	}
	return nil, nil
}
