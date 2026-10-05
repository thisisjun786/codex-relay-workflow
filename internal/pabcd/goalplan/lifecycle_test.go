package goalplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- helpers -----------------------------------------------------------------

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// lifecycleJSON normalises a value through JSON, so map key order and Go slice
// types never decide a comparison.
func lifecycleJSON(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// lifecycleProjectPlan is the recorder's projectPlan: the observable plan shape the
// recorded corpus compares, where an absent optional field projects as null exactly
// as the oracle's undefined does.
func lifecycleProjectPlan(plan *Goalplan) any {
	if plan == nil {
		return nil
	}
	workPhases := []any{}
	for i := range plan.WorkPhases {
		phase := &plan.WorkPhases[i]
		tasks := []any{}
		for j := range phase.Tasks {
			task := &phase.Tasks[j]
			var outcome any
			if task.Outcome != "" {
				outcome = task.Outcome
			}
			var dependsOn any
			if task.DependsOn != nil {
				dependsOn = task.DependsOn
			}
			tasks = append(tasks, map[string]any{"id": task.ID, "title": task.Title, "status": string(task.Status), "outcome": outcome, "dependsOn": dependsOn})
		}
		var awaits any
		if phase.AwaitsDecision != nil {
			awaits = phase.AwaitsDecision
		}
		workPhases = append(workPhases, map[string]any{"id": phase.ID, "status": string(phase.Status), "awaitsDecision": awaits, "tasks": tasks})
	}
	criteria := []any{}
	for i := range plan.Criteria {
		criterion := &plan.Criteria[i]
		var captured any
		if criterion.CapturedEvidence != nil {
			captured = *criterion.CapturedEvidence
		}
		criteria = append(criteria, map[string]any{"id": criterion.ID, "status": string(criterion.Status), "capturedEvidence": captured})
	}
	decisions := []any{}
	for i := range plan.Decisions {
		decision := &plan.Decisions[i]
		var answer, decidedAt, recommendation, options any
		if decision.Answer != "" {
			answer = decision.Answer
		}
		if decision.DecidedAt != "" {
			decidedAt = decision.DecidedAt
		}
		if decision.Recommendation != "" {
			recommendation = decision.Recommendation
		}
		if decision.Options != nil {
			options = decision.Options
		}
		decisions = append(decisions, map[string]any{"id": decision.ID, "question": decision.Question, "status": string(decision.Status), "answer": answer, "askedAt": decision.AskedAt, "decidedAt": decidedAt, "recommendation": recommendation, "options": options})
	}
	return map[string]any{"workPhases": workPhases, "criteria": criteria, "decisions": decisions}
}

// lifecycleObserve writes an operation's answer in the recorder's shape.
func lifecycleObserve(got map[string]any, result GoalplanLifecycleResult) {
	got["kind"] = string(result.Kind)
	got["reason"] = nil
	if result.Reason != "" {
		got["reason"] = result.Reason
	}
	got["plan"] = nil
	if result.Plan != nil {
		got["plan"] = lifecycleProjectPlan(result.Plan)
	}
}

func lifecycleDecodeArgs(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func lifecycleRemainingPhaseIDs(plan *Goalplan) []string {
	out := []string{}
	for _, phase := range RemainingWorkPhases(plan) {
		out = append(out, phase.ID)
	}
	return out
}

func lifecycleUnmetCriterionIDs(plan *Goalplan) []string {
	out := []string{}
	for _, criterion := range UnmetCriteria(plan) {
		out = append(out, criterion.ID)
	}
	return out
}

func lifecycleDoneWithPendingIDs(plan *Goalplan) []string {
	out := []string{}
	for _, phase := range DoneWorkPhasesWithPendingTasks(plan) {
		out = append(out, phase.ID)
	}
	return out
}

// --- recorded oracle corpus --------------------------------------------------

type lifecycleOracleExpect struct {
	Kind   string   `json:"kind"`
	Reason *string  `json:"reason"`
	Plan   any      `json:"plan"`
	IDs    []string `json:"ids"`
	Value  *bool    `json:"value"`
}

type lifecycleOracleCase struct {
	Name   string                `json:"name"`
	Fn     string                `json:"fn"`
	Plan   json.RawMessage       `json:"plan"`
	Args   json.RawMessage       `json:"args"`
	Input  any                   `json:"input"`
	Expect lifecycleOracleExpect `json:"expect"`
}

// TestGoalplanLifecycleOracleCorpus replays the answers recorded from the CXC v0.2.40
// build (goalplan.ts:1236-1445, commit 3c1459ac) for the lifecycle operations: the
// kind, the refusal or unchanged reason, the projected resulting plan, and the
// projected input a mutation would change. The corpus is generated (record-oracle.mjs
// beside it) and never hand-edited.
func TestGoalplanLifecycleOracleCorpus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "lifecycle", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Oracle       string                `json:"oracle"`
		RecordedWith string                `json:"recordedWith"`
		Cases        []lifecycleOracleCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) != 78 {
		t.Fatalf("recorded corpus changed: %d cases", len(corpus.Cases))
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var plan Goalplan
			if err := json.Unmarshal(c.Plan, &plan); err != nil {
				t.Fatal(err)
			}
			before := mustJSON(t, &plan)
			got := map[string]any{}
			switch c.Fn {
			case "ask":
				var args struct {
					ID             string   `json:"id"`
					Question       string   `json:"question"`
					Recommendation *string  `json:"recommendation"`
					Options        []string `json:"options"`
					WorkPhaseIDs   []string `json:"workPhaseIds"`
					AskedAt        string   `json:"askedAt"`
				}
				lifecycleDecodeArgs(t, c.Args, &args)
				lifecycleObserve(got, AskGoalplanDecision(&plan, AskGoalplanDecisionInput{args.ID, args.Question, args.Recommendation, args.Options, args.WorkPhaseIDs, args.AskedAt}))
			case "decide":
				var args struct {
					ID        string `json:"id"`
					Answer    string `json:"answer"`
					DecidedAt string `json:"decidedAt"`
				}
				lifecycleDecodeArgs(t, c.Args, &args)
				lifecycleObserve(got, DecideGoalplanDecision(&plan, args.ID, args.Answer, args.DecidedAt))
			case "add":
				var args struct {
					WorkPhaseID string `json:"workPhaseId"`
					Input       struct {
						ID        string   `json:"id"`
						Title     string   `json:"title"`
						DependsOn []string `json:"dependsOn"`
					} `json:"input"`
				}
				lifecycleDecodeArgs(t, c.Args, &args)
				lifecycleObserve(got, AddGoalplanTask(&plan, args.WorkPhaseID, AddGoalplanTaskInput{args.Input.ID, args.Input.Title, args.Input.DependsOn}))
			case "complete":
				var args struct {
					WorkPhaseID string `json:"workPhaseId"`
					TaskID      string `json:"taskId"`
					Outcome     string `json:"outcome"`
				}
				lifecycleDecodeArgs(t, c.Args, &args)
				lifecycleObserve(got, CompleteGoalplanTask(&plan, args.WorkPhaseID, args.TaskID, args.Outcome))
			case "meet":
				var args struct {
					CriterionID string `json:"criterionId"`
					Evidence    string `json:"evidence"`
				}
				lifecycleDecodeArgs(t, c.Args, &args)
				lifecycleObserve(got, MeetGoalplanCriterion(&plan, args.CriterionID, args.Evidence))
			case "unmet":
				got["ids"] = lifecycleUnmetCriterionIDs(&plan)
			case "doneWithPending":
				got["ids"] = lifecycleDoneWithPendingIDs(&plan)
			case "isComplete":
				got["value"] = IsGoalplanComplete(&plan)
			default:
				t.Fatalf("unknown fn %q", c.Fn)
			}
			want := map[string]any{}
			if c.Expect.Kind != "" {
				want["kind"] = c.Expect.Kind
				want["reason"] = nil
				if c.Expect.Reason != nil {
					want["reason"] = *c.Expect.Reason
				}
				want["plan"] = c.Expect.Plan
			} else if c.Expect.IDs != nil {
				want["ids"] = c.Expect.IDs
			} else if c.Expect.Value != nil {
				want["value"] = *c.Expect.Value
			}
			if !reflect.DeepEqual(lifecycleJSON(t, got), lifecycleJSON(t, want)) {
				gotRaw, _ := json.Marshal(lifecycleJSON(t, got))
				wantRaw, _ := json.Marshal(lifecycleJSON(t, want))
				t.Fatalf("case %s\ngot  %s\nwant %s", c.Name, gotRaw, wantRaw)
			}
			if after := mustJSON(t, &plan); after != before {
				t.Fatalf("case %s mutated its input plan:\n%s\nwant\n%s", c.Name, after, before)
			}
			if after := lifecycleProjectPlan(&plan); !reflect.DeepEqual(lifecycleJSON(t, after), lifecycleJSON(t, c.Input)) {
				t.Fatalf("case %s mutated its input plan:\n%s", c.Name, mustJSON(t, after))
			}
		})
	}
}

// --- the three oracle cases that call these operations (B class) -------------

// goalplan.test.ts:516-537 "030: derived helpers (remaining/nextOpen/unmet/complete)".
func TestGoalplanLifecyclePortedDerivedHelpersCase(t *testing.T) {
	plan := BuildGoalplan(NewGoalplanInput{Objective: "loop", Criteria: []NewGoalplanCriterion{{Scenario: "c"}}})
	plan.WorkPhases = []GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: WorkPhaseDone, Tasks: []GoalplanTask{{ID: "t-1", Title: "a", Status: TaskDone}}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{
			{ID: "t-2", Title: "b", Status: TaskDone},
			{ID: "t-3", Title: "c", Status: TaskPending},
		}, CriteriaIDs: []string{"c-1"}},
	}
	if got := lifecycleRemainingPhaseIDs(plan); !reflect.DeepEqual(got, []string{"wp-2"}) {
		t.Fatalf("remainingWorkPhases = %v", got)
	}
	if next := NextOpenTask(plan); next == nil || next.Task.ID != "t-3" {
		t.Fatalf("nextOpenTask = %#v", next)
	}
	if got := lifecycleUnmetCriterionIDs(plan); !reflect.DeepEqual(got, []string{"c-1"}) {
		t.Fatalf("unmetCriteria = %v", got)
	}
	if IsGoalplanComplete(plan) {
		t.Fatal("isGoalplanComplete = true before the work is done")
	}
	for i := range plan.WorkPhases {
		plan.WorkPhases[i].Status = WorkPhaseDone
		for j := range plan.WorkPhases[i].Tasks {
			plan.WorkPhases[i].Tasks[j].Status = TaskDone
		}
	}
	evidence := "done"
	for i := range plan.Criteria {
		plan.Criteria[i].Status = CriterionMet
		plan.Criteria[i].CapturedEvidence = &evidence
	}
	if next := NextOpenTask(plan); next != nil {
		t.Fatalf("nextOpenTask = %#v after the work is done", next)
	}
	if !IsGoalplanComplete(plan) {
		t.Fatal("isGoalplanComplete = false after the work is done")
	}
}

// work-phase-states.test.ts:456-465 "decide refuses an ambiguous decision id and
// leaves the plan unchanged".
func TestGoalplanLifecyclePortedAmbiguousDecideCase(t *testing.T) {
	askedAt := "2026-09-28T00:00:00.000Z"
	plan := &Goalplan{
		WorkPhases: []GoalplanWorkPhase{{ID: "root", Title: "r", Status: WorkPhasePending, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}, AwaitsDecision: []string{"dec-1"}}},
		Criteria:   []GoalplanCriterion{},
		Decisions: []GoalplanDecision{
			{ID: "dec-1", Question: "First", Status: DecisionOpen, AskedAt: askedAt},
			{ID: "dec-1", Question: "Second", Status: DecisionOpen, AskedAt: askedAt},
		},
	}
	before := mustJSON(t, plan.Decisions)
	result := DecideGoalplanDecision(plan, "dec-1", "yes", "2026-09-28T01:00:00.000Z")
	if result.Kind != GoalplanLifecycleRejected || !strings.Contains(result.Reason, "ambiguous") {
		t.Fatalf("decide = %#v", result)
	}
	if after := mustJSON(t, plan.Decisions); after != before {
		t.Fatalf("ambiguous decide changed the plan:\n%s", after)
	}
}

// goalplan-public-surface.test.ts:877-895 "askGoalplanDecision rejects empty, blank
// and repeated options (library)".
func TestGoalplanLifecyclePortedAskOptionCase(t *testing.T) {
	plan := &Goalplan{WorkPhases: []GoalplanWorkPhase{}, Criteria: []GoalplanCriterion{}}
	base := func() AskGoalplanDecisionInput {
		return AskGoalplanDecisionInput{ID: "dec-1", Question: "Choose API", WorkPhaseIDs: []string{}, AskedAt: "2026-09-30T00:00:00.000Z"}
	}
	reason := func(options []string) string {
		input := base()
		input.Options = options
		result := AskGoalplanDecision(plan, input)
		if result.Kind != GoalplanLifecycleRejected {
			t.Fatalf("options %#v: kind = %v", options, result.Kind)
		}
		return result.Reason
	}
	if got := reason([]string{}); !strings.Contains(got, "must not be empty") {
		t.Fatalf("empty options: %q", got)
	}
	if got := reason([]string{" "}); !strings.Contains(got, "non-empty text") {
		t.Fatalf("blank option: %q", got)
	}
	if got := reason([]string{"A", " A"}); !strings.Contains(got, "duplicate decision option 'A'") {
		t.Fatalf("repeated option: %q", got)
	}
	input := base()
	input.Options = []string{" A ", "B"}
	recommendation := " A"
	input.Recommendation = &recommendation
	result := AskGoalplanDecision(plan, input)
	if result.Kind != GoalplanLifecycleChanged || result.Plan == nil || len(result.Plan.Decisions) != 1 {
		t.Fatalf("accepted options = %#v", result)
	}
	stored := result.Plan.Decisions[0]
	if !reflect.DeepEqual(stored.Options, []string{"A", "B"}) || stored.Recommendation != "A" {
		t.Fatalf("stored = %#v", stored)
	}
}

// --- input-shape complements the JSON corpus cannot express ------------------

func TestGoalplanLifecycleInputShapes(t *testing.T) {
	ts := "2026-01-01T00:00:00.000Z"
	open := func() *Goalplan {
		return &Goalplan{
			WorkPhases: []GoalplanWorkPhase{{ID: "wp1", Title: "Exporter", Status: WorkPhaseInProgress, Tasks: []GoalplanTask{{ID: "t-1", Title: "a", Status: TaskDone, Outcome: "ok"}, {ID: "t-2", Title: "b", Status: TaskPending}}, CriteriaIDs: []string{}}},
			Criteria:   []GoalplanCriterion{{ID: "c-1", Scenario: "s", Status: CriterionOpen}},
		}
	}
	// A nil Options is absent; the corpus carries the present-empty form as an explicit
	// empty list, so the two forms stay distinguishable. The
	// ask is accepted and stores no options field.
	plan := open()
	result := AskGoalplanDecision(plan, AskGoalplanDecisionInput{ID: "dec-1", Question: "Choose", WorkPhaseIDs: []string{"wp1"}, AskedAt: ts})
	if result.Kind != GoalplanLifecycleChanged || result.Plan == nil {
		t.Fatalf("nil options ask = %#v", result)
	}
	if stored := result.Plan.Decisions[0]; stored.Options != nil {
		t.Fatalf("nil options stored %#v", stored.Options)
	}
	if plan.Decisions != nil {
		t.Fatal("ask mutated the input plan")
	}
	// Unchanged keeps the very same plan pointer, as the oracle returns the same object.
	done := open()
	doneResult := CompleteGoalplanTask(done, "wp1", "t-1", "again")
	if doneResult.Kind != GoalplanLifecycleUnchanged || doneResult.Plan != done {
		t.Fatalf("already done = %#v", doneResult)
	}
	met := open()
	evidence := "done"
	met.Criteria[0].Status = CriterionMet
	met.Criteria[0].CapturedEvidence = &evidence
	metResult := MeetGoalplanCriterion(met, "c-1", "again")
	if metResult.Kind != GoalplanLifecycleUnchanged || metResult.Plan != met {
		t.Fatalf("already met = %#v", metResult)
	}
	// A plan with no phases and no criteria is vacuously complete.
	if !IsGoalplanComplete(&Goalplan{}) {
		t.Fatal("empty plan is not complete")
	}
	if ids := lifecycleDoneWithPendingIDs(&Goalplan{}); len(ids) != 0 {
		t.Fatalf("done-with-pending on empty plan = %v", ids)
	}
}

// A changed result must not share a backing array with its input. The corpus compares
// projected values and cannot see aliasing, so this probe writes through every slice of
// the result and re-reads the input.
func TestGoalplanLifecycleResultDoesNotAliasInput(t *testing.T) {
	ts := "2026-01-01T00:00:00.000Z"
	decisions := make([]GoalplanDecision, 0, 4)
	decisions = append(decisions, GoalplanDecision{ID: "dec-1", Question: "Choose", Status: DecisionOpen, AskedAt: ts})
	phases := make([]GoalplanWorkPhase, 0, 4)
	phases = append(phases, GoalplanWorkPhase{ID: "wp1", Title: "Exporter", Status: WorkPhasePending, Tasks: make([]GoalplanTask, 0, 4), CriteriaIDs: []string{}})
	plan := &Goalplan{WorkPhases: phases, Criteria: []GoalplanCriterion{}, Decisions: decisions}
	result := AskGoalplanDecision(plan, AskGoalplanDecisionInput{ID: "dec-2", Question: "Second", WorkPhaseIDs: []string{"wp1"}, AskedAt: ts})
	if result.Kind != GoalplanLifecycleChanged || result.Plan == nil {
		t.Fatalf("ask = %#v", result)
	}
	result.Plan.Decisions[0].Question = "overwritten"
	result.Plan.Decisions = append(result.Plan.Decisions, GoalplanDecision{ID: "dec-3", Question: "Third", AskedAt: ts})
	result.Plan.WorkPhases[0].AwaitsDecision = append(result.Plan.WorkPhases[0].AwaitsDecision, "dec-9")
	result.Plan.WorkPhases = append(result.Plan.WorkPhases, GoalplanWorkPhase{ID: "wp2", Title: "Extra", CriteriaIDs: []string{}})
	if plan.Decisions[0].Question != "Choose" {
		t.Fatalf("result aliases the input decisions: %q", plan.Decisions[0].Question)
	}
	if len(plan.Decisions) != 1 || len(plan.WorkPhases) != 1 || len(plan.WorkPhases[0].AwaitsDecision) != 0 {
		t.Fatalf("result shares backing arrays with the input: %+v", plan)
	}
}
