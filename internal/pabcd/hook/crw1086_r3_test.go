package hook

import (
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1086 as the third verification round of 5888f791 judged it: an unreadable bound goalplan keeps the metric scope.

// A bound goalplan that cannot be read keeps the judgment scope of the work phase recorded for it, not only the work phase: a
// finished work phase's rising row does not recharge the active one's spent budget, and its flat rows are not the active one's
// plateau (CRW-1086 d2, CRW-1088). Red on 5888f791: with the plan unreadable every row of the ledger was judged.
func TestCRW1086UnreadablePlanKeepsTheRecordedMetricScope(t *testing.T) {
	bound := func(t *testing.T) (string, func(), func() StopAnswer) {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug, s.StopBlockTurnID = "two", ptr(crw1086Turn) })
		stopWritePlan(t, cwd, "two", func(p *goalplan.Goalplan) {
			p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp-old", "Old", goalplan.WorkPhaseDone), stopWorkPhase("wp-new", "New", goalplan.WorkPhaseInProgress)}
			p.ActiveWorkPhaseID = ptr("wp-new")
		})
		hide := func() {
			t.Helper()
			dir, err := goalplan.GoalplanDir(cwd, "two")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(dir, dir+".away"); err != nil {
				t.Fatal(err)
			}
		}
		return cwd, hide, func() StopAnswer { return crw1086Stop(cwd, env) }
	}
	t.Run("an inactive work phase's rise is no progress", func(t *testing.T) {
		cwd, hide, stop := bound(t)
		crw1088Record(t, cwd, "score", 1, ptr("wp-old"))
		for i := 1; i <= StopMaxBlocks; i++ {
			if a := stop(); !crw1086IsBlock(a) {
				t.Fatalf("Stop %d must block: %+v", i, a)
			}
		}
		if a := stop(); a != (StopAnswer{}) {
			t.Fatalf("the fourth Stop must release: %+v", a)
		}
		hide()
		crw1088Record(t, cwd, "score", 2, ptr("wp-old"))
		if a := stop(); a != (StopAnswer{}) {
			t.Fatalf("wp-old's rising row recharged wp-new's spent budget while the plan was unreadable: %+v", a)
		}
	})
	t.Run("an inactive work phase's flat rows are not the plateau", func(t *testing.T) {
		cwd, hide, stop := bound(t)
		if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
			t.Fatal(err)
		}
		if a := stop(); !crw1086IsBlock(a) {
			t.Fatalf("the first Stop must block: %+v", a)
		}
		hide()
		crw1088Record(t, cwd, "score", 1, ptr("wp-old"))
		crw1088Record(t, cwd, "score", 1, ptr("wp-old"))
		if r := stopBlockReason(t, stop()); strings.HasPrefix(r, "[crw — objective plateau]") {
			t.Fatalf("wp-old's flat rows answered wp-new with the plateau block while the plan was unreadable: %s", r)
		}
	})
}
