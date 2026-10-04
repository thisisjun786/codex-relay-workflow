package goalplan

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

func integrityPlan(phases ...GoalplanWorkPhase) *Goalplan {
	v := float64(3)
	return &Goalplan{Objective: "integrity fixture", Slug: "integrity", WorkPhases: phases,
		Criteria: []GoalplanCriterion{}, SchemaVersion: &v, Host: GoalplanHostLink{Source: HostSourceNone}}
}

func integrityPhase(id string, dependencies ...string) GoalplanWorkPhase {
	return GoalplanWorkPhase{ID: id, Title: id, Status: WorkPhasePending, Tasks: []GoalplanTask{},
		CriteriaIDs: []string{}, DependsOn: dependencies}
}

func integrityTask(id string, dependencies ...string) GoalplanTask {
	return GoalplanTask{ID: id, Title: id, Status: TaskPending, DependsOn: dependencies}
}

func integrityReasonsEqual(t *testing.T, got []string, want ...string) {
	t.Helper()
	if got == nil || len(got) != len(want) || !reflect.DeepEqual(append([]string{}, got...), append([]string{}, want...)) {
		t.Errorf("reasons = %#v, want %#v (non-nil)", got, want)
	}
}

// Eleven B-class scenarios from goalplan-integrity.test.ts:20-173,192-226.
// The validateGoalplan ordering scenario belongs to the full-validator slice.
func TestIntegrityBPhaseDanglingAndSelf(t *testing.T) {
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(integrityPhase("wp-b", "ghost"))),
		"work phase wp-b depends on unknown work phase 'ghost'")
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(integrityPhase("wp-a", "wp-a"))), "work phase wp-a depends on itself")
}

func TestIntegrityBPhaseCycle(t *testing.T) {
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(integrityPhase("b", "a"), integrityPhase("a", "b"))),
		"work phase dependency cycle: a -> b -> a")
}

func TestIntegrityBTaskScope(t *testing.T) {
	a, b := integrityPhase("wp-a"), integrityPhase("wp-b")
	a.Tasks = []GoalplanTask{integrityTask("shared"), integrityTask("only-in-a")}
	b.Tasks = []GoalplanTask{integrityTask("shared"), integrityTask("leaf", "shared", "only-in-a")}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(a, b)),
		"task wp-b/leaf depends on unknown task 'only-in-a' in the same work phase")
}

func TestIntegrityBTaskSelfAndCycle(t *testing.T) {
	p := integrityPhase("wp-a")
	p.Tasks = []GoalplanTask{integrityTask("a", "a")}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(p)), "task wp-a/a depends on itself")
	p.Tasks = []GoalplanTask{integrityTask("b", "a"), integrityTask("a", "b")}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(p)), "task dependency cycle in work phase wp-a: a -> b -> a")
}

func TestIntegrityBDuplicateAuthorityScopes(t *testing.T) {
	a, b := integrityPhase("wp-a"), integrityPhase("wp-a")
	a.Tasks = []GoalplanTask{integrityTask("dup-task"), integrityTask("dup-task")}
	a.CriteriaIDs = []string{"missing-criterion"}
	b.Tasks = []GoalplanTask{integrityTask("dup-task")}
	p := integrityPlan(a, b)
	p.Criteria = []GoalplanCriterion{{ID: "c-dup"}, {ID: "c-dup"}}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(p),
		"duplicate work phase id 'wp-a' makes dependency references ambiguous",
		"work phase wp-a has duplicate task id 'dup-task', so task dependency references are ambiguous",
		"duplicate criterion id 'c-dup' makes criteriaIds references ambiguous",
		"work phase wp-a references unknown criterion 'missing-criterion'")
}

func TestIntegrityBDonePhaseDependencies(t *testing.T) {
	for _, status := range []WorkPhaseStatus{WorkPhasePending, WorkPhaseInProgress, WorkPhaseBlocked, WorkPhaseSuperseded, WorkPhaseDone} {
		t.Run(string(status), func(t *testing.T) {
			base, leaf := integrityPhase("base"), integrityPhase("leaf", "base")
			base.Status, leaf.Status = status, WorkPhaseDone
			got := GoalplanDependencyCompletionReasons(integrityPlan(base, leaf))
			if status == WorkPhaseDone {
				integrityReasonsEqual(t, got)
			} else {
				integrityReasonsEqual(t, got, "work phase leaf is done while dependency work phase(s) are not done: base")
			}
		})
	}
}

func TestIntegrityBDoneTaskDependencies(t *testing.T) {
	p := integrityPhase("wp-a")
	base, leaf := integrityTask("base"), integrityTask("leaf", "base")
	leaf.Status, leaf.Outcome = TaskDone, "leaf finished"
	p.Tasks = []GoalplanTask{base, leaf}
	plan := integrityPlan(p)
	integrityReasonsEqual(t, GoalplanDependencyCompletionReasons(plan), "task wp-a/leaf is done while dependency task(s) are not done: base")
	plan.WorkPhases[0].Tasks[0].Status, plan.WorkPhases[0].Tasks[0].Outcome = TaskDone, "base finished"
	integrityReasonsEqual(t, GoalplanDependencyCompletionReasons(plan))
}

func TestIntegrityBSchemaThreeOutcomes(t *testing.T) {
	p := integrityPhase("wp-a")
	p.Tasks = []GoalplanTask{{ID: "done-missing", Status: TaskDone}, {ID: "done-blank", Status: TaskDone, Outcome: "   "},
		{ID: "pending-present", Status: TaskPending, Outcome: "premature"}, {ID: "done-valid", Status: TaskDone, Outcome: "8 pass, 0 fail"}}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(p)), "task wp-a/done-missing is done but has no non-empty outcome",
		"task wp-a/done-blank is done but has no non-empty outcome", "task wp-a/pending-present is pending but has outcome")
}

func TestIntegrityBLegacyOutcomes(t *testing.T) {
	for _, version := range []float64{1, 2} {
		p := integrityPlan(integrityPhase("wp-a"))
		p.SchemaVersion = &version
		p.WorkPhases[0].Status, p.WorkPhases[0].Tasks = WorkPhaseDone, []GoalplanTask{{ID: "legacy-done", Status: TaskDone}}
		integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(p))
	}
}

func TestIntegrityBJoiningDAG(t *testing.T) {
	p := integrityPlan(integrityPhase("a"), integrityPhase("b", "a"), integrityPhase("c", "a"), integrityPhase("d", "b", "c"), integrityPhase("solo"))
	p.WorkPhases[4].Tasks = []GoalplanTask{integrityTask("t-a"), integrityTask("t-b", "t-a"), integrityTask("t-c", "t-a"), integrityTask("t-d", "t-b", "t-c")}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(p))
}

func TestIntegrityBRepeatedDependencies(t *testing.T) {
	p := integrityPhase("wp-1", "ghost", "ghost", "ghost", "ghost")
	p.Tasks, p.CriteriaIDs = []GoalplanTask{{ID: "t-1", Status: TaskPending, Outcome: "premature"}}, []string{"c-missing"}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(integrityPlan(p)), "work phase wp-1 depends on unknown work phase 'ghost'",
		"task wp-1/t-1 is pending but has outcome", "work phase wp-1 references unknown criterion 'c-missing'")
}

func TestIntegrityOracle(t *testing.T) {
	var corpus struct {
		Cases []struct {
			ID                                 string
			Plan                               Goalplan
			Marker                             bool
			Definition, Completion, Superseded []string
			Version                            float64
			QA                                 bool
		}
		Marker string
	}
	raw, err := os.ReadFile("testdata/integrity/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) != 25 {
		t.Fatalf("oracle cases = %d, want 25", len(corpus.Cases))
	}
	for _, c := range corpus.Cases {
		t.Run(c.ID, func(t *testing.T) {
			before := compact(t, &c.Plan)
			integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(&c.Plan), c.Definition...)
			integrityReasonsEqual(t, GoalplanDependencyCompletionReasons(&c.Plan), c.Completion...)
			integrityReasonsEqual(t, supersededIntegrityReasons(&c.Plan), c.Superseded...)
			if got := EffectiveSchemaVersion(&c.Plan, c.Marker); got != c.Version {
				t.Errorf("version = %v, want %v", got, c.Version)
			}
			if got := ComputeQaRequired(&c.Plan); got != c.QA {
				t.Errorf("QA = %v, want %v", got, c.QA)
			}
			if after := compact(t, &c.Plan); after != before {
				t.Errorf("plan mutated:\n%s\n%s", before, after)
			}
		})
	}
	got, err := SchemaMarkerPath("/synthetic", "integrity")
	if want := strings.ReplaceAll(corpus.Marker, ".codexclaw", crwdir.DirName); err != nil || got != want {
		t.Errorf("marker = %q, %v; want %q", got, err, want)
	}
}

func TestIntegrityDependencyHelpers(t *testing.T) {
	integrityReasonsEqual(t, duplicateIDs([]string{"z", "a", "z", "a", "z"}), "a", "z")
	integrityReasonsEqual(t, duplicateIDs([]string{"\uE000", "\U00010000", "\uE000", "\U00010000"}), "\U00010000", "\uE000")
	integrityReasonsEqual(t, duplicateIDs(nil))
	for _, nodes := range [][]dependencyNode{nil, {{id: "a", dependsOn: []string{"ghost"}}},
		{{id: "a", dependsOn: []string{"b"}}, {id: "b", dependsOn: []string{"a"}}, {id: "a"}}} {
		if cycle := findDependencyCycle(nodes); cycle != nil {
			t.Errorf("non-cycle = %#v", cycle)
		}
	}
	integrityReasonsEqual(t, findDependencyCycle([]dependencyNode{{id: "a", dependsOn: []string{"a"}}}), "a", "a")
}

func TestIntegritySchemaAndQa(t *testing.T) {
	p := integrityPlan()
	p.SchemaVersion = nil
	if EffectiveSchemaVersion(p, false) != 1 || EffectiveSchemaVersion(p, true) != 2 {
		t.Error("absent schema defaults/promotion differ")
	}
	// These answers were observed independently in the Node oracle, including -0.
	for _, c := range []struct{ declared, plain, promoted float64 }{
		{math.NaN(), math.NaN(), math.NaN()}, {math.Inf(1), math.Inf(1), math.Inf(1)},
		{math.Inf(-1), math.Inf(-1), 2}, {math.Copysign(0, -1), math.Copysign(0, -1), 2},
		{0, 0, 2}, {1, 1, 2}, {2, 2, 2}, {2.5, 2.5, 2.5}, {3, 3, 3},
	} {
		p.SchemaVersion = &c.declared
		for i, want := range []float64{c.plain, c.promoted} {
			marker := i == 1
			got := EffectiveSchemaVersion(p, marker)
			if math.IsNaN(want) {
				if !math.IsNaN(got) {
					t.Errorf("NaN promoted to %v", got)
				}
			} else if got != want || math.Signbit(got) != math.Signbit(want) {
				t.Errorf("schema %v marker %v = %v, want %v", c.declared, marker, got, want)
			}
		}
	}
	for _, c := range []struct {
		surface CriterionSurface
		want    bool
	}{
		{"", false}, {SurfaceLogic, false}, {SurfaceWeb, true}, {SurfaceTUI, true}, {SurfaceDesktop, true}, {"unknown", false},
	} {
		p.Criteria = []GoalplanCriterion{{Surface: SurfaceLogic, Status: CriterionMet}, {Surface: c.surface, Status: CriterionMet}}
		if ComputeQaRequired(p) != c.want {
			t.Errorf("QA surface %q", c.surface)
		}
	}
}

func TestIntegrityCompletionUsesPhaseLocalOrder(t *testing.T) {
	other, phase := integrityPhase("other"), integrityPhase("p", "ghost", "other", "ghost")
	other.Tasks = []GoalplanTask{{ID: "a", Status: TaskDone}, {ID: "z", Status: TaskDone}}
	phase.Status = WorkPhaseDone
	phase.Tasks = []GoalplanTask{integrityTask("z"), integrityTask("a"), {ID: "leaf", Status: TaskDone, DependsOn: []string{"z", "a", "z", "missing"}}}
	integrityReasonsEqual(t, GoalplanDependencyCompletionReasons(integrityPlan(other, phase)),
		"work phase p is done while dependency work phase(s) are not done: ghost, other",
		"task p/leaf is done while dependency task(s) are not done: z, a, missing")
}

func TestIntegritySupersededTargets(t *testing.T) {
	for _, status := range []WorkPhaseStatus{WorkPhasePending, WorkPhaseInProgress, WorkPhaseBlocked, WorkPhaseDone} {
		old, target := integrityPhase("old"), integrityPhase("target")
		old.Status, old.SupersededBy, target.Status = WorkPhaseSuperseded, ptr("target"), status
		integrityReasonsEqual(t, supersededIntegrityReasons(integrityPlan(old, target)))
	}
	for _, by := range []*string{nil, ptr(""), ptr(" ")} {
		old := integrityPhase("old")
		old.Status, old.SupersededBy = WorkPhaseSuperseded, by
		integrityReasonsEqual(t, supersededIntegrityReasons(integrityPlan(old)), "work phase old is superseded but does not name what replaced it (supersededBy)")
	}
}

func TestIntegrityMarkerDelegatesAndCreatesNothing(t *testing.T) {
	root := t.TempDir()
	got, err := SchemaMarkerPath(root, "safe")
	want := filepath.Join(root, crwdir.DirName, GoalplansSubdir, "safe", "schema-v2.marker")
	if err != nil || got != want {
		t.Errorf("marker = %q, %v; want %q", got, err, want)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("marker created state: %v, %v", entries, err)
	}
	for _, slug := range []string{"", ".", "../escaped", "nested/name"} {
		_, wantErr := GoalplanDir(root, slug)
		_, err := SchemaMarkerPath(root, slug)
		if err == nil || wantErr == nil || err.Error() != wantErr.Error() {
			t.Errorf("slug %q = %v, want %v", slug, err, wantErr)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, crwdir.DirName)); err != nil {
		t.Fatal(err)
	}
	_, wantErr := GoalplanDir(root, "safe")
	_, err = SchemaMarkerPath(root, "safe")
	if err == nil || wantErr == nil || err.Error() != wantErr.Error() {
		t.Errorf("linked state = %v, want %v", err, wantErr)
	}
}

func TestIntegrityReadAndRejectKeepsBytes(t *testing.T) {
	root := t.TempDir()
	dir, err := GoalplanDir(root, "integrity")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := integrityPlan(integrityPhase("p", "ghost", "ghost"))
	files := map[string]string{GoalplanFile: compact(t, p), GoalplanLedgerFile: "ledger bytes\n", "schema-v2.marker": "marker bytes\n"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := ReadGoalplan(root, "integrity")
	if read == nil {
		t.Fatal("test plan not readable")
	}
	integrityReasonsEqual(t, GoalplanDefinitionIntegrityReasons(read), "work phase p depends on unknown work phase 'ghost'")
	GoalplanDependencyCompletionReasons(read)
	supersededIntegrityReasons(read)
	EffectiveSchemaVersion(read, true)
	ComputeQaRequired(read)
	for name, body := range files {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(raw) != body {
			t.Errorf("%s changed: %q, %v", name, raw, err)
		}
	}
}
