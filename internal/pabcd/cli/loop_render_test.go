package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// TestLoopRenderPlanOracle renders the recorded plans with the renderer this issue ports
// and compares byte for byte, including the writeLock absent and present branches. The
// plan is the oracle's own written plan, so the input shape is the oracle's too.
func TestLoopRenderPlanOracle(t *testing.T) {
	o := loadLoopOracle(t)
	sub := loopSubstitution(t)
	cwd := t.TempDir()
	if len(o.Plans) == 0 {
		t.Fatal("oracle contains no rendered plans")
	}
	for _, c := range o.Plans {
		t.Run(c.Name, func(t *testing.T) {
			var plan goalplan.Goalplan
			planJSON := strings.ReplaceAll(string(c.Plan), "<TS>", "2026-01-01T00:00:00.000Z")
			if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
				t.Fatal(err)
			}
			var lock *goalplan.GoalplanLockStatus
			if c.Lock {
				lock = &goalplan.GoalplanLockStatus{
					Path:   filepath.Join(cwd, ".crw", "goalplans", plan.Slug, ".goalplan.lock"),
					Exists: c.Present,
				}
				if c.Present {
					age := 42.0
					lock.AgeMs = &age
				}
			}
			want := strings.ReplaceAll(sub.Expected(c.Output), "<WS>", cwd)
			if c.Present {
				want = strings.ReplaceAll(want, "<AGE>", "42")
			}
			if got := RenderLoopPlan(&plan, lock); got != want {
				t.Fatalf("render %s:\n got %q\nwant %q", c.Name, got, want)
			}
		})
	}
}
