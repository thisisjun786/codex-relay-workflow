package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1189 (R3-S2-F1): the continuation command of a Stop block is copied by the agent, so it carries no placeholder it
// cannot fill. A session without a bound goalplan has no workPhaseId and no testReceiptPath to give: the keys are left out.
func TestStopContinuationOfAnUnboundSessionNamesNoPlaceholder(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC} {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, phase)
		got := stopBlockReason(t, stopRun(cwd, env))
		for _, bad := range []string{"<bound goalplan only", "\"workPhaseId\"", "\"testReceiptPath\""} {
			if strings.Contains(got, bad) {
				t.Errorf("phase %s: %q must not be in %q", phase, bad, got)
			}
		}
		if phase == state.PhaseC && !strings.Contains(got, "\"checkOutput\":\"<test tail>\",\"exitCode\":0}'") {
			t.Errorf("phase C keeps the keys every session needs: %q", got)
		}
		if phase == state.PhaseP && !strings.Contains(got, "\"planUnit\":\"devlog/_plan/YYMMDD_slug\"") {
			t.Errorf("phase P keeps the planUnit key: %q", got)
		}
	}
}

// A bound session gets the real work phase id of its goalplan and, at C, the path its receipt command writes to.
func TestStopContinuationOfABoundSessionCarriesTheRealValues(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC} {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, phase, func(s *state.State) { s.Slug = "export" })
		stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
			p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp7", "Exporter", goalplan.WorkPhaseInProgress)}
		})
		got := stopBlockReason(t, stopRun(cwd, env))
		if strings.Contains(got, "<bound goalplan only") {
			t.Errorf("phase %s: placeholder left in %q", phase, got)
		}
		if !strings.Contains(got, "\"workPhaseId\":\"wp7\"}'") {
			t.Errorf("phase %s: the bound work phase id is missing in %q", phase, got)
		}
		if phase != state.PhaseC {
			continue
		}
		if !strings.Contains(got, "\"testReceiptPath\":\".crw/evidence/rec-s1/test-receipt.json\"") {
			t.Errorf("phase C: the receipt path is missing in %q", got)
		}
		if !strings.Contains(got, "`CRW pabcd receipt test --session rec-s1 -- <the check command>`") {
			t.Errorf("phase C: the command that writes the receipt is missing in %q", got)
		}
	}
}

// A bound goalplan with no runnable work phase has no id to give: the key is left out, not filled with a marker.
func TestStopContinuationOfABoundSessionWithoutARunnablePhaseLeavesTheIDOut(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "done" })
	stopWritePlan(t, cwd, "done", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "Done", goalplan.WorkPhaseDone)}
	})
	got := stopBlockReason(t, stopRun(cwd, env))
	if strings.Contains(got, "<bound") || strings.Contains(got, "\"workPhaseId\"") {
		t.Errorf("no id to give: %q", got)
	}
}
