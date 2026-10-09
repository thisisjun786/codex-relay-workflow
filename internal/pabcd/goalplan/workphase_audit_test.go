package goalplan

import "testing"

// CRW-859 (package audit G2, G3). The oracle's closeFixedWorkPhase maps every work phase
// whose id equals the target to done (goalplan.ts:2020-2023), and its advance answers
// no_active when the effective cursor is falsy, which includes the empty string
// (goalplan.ts:2217). These rows are the malformed plans the audit named.

func auditPhase(id string, status WorkPhaseStatus) GoalplanWorkPhase {
	return GoalplanWorkPhase{ID: id, Status: status, Tasks: []GoalplanTask{}}
}

// G2: a duplicate id closes every phase carrying it, so no pending copy is re-activated.
func TestCRW859_CloseFixedClosesEveryPhaseWithTheTargetID(t *testing.T) {
	plan := &Goalplan{
		WorkPhases: []GoalplanWorkPhase{
			auditPhase("dup", WorkPhaseInProgress),
			auditPhase("dup", WorkPhasePending),
		},
	}
	cursor := "dup"
	plan.ActiveWorkPhaseID = &cursor

	got := workPhaseCloseFixed(plan, "dup", WorkPhaseRecordedNext{})
	if got.Kind != WorkPhaseCloseFixedOK {
		t.Fatalf("close kind = %q, want ok", got.Kind)
	}
	for i, wp := range got.Plan.WorkPhases {
		if wp.Status != WorkPhaseDone {
			t.Errorf("phase %d (id %q) status = %q, want done", i, wp.ID, wp.Status)
		}
	}
	if got.Plan.ActiveWorkPhaseID != nil {
		t.Errorf("cursor = %q, want nil: no runnable phase is left", *got.Plan.ActiveWorkPhaseID)
	}
	if plan.WorkPhases[1].Status != WorkPhasePending {
		t.Errorf("caller's plan was edited in place")
	}
}

// G2 through the advance door the D close uses: the same duplicate pair must not come back
// as an open phase after the advance.
func TestCRW859_AdvanceClosesEveryPhaseWithTheCursorID(t *testing.T) {
	cursor := "dup"
	plan := &Goalplan{
		ActiveWorkPhaseID: &cursor,
		WorkPhases: []GoalplanWorkPhase{
			auditPhase("dup", WorkPhaseInProgress),
			auditPhase("dup", WorkPhasePending),
		},
	}
	got := workPhaseAdvance(plan)
	if got.Kind != WorkPhaseAdvanceOK {
		t.Fatalf("advance kind = %q, want ok", got.Kind)
	}
	for i, wp := range got.Plan.WorkPhases {
		if wp.Status != WorkPhaseDone {
			t.Errorf("phase %d (id %q) status = %q, want done", i, wp.ID, wp.Status)
		}
	}
}

// G3: an empty-string effective cursor is falsy in the oracle, so the advance answers
// no_active and closes nothing.
func TestCRW859_AdvanceTreatsAnEmptyEffectiveCursorAsNoActive(t *testing.T) {
	plan := &Goalplan{WorkPhases: []GoalplanWorkPhase{auditPhase("", WorkPhasePending)}}
	got := workPhaseAdvance(plan)
	if got.Kind != WorkPhaseAdvanceNoActive {
		t.Fatalf("advance kind = %q, want no_active (oracle: falsy effective id)", got.Kind)
	}
	if got.ClosedID != nil {
		t.Errorf("closed id = %q, want none", *got.ClosedID)
	}
	if plan.WorkPhases[0].Status != WorkPhasePending {
		t.Errorf("phase with empty id was closed")
	}
}

// G3 control: an empty explicit cursor is unset, and the next runnable phase is the effective one.
func TestCRW859_EmptyExplicitCursorFallsThroughToTheNextPhase(t *testing.T) {
	empty := ""
	plan := &Goalplan{
		ActiveWorkPhaseID: &empty,
		WorkPhases:        []GoalplanWorkPhase{auditPhase("p1", WorkPhasePending)},
	}
	got := EffectiveActiveWorkPhaseID(plan)
	if got == nil || *got != "p1" {
		t.Fatalf("effective id = %v, want p1", got)
	}
}
