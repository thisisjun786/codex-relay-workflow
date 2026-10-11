package daemon

// MaxLiveOrchestrators bounds the orchestrators the per-tick budgets scale with: more than this many live
// project parents does not lengthen a tick further, so a registry full of stale bindings cannot turn one
// tick into an unbounded pass over the host.
const MaxLiveOrchestrators = 32

// Budgets are the per-tick budgets of one tick: the policy's constants, raised for the orchestrators the
// store holds live (BudgetsFor).
type Budgets struct {
	// MaxSends is the tick's budget of delivery attempts, and MaxSendsPerParentPerTick the share of it one
	// parent may use (delivery.Scheduler).
	MaxSends, MaxSendsPerParentPerTick int
	// MaxProjects is how many projects the supervisor pass visits, and MaxSupervisorSends how many reports
	// it sends (supervisor.Channel.AutoSend).
	MaxProjects, MaxSupervisorSends int
}

// BudgetsFor are the budgets of a tick for live orchestrators, the tasks that hold the live parent binding
// of a project, and a base share of MaxSendsPerParentPerTick (delivery.RetryPolicy). With none the policy's
// constants stand. Each live orchestrator adds to them what it needs of one tick:
//
//   - MaxProjects is at least one project for each, so the supervisor pass reaches every project in a
//     rotation;
//   - MaxSupervisorSends is at least one report for each;
//   - MaxSends is at least OrchestratorSends attempts for each: the results of the issues it runs, and the
//     corrections it sends back;
//   - MaxSendsPerParentPerTick is at least OrchestratorSends for the parents that are orchestrators, so one
//     tick reaches the recipients of an orchestrator that runs many issues.
//
// The count is bounded by MaxLiveOrchestrators. A policy value is only ever raised, and a negative one, which turns
// its budget off, stays off.
func (p Policy) BudgetsFor(live, parentShare int) Budgets {
	b := Budgets{MaxSends: p.MaxSends, MaxSendsPerParentPerTick: parentShare, MaxProjects: p.MaxProjects, MaxSupervisorSends: p.MaxSupervisorSends}
	n := min(live, MaxLiveOrchestrators)
	if n <= 0 {
		return b
	}
	raise := func(base *int, least int) {
		if *base >= 0 {
			*base = max(*base, least)
		}
	}
	raise(&b.MaxSends, p.OrchestratorSends*n)
	raise(&b.MaxSendsPerParentPerTick, p.OrchestratorSends)
	raise(&b.MaxProjects, n)
	raise(&b.MaxSupervisorSends, n)
	return b
}
