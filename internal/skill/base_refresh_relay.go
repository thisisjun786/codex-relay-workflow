package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

func init() { dagsched.RegisterRefreshMechanical(settleRelayRefresh) }

// settleRelayRefresh shares the full check's rule evaluator, but checks only the
// mechanical conflict files. The relay proves the other files separately.
func settleRelayRefresh(ctx context.Context, checkout string, st dagsched.RefreshStep, regions []dagsched.Region, decided map[string]string) (*dagsched.RefreshMechanicalRefusal, error) {
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
	// The relay decided, per path, which rule settles the place (CRW-898, item 6): it weighed the
	// candidate's declaration and every contributing node's, and it hands that decision here rather
	// than a bare list of paths, so the checker never re-derives the decision from the candidate
	// alone. A path is in this check's reading when git could not merge it, or when the head
	// recorded it again over a clean merge under a declared regenerate rule or the built-in
	// plugin-version rule (CRW-732). Every other path is not one this check settles.
	paths := make([]string, 0, len(decided))
	for p := range decided {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var differing map[string]bool
	for _, p := range paths {
		if _, ok := merged.conflicts[p]; ok {
			continue
		}
		if p != pluginversion.ManifestRepoPath {
			return nil, fmt.Errorf("%s is not a conflict in the mechanical check's reading", p)
		}
		if differing == nil {
			names, err := g.differingAll(ctx, merged.tree, st.Tree)
			if err != nil {
				return nil, err
			}
			differing = map[string]bool{}
			for _, name := range names {
				differing[name] = true
			}
		}
		if !differing[p] {
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
	// The plugin manifest's version line is the one place no declaration has to cover with one agreed
	// rule: the built-in rule recomputes it from the head, and it settles the path whether the head
	// re-recorded it over a conflict or over a clean merge (CRW-732). A declaration that does settle
	// the place keeps its say: ruleFor answered for it above.
	builtin := map[string]builtinResolution{}
	for _, p := range eligible {
		if _, ok := cov.ruleFor(p); ok {
			continue
		}
		if p != pluginversion.ManifestRepoPath {
			continue
		}
		resolution, settled, err := g.pluginVersionResolution(ctx, st.Previous, st.Head, st.BaseParent)
		if err != nil {
			return nil, err
		}
		if !settled {
			code := "differs_outside_mechanical"
			if _, ok := merged.conflicts[p]; ok {
				code = "conflict_outside_mechanical"
			}
			return &dagsched.RefreshMechanicalRefusal{Detail: fmt.Sprintf("%s: the head's %s is not the version the payload of %s derives, and no declaration given settles the path with one rule", code, p, st.Head)}, nil
		}
		builtin[p] = resolution
	}
	_, why, err := g.settleMechanical(ctx, facts, st.Previous, st.Head, st.BaseParent, cov, decided, eligible, merged, builtin, defaultRegenerateTimeout)
	if err != nil {
		return nil, err
	}
	if why != nil {
		return &dagsched.RefreshMechanicalRefusal{Detail: why.code + ": " + why.detail}, nil
	}
	rules := map[string]string{}
	for p, resolution := range builtin {
		rules[p] = resolution.rule
	}
	if len(hand) > 0 {
		return &dagsched.RefreshMechanicalRefusal{Manual: hand, Rules: rules}, nil
	}
	if len(rules) > 0 {
		return &dagsched.RefreshMechanicalRefusal{Rules: rules}, nil
	}
	return nil, nil
}
