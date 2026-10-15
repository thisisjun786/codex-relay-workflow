package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
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

// TestDesignAuditCarriesPriorFindingsAndDispositionsFromRoundTwo (CRW-1452): the design audit is the critic's call, and from its second
// round on the critic's packet carries the findings of the earlier rounds by id (r<round>.<index>) with the architect's disposition of
// each. The critic answers needs_changes in round 1 with one P1 finding; the design-fix packet names the finding by its id and asks for
// one disposition per id; the architect folds it, and the critic approves in round 2 over a packet that shows the finding, its id and
// the disposition with its reason. The first round's packet has no PRIOR FINDINGS.
func TestDesignAuditCarriesPriorFindingsAndDispositionsFromRoundTwo(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	const finding = "the done condition of A-1 names no file a test can read"
	const reason = "the done condition now names node-A-1.txt"
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		switch c.Label {
		case "design-audit:r1":
			return labSay(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "issue A-1", "severity": "P1", "finding": finding,
				"evidence": []string{"README.md:1"}}}, "summary": "one P1 finding"}), true
		case "design-audit:r2":
			return labSay(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the finding is folded"}), true
		case "design-fix:r1":
			d := l.design()
			d.Dispositions = []ops.Disposition{{Finding: "r1.1", Disposition: "folded", Reason: reason}}
			return labSay(d), true
		}
		return fakehost.TurnPlan{}, false
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))

	roles := map[string]string{}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && strings.HasPrefix(e.Label, "design-") {
			roles[e.Label] = str(e.Data, "role")
		}
	}
	if roles["design-audit:r1"] != "critic" || roles["design-audit:r2"] != "critic" || roles["design-fix:r1"] != "architect" {
		t.Errorf("the roles of the design loop's calls: %v (want both audits the critic's, the fix the architect's)", roles)
	}
	l.mu.Lock()
	r1, fix, r2 := l.texts["design-audit:r1"], l.texts["design-fix:r1"], l.texts["design-audit:r2"]
	l.mu.Unlock()
	if r1 == "" || fix == "" || r2 == "" {
		t.Fatalf("the model was not given every call of the design loop: %v", roles)
	}
	if !strings.Contains(r1, "ROLE: critic.") || strings.Contains(r1, "PRIOR FINDINGS") {
		t.Errorf("the first round's critic packet:\n%s", r1)
	}
	if !strings.Contains(fix, "r1.1 [P1] issue A-1: "+finding) || !strings.Contains(fix, "dispositions") || !strings.Contains(fix, "r1.1") {
		t.Errorf("the design-fix packet does not name the finding by its id and ask for its disposition:\n%s", fix)
	}
	want := "- r1.1 [P1] issue A-1: " + finding + " -> folded: " + reason
	if !strings.Contains(r2, "ROLE: critic.") || !strings.Contains(r2, "PRIOR FINDINGS") || !strings.Contains(r2, want) {
		t.Errorf("the second round's critic packet lacks %q:\n%s", want, r2)
	}
}
