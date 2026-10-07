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
		// A clean difference is one this check settles only when the relay decided a rule for
		// it: the built-in version line, or a declared regenerate command. A clean difference
		// with no such decision is not one this check may look at.
		rule, decided := decided[p]
		if !decided || (p != pluginversion.ManifestRepoPath && !strings.HasPrefix(rule, dagsched.RuleRegeneratePref)) {
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
	// The command each path is settled by is the relay"s decision for it (CRW-898, item 6):
	// the relay weighed the candidate"s declaration and every contributing node"s, and a
	// candidate-only declaration must not block the built-in rule. An empty decision is the
	// built-in plugin-version rule, which is not a regenerate command run over the file.
	commands := map[string][]string{}
	for _, p := range paths {
		if rule, ok := decided[p]; ok && strings.HasPrefix(rule, dagsched.RuleRegeneratePref) {
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
	// A clean difference is never a hand resolution: git merged the path without a conflict, so
	// there is nothing for a parent to have read and named, and a declaration whose command did not
	// reproduce the file must not become one. The path is refused instead (CRW-898, item 9). A
	// conflicted path keeps the manual route, which is what a parent's --resolved name is for.
	for _, p := range paths {
		// The plugin manifest keeps its CRW-732 route: a declared rule the checker cannot prove leaves
		// it to the parent's --resolved name, because the manifest is the one path whose updater needs
		// the existing file and whose built-in rule is tried beside the declaration. Every other clean
		// difference is refused, since git merged it and no hand resolution can exist there.
		if _, conflicted := merged.conflicts[p]; conflicted || !manual[p] || p == pluginversion.ManifestRepoPath {
			continue
		}
		return &dagsched.RefreshMechanicalRefusal{Detail: fmt.Sprintf("tree_differs: regenerate:%s did not reproduce %s, and git merged it cleanly, so the head's content there is not a regeneration of the base and no hand resolution can make it one", strings.TrimPrefix(decided[p], dagsched.RuleRegeneratePref), p)}, nil
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
		// The relay decided which paths the built-in rule settles (CRW-898, item 6): an empty
		// decision for the manifest is that decision, whatever the candidate declared alone.
		if rule, ok := decided[p]; !ok || rule != dagsched.RefreshDecisionBuiltin {
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
