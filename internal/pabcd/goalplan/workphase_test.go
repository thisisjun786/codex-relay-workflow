package goalplan

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
)

// The oracle's answers for the work-phase close, resume-absent-target and advance cases
// were recorded once with testdata/workphase/record-oracle.mjs (CXC v0.2.40 3c1459ac under
// Node 24); no Node runs here. A case holds the plan the oracle's own test handed it, the
// work-phase id it closed, the recordedNext it passed, and the compact answer it gave.
//
// The corpus is the oracle's own test files - work-phase-states.test.ts,
// goalplan-regression.test.ts:184-191 and goalplan-concurrency.test.ts:224-341 - plus the
// branches those tests do not reach (the null retry, both corrupt markers, the settled
// done-successor normalisation and every refusal variant), recorded from the same oracle
// build rather than asserted from this port's reading of the source.

type workPhaseRecordedNext struct {
	Present bool    `json:"present"`
	Value   *string `json:"value"`
}

type workPhaseRecordedCase struct {
	Test         string                `json:"test"`
	Target       *string               `json:"target"`
	Plan         Goalplan              `json:"plan"`
	RecordedNext workPhaseRecordedNext `json:"recordedNext"`
	Expected     map[string]any        `json:"expected"`
}

type workPhaseRecordedCorpus struct {
	Oracle       string                  `json:"oracle"`
	CloseFixed   []workPhaseRecordedCase `json:"closeFixed"`
	ResumeAbsent []workPhaseRecordedCase `json:"resumeAbsent"`
	Advance      []workPhaseRecordedCase `json:"advance"`
}

func loadWorkPhaseCorpus(t *testing.T) workPhaseRecordedCorpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/workphase/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus workPhaseRecordedCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.Oracle != "CXC v0.2.40 3c1459ac" {
		t.Fatalf("recorded corpus names oracle %q", corpus.Oracle)
	}
	// A shrink guard, not a pinned count: a corpus that lost cases is not a regression
	// target, it is a broken recording.
	if len(corpus.CloseFixed) < 35 || len(corpus.ResumeAbsent) < 11 || len(corpus.Advance) < 13 {
		t.Fatalf("recorded corpus shrank: close=%d resume=%d advance=%d",
			len(corpus.CloseFixed), len(corpus.ResumeAbsent), len(corpus.Advance))
	}
	return corpus
}

// workPhaseRecordedNextValue is the oracle's third argument: undefined, null, or an id.
func workPhaseRecordedNextValue(r workPhaseRecordedNext) WorkPhaseRecordedNext {
	if !r.Present {
		return WorkPhaseRecordedNext{}
	}
	return WorkPhaseRecordedNext{Known: true, ID: r.Value}
}

// workPhaseProjectPlan is the plan projection both sides compare: the fields a close, a
// resume or an advance writes, and nothing a timestamp or a title could make unstable.
func workPhaseProjectPlan(plan *Goalplan) map[string]any {
	phases := make([]any, 0, len(plan.WorkPhases))
	for i := range plan.WorkPhases {
		wp := &plan.WorkPhases[i]
		deps := make([]any, 0, len(wp.DependsOn))
		for _, id := range wp.DependsOn {
			deps = append(deps, id)
		}
		tasks := make([]any, 0, len(wp.Tasks))
		for j := range wp.Tasks {
			tasks = append(tasks, map[string]any{"id": wp.Tasks[j].ID, "status": string(wp.Tasks[j].Status)})
		}
		phases = append(phases, map[string]any{
			"id": wp.ID, "status": string(wp.Status), "dependsOn": deps, "tasks": tasks,
		})
	}
	var cursor any
	if plan.ActiveWorkPhaseID != nil {
		cursor = *plan.ActiveWorkPhaseID
	}
	return map[string]any{"activeWorkPhaseId": cursor, "workPhases": phases}
}

func workPhaseTaskIDs(tasks []GoalplanTask) []any {
	out := make([]any, 0, len(tasks))
	for i := range tasks {
		out = append(out, tasks[i].ID)
	}
	return out
}

func workPhaseStringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

func workPhaseProjectCloseFixed(result WorkPhaseCloseFixedResult) map[string]any {
	out := map[string]any{"kind": string(result.Kind)}
	switch result.Kind {
	case WorkPhaseCloseFixedOK:
		var closed any
		if result.ClosedID != nil {
			closed = *result.ClosedID
		}
		out["closedId"] = closed
		out["plan"] = workPhaseProjectPlan(result.Plan)
	case WorkPhaseCloseFixedTasksPending:
		out["workPhaseId"] = *result.WorkPhaseID
		out["pending"] = workPhaseTaskIDs(result.Pending)
	case WorkPhaseCloseFixedNotRunnable:
		out["status"] = string(result.Status)
	case WorkPhaseCloseFixedDependenciesUnmet:
		out["unmet"] = workPhaseStringList(result.Unmet)
	case WorkPhaseCloseFixedSuccessorLost:
		out["successorId"] = *result.SuccessorID
		out["reason"] = result.Reason
	}
	return out
}

func workPhaseProjectResumeAbsent(result WorkPhaseResumeAbsentTargetResult) map[string]any {
	out := map[string]any{"kind": string(result.Kind)}
	switch result.Kind {
	case WorkPhaseResumeActivate:
		out["plan"] = workPhaseProjectPlan(result.Plan)
	case WorkPhaseResumeSuccessorLost:
		out["successorId"] = *result.SuccessorID
		out["reason"] = result.Reason
	}
	return out
}

func workPhaseProjectAdvance(result WorkPhaseAdvanceResult) map[string]any {
	out := map[string]any{"kind": string(result.Kind)}
	switch result.Kind {
	case WorkPhaseAdvanceOK:
		var closed any
		if result.ClosedID != nil {
			closed = *result.ClosedID
		}
		out["closedId"] = closed
		out["plan"] = workPhaseProjectPlan(result.Plan)
	case WorkPhaseAdvanceTasksPending:
		out["workPhaseId"] = *result.WorkPhaseID
		out["pending"] = workPhaseTaskIDs(result.Pending)
	}
	return out
}

func TestWorkPhaseOracleParity(t *testing.T) {
	corpus := loadWorkPhaseCorpus(t)
	families := []struct {
		name   string
		cases  []workPhaseRecordedCase
		replay func(*Goalplan, workPhaseRecordedCase) map[string]any
	}{
		{"closeFixed", corpus.CloseFixed, func(plan *Goalplan, c workPhaseRecordedCase) map[string]any {
			return workPhaseProjectCloseFixed(workPhaseCloseFixed(plan, *c.Target, workPhaseRecordedNextValue(c.RecordedNext)))
		}},
		{"resumeAbsent", corpus.ResumeAbsent, func(plan *Goalplan, c workPhaseRecordedCase) map[string]any {
			var next string
			if c.RecordedNext.Value != nil {
				next = *c.RecordedNext.Value
			}
			return workPhaseProjectResumeAbsent(workPhaseResumeAbsentTarget(plan, next))
		}},
		{"advance", corpus.Advance, func(plan *Goalplan, c workPhaseRecordedCase) map[string]any {
			return workPhaseProjectAdvance(workPhaseAdvance(plan))
		}},
	}
	for _, family := range families {
		for i, c := range family.cases {
			t.Run(fmt.Sprintf("%s/%02d_%s", family.name, i, c.Test), func(t *testing.T) {
				// The oracle builds new arrays and phase objects and shares the task
				// arrays; a port that mutated in place would corrupt its caller's plan.
				before, err := json.Marshal(&c.Plan)
				if err != nil {
					t.Fatal(err)
				}
				got := compact(t, family.replay(&c.Plan, c))
				after, err := json.Marshal(&c.Plan)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatalf("the transform mutated its plan\nbefore %s\nafter  %s", before, after)
				}
				if want := compact(t, c.Expected); got != want {
					t.Fatalf("answered\n%s\nwant\n%s", got, want)
				}
			})
		}
	}
}

// TestWorkPhaseResultJSONKeys pins the wire shape of the three result unions, because the
// callers that publish them are later issues: a kind carries exactly the keys the oracle's
// variant carries and no others.
func TestWorkPhaseResultJSONKeys(t *testing.T) {
	plan := BuildGoalplan(NewGoalplanInput{Objective: "shape"})
	closed := "wp-1"
	pending := []GoalplanTask{{ID: "t1", Title: "t", Status: TaskPending}}
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"close ok", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedOK, ClosedID: &closed, Plan: plan}, []string{"closedId", "kind", "plan"}},
		{"close already_done", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedAlreadyDone}, []string{"kind"}},
		{"close absent", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedAbsent}, []string{"kind"}},
		{"close not_runnable", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedNotRunnable, Status: WorkPhaseBlocked}, []string{"kind", "status"}},
		{"close dependencies_unmet", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedDependenciesUnmet, Unmet: []string{"wp-9"}}, []string{"kind", "unmet"}},
		{"close tasks_pending", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedTasksPending, WorkPhaseID: workPhaseText("wp-1"), Pending: pending}, []string{"kind", "pending", "workPhaseId"}},
		{"close successor_lost", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedSuccessorLost, SuccessorID: workPhaseText("wp-2"), Reason: "absent"}, []string{"kind", "reason", "successorId"}},
		{"resume activate", WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeActivate, Plan: plan}, []string{"kind", "plan"}},
		{"resume cleanup", WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeCleanup}, []string{"kind"}},
		{"resume successor_lost", WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeSuccessorLost, SuccessorID: workPhaseText("wp-2"), Reason: "absent"}, []string{"kind", "reason", "successorId"}},
		{"advance ok", WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceOK, ClosedID: &closed, Plan: plan}, []string{"closedId", "kind", "plan"}},
		{"advance tasks_pending", WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceTasksPending, WorkPhaseID: workPhaseText("wp-1"), Pending: pending}, []string{"kind", "pending", "workPhaseId"}},
		{"advance no_active", WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceNoActive}, []string{"kind"}},
		// The oracle's variants always carry these keys, and the empty string is a value
		// they can hold: a plan can name a work phase with an empty id, and the corrupt
		// marker a fixed close refuses is exactly the empty successor. omitempty on a
		// plain string dropped both keys, so the fields are pointers and these cases pin
		// the difference.
		{"close successor_lost empty id", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedSuccessorLost, SuccessorID: workPhaseText(""), Reason: "corrupt"}, []string{"kind", "reason", "successorId"}},
		{"close tasks_pending empty phase id", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedTasksPending, WorkPhaseID: workPhaseText(""), Pending: pending}, []string{"kind", "pending", "workPhaseId"}},
		{"close ok empty closed id", WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedOK, ClosedID: workPhaseText(""), Plan: plan}, []string{"closedId", "kind", "plan"}},
		{"resume successor_lost empty id", WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeSuccessorLost, SuccessorID: workPhaseText(""), Reason: "corrupt"}, []string{"kind", "reason", "successorId"}},
		{"advance tasks_pending empty phase id", WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceTasksPending, WorkPhaseID: workPhaseText(""), Pending: pending}, []string{"kind", "pending", "workPhaseId"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.value)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			got := slices.Sorted(maps.Keys(decoded))
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s keys = %v; want %v", c.name, got, c.want)
			}
		})
	}
}
