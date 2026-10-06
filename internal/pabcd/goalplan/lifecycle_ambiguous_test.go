package goalplan

import (
	"testing"
)

// The oracle (goalplan.ts:1351-1406 at v0.2.40) judged the first entry of a duplicated id and
// then rewrote every entry carrying it, so a plan whose duplicate ids slipped past validation
// lost the other entry's outcome or captured evidence. This port refuses instead, in the shape
// DecideGoalplanDecision already uses (lifecycle.go): nothing changes and the plan is not
// repaired here. The recorded corpus keeps the oracle's answers for the cases these rules move
// (complete_ok_duplicate_task_ids, meet_ok_duplicate_criterion_ids and
// meet_unchanged_duplicate_met_then_open), tagged intentionally-changed in the replay below.

// goalplanAmbiguousCompletePlan is the issue's reproduction: one work phase holding two tasks
// of one id, the second already done with its own outcome.
func goalplanAmbiguousCompletePlan() *Goalplan {
	plan := BuildGoalplan(NewGoalplanInput{Objective: "Ambiguous ids", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	plan.WorkPhases = []GoalplanWorkPhase{{
		ID: "wp1", Title: "Exporter", Status: WorkPhaseInProgress,
		Tasks: []GoalplanTask{
			{ID: "t-1", Title: "A", Status: TaskPending},
			{ID: "t-1", Title: "B", Status: TaskDone, Outcome: "original-B-proof"},
		},
		CriteriaIDs: []string{},
	}}
	plan.Criteria = []GoalplanCriterion{}
	return plan
}

// goalplanAmbiguousMeetPlan is the criteria counterpart: one open criterion of an id the plan
// already holds a met one of, whose captured evidence must survive the refusal.
func goalplanAmbiguousMeetPlan() *Goalplan {
	proof := "original-B-proof"
	plan := BuildGoalplan(NewGoalplanInput{Objective: "Ambiguous ids", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	plan.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "Exporter", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{}, CriteriaIDs: []string{"c-1"}}}
	plan.Criteria = []GoalplanCriterion{
		{ID: "c-1", Scenario: "s", Surface: SurfaceLogic, Status: CriterionOpen},
		{ID: "c-1", Scenario: "s", Surface: SurfaceLogic, Status: CriterionMet, CapturedEvidence: &proof},
	}
	return plan
}

func TestGoalplanAmbiguousCompleteTaskRefusesAndKeepsTheOtherOutcome(t *testing.T) {
	plan := goalplanAmbiguousCompletePlan()
	before := mustJSON(t, plan)
	result := CompleteGoalplanTask(plan, "wp1", "t-1", "new-A-proof")
	if result.Kind != GoalplanLifecycleRejected {
		t.Fatalf("complete with duplicate task ids = %#v", result)
	}
	want := "task id 'wp1/t-1' is ambiguous (2 entries); repair the plan first"
	if result.Reason != want {
		t.Fatalf("reason = %q, want %q", result.Reason, want)
	}
	if after := mustJSON(t, plan); after != before {
		t.Fatalf("refused complete changed the plan:\n%s", after)
	}
	if got := plan.WorkPhases[0].Tasks[1].Outcome; got != "original-B-proof" {
		t.Fatalf("B outcome = %q, want original-B-proof", got)
	}
	if got := plan.WorkPhases[0].Tasks[0].Status; got != TaskPending {
		t.Fatalf("A status = %s, want pending", got)
	}
}

// The write path is where the loss happened: under the goalplan write lock the refused operation
// must leave the stored plan alone, where the oracle's answer wrote the other task's outcome over B's.
func TestGoalplanAmbiguousCompleteTaskKeepsTheOtherOutcomeOnDisk(t *testing.T) {
	cwd := t.TempDir()
	plan := goalplanAmbiguousCompletePlan()
	writeTestRequire(t, WriteGoalplan(cwd, plan))
	lock, err := WithGoalplanWriteLock(cwd, plan.Slug, func(current *Goalplan) (GoalplanLifecycleResult, error) {
		result := CompleteGoalplanTask(current, "wp1", "t-1", "new-A-proof")
		if result.Kind == GoalplanLifecycleChanged {
			if err := WriteGoalplan(cwd, result.Plan); err != nil {
				return result, err
			}
		}
		return result, nil
	}, nil)
	writeTestRequire(t, err)
	if lock.Kind != "ok" || lock.Value == nil {
		t.Fatalf("write lock = %#v", lock)
	}
	if lock.Value.Kind != GoalplanLifecycleRejected {
		t.Fatalf("complete under lock = %s, want rejected", lock.Value.Kind)
	}
	revived := ReadGoalplan(cwd, plan.Slug)
	if revived == nil || len(revived.WorkPhases) != 1 || len(revived.WorkPhases[0].Tasks) != 2 {
		t.Fatalf("revived = %#v", revived)
	}
	if got := revived.WorkPhases[0].Tasks[1].Outcome; got != "original-B-proof" {
		t.Fatalf("stored B outcome = %q, want original-B-proof", got)
	}
}

func TestGoalplanAmbiguousMeetCriterionRefusesAndKeepsTheOtherEvidence(t *testing.T) {
	plan := goalplanAmbiguousMeetPlan()
	before := mustJSON(t, plan)
	result := MeetGoalplanCriterion(plan, "c-1", "new-A-proof")
	if result.Kind != GoalplanLifecycleRejected {
		t.Fatalf("meet with duplicate criterion ids = %#v", result)
	}
	want := "criterion id 'c-1' is ambiguous (2 entries); repair the plan first"
	if result.Reason != want {
		t.Fatalf("reason = %q, want %q", result.Reason, want)
	}
	if after := mustJSON(t, plan); after != before {
		t.Fatalf("refused meet changed the plan:\n%s", after)
	}
	if got := plan.Criteria[1].CapturedEvidence; got == nil || *got != "original-B-proof" {
		t.Fatalf("B evidence = %v, want original-B-proof", got)
	}
	if got := plan.Criteria[0].Status; got != CriterionOpen {
		t.Fatalf("A status = %s, want open", got)
	}
}

func TestGoalplanAmbiguousMeetCriterionKeepsTheOtherEvidenceOnDisk(t *testing.T) {
	cwd := t.TempDir()
	plan := goalplanAmbiguousMeetPlan()
	writeTestRequire(t, WriteGoalplan(cwd, plan))
	lock, err := WithGoalplanWriteLock(cwd, plan.Slug, func(current *Goalplan) (GoalplanLifecycleResult, error) {
		result := MeetGoalplanCriterion(current, "c-1", "new-A-proof")
		if result.Kind == GoalplanLifecycleChanged {
			if err := WriteGoalplan(cwd, result.Plan); err != nil {
				return result, err
			}
		}
		return result, nil
	}, nil)
	writeTestRequire(t, err)
	if lock.Kind != "ok" || lock.Value == nil {
		t.Fatalf("write lock = %#v", lock)
	}
	if lock.Value.Kind != GoalplanLifecycleRejected {
		t.Fatalf("meet under lock = %s, want rejected", lock.Value.Kind)
	}
	revived := ReadGoalplan(cwd, plan.Slug)
	if revived == nil || len(revived.Criteria) != 2 {
		t.Fatalf("revived = %#v", revived)
	}
	if got := revived.Criteria[1].CapturedEvidence; got == nil || *got != "original-B-proof" {
		t.Fatalf("stored B evidence = %v, want original-B-proof", got)
	}
}

func TestGoalplanAmbiguousCompleteTaskRefusesDuplicateWorkPhaseIDs(t *testing.T) {
	plan := BuildGoalplan(NewGoalplanInput{Objective: "Ambiguous phases", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	plan.WorkPhases = []GoalplanWorkPhase{
		{ID: "wp1", Title: "Exporter", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{{ID: "t-1", Title: "A", Status: TaskPending}}, CriteriaIDs: []string{}},
		{ID: "wp1", Title: "Exporter copy", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{{ID: "t-1", Title: "B", Status: TaskPending}}, CriteriaIDs: []string{}},
	}
	plan.Criteria = []GoalplanCriterion{}
	before := mustJSON(t, plan)
	result := CompleteGoalplanTask(plan, "wp1", "t-1", "ok")
	if result.Kind != GoalplanLifecycleRejected {
		t.Fatalf("complete with duplicate work phase ids = %#v", result)
	}
	want := "work phase id 'wp1' is ambiguous (2 entries); repair the plan first"
	if result.Reason != want {
		t.Fatalf("reason = %q, want %q", result.Reason, want)
	}
	if after := mustJSON(t, plan); after != before {
		t.Fatalf("refused complete changed the plan:\n%s", after)
	}
}

// The ambiguity refusal precedes the already-done answer: with the first task of a duplicated id
// already done, the oracle answered unchanged and this port refuses, because the plan is not
// repaired here and the second entry is not the one the caller named. (The corpus pins the same
// ordering for meet in meet_unchanged_duplicate_met_then_open.)
func TestGoalplanAmbiguousCompleteTaskPrecedesTheAlreadyDoneAnswer(t *testing.T) {
	plan := BuildGoalplan(NewGoalplanInput{Objective: "Ambiguous ids", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	plan.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "Exporter", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{
		{ID: "t-1", Title: "A", Status: TaskDone, Outcome: "first-proof"},
		{ID: "t-1", Title: "B", Status: TaskPending},
	}, CriteriaIDs: []string{}}}
	plan.Criteria = []GoalplanCriterion{}
	before := mustJSON(t, plan)
	result := CompleteGoalplanTask(plan, "wp1", "t-1", "again")
	if result.Kind != GoalplanLifecycleRejected {
		t.Fatalf("complete with a done first duplicate = %#v", result)
	}
	want := "task id 'wp1/t-1' is ambiguous (2 entries); repair the plan first"
	if result.Reason != want {
		t.Fatalf("reason = %q, want %q", result.Reason, want)
	}
	if after := mustJSON(t, plan); after != before {
		t.Fatalf("refused complete changed the plan:\n%s", after)
	}
	if got := plan.WorkPhases[0].Tasks[0].Outcome; got != "first-proof" {
		t.Fatalf("A outcome = %q, want first-proof", got)
	}
}

// The refusals are scoped to the operations that rewrite every entry of an id: complete, meet and
// decide without duplicates answer exactly as before, and an id that is in no entry still reads
// "not in this plan" ahead of the ambiguity rule.
func TestGoalplanAmbiguousOperationsWithoutDuplicatesUnchanged(t *testing.T) {
	ts := "2026-01-01T00:00:00.000Z"
	plan := BuildGoalplan(NewGoalplanInput{Objective: "Single ids", Now: func() string { return ts }})
	plan.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "Exporter", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{{ID: "t-1", Title: "A", Status: TaskPending}}, CriteriaIDs: []string{"c-1"}}}
	plan.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", Surface: SurfaceLogic, Status: CriterionOpen}}
	plan.Decisions = []GoalplanDecision{{ID: "dec-1", Question: "Which?", Status: DecisionOpen, AskedAt: ts}}

	complete := CompleteGoalplanTask(plan, "wp1", "t-1", "done-proof")
	if complete.Kind != GoalplanLifecycleChanged || complete.Plan == nil || complete.Plan.WorkPhases[0].Tasks[0].Outcome != "done-proof" {
		t.Fatalf("complete = %#v", complete)
	}
	meet := MeetGoalplanCriterion(plan, "c-1", "met-proof")
	if meet.Kind != GoalplanLifecycleChanged || meet.Plan == nil {
		t.Fatalf("meet = %#v", meet)
	}
	if got := meet.Plan.Criteria[0].CapturedEvidence; got == nil || *got != "met-proof" {
		t.Fatalf("meet evidence = %v", got)
	}
	decide := DecideGoalplanDecision(plan, "dec-1", "yes", "2026-01-01T01:00:00.000Z")
	if decide.Kind != GoalplanLifecycleChanged || decide.Plan == nil || decide.Plan.Decisions[0].Status != DecisionDecided {
		t.Fatalf("decide = %#v", decide)
	}
	// A missing id keeps the "not in this plan" refusal ahead of the ambiguity rule.
	if got := CompleteGoalplanTask(plan, "wp1", "ghost", "x"); got.Kind != GoalplanLifecycleRejected || got.Reason != "task 'wp1/ghost' is not in this plan" {
		t.Fatalf("missing task = %#v", got)
	}
	if got := MeetGoalplanCriterion(plan, "ghost", "x"); got.Kind != GoalplanLifecycleRejected || got.Reason != "criterion 'ghost' is not in this plan" {
		t.Fatalf("missing criterion = %#v", got)
	}
}
