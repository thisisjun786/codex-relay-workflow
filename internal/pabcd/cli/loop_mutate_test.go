package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The cases below port the write and refusal parts of goalplan-public-surface.test.ts and the cli part of
// steering.test.ts (CXC v0.2.40). The plan fixture is the oracle's: a base phase that is done, a live phase
// with four tasks (one done, one ready, one waiting on a later task) and a blocked phase behind it.

const loopMutSession = "sess-public"

func loopMutPhases() []goalplan.GoalplanWorkPhase {
	return []goalplan.GoalplanWorkPhase{
		{
			ID: "wp-base", Title: "base", Status: goalplan.WorkPhaseDone, CriteriaIDs: []string{},
			Tasks: []goalplan.GoalplanTask{
				{ID: "shared", Title: "base task", Status: goalplan.TaskDone, Outcome: "base shipped"},
				{ID: "base-only", Title: "base-only task", Status: goalplan.TaskDone, Outcome: "base only shipped"},
			},
		},
		{
			ID: "wp-live", Title: "live", Status: goalplan.WorkPhaseInProgress, DependsOn: []string{"wp-base"}, CriteriaIDs: []string{"c-1"},
			Tasks: []goalplan.GoalplanTask{
				{ID: "shared", Title: "local prerequisite", Status: goalplan.TaskDone, Outcome: "local ready"},
				{ID: "ready-task", Title: "ready task", Status: goalplan.TaskPending, DependsOn: []string{"shared"}},
				{ID: "blocked-task", Title: "blocked task", Status: goalplan.TaskPending, DependsOn: []string{"later"}},
				{ID: "later", Title: "later task", Status: goalplan.TaskPending},
			},
		},
		{
			ID: "wp-blocked", Title: "blocked", Status: goalplan.WorkPhasePending, DependsOn: []string{"wp-live"}, CriteriaIDs: []string{},
			Tasks: []goalplan.GoalplanTask{{ID: "ready-task", Title: "same id elsewhere", Status: goalplan.TaskPending}},
		},
	}
}

// loopMutWorkspace writes the oracle's fixture plan and binds loopMutSession to it.
func loopMutWorkspace(t *testing.T, mutate func(*goalplan.Goalplan)) (string, string) {
	t.Helper()
	cwd := loopReadWorkspace(t)
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{
		Objective: "public surface fixture",
		Criteria:  []goalplan.NewGoalplanCriterion{{Scenario: "contract is verified", ExpectedEvidence: "go test exits 0"}},
		Now:       func() string { return "2026-08-29T00:00:00.000Z" },
	})
	live := "wp-live"
	plan.ActiveWorkPhaseID = &live
	plan.WorkPhases = loopMutPhases()
	if mutate != nil {
		mutate(plan)
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	bound := state.DefaultState(loopMutSession, "")
	bound.Slug = plan.Slug
	if err := state.WriteState(cwd, bound); err != nil {
		t.Fatal(err)
	}
	return cwd, plan.Slug
}

func loopMutFile(t *testing.T, cwd, slug, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "goalplans", slug, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// loopMutUnchanged fails when the plan or ledger bytes differ from the snapshot.
type loopMutSnapshot struct{ plan, ledger string }

func loopMutTake(t *testing.T, cwd, slug string) loopMutSnapshot {
	t.Helper()
	return loopMutSnapshot{loopMutFile(t, cwd, slug, "goalplan.json"), loopMutFile(t, cwd, slug, "ledger.jsonl")}
}

func (s loopMutSnapshot) assertUnchanged(t *testing.T, cwd, slug string) {
	t.Helper()
	now := loopMutTake(t, cwd, slug)
	if now.plan != s.plan {
		t.Errorf("goalplan.json changed")
	}
	if now.ledger != s.ledger {
		t.Errorf("ledger.jsonl changed: %q -> %q", s.ledger, now.ledger)
	}
}

func loopMutRun(t *testing.T, cwd string, wantCode int, argv ...string) string {
	t.Helper()
	got := loopRun(t, cwd, append(argv, "--cwd", cwd)...)
	if got.Code != wantCode {
		t.Fatalf("%v: code %d, want %d (%q)", argv, got.Code, wantCode, got.Output)
	}
	return got.Output
}

func loopMutPlan(t *testing.T, cwd, slug string) *goalplan.Goalplan {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		t.Fatal("plan missing")
	}
	return plan
}

// steering.test.ts "cli: steer applies a batch passed inline" and the file form.
func TestLoopSteerAppliesInlineAndFileBatches(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	batch := `{"idempotencyKey":"k1","rationale":"user asked","evidence":"chat","ops":[{"kind":"annotate","note":"prefer streaming"}]}`
	out := loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", batch)
	if out != "loop steer: applied k1 (1 op(s): annotate)" {
		t.Fatalf("output = %q", out)
	}
	if got := len(loopMutPlan(t, cwd, slug).SteeringLog); got != 1 {
		t.Fatalf("steeringLog = %d", got)
	}
	again := loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", batch)
	if !strings.HasPrefix(again, "loop steer: k1 was already applied at ") || !strings.HasSuffix(again, " — nothing to do") {
		t.Fatalf("repeat output = %q", again)
	}
	file := filepath.Join(cwd, "batch.json")
	if err := os.WriteFile(file, []byte(strings.Replace(batch, `"k1"`, `"k2"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out = loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", file)
	if out != "loop steer: applied k2 (1 op(s): annotate)" {
		t.Fatalf("file output = %q", out)
	}
}

// steering.test.ts cli refusals: canonical session, bound goalplan, JSON, both flags.
func TestLoopSteerRefusals(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	batch := `{"idempotencyKey":"k1","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"non-canonical", []string{"steer", "--session", "a/b", "--batch-json", batch},
			`loop steer: --session "a/b" is not a canonical session id — it would resolve to a different state file and steer another goal`},
		{"no session", []string{"steer", "--batch-json", "{}"}, "loop steer: --session <id> is required"},
		{"blank session", []string{"steer", "--session", "  ", "--batch-json", "{}"}, "loop steer: --session <id> is required"},
		{"no batch", []string{"steer", "--session", loopMutSession}, "loop steer: --batch-json <path-or-json> is required"},
		{"unreadable file", []string{"steer", "--session", loopMutSession, "--batch-json", "nope.json"},
			"loop steer: could not read the batch at nope.json (ENOENT: no such file or directory, open '" + filepath.Join(cwd, "nope.json") + "')"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := loopMutRun(t, cwd, 1, c.argv...); out != c.want {
				t.Fatalf("output = %q, want %q", out, c.want)
			}
		})
	}
	if out := loopMutRun(t, cwd, 1, "steer", "--session", loopMutSession, "--batch-json", "{not json"); !strings.HasPrefix(out, "loop steer: batch is not valid JSON (") {
		t.Fatalf("malformed JSON output = %q", out)
	}
	loopSession(t, cwd, "sess-2")
	if out := loopMutRun(t, cwd, 1, "steer", "--session", "sess-2", "--batch-json", batch); out != "loop steer: session 'sess-2' has no bound goalplan — run `crw pabcd loop init --session sess-2` first" {
		t.Fatalf("unbound output = %q", out)
	}
	if out := loopMutRun(t, cwd, 1, "steer", "--session", loopMutSession, "--batch-json", `{"idempotencyKey":"k","rationale":"r","evidence":"e","ops":[]}`); !strings.Contains(out, "ops must be a non-empty array") {
		t.Fatalf("rejected output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
}

// public-surface "add-work-phase without dependencies reuses the pre-upgrade idempotency key".
func TestLoopAddWorkPhaseReusesTheContentDerivedKey(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, func(plan *goalplan.Goalplan) {
		plan.SteeringLog = []goalplan.SteeringEntry{{
			IdempotencyKey: "add-work-phase-c90b4bd0e709", Rationale: "cxc loop add-work-phase", Evidence: "wp-new: new",
			AppliedAt: "2026-08-28T00:00:00.000Z", Summary: "1 op(s): add-work-phase",
		}}
	})
	before := loopMutTake(t, cwd, slug)
	out := loopMutRun(t, cwd, 0, "add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new")
	if !strings.HasPrefix(out, "loop add-work-phase: already applied at ") || !strings.HasSuffix(out, " - nothing to do") {
		t.Fatalf("output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
}

// Post-evaluation 1c7e2a5d d1: a recorded key whose phase is not in the plan (the pre-upgrade state) cannot show what
// prerequisites it was registered with, so a retry that names some is not the exact legacy retry.
func TestLoopAddWorkPhaseLegacyKeyRefusesChangedDependencies(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, func(plan *goalplan.Goalplan) {
		plan.SteeringLog = []goalplan.SteeringEntry{{
			IdempotencyKey: "add-work-phase-c90b4bd0e709", Rationale: "cxc loop add-work-phase", Evidence: "wp-new: new",
			AppliedAt: "2026-08-28T00:00:00.000Z", Summary: "1 op(s): add-work-phase",
		}}
	})
	before := loopMutTake(t, cwd, slug)
	for _, deps := range [][]string{{"ghost"}, {"wp-base"}, {"wp-base", "wp-live"}} {
		argv := []string{"add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new"}
		for _, dep := range deps {
			argv = append(argv, "--depends-on", dep)
		}
		out := loopMutRun(t, cwd, 1, argv...)
		if out != "loop add-work-phase: work phase 'wp-new' has a recorded key but is not in the plan, so its --depends-on cannot be checked against what was registered" {
			t.Errorf("%v: output = %q", deps, out)
		}
	}
	before.assertUnchanged(t, cwd, slug)
	// The exact legacy retry (no prerequisites) is still the recorded duplicate.
	out := loopMutRun(t, cwd, 0, "add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new")
	if !strings.Contains(out, ": already applied at ") {
		t.Fatalf("legacy retry output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
}

// public-surface "comma dependency is rejected while repeated flags persist dependencies".
func TestLoopAddWorkPhaseDependencies(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	out := loopMutRun(t, cwd, 1, "add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new", "--depends-on", "wp-base,wp-live")
	if out != "loop add-work-phase: work phase wp-new depends on unknown work phase 'wp-base,wp-live'" {
		t.Fatalf("output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
	rendered := loopMutRun(t, cwd, 0, "add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new",
		"--depends-on", "wp-base", "--depends-on", "wp-live")
	if !strings.Contains(rendered, "  - wp-new [pending] new") {
		t.Fatalf("rendered plan = %q", rendered)
	}
	phases := loopMutPlan(t, cwd, slug).WorkPhases
	if got := phases[len(phases)-1].DependsOn; strings.Join(got, ",") != "wp-base,wp-live" {
		t.Fatalf("dependsOn = %v", got)
	}
	if !strings.Contains(loopMutFile(t, cwd, slug, "ledger.jsonl"), `"event":"dependency_registered"`) {
		t.Fatal("no dependency_registered row")
	}
}

// public-surface "add-work-phase / add-criterion without a session or id".
func TestLoopAddVerbsRequireTheirArguments(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"add-work-phase", "--id", "wp1", "--title", "x"}, "loop add-work-phase: --session <id> is required"},
		{[]string{"add-work-phase", "--session", loopMutSession, "--id", "wp1"}, "loop add-work-phase: --id <id> and --title <text> are both required"},
		{[]string{"add-criterion", "--session", loopMutSession}, `loop add-criterion: --criterion "<scenario>" is required`},
		{[]string{"add-criterion", "--session", "a/b", "--criterion", "x"},
			`loop add-criterion: --session "a/b" is not a canonical session id - it would resolve to a different state file and steer another goal`},
		{[]string{"add-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "x"}, "loop add-task: --work-phase, --id, and non-empty --title are required"},
		{[]string{"meet-criterion", "--session", loopMutSession, "--id", "c-1"}, "loop meet-criterion: --id and non-empty --evidence are required"},
		{[]string{"ask", "--session", loopMutSession, "--id", "d"}, "loop ask: --id and non-empty --question are required"},
		{[]string{"decide", "--session", loopMutSession, "--id", "d"}, "loop decide: --id and non-empty --answer are required"},
		{[]string{"decide", "--id", "d", "--answer", "x"}, "loop decide: --session <id> is required"},
		{[]string{"decide", "--session", "a/b", "--id", "d", "--answer", "x"}, "loop decide: session id is not canonical"},
	}
	for _, c := range cases {
		if out := loopMutRun(t, cwd, 1, c.argv...); out != c.want {
			t.Errorf("%v: output = %q, want %q", c.argv, out, c.want)
		}
	}
	loopSession(t, cwd, "sess-2")
	if out := loopMutRun(t, cwd, 1, "add-work-phase", "--session", "sess-2", "--id", "w", "--title", "t"); out != "loop add-work-phase: session 'sess-2' has no bound goalplan - run `crw pabcd loop init --session sess-2` first" {
		t.Errorf("unbound output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
}

// public-surface "add-task uses phase-local uniqueness and terminal rejection writes nothing".
func TestLoopAddTaskUniquenessAndTerminalPhase(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	loopMutRun(t, cwd, 0, "add-task", "--session", loopMutSession, "--work-phase", "wp-blocked", "--id", "shared", "--title", "local shared")
	tasks := loopMutPlan(t, cwd, slug).WorkPhases[2].Tasks
	if tasks[len(tasks)-1].ID != "shared" {
		t.Fatalf("tasks = %+v", tasks)
	}
	out := loopMutRun(t, cwd, 1, "add-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "shared", "--title", "duplicate")
	if out != "loop add-task: task 'wp-live/shared' is already in this work phase" {
		t.Fatalf("duplicate output = %q", out)
	}
	before := loopMutTake(t, cwd, slug)
	out = loopMutRun(t, cwd, 1, "add-task", "--session", loopMutSession, "--work-phase", "wp-base", "--id", "new", "--title", "new")
	if out != "loop add-task: work phase 'wp-base' is done and cannot accept a new task" {
		t.Fatalf("terminal output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
}

// public-surface "add-task accepts same-phase dependencies and rejects cross-phase, self, and comma references".
func TestLoopAddTaskDependencies(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	out := loopMutRun(t, cwd, 0, "add-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "dependent",
		"--title", "same-phase dependent", "--depends-on", "shared", "--depends-on", "later")
	if out != "loop add-task: "+slug+" dependent applied" {
		t.Fatalf("output = %q", out)
	}
	var stored []string
	for _, task := range loopMutPlan(t, cwd, slug).WorkPhases[1].Tasks {
		if task.ID == "dependent" {
			stored = task.DependsOn
		}
	}
	if strings.Join(stored, ",") != "shared,later" {
		t.Fatalf("dependsOn = %v", stored)
	}
	ledger := loopMutFile(t, cwd, slug, "ledger.jsonl")
	if !strings.Contains(ledger, `"event":"dependency_registered"`) || !strings.Contains(ledger, `"detail":"task wp-live/dependent depends on shared, later"`) {
		t.Fatalf("ledger = %q", ledger)
	}
	before := loopMutTake(t, cwd, slug)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--id", "cross-phase", "--title", "cross phase", "--depends-on", "base-only"},
			"loop add-task: task wp-live/cross-phase depends on unknown task 'base-only' in the same work phase"},
		{[]string{"--id", "self", "--title", "self", "--depends-on", "self"}, "loop add-task: task wp-live/self depends on itself"},
		{[]string{"--id", "comma", "--title", "comma", "--depends-on", "shared,later"},
			"loop add-task: task wp-live/comma depends on unknown task 'shared,later' in the same work phase"},
	}
	for _, c := range cases {
		argv := append([]string{"add-task", "--session", loopMutSession, "--work-phase", "wp-live"}, c.args...)
		if got := loopMutRun(t, cwd, 1, argv...); got != c.want {
			t.Errorf("output = %q, want %q", got, c.want)
		}
		before.assertUnchanged(t, cwd, slug)
	}
}

// public-surface "complete-task stores trimmed outcome and appends identical detail".
func TestLoopCompleteTaskStoresTheTrimmedOutcomeAndOneLedgerRow(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	out := loopMutRun(t, cwd, 0, "complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task",
		"--outcome", "  go test: 24 pass  ")
	if out != "loop complete-task: "+slug+" ready-task applied" {
		t.Fatalf("output = %q", out)
	}
	plan := loopMutPlan(t, cwd, slug)
	task := plan.WorkPhases[1].Tasks[1]
	if task.Status != goalplan.TaskDone || task.Outcome != "go test: 24 pass" || plan.WorkPhases[1].Status != goalplan.WorkPhaseInProgress ||
		plan.Criteria[0].Status != goalplan.CriterionOpen {
		t.Fatalf("plan = %+v", plan.WorkPhases[1])
	}
	rows := strings.Split(strings.TrimSpace(loopMutFile(t, cwd, slug, "ledger.jsonl")), "\n")
	if len(rows) != 1 || !strings.Contains(rows[0], `"event":"task_done"`) || !strings.Contains(rows[0], `"detail":"go test: 24 pass"`) {
		t.Fatalf("ledger = %q", rows)
	}
}

// public-surface "ledger append failure keeps the committed lifecycle state and returns code 0 with a warning".
func TestLoopLifecycleLedgerFailureKeepsTheCommittedState(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	if err := os.Mkdir(filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := loopMutRun(t, cwd, 0, "complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task",
		"--outcome", "authoritative plan proof")
	if !strings.HasPrefix(out, "loop complete-task: "+slug+" ready-task applied\nwarning: goalplan state was committed, but ledger append failed:") {
		t.Fatalf("output = %q", out)
	}
	task := loopMutPlan(t, cwd, slug).WorkPhases[1].Tasks[1]
	if task.Status != goalplan.TaskDone || task.Outcome != "authoritative plan proof" {
		t.Fatalf("task = %+v", task)
	}
}

// public-surface "missing and blank outcome leave plan and ledger unchanged" and the blocker rejections.
func TestLoopCompleteTaskRefusalsWriteNothing(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	const required = "loop complete-task: --work-phase, --id, and non-empty --outcome are required"
	for _, tail := range [][]string{nil, {"--outcome", "   "}} {
		argv := append([]string{"complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task"}, tail...)
		if out := loopMutRun(t, cwd, 1, argv...); out != required {
			t.Errorf("output = %q", out)
		}
	}
	for phase, id := range map[string]string{"wp-live": "blocked-task", "wp-blocked": "ready-task"} {
		out := loopMutRun(t, cwd, 1, "complete-task", "--session", loopMutSession, "--work-phase", phase, "--id", id, "--outcome", "must not commit")
		if want := "loop complete-task: task '" + phase + "/" + id + "' is not ready"; out != want {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
	before.assertUnchanged(t, cwd, slug)
}

// public-surface "complete-task retry preserves first outcome and skips write and append".
func TestLoopCompleteTaskRetryKeepsTheFirstOutcome(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	loopMutRun(t, cwd, 0, "complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task", "--outcome", "first proof")
	after := loopMutTake(t, cwd, slug)
	out := loopMutRun(t, cwd, 0, "complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task", "--outcome", "replacement proof")
	if out != "loop complete-task: task 'wp-live/ready-task' is already done; nothing to do" {
		t.Fatalf("output = %q", out)
	}
	after.assertUnchanged(t, cwd, slug)
	if got := loopMutPlan(t, cwd, slug).WorkPhases[1].Tasks[1].Outcome; got != "first proof" {
		t.Fatalf("outcome = %q", got)
	}
}

// public-surface "criterion evidence is trimmed and retry keeps the first evidence" and its refusals.
func TestLoopMeetCriterion(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	if out := loopMutRun(t, cwd, 1, "meet-criterion", "--session", loopMutSession, "--id", "c-404", "--evidence", "proof"); out != "loop meet-criterion: criterion 'c-404' is not in this plan" {
		t.Errorf("missing output = %q", out)
	}
	if out := loopMutRun(t, cwd, 1, "meet-criterion", "--session", loopMutSession, "--id", "c-1", "--evidence", "   "); out != "loop meet-criterion: --id and non-empty --evidence are required" {
		t.Errorf("blank output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)
	if out := loopMutRun(t, cwd, 0, "meet-criterion", "--session", loopMutSession, "--id", "c-1", "--evidence", "  exit 0  "); out != "loop meet-criterion: "+slug+" c-1 applied" {
		t.Fatalf("output = %q", out)
	}
	after := loopMutTake(t, cwd, slug)
	loopMutRun(t, cwd, 0, "meet-criterion", "--session", loopMutSession, "--id", "c-1", "--evidence", "replacement")
	after.assertUnchanged(t, cwd, slug)
	got := loopMutPlan(t, cwd, slug).Criteria[0].CapturedEvidence
	if got == nil || *got != "exit 0" {
		t.Fatalf("capturedEvidence = %v", got)
	}
	if !strings.Contains(after.ledger, `"event":"criterion_met"`) || !strings.Contains(after.ledger, `"detail":"exit 0"`) {
		t.Fatalf("ledger = %q", after.ledger)
	}
}

// public-surface "lock contention fails closed and leaves both files unchanged".
func TestLoopLifecycleLockContentionFailsClosed(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	if err := os.Mkdir(filepath.Join(cwd, ".crw", "goalplans", slug, ".goalplan.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := loopMutTake(t, cwd, slug)
	for _, argv := range [][]string{
		{"complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task", "--outcome", "must not commit"},
		{"ask", "--session", loopMutSession, "--id", "d1", "--question", "Q"},
		{"add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new"},
	} {
		out := loopMutRun(t, cwd, 1, argv...)
		if !strings.HasPrefix(out, "loop "+argv[0]+": goalplan '"+slug+"' is busy.") || !strings.Contains(out, ".goalplan.lock") {
			t.Errorf("%s output = %q", argv[0], out)
		}
	}
	before.assertUnchanged(t, cwd, slug)
}

// public-surface "add-criterion takes --surface desktop, refuses unknown surfaces" and the presented rules.
func TestLoopAddCriterionSurfaces(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	out := loopMutRun(t, cwd, 0, "add-criterion", "--session", loopMutSession, "--criterion", "tray popup matrix", "--surface", "desktop")
	if !strings.Contains(out, "] tray popup matrix") {
		t.Fatalf("rendered plan = %q", out)
	}
	for _, c := range loopMutPlan(t, cwd, slug).Criteria {
		if c.Scenario == "tray popup matrix" && c.Surface != goalplan.SurfaceDesktop {
			t.Fatalf("surface = %q", c.Surface)
		}
	}
	before := loopMutTake(t, cwd, slug)
	if out := loopMutRun(t, cwd, 1, "add-criterion", "--session", loopMutSession, "--criterion", "x", "--surface", "native"); out != "loop add-criterion: --surface must be logic|web|tui|desktop (got 'native')" {
		t.Errorf("unknown surface output = %q", out)
	}
	for _, argv := range [][]string{
		{"--criterion", "native", "--presented", "native"},
		{"--criterion", "web", "--surface", "web", "--presented", "native"},
		{"--criterion", "unknown", "--surface", "desktop", "--presented=web"},
	} {
		out := loopMutRun(t, cwd, 1, append([]string{"add-criterion", "--session", loopMutSession}, argv...)...)
		if out != "loop add-criterion: --presented native requires --surface desktop" {
			t.Errorf("%v: output = %q", argv, out)
		}
	}
	before.assertUnchanged(t, cwd, slug)
	loopMutRun(t, cwd, 0, "add-criterion", "--session", loopMutSession, "--criterion", "native app", "--surface", "desktop", "--presented", "native")
	loopMutRun(t, cwd, 0, "add-criterion", "--session", loopMutSession, "--criterion", "native dialog", "--surface=desktop", "--presented=native")
	native := 0
	for _, c := range loopMutPlan(t, cwd, slug).Criteria {
		if c.Presented == goalplan.PresentedNative && c.Surface == goalplan.SurfaceDesktop {
			native++
		}
	}
	if native != 2 {
		t.Fatalf("native desktop criteria = %d", native)
	}
	// A repeat of the same criterion is a recorded duplicate, not a second criterion.
	dup := loopMutRun(t, cwd, 0, "add-criterion", "--session", loopMutSession, "--criterion", "native app", "--surface", "desktop", "--presented", "native")
	if !strings.HasPrefix(dup, "loop add-criterion: already applied at ") {
		t.Fatalf("duplicate output = %q", dup)
	}
}

// public-surface "ask records an open decision and hides only linked work phases" / "decide releases ...".
func TestLoopAskAndDecide(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, func(plan *goalplan.Goalplan) {
		plan.WorkPhases = append(plan.WorkPhases, goalplan.GoalplanWorkPhase{
			ID: "wp-explicit", Title: "explicit", Status: goalplan.WorkPhaseBlocked, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{},
			BlockedReason: loopString("vendor"),
		})
	})
	out := loopMutRun(t, cwd, 0, "ask", "--session", loopMutSession, "--id", "dec-1", "--question", "Choose API",
		"--option", "A", "--option", "B", "--recommendation", "A", "--work-phase", "wp-live", "--work-phase", "wp-explicit")
	if out != "loop ask: "+slug+" dec-1 applied" {
		t.Fatalf("ask output = %q", out)
	}
	plan := loopMutPlan(t, cwd, slug)
	decision := plan.Decisions[0]
	if decision.Status != goalplan.DecisionOpen || strings.Join(decision.Options, "|") != "A|B" || decision.Recommendation != "A" || decision.AskedAt == "" {
		t.Fatalf("decision = %+v", decision)
	}
	if got := plan.WorkPhases[1].AwaitsDecision; len(got) != 1 || got[0] != "dec-1" {
		t.Fatalf("awaitsDecision = %v", got)
	}
	if shown := loopMutRun(t, cwd, 0, "show", "--session", loopMutSession); !strings.Contains(shown, "options: A | B (recommended: A)") || !strings.Contains(shown, "waiting: wp-live, wp-explicit") {
		t.Fatalf("show = %q", shown)
	}
	before := loopMutTake(t, cwd, slug)
	if out := loopMutRun(t, cwd, 1, "ask", "--session", loopMutSession, "--id", "dec-2", "--question", " Choose API "); !strings.Contains(out, "dec-1") {
		t.Errorf("duplicate question output = %q", out)
	}
	if out := loopMutRun(t, cwd, 1, "ask", "--session", loopMutSession, "--id", "dec-2", "--question", "Other", "--work-phase", "ghost"); !strings.Contains(out, "ghost") {
		t.Errorf("unknown phase output = %q", out)
	}
	if out := loopMutRun(t, cwd, 1, "ask", "--session", loopMutSession, "--id", "dec-2", "--question", "Pick",
		"--option", "A", "--option", "B", "--recommendation", "C"); !strings.Contains(out, "must be one of the options") {
		t.Errorf("recommendation output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)

	if out := loopMutRun(t, cwd, 0, "decide", "--session", loopMutSession, "--id", "dec-1", "--answer", "something else"); out != "loop decide: "+slug+" dec-1 applied" {
		t.Fatalf("decide output = %q", out)
	}
	plan = loopMutPlan(t, cwd, slug)
	if d := plan.Decisions[0]; d.Status != goalplan.DecisionDecided || d.Answer != "something else" || strings.Join(d.Options, "|") != "A|B" || d.DecidedAt == "" {
		t.Fatalf("decided = %+v", d)
	}
	if plan.WorkPhases[3].BlockedReason == nil || *plan.WorkPhases[3].BlockedReason != "vendor" {
		t.Fatalf("explicit block released: %+v", plan.WorkPhases[3])
	}
	after := loopMutTake(t, cwd, slug)
	if out := loopMutRun(t, cwd, 0, "decide", "--session", loopMutSession, "--id", "dec-1", "--answer", "something else"); !strings.HasSuffix(out, "; nothing to do") {
		t.Errorf("same answer output = %q", out)
	}
	after.assertUnchanged(t, cwd, slug)
	if out := loopMutRun(t, cwd, 1, "decide", "--session", loopMutSession, "--id", "dec-1", "--answer", "Use v3"); !strings.HasPrefix(out, "loop decide: ") {
		t.Errorf("different answer output = %q", out)
	}
	after.assertUnchanged(t, cwd, slug)
	if strings.Contains(loopMutFile(t, cwd, slug, "ledger.jsonl"), "dec-1") {
		t.Error("ask and decide write no ledger row")
	}
}

// A decision write that is not an object key: ask without --option stores no options key.
func TestLoopAskWithoutOptionsStoresNoOptionsKey(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	loopMutRun(t, cwd, 0, "ask", "--session", loopMutSession, "--id", "dec-1", "--question", "Choose API")
	if strings.Contains(loopMutFile(t, cwd, slug, "goalplan.json"), `"options"`) {
		t.Fatal("options key stored")
	}
}

// Port deviation (docs/port-cxc/known-defects/CRW-383.md): the oracle's add-criterion / add-work-phase drop the
// steering warning (ledger append failure) on the floor; this port appends it to the plan text.
func TestLoopAddVerbsReportTheLedgerWarning(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	if err := os.Mkdir(filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := loopMutRun(t, cwd, 0, "add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new")
	if !strings.Contains(out, "  - wp-new [pending] new") || !strings.Contains(out, "\nwarning: the batch was applied but its ledger entry could not be written") {
		t.Fatalf("output = %q", out)
	}
	if len(loopMutPlan(t, cwd, slug).WorkPhases) != 4 {
		t.Fatal("phase not committed")
	}
}

// RunLoopCli answers a mutating verb with the verb's own text, never the seam's.
func TestLoopMutatingVerbsAreNoLongerTheSeam(t *testing.T) {
	cwd := loopReadWorkspace(t)
	out := loopMutRun(t, cwd, 1, "add-criterion", "--criterion", "x")
	if out != "loop add-criterion: --session <id> is required" {
		t.Fatalf("output = %q", out)
	}
}

// Port deviation (docs/port-cxc/known-defects/CRW-383.md): a plan write that published and then failed the
// directory sync is a written plan, so the verb answers its success with the warning, and the ledger row is
// still appended, where the oracle throws after the plan has already moved.
func TestLoopLifecycleAndDecisionKeepAPublishedPlanWithAWarning(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	loopInitWriteGoalplanHook = func(cwd string, plan *goalplan.Goalplan) error {
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			return err
		}
		return &state.PublishedError{Err: errors.New("sync failed")}
	}
	t.Cleanup(func() { loopInitWriteGoalplanHook = nil })
	out := loopMutRun(t, cwd, 0, "complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task", "--outcome", "done")
	if want := "loop complete-task: " + slug + " ready-task applied\ngoalplan '" + slug + "' was published but its directory could not be synced: sync failed"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if !strings.Contains(loopMutFile(t, cwd, slug, "ledger.jsonl"), `"event":"task_done"`) {
		t.Fatal("no task_done row after a published plan")
	}
	out = loopMutRun(t, cwd, 0, "ask", "--session", loopMutSession, "--id", "dec-1", "--question", "Choose API")
	if !strings.HasSuffix(out, "was published but its directory could not be synced: sync failed") || len(loopMutPlan(t, cwd, slug).Decisions) != 1 {
		t.Fatalf("ask output = %q", out)
	}
	// A write that fails before the rename is an error and publishes nothing.
	loopInitWriteGoalplanHook = func(string, *goalplan.Goalplan) error { return errors.New("disk full") }
	before := loopMutTake(t, cwd, slug)
	args, err := ParseLoopCliArgs([]string{"meet-criterion", "--session", loopMutSession, "--id", "c-1", "--evidence", "e", "--cwd", cwd}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunLoopCli(args); err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v", err)
	}
	before.assertUnchanged(t, cwd, slug)
}

// Pre-merge evaluation e0c3603e d1: the content-derived key hashes the scenario (or the id and title) alone, so a
// retry with other options landed on the recorded key and answered "already applied" without storing them. Only an
// exact retry is a no-op; a retry that asks for another surface, presentation or prerequisite list is the
// transaction's usual conflict.
func TestLoopAddVerbsRefuseARetryWithOtherOptions(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	loopMutRun(t, cwd, 0, "add-criterion", "--session", loopMutSession, "--criterion", "tray matrix", "--surface", "logic")
	loopMutRun(t, cwd, 0, "add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new")
	before := loopMutTake(t, cwd, slug)

	for _, argv := range [][]string{
		{"--criterion", "tray matrix", "--surface", "desktop", "--presented", "native"},
		{"--criterion", "tray matrix", "--surface", "desktop"},
		{"--criterion", "tray matrix", "--surface", "web"},
	} {
		out := loopMutRun(t, cwd, 1, append([]string{"add-criterion", "--session", loopMutSession}, argv...)...)
		if out != `loop add-criterion: a criterion with scenario "tray matrix" is already registered with another surface or presentation` {
			t.Errorf("%v: output = %q", argv, out)
		}
	}
	for _, deps := range [][]string{{"wp-base"}, {"ghost"}, {"wp-base", "wp-live"}} {
		argv := []string{"add-work-phase", "--session", loopMutSession, "--id", "wp-new", "--title", "new"}
		for _, dep := range deps {
			argv = append(argv, "--depends-on", dep)
		}
		out := loopMutRun(t, cwd, 1, argv...)
		if out != "loop add-work-phase: work phase 'wp-new' is already registered with other prerequisites" {
			t.Errorf("%v: output = %q", deps, out)
		}
	}
	before.assertUnchanged(t, cwd, slug)

	// The exact retries stay recorded duplicates, native presentation and dependencies included.
	loopMutRun(t, cwd, 0, "add-criterion", "--session", loopMutSession, "--criterion", "native app", "--surface", "desktop", "--presented", "native")
	loopMutRun(t, cwd, 0, "add-work-phase", "--session", loopMutSession, "--id", "wp-dep", "--title", "dep", "--depends-on", "wp-base", "--depends-on", "wp-live")
	again := loopMutTake(t, cwd, slug)
	for _, argv := range [][]string{
		{"add-criterion", "--criterion", "tray matrix", "--surface", "logic"},
		{"add-criterion", "--criterion", "tray matrix"},
		{"add-criterion", "--criterion", "native app", "--surface", "desktop", "--presented", "native"},
		{"add-work-phase", "--id", "wp-new", "--title", "new"},
		{"add-work-phase", "--id", "wp-dep", "--title", "dep", "--depends-on", "wp-live", "--depends-on", "wp-base"},
	} {
		out := loopMutRun(t, cwd, 0, append(argv[:1:1], append([]string{"--session", loopMutSession}, argv[1:]...)...)...)
		if !strings.Contains(out, ": already applied at ") {
			t.Errorf("%v: output = %q", argv, out)
		}
	}
	again.assertUnchanged(t, cwd, slug)
	for _, c := range loopMutPlan(t, cwd, slug).Criteria {
		if c.Scenario == "tray matrix" && (c.Surface != goalplan.SurfaceLogic || c.Presented != "") {
			t.Fatalf("criterion = %+v", c)
		}
	}
}

// Pre-merge evaluation e0c3603e d2: the batch decoder replaced an unpaired surrogate (and a byte that is not UTF-8)
// with U+FFFD, so two distinct idempotency keys collapsed into one and the second batch was dropped as a duplicate;
// the goalplan reader refuses the same loss.
func TestLoopSteerRefusesATextThatDecodesLossily(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	batch := func(key string) string {
		return `{"idempotencyKey":"` + key + `","rationale":"r","evidence":"e","ops":[{"kind":"add-criterion","scenario":"first","surface":"logic"}]}`
	}
	for _, key := range []string{`\ud800`, `\udc00`, `a\ud800b`} {
		out := loopMutRun(t, cwd, 1, "steer", "--session", loopMutSession, "--batch-json", batch(key))
		if !strings.HasPrefix(out, "loop steer: batch holds an unpaired JSON surrogate at byte ") || !strings.HasSuffix(out, " that would lose stored text") {
			t.Errorf("%s: output = %q", key, out)
		}
	}
	file := filepath.Join(cwd, "bad.json")
	if err := os.WriteFile(file, []byte(strings.Replace(batch("k"), `"r"`, "\"r\xff\"", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := loopMutRun(t, cwd, 1, "steer", "--session", loopMutSession, "--batch-json", file); !strings.HasPrefix(out, "loop steer: batch holds a byte that is not UTF-8 at byte ") {
		t.Errorf("invalid UTF-8 output = %q", out)
	}
	before.assertUnchanged(t, cwd, slug)

	// A complete escaped pair and an escaped backslash keep their meaning; the key is stored decoded.
	if out := loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", batch(`\ud83d\ude00`)); !strings.HasPrefix(out, "loop steer: applied ") {
		t.Errorf("pair output = %q", out)
	}
	if !slices.ContainsFunc(loopMutPlan(t, cwd, slug).SteeringLog, func(e goalplan.SteeringEntry) bool { return e.IdempotencyKey == "\U0001F600" }) {
		t.Errorf("steering log = %+v, want the key U+1F600", loopMutPlan(t, cwd, slug).SteeringLog)
	}
	if out := loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", strings.Replace(batch(`\\ud800`), `"first"`, `"second"`, 1)); !strings.HasPrefix(out, "loop steer: applied ") {
		t.Errorf("escaped backslash output = %q", out)
	}
}

// Pre-merge evaluation e0c3603e d3: the ledger row of a lifecycle verb was appended after the write lock was
// released, so a later writer could commit (and log) first and the ledger would order the two transitions the
// other way round. The row belongs inside the lock, as the steering transaction writes its own.
func TestLoopLifecycleAppendsItsLedgerRowInsideTheWriteLock(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	lock := filepath.Join(cwd, ".crw", "goalplans", slug, goalplan.GoalplanLockDir)
	var held []bool
	previous := loopAppendLedger
	loopAppendLedger = func(cwd, slug string, entry goalplan.GoalplanLedgerEntry) error {
		_, err := os.Stat(lock)
		held = append(held, err == nil)
		return previous(cwd, slug, entry)
	}
	t.Cleanup(func() { loopAppendLedger = previous })

	loopMutRun(t, cwd, 0, "add-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "t-new", "--title", "new", "--depends-on", "ready-task")
	loopMutRun(t, cwd, 0, "complete-task", "--session", loopMutSession, "--work-phase", "wp-live", "--id", "ready-task", "--outcome", "proof")
	loopMutRun(t, cwd, 0, "meet-criterion", "--session", loopMutSession, "--id", "c-1", "--evidence", "proof")
	if len(held) != 3 {
		t.Fatalf("ledger appends = %v", held)
	}
	for i, inside := range held {
		if !inside {
			t.Errorf("append %d ran after the write lock was released", i)
		}
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock left behind: %v", err)
	}
}
