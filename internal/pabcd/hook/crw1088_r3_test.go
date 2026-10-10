package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1088 as the third verification round of 5888f791 judged it: the bound on the divergence windows a state remembers.

// crwSetWindows writes windows as the state's stopDivergenceWindows member, as a state file holds it.
func crwSetWindows(t *testing.T, cwd string, windows map[string]any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(stopStateBytes(t, cwd)), &m); err != nil {
		t.Fatal(err)
	}
	m["stopDivergenceWindows"] = windows
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.StatePath(cwd, stopSID), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// crwWindowRig is a maximize session in flight at B bound to a plan with work phases ids, each with two flat score rows.
func crwWindowRig(t *testing.T, ids ...string) (string, func(string) string) {
	t.Helper()
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "phases" })
	stopWritePlan(t, cwd, "phases", func(p *goalplan.Goalplan) {
		for _, id := range ids {
			p.WorkPhases = append(p.WorkPhases, stopWorkPhase(id, id, goalplan.WorkPhaseInProgress))
		}
	})
	if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		crw1088Record(t, cwd, "score", 1, ptr(id))
		crw1088Record(t, cwd, "score", 1, ptr(id))
	}
	visit := func(id string) string {
		t.Helper()
		p := goalplan.ReadGoalplan(cwd, "phases")
		p.ActiveWorkPhaseID = ptr(id)
		if err := goalplan.WriteGoalplan(cwd, p); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(stopBlockReason(t, stopRun(cwd, env)), "[crw — objective plateau]") {
			return "plateau"
		}
		return "continue"
	}
	return cwd, visit
}

// The windows a state remembers are bounded, and the bound evicts only the series updated longest ago (CRW-1088 d2). Red on
// 5888f791: at 64 series every write, the update of a series already remembered included, dropped all the others, so a window
// answered for another work phase was asked for again on the next visit.
func TestCRW1088WindowCapEvictsOnlyTheLeastRecentlyUpdatedSeries(t *testing.T) {
	t.Run("an update of a remembered series keeps the others", func(t *testing.T) {
		cwd, visit := crwWindowRig(t, "wp-a", "wp-b")
		windows := map[string]any{"score@wp-a": 2, "score@wp-b": 2}
		for i := len(windows); i < stopMaxDivergenceWindows; i++ {
			windows[fmt.Sprintf("other%02d@wp-x", i)] = 1
		}
		crwSetWindows(t, cwd, windows)
		crw1088Record(t, cwd, "score", 1, ptr("wp-a")) // a new window of wp-a, while 64 series are remembered
		if k := visit("wp-a"); k != "plateau" {
			t.Fatalf("the new window of wp-a: %s, want plateau", k)
		}
		if k := visit("wp-b"); k != "continue" {
			t.Errorf("wp-b's answered window after wp-a's update at the cap: %s, want continue", k)
		}
		if got := len(state.ReadState(cwd, stopSID).StopDivergenceWindows); got != stopMaxDivergenceWindows {
			t.Errorf("%d series remembered, want %d", got, stopMaxDivergenceWindows)
		}
	})
	t.Run("a new series evicts the least recently updated one only", func(t *testing.T) {
		cwd, visit := crwWindowRig(t, "wp-b", "wp-c")
		windows := map[string]any{
			"stale@wp-z": map[string]any{"rows": 1, "seq": 1},
			"score@wp-b": map[string]any{"rows": 2, "seq": 100},
		}
		for i := len(windows); i < stopMaxDivergenceWindows; i++ {
			windows[fmt.Sprintf("other%02d@wp-x", i)] = map[string]any{"rows": 1, "seq": 1 + i}
		}
		crwSetWindows(t, cwd, windows)
		if k := visit("wp-c"); k != "plateau" {
			t.Fatalf("wp-c's first window: %s, want plateau", k)
		}
		got := state.ReadState(cwd, stopSID).StopDivergenceWindows
		if _, ok := got["stale@wp-z"]; ok || len(got) != stopMaxDivergenceWindows {
			t.Errorf("after a 65th series: %d remembered, stale@wp-z kept %v; want 64 without it", len(got), ok)
		}
		for _, series := range []string{"score@wp-b", "score@wp-c", "other02@wp-x"} {
			if _, ok := got[series]; !ok {
				t.Errorf("%s was evicted", series)
			}
		}
		if k := visit("wp-b"); k != "continue" {
			t.Errorf("wp-b's answered window after the eviction: %s, want continue", k)
		}
	})
}
