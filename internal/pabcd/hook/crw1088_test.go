package hook

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1088: the Stop progress judgment compares a new metric row only with the earlier row of the same metric and work
// phase (a singleton is unknown and recharges nothing), and the plateau of a bound goalplan is judged on its active work
// phase, once per evaluation window.

func crw1088Record(t *testing.T, cwd, name string, value float64, workPhase *string) {
	t.Helper()
	if _, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{SessionID: stopSID, MetricName: name, Value: value, Source: metric.OperatorEntered, WorkPhaseID: workPhase}); err != nil {
		t.Fatal(err)
	}
}

func crw1088Count(t *testing.T, cwd string) float64 {
	t.Helper()
	return state.ReadState(cwd, stopSID).StopBlockCount
}

// End condition 1: a new singleton metric name before every Stop is no evaluation of anything, so after the first phase
// observation the count climbs to the budget and the fourth Stop releases (the oracle reset the count on each one).
func TestCRW1088AlternatingSingletonMetricsDoNotRecharge(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	var answers []bool
	for i, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		crw1088Record(t, cwd, name, 1, nil)
		answers = append(answers, strings.Contains(stopRun(cwd, env).Stdout, `"decision":"block"`))
		if i < StopMaxBlocks {
			if got := crw1088Count(t, cwd); got != float64(i+1) {
				t.Errorf("Stop %d after singleton %q: count %v, want %d", i+1, name, got, i+1)
			}
		}
	}
	if want := []bool{true, true, true, false, false}; !slices.Equal(answers, want) {
		t.Errorf("answers %v, want %v", answers, want)
	}
}

// End condition 2: two comparable rising rows recharge once; an equal or falling row does not.
func TestCRW1088ComparableRowsRechargeOnlyWhenRising(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	stopRun(cwd, env) // the phase observation: count 1
	crw1088Record(t, cwd, "score", 1, nil)
	crw1088Record(t, cwd, "score", 2, nil)
	stopRun(cwd, env)
	if got := crw1088Count(t, cwd); got != 1 {
		t.Fatalf("two rising rows: count %v, want 1", got)
	}
	stopRun(cwd, env)
	if got := crw1088Count(t, cwd); got != 2 {
		t.Errorf("the same rows recharged twice: count %v", got)
	}
	for _, v := range []float64{2, 1.5} {
		crw1088Record(t, cwd, "score", v, nil)
		before := crw1088Count(t, cwd)
		stopRun(cwd, env)
		if after := crw1088Count(t, cwd); after != before+1 && !(before == StopMaxBlocks && after == StopMaxBlocks+1) {
			t.Errorf("a non-improving row %v recharged: count %v -> %v", v, before, after)
		}
	}
}

// End condition 3: with wp-new bound and only wp-old's rows flat, the Stop is the ordinary continuation; once wp-new's own
// window is flat, the divergence block comes once for that window, and again only when a new row opens a new window.
func TestCRW1088PlateauIsJudgedOnTheActiveWorkPhaseOncePerWindow(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "phases" })
	stopWritePlan(t, cwd, "phases", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp-old", "Old", goalplan.WorkPhaseDone), stopWorkPhase("wp-new", "New", goalplan.WorkPhaseInProgress)}
	})
	if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
		t.Fatal(err)
	}
	old, cur := ptr("wp-old"), ptr("wp-new")
	crw1088Record(t, cwd, "score", 1, old)
	crw1088Record(t, cwd, "score", 1, old)
	kind := func(t *testing.T, env host.LookupEnv) string {
		t.Helper()
		reason := stopBlockReason(t, stopRun(cwd, env))
		switch {
		case strings.HasPrefix(reason, "[crw — objective plateau]"):
			return "plateau"
		case strings.HasPrefix(reason, "[crw — continue PABCD]"):
			return "continue"
		}
		t.Fatalf("unexpected block %q", reason)
		return ""
	}
	if got := kind(t, env); got != "continue" {
		t.Fatalf("wp-old's flat rows redirected wp-new: %s", got)
	}
	crw1088Record(t, cwd, "score", 1, cur)
	crw1088Record(t, cwd, "score", 1, cur)
	if got := kind(t, env); got != "plateau" {
		t.Fatalf("wp-new's flat window: %s", got)
	}
	if got := kind(t, env); got != "continue" {
		t.Errorf("the same window asked for divergence again: %s", got)
	}
	crw1088Record(t, cwd, "score", 1, cur) // new evaluation evidence: a new window
	// the cycle moves on as a transition does (the counter is reset), so the budget is not what this case measures
	s := state.ReadState(cwd, stopSID)
	s.Phase, s.StopBlockPhase, s.StopBlockCount = state.PhaseC, nil, 0
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	if got := kind(t, env); got != "plateau" {
		t.Errorf("a new flat window: %s", got)
	}
}
