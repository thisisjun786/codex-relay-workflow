package skill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

func init() { dagsched.RegisterRefreshMechanical(settleRelayRefresh) }

// settleRelayRefresh shares the full check's rule evaluator, but checks only the
// mechanical conflict files. The relay proves the other files separately.
func settleRelayRefresh(ctx context.Context, checkout string, st dagsched.RefreshStep, regions []dagsched.Region, paths []string) (*dagsched.RefreshMechanicalRefusal, error) {
	cov := coverage{regionSet{regions: regions}}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout+time.Duration(8*len(cov.regenerateCommands()))*defaultRegenerateTimeout)
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
	commands := map[string][]string{}
	for _, p := range paths {
		if rule, ok := cov.ruleFor(p); ok && strings.HasPrefix(rule, dagsched.RuleRegeneratePref) {
			command := strings.TrimPrefix(rule, dagsched.RuleRegeneratePref)
			commands[command] = append(commands[command], p)
		}
	}
	manual := map[string]bool{}
	for _, command := range sortedCommands(commands) {
		for _, tree := range []string{st.Previous, st.BaseParent, st.Head} {
			for run := 0; run < 2; run++ {
				ok := g.reconstructOutputs(ctx, tree, command, regions, commands[command], defaultRegenerateTimeout)
				for _, p := range commands[command] {
					if !ok[p] {
						manual[p] = true
					}
				}
			}
		}
	}
	var eligible, hand []string
	for _, p := range paths {
		if manual[p] {
			hand = append(hand, p)
		} else {
			eligible = append(eligible, p)
		}
	}
	facts := refreshFacts{parents: []string{st.Previous, st.BaseParent}, mergeTree: merged.tree, headTree: st.Tree}
	// The relay classifies only the paths a declaration covers, so no built-in rule applies here.
	_, why, err := g.settleMechanical(ctx, facts, st.Previous, st.Head, st.BaseParent, cov, eligible, merged, nil, defaultRegenerateTimeout)
	if err != nil {
		return nil, err
	}
	if why != nil {
		return &dagsched.RefreshMechanicalRefusal{Detail: why.code + ": " + why.detail}, nil
	}
	if len(hand) > 0 {
		return &dagsched.RefreshMechanicalRefusal{Manual: hand}, nil
	}
	return nil, nil
}
