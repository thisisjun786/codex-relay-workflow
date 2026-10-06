package hook

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"

// ActiveWorkPhaseOpts ports activeWorkPhaseOpts (CXC v0.2.40 hook.ts:373-385): the
// fail-open loader for the B-directive starvation opts. It resolves the bound
// goalplan's effective active work-phase, or nil when the slug is empty, no goalplan
// resolves, the effective cursor is nil, or the named phase is gone. Reading the plan
// is this package's first use of goalplan; the edge is one-way (goalplan does not
// import hook).
func ActiveWorkPhaseOpts(cwd, slug string) *DirectiveOptions {
	if slug == "" {
		return nil
	}
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		return nil
	}
	id := goalplan.EffectiveActiveWorkPhaseID(plan)
	if id == nil {
		return nil
	}
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].ID == *id {
			return &DirectiveOptions{ActiveWorkPhase: &ActiveWorkPhase{ID: plan.WorkPhases[i].ID, Title: plan.WorkPhases[i].Title}}
		}
	}
	return nil
}
