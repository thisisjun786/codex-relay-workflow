package ops

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// TestAnEndedLandGivesItsReleaseBack (CRW-1354, post-evaluation d1): an escalation with end_land - a gate-only land whose head did not
// pass its gates - asks the relay to close the node's execution with land_ended whatever its own reason, and the row says it closed it;
// an escalation without it closes only for the reasons that give a node up. A close the relay refuses is recorded on the row
// (close_refused) and the op completes: the land ends escalated with its execution as the relay keeps it. A lost epoch is the run's.
func TestAnEndedLandGivesItsReleaseBack(t *testing.T) {
	w := newWorld(t)
	w.must("escalate", "n1", EscalateInfo{Reason: "land_review_needs_changes", EndLand: true})
	if len(w.relay.closes) != 1 || w.relay.closes[0].Reason != ReasonLandEnded || w.relay.closes[0].Node != "n1" {
		t.Fatalf("the closes of an ended land: %+v", w.relay.closes)
	}
	row := w.j.Events()[len(w.j.Events())-1]
	if row.Data["closed"] != true || row.Data["end_land"] != true {
		t.Errorf("the row of an ended land: %v", row.Data)
	}
	w.must("escalate", "n2", EscalateInfo{Reason: "land_review_needs_changes"})
	if len(w.relay.closes) != 1 {
		t.Errorf("an escalation without end_land closed the execution: %+v", w.relay.closes)
	}
	w.must("escalate", "n3", EscalateInfo{Reason: "tiers_exhausted", EndLand: true})
	if last := w.relay.closes[len(w.relay.closes)-1]; last.Reason != "tiers_exhausted" {
		t.Errorf("a reason that gives a node up keeps its own close reason: %+v", last)
	}

	w.relay.closeErr = errors.New("disposition_conflict: the head may have landed")
	w.must("escalate", "n4", EscalateInfo{Reason: "integration_refused", EndLand: true})
	row = w.j.Events()[len(w.j.Events())-1]
	if row.Data["closed"] != false || row.Data["close_refused"] == nil || row.Event != engine.EvCompleted {
		t.Errorf("a refused close of an ended land: %v", row.Data)
	}
	w.relay.closeErr = fmt.Errorf("%w plan-1 (another session claimed it)", ErrEpochLost)
	if _, err := w.op("escalate", "n5", EscalateInfo{Reason: "land_verification_failed", EndLand: true}); !errors.Is(err, ErrEpochLost) {
		t.Errorf("a lost epoch at the close of an ended land: %v", err)
	}
}
