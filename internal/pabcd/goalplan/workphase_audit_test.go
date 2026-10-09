package goalplan

import (
	"slices"
	"testing"
)

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

// CRW-1076. When the recorded successor is already done, the oracle keeps the cursor with one
// find over (id, in_progress, ready) (goalplan.ts:2085), so a second phase carrying the cursor's id
// is found when the first is blocked. The expected values are closeFixedWorkPhase from the oracle's
// dist/goalplan.js, called with the same plans (closing wp1, recorded next wp2).
func TestCRW1076_CloseFixedKeepsTheCursorOnTheRunnableDuplicate(t *testing.T) {
	cases := []struct {
		name   string
		phases []GoalplanWorkPhase
		want   []WorkPhaseStatus
	}{
		{"blocked wp3 then runnable wp3", []GoalplanWorkPhase{
			auditPhase("wp1", WorkPhasePending), auditPhase("wp2", WorkPhaseDone),
			auditPhase("wp3", WorkPhaseBlocked), auditPhase("wp3", WorkPhaseInProgress)},
			[]WorkPhaseStatus{WorkPhaseDone, WorkPhaseDone, WorkPhaseInProgress, WorkPhaseInProgress}},
		{"runnable wp3 then blocked wp3", []GoalplanWorkPhase{
			auditPhase("wp1", WorkPhasePending), auditPhase("wp2", WorkPhaseDone),
			auditPhase("wp3", WorkPhaseInProgress), auditPhase("wp3", WorkPhaseBlocked)},
			[]WorkPhaseStatus{WorkPhaseDone, WorkPhaseDone, WorkPhaseInProgress, WorkPhaseInProgress}},
		{"no duplicate", []GoalplanWorkPhase{
			auditPhase("wp1", WorkPhasePending), auditPhase("wp2", WorkPhaseDone),
			auditPhase("wp3", WorkPhaseInProgress)},
			[]WorkPhaseStatus{WorkPhaseDone, WorkPhaseDone, WorkPhaseInProgress}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cursor, next := "wp3", "wp2"
			plan := &Goalplan{ActiveWorkPhaseID: &cursor, WorkPhases: c.phases}
			before := make([]GoalplanWorkPhase, len(c.phases))
			copy(before, c.phases)
			got := workPhaseCloseFixed(plan, "wp1", WorkPhaseRecordedNext{Known: true, ID: &next})
			if got.Kind != WorkPhaseCloseFixedOK || got.Plan == nil {
				t.Fatalf("kind = %q, want ok", got.Kind)
			}
			if got.Plan.ActiveWorkPhaseID == nil || *got.Plan.ActiveWorkPhaseID != "wp3" {
				t.Errorf("cursor = %v, want wp3", got.Plan.ActiveWorkPhaseID)
			}
			var statuses []WorkPhaseStatus
			for _, wp := range got.Plan.WorkPhases {
				statuses = append(statuses, wp.Status)
			}
			if !slices.Equal(statuses, c.want) {
				t.Errorf("statuses = %v, want %v", statuses, c.want)
			}
			for i := range before {
				if plan.WorkPhases[i].Status != before[i].Status || *plan.ActiveWorkPhaseID != "wp3" {
					t.Errorf("the input plan changed at phase %d", i)
				}
			}
		})
	}
}

// CRW-1076. The readiness half of the same find: a phase carrying the cursor's id that waits on an unfinished dependency is
// not found, and a later one that is ready is. Expected values are closeFixedWorkPhase from the oracle's dist/goalplan.js on
// the same plans (closing wp1, recorded next wp2, wp9 pending).
func TestCRW1076_CloseFixedKeepsTheCursorOnlyOnAReadyDuplicate(t *testing.T) {
	waiting := func() GoalplanWorkPhase {
		wp := auditPhase("wp3", WorkPhaseInProgress)
		wp.DependsOn = []string{"wp9"}
		return wp
	}
	cases := []struct {
		name   string
		phases []GoalplanWorkPhase
		want   string // the cursor, "" for none
	}{
		{"waiting wp3 then ready wp3", []GoalplanWorkPhase{waiting(), auditPhase("wp3", WorkPhaseInProgress)}, "wp3"},
		{"ready wp3 then waiting wp3", []GoalplanWorkPhase{auditPhase("wp3", WorkPhaseInProgress), waiting()}, "wp3"},
		{"only waiting wp3", []GoalplanWorkPhase{waiting(), waiting()}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phases := append([]GoalplanWorkPhase{auditPhase("wp1", WorkPhasePending), auditPhase("wp2", WorkPhaseDone)}, c.phases...)
			phases = append(phases, auditPhase("wp9", WorkPhasePending))
			cursor, next := "wp3", "wp2"
			got := workPhaseCloseFixed(&Goalplan{ActiveWorkPhaseID: &cursor, WorkPhases: phases}, "wp1", WorkPhaseRecordedNext{Known: true, ID: &next})
			if got.Kind != WorkPhaseCloseFixedOK || got.Plan == nil {
				t.Fatalf("kind = %q, want ok", got.Kind)
			}
			if have := got.Plan.ActiveWorkPhaseID; (c.want == "") != (have == nil) || have != nil && *have != c.want {
				t.Errorf("cursor = %v, want %q", have, c.want)
			}
		})
	}
}
