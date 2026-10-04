package goalplan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func queryTestJSON(t *testing.T, value any) any {
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

// Query-bearing assertions of goalplan.test.ts and work-phase-states.test.ts,
// plus the regression selector and direct edge probes, recorded from Node 24.
// Original lifecycle/validator assertions stay with their owning ports.
func TestQueryOracleParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/query/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		SelectedOriginalCalls int
		SelectedOriginalTests []string
		Cases                 []struct {
			Test, Fn string
			Plan     Goalplan
			Phase    GoalplanWorkPhase
			Expected any
		}
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.SelectedOriginalCalls != 44 || len(corpus.SelectedOriginalTests) != 26 || len(corpus.Cases) != 687 {
		t.Fatalf("recorded corpus shrank: %+v", corpus.SelectedOriginalTests)
	}
	for i, c := range corpus.Cases {
		t.Run(fmt.Sprintf("%03d_%s_%s", i, c.Fn, c.Test), func(t *testing.T) {
			before := queryTestJSON(t, &c.Plan)
			var got any
			switch c.Fn {
			case "remainingWorkPhases":
				got = RemainingWorkPhases(&c.Plan)
			case "readyWorkPhases":
				got = ReadyWorkPhases(&c.Plan)
			case "readyTasks":
				got = ReadyTasks(&c.Plan)
			case "nextOpenTask":
				got = NextOpenTask(&c.Plan)
			case "dependencyWaitReasons":
				got = DependencyWaitReasons(&c.Plan)
			case "dependencyDeadlock":
				got = DetectDependencyDeadlock(&c.Plan)
			case "remainingWorkAwaitsDecisions":
				got = RemainingWorkAwaitsDecisions(&c.Plan)
			case "openDecisionIdsForPhase":
				got = OpenDecisionIDsForPhase(&c.Plan, &c.Phase)
			default:
				t.Fatal(c.Fn)
			}
			if actual := queryTestJSON(t, got); !reflect.DeepEqual(actual, c.Expected) {
				t.Errorf("%s = %#v; want %#v", c.Fn, actual, c.Expected)
			}
			if !reflect.DeepEqual(before, queryTestJSON(t, &c.Plan)) {
				t.Fatal("query mutated its plan")
			}
		})
	}
}

// goalplan-regression.test.ts:66-104: fixed pre-change parser result set.
func TestQueryRegressionParserBaseline(t *testing.T) {
	type result struct {
		Kind  string `json:"kind"`
		Field string `json:"field,omitempty"`
	}
	type entry struct {
		Ordinal     int    `json:"ordinal"`
		Alias       string `json:"alias"`
		SourceClass string `json:"sourceClass"`
		Expected    result `json:"expected"`
	}
	var snapshot struct {
		SourceCount int
		Manifest    []entry
		Fixtures    []struct {
			entry
			Plan json.RawMessage
		}
	}
	raw, err := os.ReadFile("testdata/query/goalplans-pre-change-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceCount != 91 || len(snapshot.Manifest) != 91 || len(snapshot.Fixtures) != 91 {
		t.Fatal("fixed baseline count changed")
	}
	cwd := t.TempDir()
	actual := []entry{}
	normal, legacy := 0, 0
	for _, f := range snapshot.Fixtures {
		dir, err := GoalplanDir(cwd, f.Alias)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, GoalplanFile), f.Plan, 0o600); err != nil {
			t.Fatal(err)
		}
		read := ReadGoalplanDetailed(cwd, f.Alias)
		got := result{Kind: "parsed"}
		if read.Plan == nil || read.Diagnostic != nil {
			if read.Diagnostic == nil {
				t.Fatal("missing diagnostic")
			}
			got.Kind = read.Diagnostic.Kind
			if got.Kind == "invalid-shape" {
				got.Field = "other-shape"
				if read.Diagnostic.Field == "criteria[] entries (each needs scenario/expectedEvidence/status)" {
					got.Field = "criteria-shape"
				}
			}
		}
		actual = append(actual, entry{f.Ordinal, f.Alias, f.SourceClass, got})
		if f.SourceClass == "normal" && got.Kind == "parsed" {
			normal++
		}
		if f.SourceClass == "legacy-text-criterion" {
			legacy++
			if got != (result{Kind: "invalid-shape", Field: "criteria-shape"}) {
				t.Errorf("legacy result: %+v", got)
			}
		}
	}
	if !reflect.DeepEqual(actual, snapshot.Manifest) || normal != 90 || legacy != 1 {
		t.Fatalf("parser set differs: normal=%d legacy=%d", normal, legacy)
	}
}

func queryTestPlan(phases ...GoalplanWorkPhase) *Goalplan {
	p := BuildGoalplan(NewGoalplanInput{Objective: "query fixture", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	p.WorkPhases = phases
	return p
}
func queryTestPhase(id string, tasks ...GoalplanTask) GoalplanWorkPhase {
	return GoalplanWorkPhase{ID: id, Title: id, Status: WorkPhasePending, Tasks: append([]GoalplanTask{}, tasks...), CriteriaIDs: []string{}}
}
func queryTestTask(id string, dependencies ...string) GoalplanTask {
	return GoalplanTask{ID: id, Title: id, Status: TaskPending, DependsOn: dependencies}
}

func TestQueryPredicatesAndIdentity(t *testing.T) {
	a, b := queryTestPhase("a", queryTestTask("t")), queryTestPhase("b", queryTestTask("leaf", "t"), queryTestTask("t"))
	b.DependsOn = []string{"a"}
	p := queryTestPlan(a, b)
	if WorkPhaseDependenciesMet(p, &p.WorkPhases[1]) || WorkPhaseReadyConditionsMet(p, &p.WorkPhases[1]) || IsRunnablePhase(p, &p.WorkPhases[1]) {
		t.Fatal("unfinished phase dependency released")
	}
	if TaskDependenciesMet(&p.WorkPhases[1], &p.WorkPhases[1].Tasks[0]) {
		t.Fatal("cross-phase task released")
	}
	p.WorkPhases[0].Status = WorkPhaseDone
	if !WorkPhaseDependenciesMet(p, &p.WorkPhases[1]) || !WorkPhaseReadyConditionsMet(p, &p.WorkPhases[1]) || !IsRunnablePhase(p, &p.WorkPhases[1]) {
		t.Fatal("done dependency not released")
	}
	next := NextOpenTask(p)
	if next == nil || next.WP != &p.WorkPhases[1] || next.Task != &p.WorkPhases[1].Tasks[1] {
		t.Fatalf("reference selection: %+v", next)
	}
	ready := ReadyTasks(p)
	if len(ready) != 1 || ready[0].Task != next.Task || ready[0].WorkPhaseID != "b" {
		t.Fatalf("ready reference: %+v", ready)
	}
	p.WorkPhases[1].Tasks[1].Status = TaskDone
	if !TaskDependenciesMet(&p.WorkPhases[1], &p.WorkPhases[1].Tasks[0]) {
		t.Fatal("done local dependency not released")
	}
	if phases := RemainingWorkPhases(p); len(phases) != 1 || phases[0] != &p.WorkPhases[1] {
		t.Fatal("remaining lost identity")
	}
	if phases := ReadyWorkPhases(p); len(phases) != 1 || phases[0] != &p.WorkPhases[1] {
		t.Fatal("ready phases lost identity")
	}
	p.WorkPhases[1].DependsOn = []string{"missing"}
	if WorkPhaseDependenciesMet(p, &p.WorkPhases[1]) {
		t.Fatal("missing phase released")
	}
}

func TestQueryDuplicateNextPairKept(t *testing.T) {
	first, second := queryTestPhase("dup"), queryTestPhase("dup", queryTestTask("later"))
	first.Status = WorkPhaseDone
	p := queryTestPlan(first, second)
	next := NextOpenTask(p)
	if next == nil || next.WP != &p.WorkPhases[0] || next.Task != &p.WorkPhases[1].Tasks[0] {
		t.Fatalf("oracle first-ID pair changed: %+v", next)
	}
}

// Regression test 3 has no nextOpenTask call. These selector observations on its
// original outcome shapes pin selection independently of schema-version outcome validation.
func TestQueryRegressionOutcomeSelection(t *testing.T) {
	for _, version := range []float64{1, 2, 3} {
		for _, task := range []GoalplanTask{
			{ID: "t1", Title: "done legacy task", Status: TaskDone},
			{ID: "t1", Title: "pending v3 task", Status: TaskPending, Outcome: "must not exist yet"},
			{ID: "t1", Title: "done v3 task", Status: TaskDone, Outcome: "tests: 12 passed"},
		} {
			p := queryTestPlan(queryTestPhase("wp1", task))
			p.SchemaVersion = &version
			p.WorkPhases[0].Status = WorkPhaseDone
			if task.Status == TaskPending {
				p.WorkPhases[0].Status = WorkPhaseInProgress
			}
			next := NextOpenTask(p)
			if (next != nil) != (task.Status == TaskPending) {
				t.Fatalf("schema %g task %+v => %+v", version, task, next)
			}
		}
	}
}

func TestQueryDecisionReleaseAndMalformedStructure(t *testing.T) {
	p := queryTestPlan(queryTestPhase("root"))
	p.WorkPhases[0].AwaitsDecision = []string{"d"}
	p.Decisions = []GoalplanDecision{{ID: "d", Question: "Choose", Status: DecisionOpen, AskedAt: "2026-01-01T00:00:00.000Z"}}
	if !RemainingWorkAwaitsDecisions(p) {
		t.Fatal("valid decision wait did not release")
	}
	for _, breakPlan := range []func(*Goalplan){
		func(p *Goalplan) { v := float64(4); p.SchemaVersion = &v },
		func(p *Goalplan) { p.Criteria = []GoalplanCriterion{{ID: "c", Status: CriterionMet}} },
		func(p *Goalplan) {
			x := "\uFEFF"
			p.Criteria = []GoalplanCriterion{{ID: "c", Status: CriterionMet, CapturedEvidence: &x}}
		},
		func(p *Goalplan) {
			x := queryTestPhase("done", queryTestTask("open"))
			x.Status = WorkPhaseDone
			p.WorkPhases = append(p.WorkPhases, x)
		},
		func(p *Goalplan) { p.WorkPhases[0].DependsOn = []string{"missing"} },
		func(p *Goalplan) { p.WorkPhases[0].Status = WorkPhaseBlocked },
	} {
		raw, _ := json.Marshal(p)
		var broken Goalplan
		if err := json.Unmarshal(raw, &broken); err != nil {
			t.Fatal(err)
		}
		breakPlan(&broken)
		if RemainingWorkAwaitsDecisions(&broken) {
			t.Fatalf("broken structure released: %+v", broken)
		}
	}
	p.WorkPhases[0].AwaitsDecision = []string{"d", "d", "ghost"}
	if got := OpenDecisionIDsForPhase(p, &p.WorkPhases[0]); !slices.Equal(got, []string{"d", "ghost"}) {
		t.Fatalf("decision order: %v", got)
	}
}
