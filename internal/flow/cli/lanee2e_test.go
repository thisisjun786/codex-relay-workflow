package cli

import (
	"fmt"
	"strings"
	"testing"
)

// twoMilestones is the plan of the two-milestone fixture: M1 holds A-1 and A-2 (A-2 after A-1), M2 holds B-1 after A-2.
func twoMilestones() []labIssue {
	return []labIssue{{Key: "A-1", Milestone: "M1"}, {Key: "A-2", Milestone: "M1", After: []string{"A-1"}}, {Key: "B-1", Milestone: "M2", After: []string{"A-2"}}}
}

// runMilestones starts a project lane run for each milestone in turn and runs it to its end (resuming it after every kill); it answers the
// runs.
func runMilestones(l *laneLab, milestones ...string) []string {
	var runs []string
	for _, ms := range milestones {
		spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":%q}`, l.plan, ms), "--handoff", l.file)
		l.closed(l.runToEnd(spec.RunID))
		runs = append(runs, spec.RunID)
	}
	return runs
}

// TestProjectLaneTwoMilestonesOnTempStore (CRW-1277, the flow project's finish line): on a fake host, a temporary store and temporary git,
// `crw flow` runs of the project lane drive a two-milestone plan to integrated heads through the relay's scheduler: every issue node is
// released to an agent of the run, accepted on the agent path with its own acceptance and integrated onto the target branch, and each
// milestone closes. A process killed right after any effect-bearing call of the relay, before the lane recorded the answer, is resumed and
// the plan ends the same: no node is released, accepted or merged twice.
func TestProjectLaneTwoMilestonesOnTempStore(t *testing.T) {
	clean := newLaneLab(t, twoMilestones()...)
	runMilestones(clean, "M1", "M2")
	clean.effectsOnce("A-1", "A-2", "B-1")
	if strings.Join(clean.reports, ",") != "plan-1/M1,plan-1/M2" {
		t.Errorf("the milestone reports sent up: %v", clean.reports)
	}
	writes := clean.writes
	if writes < 9 {
		t.Fatalf("the clean run made %d effect-bearing relay calls: %v", writes, clean.effects)
	}
	t.Logf("the clean run made %d effect-bearing relay calls: %s", writes, strings.Join(clean.effects, " "))
	if t.Failed() || testing.Short() {
		return
	}
	for k := 1; k <= writes; k++ {
		t.Run(fmt.Sprintf("kill after call %d", k), func(t *testing.T) {
			l := newLaneLab(t, twoMilestones()...)
			l.killAt = k
			runMilestones(l, "M1", "M2")
			// the call the process was killed after was asked again by the resumed process, which the relay answered as a replay
			asked := 0
			for _, e := range l.effects {
				if e == l.effects[k-1] {
					asked++
				}
			}
			if l.processes != 3 || asked != 2 {
				t.Fatalf("killed after %s: %d processes, the call asked %d times: %v", l.effects[k-1], l.processes, asked, l.effects)
			}
			l.effectsOnce("A-1", "A-2", "B-1")
		})
	}
	// a process killed after an integration batch moved the branch and before the run observed the landing: the resumed process asks
	// again, the relay merges nothing again, and the observation is made
	t.Run("kill between the batch and its observation", func(t *testing.T) {
		l := newLaneLab(t, twoMilestones()...)
		l.killBetween = l.nodeOf["A-2"]
		runMilestones(l, "M1", "M2")
		if l.processes != 3 {
			t.Fatalf("%d processes: %v", l.processes, l.effects)
		}
		l.effectsOnce("A-1", "A-2", "B-1")
		if n := l.count("SELECT COUNT(*) FROM dag_integration_observations WHERE acceptance_id IN (SELECT acceptance_id FROM dag_agent_acceptances WHERE node_id = ?)", l.nodeOf["A-2"]); n == 0 {
			t.Errorf("the landing of A-2 was never observed")
		}
	})
}
