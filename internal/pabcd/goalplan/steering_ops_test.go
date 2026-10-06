package goalplan

import (
	"reflect"
	"strings"
	"testing"
)

// The B-class validation and ops scenarios of CXC v0.2.40 test/steering.test.ts
// (commit 3c1459ac), re-driven through this unit's pure functions: the cases that
// need applySteeringBatch (the idempotency key, the shared write lock and the
// ledger) belong to B15b / CRW-643 and are not exercised here.

// steeringOpsTestPlan is buildGoalplan's shape: an empty schema-3 plan, whose
// definition and dependency integrity both hold.
func steeringOpsTestPlan() *Goalplan {
	v := float64(3)
	return &Goalplan{
		Objective:     "steering fixture",
		Slug:          "steering",
		CreatedAt:     "2026-01-01T00:00:00.000Z",
		UpdatedAt:     "2026-01-01T00:00:00.000Z",
		WorkPhases:    []GoalplanWorkPhase{},
		Criteria:      []GoalplanCriterion{},
		Host:          GoalplanHostLink{Source: HostSourceNone},
		SchemaVersion: &v,
	}
}

// steeringOpsTestBatch is steering.test.ts's batch(): one annotate op, overridable.
func steeringOpsTestBatch(over map[string]any) map[string]any {
	batch := map[string]any{
		"idempotencyKey": "k1",
		"rationale":      "the scope shifted after the audit",
		"evidence":       "devlog/_plan/x/090.md:12",
		"ops":            []any{map[string]any{"kind": "annotate", "note": "narrowed to the parser"}},
	}
	for key, value := range over {
		batch[key] = value
	}
	return batch
}

// "rationale, evidence and idempotencyKey are all required" (:84-93), including the
// oracle's trim().length === 0 test, which refuses whitespace-only text.
func TestSteeringOpsBatchFieldsRequired(t *testing.T) {
	for _, key := range []string{"idempotencyKey", "rationale", "evidence"} {
		for _, value := range []any{"", "   "} {
			_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{key: value}))
			want := key + " is required and must be a non-empty string"
			if reason != want {
				t.Errorf("%s=%q: reason = %q, want %q", key, value, reason, want)
			}
		}
	}
}

// The batch itself: "batch must be a JSON object", and an ARRAY is refused here
// (Array.isArray, :75) even though the op check below does not test for one.
func TestSteeringOpsBatchMustBeObject(t *testing.T) {
	for _, raw := range []any{nil, "x", 5.0, true, []any{}} {
		_, reason := steeringOpsValidateBatch(raw)
		if reason != "batch must be a JSON object" {
			t.Errorf("raw %#v: reason = %q", raw, reason)
		}
	}
}

// "an empty ops array is rejected" (:165-170).
func TestSteeringOpsEmptyOpsRejected(t *testing.T) {
	_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{}}))
	if !strings.Contains(reason, "non-empty array") {
		t.Errorf("reason = %q, want it to name the non-empty array", reason)
	}
	if _, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": "x"})); !strings.Contains(reason, "non-empty array") {
		t.Errorf("non-array ops: reason = %q", reason)
	}
}

// "one invalid op rejects the whole batch" (:95-111).
func TestSteeringOpsOneInvalidOpRejectsWholeBatch(t *testing.T) {
	_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "annotate", "note": "fine"},
		map[string]any{"kind": "annotate", "note": "also fine"},
		map[string]any{"kind": "annotate"},
	}}))
	if reason != "ops[2] is an annotate without a note" {
		t.Errorf("reason = %q", reason)
	}
}

// "a weakening op kind is rejected with the supported set named" (:113-118).
func TestSteeringOpsUnsupportedKindNamesSupportedSet(t *testing.T) {
	_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "retitle_work_phase", "note": "x"},
	}}))
	want := "ops[0].kind \"retitle_work_phase\" is not supported - use \"annotate\", \"add-criterion\", or \"add-work-phase\""
	if reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	if _, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{map[string]any{"note": "x"}}})); reason != "ops[0].kind must be a string" {
		t.Errorf("missing kind: reason = %q", reason)
	}
}

// The op object test (:90) is typeof-only, so an ARRAY op falls through to the kind
// check and answers the kind reason (:92) - not "must be an object".
func TestSteeringOpsArrayOpFallsThroughToKindReason(t *testing.T) {
	for _, raw := range []any{nil, "x", 5.0, []any{}} {
		_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{raw}}))
		want := "ops[0] must be an object"
		if _, isArray := raw.([]any); isArray {
			want = "ops[0].kind must be a string"
		}
		if reason != want {
			t.Errorf("op %#v: reason = %q, want %q", raw, reason, want)
		}
	}
}

// "duplicate dependencies are rejected before write" (:379-392).
func TestSteeringOpsDuplicateDependenciesRejected(t *testing.T) {
	_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "add-work-phase", "id": "wp-c", "title": "C", "dependsOn": []any{"wp-a", "wp-a"}},
	}}))
	if reason != "ops[0].dependsOn must not contain duplicate ids" {
		t.Errorf("reason = %q", reason)
	}
	for _, bad := range []any{"x", []any{"wp-a", ""}, []any{"wp-a", 5.0}} {
		_, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
			map[string]any{"kind": "add-work-phase", "id": "wp-c", "title": "C", "dependsOn": bad},
		}}))
		if reason != "ops[0].dependsOn must be an array of non-empty work-phase ids" {
			t.Errorf("dependsOn %#v: reason = %q", bad, reason)
		}
	}
}

// The remaining add-work-phase field rules (:128-133).
func TestSteeringOpsAddWorkPhaseFields(t *testing.T) {
	cases := []struct {
		op   map[string]any
		want string
	}{
		{map[string]any{"kind": "add-work-phase", "title": "C"}, "ops[0].id must be a short lowercase work-phase id, e.g. \"wp04-loop-criteria\""},
		{map[string]any{"kind": "add-work-phase", "id": "WP-C", "title": "C"}, "ops[0].id must be a short lowercase work-phase id, e.g. \"wp04-loop-criteria\""},
		{map[string]any{"kind": "add-work-phase", "id": "wp-c"}, "ops[0] is an add-work-phase without a title"},
		{map[string]any{"kind": "add-work-phase", "id": "wp-c", "title": "   "}, "ops[0] is an add-work-phase without a title"},
	}
	for _, c := range cases {
		if _, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{c.op}})); reason != c.want {
			t.Errorf("op %#v: reason = %q, want %q", c.op, reason, c.want)
		}
	}
	batch, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "add-work-phase", "id": "wp-c", "title": "  C  "},
	}}))
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if batch.Ops[0].Title != "C" {
		t.Errorf("title = %q, want the trimmed C", batch.Ops[0].Title)
	}
	// The oracle stores the batch's dependsOn as a real empty array (op.dependsOn ?? []);
	// only applyOps turns it into the absent field.
	if batch.Ops[0].DependsOn == nil || len(batch.Ops[0].DependsOn) != 0 {
		t.Errorf("dependsOn = %#v, want an empty non-nil array", batch.Ops[0].DependsOn)
	}
}

// The add-criterion field rules (:105-125): scenario trimmed, surface known and
// defaulted to logic, presented native only on desktop, expectedEvidence trimmed when
// it is a string and "" otherwise, and an annotate note kept untrimmed.
func TestSteeringOpsAddCriterionAndAnnotateFields(t *testing.T) {
	batch, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "annotate", "note": "  keep me  "},
		map[string]any{"kind": "add-criterion", "scenario": "  dual-platform suite green  "},
		map[string]any{"kind": "add-criterion", "scenario": "tray", "surface": "desktop", "presented": "native", "expectedEvidence": "  receipts  "},
		map[string]any{"kind": "add-criterion", "scenario": "num", "expectedEvidence": 5.0},
	}}))
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if batch.Ops[0].Note != "  keep me  " {
		t.Errorf("annotate note = %q, want it untrimmed", batch.Ops[0].Note)
	}
	if batch.Ops[1].Scenario != "dual-platform suite green" || batch.Ops[1].Surface != SurfaceLogic {
		t.Errorf("criterion = %#v", batch.Ops[1])
	}
	if batch.Ops[1].ExpectedEvidence != "" {
		t.Errorf("expectedEvidence = %q, want empty", batch.Ops[1].ExpectedEvidence)
	}
	if batch.Ops[2].Surface != SurfaceDesktop || batch.Ops[2].Presented != PresentedNative || batch.Ops[2].ExpectedEvidence != "receipts" {
		t.Errorf("desktop criterion = %#v", batch.Ops[2])
	}
	if batch.Ops[3].ExpectedEvidence != "" {
		t.Errorf("non-string expectedEvidence = %q, want empty", batch.Ops[3].ExpectedEvidence)
	}
	if batch.Ops[3].Presented != "" {
		t.Errorf("presented = %q, want absent", batch.Ops[3].Presented)
	}
}

// "add-criterion accepts the desktop surface and rejects an unknown one" (:416-434),
// and the presented rules (:112-117, :436-454). A present-null surface or presented
// is refused, as the oracle's !== undefined tests do.
func TestSteeringOpsCriterionSurfaceAndPresentedRules(t *testing.T) {
	cases := []struct {
		op   map[string]any
		want string
	}{
		{map[string]any{"kind": "add-criterion", "scenario": "x", "surface": "native"}, "ops[0].surface must be \"logic\", \"web\", \"tui\", or \"desktop\""},
		{map[string]any{"kind": "add-criterion", "scenario": "x", "surface": nil}, "ops[0].surface must be \"logic\", \"web\", \"tui\", or \"desktop\""},
		{map[string]any{"kind": "add-criterion", "scenario": "x", "presented": "web"}, "ops[0].presented must be \"native\""},
		{map[string]any{"kind": "add-criterion", "scenario": "x", "presented": nil}, "ops[0].presented must be \"native\""},
		{map[string]any{"kind": "add-criterion", "scenario": "x", "surface": "logic", "presented": "native"}, "ops[0].presented \"native\" requires surface \"desktop\""},
		{map[string]any{"kind": "add-criterion"}, "ops[0] is an add-criterion without a scenario"},
		{map[string]any{"kind": "add-criterion", "scenario": "   "}, "ops[0] is an add-criterion without a scenario"},
	}
	for _, c := range cases {
		if _, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{c.op}})); reason != c.want {
			t.Errorf("op %#v: reason = %q, want %q", c.op, reason, c.want)
		}
	}
}

// "add-criterion appends a criterion" (:120-141): the id, surface, status and null
// captured evidence the oracle writes.
func TestSteeringOpsAddCriterionAppends(t *testing.T) {
	next, reason := steeringOpsApplyOps(steeringOpsTestPlan(), []SteerOp{{
		Kind: SteerOpAddCriterion, Scenario: "dual-platform suite green", Surface: SurfaceLogic, ExpectedEvidence: "receipts",
	}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if len(next.Criteria) != 1 {
		t.Fatalf("criteria = %#v", next.Criteria)
	}
	got := next.Criteria[0]
	want := GoalplanCriterion{ID: "c-1", Scenario: "dual-platform suite green", ExpectedEvidence: "receipts", Status: CriterionOpen, Surface: SurfaceLogic}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("criterion = %#v, want %#v", got, want)
	}
}

// The idempotence half of :120-141 is applySteeringBatch's, but the duplicate-scenario
// refusal it lands on (:196-197) is this unit's.
func TestSteeringOpsAddCriterionRefusesDuplicateScenario(t *testing.T) {
	plan := steeringOpsTestPlan()
	plan.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "dual-platform suite green", Status: CriterionOpen}}
	_, reason := steeringOpsApplyOps(plan, []SteerOp{{Kind: SteerOpAddCriterion, Scenario: "dual-platform suite green"}})
	if reason != "a criterion with scenario \"dual-platform suite green\" is already registered" {
		t.Errorf("reason = %q", reason)
	}
}

// The minted id is max existing c-N + 1, never criteria.length, so a hand-edited gap
// cannot collide (:199-204).
func TestSteeringOpsMintsCriterionIDAboveGap(t *testing.T) {
	plan := steeringOpsTestPlan()
	plan.Criteria = []GoalplanCriterion{
		{ID: "c-2", Scenario: "second", Status: CriterionOpen},
		{ID: "c-1", Scenario: "first", Status: CriterionOpen},
	}
	next, reason := steeringOpsApplyOps(plan, []SteerOp{{Kind: SteerOpAddCriterion, Scenario: "third"}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if got := next.Criteria[len(next.Criteria)-1].ID; got != "c-3" {
		t.Errorf("minted id = %q, want c-3", got)
	}
}

// Above 2^53 the oracle's float64 arithmetic loses the increment (a known defect,
// port: kept - docs/port-cxc/known-defects.md), and a >= 400-digit run becomes
// Infinity and contributes 0 (Number.isFinite, :203).
func TestSteeringOpsMintBoundaryAtTwoToThe53(t *testing.T) {
	plan := steeringOpsTestPlan()
	plan.Criteria = []GoalplanCriterion{{ID: "c-9007199254740993", Scenario: "huge", Status: CriterionOpen}}
	next, reason := steeringOpsApplyOps(plan, []SteerOp{{Kind: SteerOpAddCriterion, Scenario: "next"}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if got := next.Criteria[len(next.Criteria)-1].ID; got != "c-9007199254740992" {
		t.Errorf("minted id = %q, want the oracle's rounded c-9007199254740992", got)
	}

	overflow := steeringOpsTestPlan()
	overflow.Criteria = []GoalplanCriterion{{ID: "c-" + strings.Repeat("9", 400), Scenario: "infinite", Status: CriterionOpen}}
	next, reason = steeringOpsApplyOps(overflow, []SteerOp{{Kind: SteerOpAddCriterion, Scenario: "next"}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if got := next.Criteria[len(next.Criteria)-1].ID; got != "c-1" {
		t.Errorf("minted id = %q, want c-1 (Infinity contributes 0)", got)
	}
}

// The minted id is spelled by JavaScript's Number::toString, so 1e21 prints as
// "1e+21" and 1000000 as "1000000" (:202).
func TestSteeringOpsMintUsesJSNumberText(t *testing.T) {
	plan := steeringOpsTestPlan()
	plan.Criteria = []GoalplanCriterion{{ID: "c-1000000000000000000000", Scenario: "1e21", Status: CriterionOpen}}
	next, reason := steeringOpsApplyOps(plan, []SteerOp{{Kind: SteerOpAddCriterion, Scenario: "next"}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if got := next.Criteria[len(next.Criteria)-1].ID; got != "c-1e+21" {
		t.Errorf("minted id = %q, want c-1e+21", got)
	}
}

// "add-work-phase appends a pending phase; duplicate id is rejected" (:143-163).
func TestSteeringOpsAddWorkPhaseAppendsPending(t *testing.T) {
	next, reason := steeringOpsApplyOps(steeringOpsTestPlan(), []SteerOp{{
		Kind: SteerOpAddWorkPhase, ID: "wp99-new", Title: "Newly scoped work",
	}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if len(next.WorkPhases) != 1 {
		t.Fatalf("workPhases = %#v", next.WorkPhases)
	}
	got := next.WorkPhases[0]
	want := GoalplanWorkPhase{ID: "wp99-new", Title: "Newly scoped work", Status: WorkPhasePending, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("work phase = %#v, want %#v", got, want)
	}

	duplicate := steeringOpsTestPlan()
	duplicate.WorkPhases = []GoalplanWorkPhase{want}
	_, reason = steeringOpsApplyOps(duplicate, []SteerOp{{Kind: SteerOpAddWorkPhase, ID: "wp99-new", Title: "Duplicate"}})
	if reason != "work phase 'wp99-new' is already in this plan" {
		t.Errorf("reason = %q", reason)
	}
}

// "same-batch backward reference succeeds and forward reference is rejected as
// dangling" (:344-377), driven through validateBatch and applyOps.
func TestSteeringOpsSameBatchBackwardReferenceAndForwardDangling(t *testing.T) {
	batch, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "add-work-phase", "id": "wp-a", "title": "A", "dependsOn": []any{}},
		map[string]any{"kind": "add-work-phase", "id": "wp-b", "title": "B", "dependsOn": []any{"wp-a"}},
	}}))
	if reason != "" {
		t.Fatalf("validate reason = %q", reason)
	}
	next, reason := steeringOpsApplyOps(steeringOpsTestPlan(), batch.Ops)
	if reason != "" {
		t.Fatalf("apply reason = %q", reason)
	}
	if len(next.WorkPhases) != 2 || next.WorkPhases[0].ID != "wp-a" || next.WorkPhases[1].ID != "wp-b" {
		t.Fatalf("workPhases = %#v", next.WorkPhases)
	}
	if !reflect.DeepEqual(next.WorkPhases[1].DependsOn, []string{"wp-a"}) {
		t.Errorf("dependsOn = %#v", next.WorkPhases[1].DependsOn)
	}
	if next.WorkPhases[0].DependsOn != nil {
		t.Errorf("wp-a dependsOn = %#v, want absent", next.WorkPhases[0].DependsOn)
	}

	dangling, reason := steeringOpsValidateBatch(steeringOpsTestBatch(map[string]any{"ops": []any{
		map[string]any{"kind": "add-work-phase", "id": "wp-x", "title": "X", "dependsOn": []any{"wp-y"}},
		map[string]any{"kind": "add-work-phase", "id": "wp-y", "title": "Y", "dependsOn": []any{"wp-x"}},
	}}))
	if reason != "" {
		t.Fatalf("validate reason = %q", reason)
	}
	if _, reason := steeringOpsApplyOps(steeringOpsTestPlan(), dangling.Ops); reason != "work phase wp-x depends on unknown work phase 'wp-y'" {
		t.Errorf("reason = %q", reason)
	}
}

// "wp7 preservation: steering RMW keeps dependsOn and outcome" (:394-414): applyOps
// keeps the plan's other fields and never mutates the input.
func TestSteeringOpsApplyOpsPreservesPlanAndDoesNotMutate(t *testing.T) {
	plan := steeringOpsTestPlan()
	plan.WorkPhases = []GoalplanWorkPhase{{
		ID: "wp-1", Title: "first", Status: WorkPhaseInProgress, CriteriaIDs: []string{},
		Tasks: []GoalplanTask{
			{ID: "t-1", Title: "first", Status: TaskDone, DependsOn: []string{}, Outcome: "first task verified"},
			{ID: "t-2", Title: "second", Status: TaskDone, DependsOn: []string{"t-1"}, Outcome: "second task verified"},
		},
	}}
	active := plan.WorkPhases[0].ID
	plan.ActiveWorkPhaseID = &active
	before := *plan
	beforePhases := append([]GoalplanWorkPhase{}, plan.WorkPhases...)

	next, reason := steeringOpsApplyOps(plan, []SteerOp{{Kind: SteerOpAnnotate, Note: "narrowed to the parser"}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if !reflect.DeepEqual(next.WorkPhases, beforePhases) {
		t.Errorf("workPhases = %#v, want %#v", next.WorkPhases, beforePhases)
	}
	if next.ActiveWorkPhaseID == nil || *next.ActiveWorkPhaseID != "wp-1" {
		t.Errorf("activeWorkPhaseId = %#v", next.ActiveWorkPhaseID)
	}
	if !reflect.DeepEqual(*plan, before) {
		t.Errorf("the input plan was mutated: %#v", *plan)
	}

	// An annotate op changes nothing at all: the returned plan equals the input.
	annotated, reason := steeringOpsApplyOps(plan, []SteerOp{{Kind: SteerOpAnnotate, Note: "ledger-only, by design"}})
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if !reflect.DeepEqual(*annotated, *plan) {
		t.Errorf("annotate changed the plan: %#v", *annotated)
	}
}
