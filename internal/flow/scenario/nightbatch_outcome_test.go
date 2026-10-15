package scenario

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// TestNightBatchTellsTheMilestoneState (CRW-1487): a night batch returns its own wrapper ({state: closed|incomplete, milestone: the
// project lane's result, lanes, ...}), so the outcome its finished row carries, and the completion notice, are the milestone's state
// and reason the project lane came to, with the wrapper's state kept beside them. Each case runs the real night-batch and project-lane
// workflows to a milestone that is escalated (the design loop's round cap), held (the final verification cannot run) or stopped (the
// run lost its plan's coordinator epoch).
func TestNightBatchTellsTheMilestoneState(t *testing.T) {
	cases := []struct {
		name, state, reason string
		setup               func(l *Lab)
	}{
		{"escalated", "escalated", "design_audit_rounds", func(l *Lab) {
			l.Sequence("^design-audit:r\\d+$", func(*Call) fakehostPlan {
				return Say(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "no", "evidence": []string{}}}, "summary": "no"})
			})
		}},
		{"held", "held", "verify_unavailable", func(l *Lab) {
			l.Verify.Unavailable = map[string]bool{"": true}
		}},
		{"stopped", "stopped", ops.ReasonEpochLost, func(l *Lab) {
			l.Relay.ReleaseErr = func(node string) error {
				return fmt.Errorf("%w plan-1 (another session claimed it): stale_coordinator_epoch", ops.ErrEpochLost)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := NewLab(t, twoNodes()...)
			sender := l.wireUpward(t, false)
			c.setup(l)
			res := l.NightBatch(nightArgs("CRW-A", "CRW-B"), false)
			if res.Status != engine.RunCompleted {
				t.Fatalf("run %s: %s", res.Status, res.Error)
			}
			if got := Value[nightResult](t, res); got.State != "incomplete" {
				t.Fatalf("the night batch's own state %q, want incomplete", got.State)
			}
			o := finishedOutcome(t, l)
			if o == nil || o.State != c.state || o.Reason != c.reason || o.RunState != "incomplete" {
				t.Fatalf("the finished outcome %+v, want the milestone's %s/%s and the run state incomplete", o, c.state, c.reason)
			}
			done := completionTexts(sender)
			if len(done) != 1 || !strings.Contains(done[0], "state="+c.state+" reason="+c.reason) || strings.Contains(done[0], "state=incomplete") {
				t.Errorf("the completion notices %q", done)
			}
		})
	}
}
