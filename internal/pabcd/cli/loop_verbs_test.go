package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// loopReadWorkspace is a temporary workspace with temporary HOME, CODEX_HOME and CRW_HOME, so a run of
// these verbs can never reach the operator's real state (the repository's rule for anything that reads
// session or config state).
func loopReadWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	return root
}

// loopRun parses and runs one crw pabcd loop command line in cwd, failing the test on a parse error
// (a case that wants the refusal calls ParseLoopCliArgs itself).
func loopRun(t *testing.T, cwd string, argv ...string) LoopCliResult {
	t.Helper()
	args, err := ParseLoopCliArgs(argv, cwd)
	if err != nil {
		t.Fatalf("parse %q: %v", argv, err)
	}
	result, err := RunLoopCli(args)
	if err != nil {
		t.Fatalf("run %q: %v", argv, err)
	}
	return result
}

// loopSession writes an IDLE session state file so the bound-verb cases have one to bind.
func loopSession(t *testing.T, cwd, id string) {
	t.Helper()
	if err := state.WriteState(cwd, state.DefaultState(id, "")); err != nil {
		t.Fatal(err)
	}
}

// TestLoopHelpTokensPrintUsageAndExitZero ports the loop part of help-verbs.test.ts: help, --help and -h
// are not unknown verbs, they print the usage block and exit 0.
func TestLoopHelpTokensPrintUsageAndExitZero(t *testing.T) {
	cwd := loopReadWorkspace(t)
	for _, token := range []string{"help", "--help", "-h"} {
		t.Run(token, func(t *testing.T) {
			result := loopRun(t, cwd, token)
			if result.Code != 0 {
				t.Fatalf("code = %d, want 0", result.Code)
			}
			for _, want := range []string{"Usage:", "crw pabcd loop init --objective", "--session <id>", "--batch-json", "idempotencyKey"} {
				if !strings.Contains(result.Output, want) {
					t.Errorf("help output is missing %q", want)
				}
			}
		})
	}
}

// TestLoopInitRefusesAnEmptyObjective is runGoalplanCli's init branch: a blank --objective is refused
// before anything is written.
func TestLoopInitRefusesAnEmptyObjective(t *testing.T) {
	cwd := loopReadWorkspace(t)
	result := loopRun(t, cwd, "init", "--objective", "   ")
	if result.Code != 1 || result.Output != "loop init: --objective \"<text>\" is required" {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
		t.Fatalf("a refused init wrote state: %v", err)
	}
}

// TestLoopInitWritesPlanAndLedgerThenRendersIt ports goalplan-public-surface.test.ts's init cases and the
// corpus's cli__loop__init_show_validate_ready step 1: the derived slug, the seeded criterion and the
// created ledger row, rendered as the plan summary.
func TestLoopInitWritesPlanAndLedgerThenRendersIt(t *testing.T) {
	cwd := loopReadWorkspace(t)
	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature", "--criterion", "CSV export works")
	want := strings.Join([]string{
		"[crw loop: ship-the-export-feature]",
		"objective: Ship the export feature",
		"host: armed=false source=none",
		"workPhases: 0 (remaining 0)",
		"criteria: 1 (unmet 1)",
		"complete: false",
		"  - c-1 [open] CSV export works",
	}, "\n")
	if result.Code != 0 || result.Output != want {
		t.Fatalf("init:\n got %q\nwant %q", result.Output, want)
	}
	plan := goalplan.ReadGoalplan(cwd, "ship-the-export-feature")
	if plan == nil || plan.Objective != "Ship the export feature" || len(plan.Criteria) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	ledger, err := os.ReadFile(filepath.Join(cwd, ".crw", "goalplans", "ship-the-export-feature", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct{ Event, Detail string }
	if err := json.Unmarshal(ledger, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Event != "created" || entry.Detail != "init objective=\"Ship the export feature\" criteria=1" {
		t.Fatalf("ledger row = %+v", entry)
	}
	again := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	if again.Code != 1 || again.Output != "loop init: a plan already exists at slug 'ship-the-export-feature' (use show/validate)" {
		t.Fatalf("repeat init: %d %q", again.Code, again.Output)
	}
}

// TestLoopInitBindsTheSlugToTheSession ports the session branch: with --session the derived slug is
// written into that session's state, so a later show can resolve it without re-typing the slug.
func TestLoopInitBindsTheSlugToTheSession(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	loopSession(t, cwd, "rec-s1")
	if got := loopRun(t, cwd, "init", "--objective", "Bound objective", "--session", "rec-s1"); got.Code != 0 {
		t.Fatalf("init: %d %q", got.Code, got.Output)
	}
	if slug := state.ReadState(cwd, "rec-s1").Slug; slug != "bound-objective" {
		t.Fatalf("bound slug = %q", slug)
	}
	shown := loopRun(t, cwd, "show", "--session", "rec-s1")
	if shown.Code != 0 || !strings.HasPrefix(shown.Output, "[crw loop: bound-objective]\n") {
		t.Fatalf("show: %d %q", shown.Code, shown.Output)
	}
	if !strings.Contains(shown.Output, "writeLock: absent path="+filepath.Join(cwd, ".crw", "goalplans", "bound-objective", ".goalplan.lock")) {
		t.Fatalf("show has no writeLock line: %q", shown.Output)
	}
}

// TestLoopShowReadFailures ports the show branch's three failure shapes: a slug with no plan, no slug
// source at all, and a session that has no bound plan.
func TestLoopShowReadFailures(t *testing.T) {
	cwd := loopReadWorkspace(t)
	absent := loopRun(t, cwd, "show", "--slug", "no-such-plan")
	want := "loop show: no plan found at slug 'no-such-plan' (" + filepath.Join(cwd, ".crw", "goalplans", "no-such-plan", "goalplan.json") +
		" does not exist) - run `crw pabcd loop init --objective \"...\"`"
	if absent.Code != 1 || absent.Output != want {
		t.Fatalf("absent:\n got %q\nwant %q", absent.Output, want)
	}
	none := loopRun(t, cwd, "show")
	if none.Code != 1 || none.Output != "loop show: --slug \"<text>\", --objective \"<text>\", or --session <id> (with a bound plan) is required" {
		t.Fatalf("no source: %d %q", none.Code, none.Output)
	}
	loopSession(t, cwd, "rec-s2")
	unbound := loopRun(t, cwd, "show", "--session", "rec-s2")
	if unbound.Code != 1 || unbound.Output != none.Output {
		t.Fatalf("unbound session: %d %q", unbound.Code, unbound.Output)
	}
}

// loopReadyFixturePlan is the plan the oracle's ready cases use: a done prerequisite phase, a live phase
// whose tasks depend on a local task, and a phase blocked behind the live one.
func loopReadyFixturePlan(t *testing.T, cwd string) *goalplan.Goalplan {
	t.Helper()
	version := 1.0
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{
		Objective:     "ready fixture",
		SchemaVersion: &version,
		Criteria:      []goalplan.NewGoalplanCriterion{{Scenario: "contract is verified"}},
	})
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-base", Title: "base", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{
			{ID: "shared", Title: "base task", Status: goalplan.TaskDone, Outcome: "base shipped"},
		}, CriteriaIDs: []string{}},
		{ID: "wp-live", Title: "live", Status: goalplan.WorkPhaseInProgress, DependsOn: []string{"wp-base"}, Tasks: []goalplan.GoalplanTask{
			{ID: "shared", Title: "local prerequisite", Status: goalplan.TaskDone, Outcome: "local ready"},
			{ID: "ready-task", Title: "ready task", Status: goalplan.TaskPending, DependsOn: []string{"shared"}},
			{ID: "blocked-task", Title: "blocked task", Status: goalplan.TaskPending, DependsOn: []string{"later"}},
			{ID: "later", Title: "later task", Status: goalplan.TaskPending},
		}, CriteriaIDs: []string{"c-1"}},
		{ID: "wp-blocked", Title: "blocked", Status: goalplan.WorkPhasePending, DependsOn: []string{"wp-live"}, Tasks: []goalplan.GoalplanTask{
			{ID: "ready-task", Title: "same id elsewhere", Status: goalplan.TaskPending},
		}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// TestLoopReadyJSONFiltersDependencies ports goalplan-public-surface.test.ts's "ready json returns
// dependency-filtered arrays": the phases and tasks that are actually runnable, with their dependencies.
func TestLoopReadyJSONFiltersDependencies(t *testing.T) {
	cwd := loopReadWorkspace(t)
	loopReadyFixturePlan(t, cwd)
	result := loopRun(t, cwd, "ready", "--slug", "ready-fixture", "--json")
	if result.Code != 0 {
		t.Fatalf("ready: %d %q", result.Code, result.Output)
	}
	var body struct {
		Slug            string `json:"slug"`
		ReadyWorkPhases []struct {
			ID, Title, Status string
			DependsOn         []string
		} `json:"readyWorkPhases"`
		ReadyTasks []struct{ WorkPhaseID, ID, Title string } `json:"readyTasks"`
	}
	if err := json.Unmarshal([]byte(result.Output), &body); err != nil {
		t.Fatal(err)
	}
	if body.Slug != "ready-fixture" || len(body.ReadyWorkPhases) != 1 || body.ReadyWorkPhases[0].ID != "wp-live" ||
		body.ReadyWorkPhases[0].Status != "in_progress" || len(body.ReadyWorkPhases[0].DependsOn) != 1 || body.ReadyWorkPhases[0].DependsOn[0] != "wp-base" {
		t.Fatalf("readyWorkPhases = %+v", body.ReadyWorkPhases)
	}
	got := []string{}
	for _, task := range body.ReadyTasks {
		got = append(got, task.WorkPhaseID+"/"+task.ID)
	}
	if strings.Join(got, ",") != "wp-live/ready-task,wp-live/later" {
		t.Fatalf("readyTasks = %v", got)
	}
	if strings.Contains(result.Output, "openDecisions") {
		t.Fatalf("a plan with no decisions key printed one: %q", result.Output)
	}
}

// TestLoopReadyTextForm ports the human form of the same answer, including the "none" spellings.
func TestLoopReadyTextForm(t *testing.T) {
	cwd := loopReadWorkspace(t)
	loopReadyFixturePlan(t, cwd)
	result := loopRun(t, cwd, "ready", "--slug", "ready-fixture")
	want := strings.Join([]string{
		"[crw loop ready: ready-fixture]",
		"readyWorkPhases: wp-live (live)",
		"readyTasks: wp-live/ready-task (ready task); wp-live/later (later task)",
	}, "\n")
	if result.Code != 0 || result.Output != want {
		t.Fatalf("ready:\n got %q\nwant %q", result.Output, want)
	}
	if empty := loopRun(t, cwd, "init", "--objective", "nothing runnable"); empty.Code != 0 {
		t.Fatal(empty.Output)
	}
	none := loopRun(t, cwd, "ready", "--slug", "nothing-runnable")
	if want := "[crw loop ready: nothing-runnable]\nreadyWorkPhases: none\nreadyTasks: none"; none.Output != want {
		t.Fatalf("none form: %q", none.Output)
	}
}

// TestLoopReadyRejectsNonCanonicalSessionFirst ports goalplan-public-surface.test.ts's "ready rejects a
// non-canonical session before a sanitized collision can expose a plan": the refusal names no plan.
func TestLoopReadyRejectsNonCanonicalSessionFirst(t *testing.T) {
	cwd := loopReadWorkspace(t)
	plan := loopReadyFixturePlan(t, cwd)
	loopSession(t, cwd, "a-b")
	bound := state.ReadState(cwd, "a-b")
	bound.Slug = plan.Slug
	if err := state.WriteState(cwd, bound); err != nil {
		t.Fatal(err)
	}
	result := loopRun(t, cwd, "ready", "--session", "a/b", "--json")
	if result.Code != 1 || result.Output != "loop ready: session id is not canonical" {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	for _, leaked := range []string{plan.Slug, "wp-live", "ready-task"} {
		if strings.Contains(result.Output, leaked) {
			t.Fatalf("the refusal leaked %q: %q", leaked, result.Output)
		}
	}
}

// TestLoopReadyRejectsABrokenDependencyGraph ports the integrity-first rule: ready refuses a graph the
// plan itself rejects instead of answering from it.
func TestLoopReadyRejectsABrokenDependencyGraph(t *testing.T) {
	cwd := loopReadWorkspace(t)
	plan := loopReadyFixturePlan(t, cwd)
	plan.WorkPhases[1].AwaitsDecision = []string{"ghost"}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	result := loopRun(t, cwd, "ready", "--slug", plan.Slug)
	if result.Code != 1 || !strings.HasPrefix(result.Output, "loop ready: "+plan.Slug+" has an invalid dependency graph") {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
}

// TestLoopValidateReportsUnmetCriteriaAndPassesAFinishedPlan ports the validate branch: the E8 gate's
// reasons, then the OK line once every criterion carries evidence.
func TestLoopValidateReportsUnmetCriteriaAndPassesAFinishedPlan(t *testing.T) {
	cwd := loopReadWorkspace(t)
	loopRun(t, cwd, "init", "--objective", "Ship the export feature", "--criterion", "CSV export works")
	failed := loopRun(t, cwd, "validate", "--slug", "ship-the-export-feature")
	want := strings.Join([]string{
		"[crw loop validate: ship-the-export-feature] FAIL",
		"  - 1 unmet criterion/criteria: c-1",
	}, "\n")
	if failed.Code != 1 || failed.Output != want {
		t.Fatalf("validate:\n got %q\nwant %q", failed.Output, want)
	}
	plan := goalplan.ReadGoalplan(cwd, "ship-the-export-feature")
	plan.Criteria[0].Status = goalplan.CriterionMet
	evidence := "node --test: 12 pass"
	plan.Criteria[0].CapturedEvidence = &evidence
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	passed := loopRun(t, cwd, "validate", "--slug", "ship-the-export-feature")
	if passed.Code != 0 || passed.Output != "[crw loop validate: ship-the-export-feature] OK \u2014 complete + all met criteria carry evidence" {
		t.Fatalf("validate ok: %d %q", passed.Code, passed.Output)
	}
}

// TestLoopInitRefusesABoundCycleInANonGitWorkspace ports nongit-bound-cycle.test.ts case 1: the bound
// cycle is refused at init, with both exits named, and nothing is written.
func TestLoopInitRefusesABoundCycleInANonGitWorkspace(t *testing.T) {
	cwd := loopReadWorkspace(t)
	loopSession(t, cwd, "019a0000-0000-7000-8000-000000000133")
	result := loopRun(t, cwd, "init", "--objective", "bound probe in a non-git tree",
		"--session", "019a0000-0000-7000-8000-000000000133", "--criterion", "probe closes a cycle")
	if result.Code != 1 {
		t.Fatalf("code = %d, want 1 (%q)", result.Code, result.Output)
	}
	for _, want := range []string{"no resolvable git source identity", "crw relay session source", "unbound", "Nothing was written"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("refusal is missing %q: %q", want, result.Output)
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "goalplans")); !os.IsNotExist(err) {
		t.Fatalf("the trap was built: %v", err)
	}
}

// TestLoopInitComposesWithABoundSourceInASplitCwd ports split-cwd-cycle.test.ts step 2: the same bound
// init is refused when the source identity is unavailable and succeeds once the binding makes it
// resolvable, so the source gate and the binding compose.
func TestLoopInitComposesWithABoundSourceInASplitCwd(t *testing.T) {
	root := loopReadWorkspace(t)
	cwd, sourceRoot := filepath.Join(root, "fsm"), filepath.Join(root, "fsm", "src")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, sourceRoot)
	const id = "019a0000-0000-7000-8000-000000000109"
	loopSession(t, cwd, id)
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--git-dir").CombinedOutput(); err == nil {
		t.Fatalf("the fixture's cwd is inside a repository, so the case proves nothing: %q", out)
	}
	refused := loopRun(t, cwd, "init", "--objective", "split cwd probe objective", "--session", id)
	if refused.Code != 1 || !strings.Contains(refused.Output, "no resolvable git source identity") {
		t.Fatalf("unbound: %d %q", refused.Code, refused.Output)
	}
	if _, err := session.Bind(cwd, id, sourceRoot); err != nil {
		t.Fatal(err)
	}
	bound := loopRun(t, cwd, "init", "--objective", "split cwd probe objective", "--session", id, "--criterion", "the probe closes a cycle")
	if bound.Code != 0 {
		t.Fatalf("bound: %d %q", bound.Code, bound.Output)
	}
	if slug := state.ReadState(cwd, id).Slug; slug != "split-cwd-probe-objective" {
		t.Fatalf("bound slug = %q", slug)
	}
}

// gitInit makes dir a repository with one commit, the fixture split-cwd-cycle.test.ts builds.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %q %v", args, out, err)
		}
	}
}

// TestLoopInitRefusesToReplaceAnUnreadablePlanFile is the data-loss fix recorded in
// docs/port-cxc/known-defects/CRW-646.md: the oracle reads a damaged plan file as an absent one and
// its rename then replaces the bytes, so the port refuses instead and leaves the file as it is.
func TestLoopInitRefusesToReplaceAnUnreadablePlanFile(t *testing.T) {
	cwd := loopReadWorkspace(t)
	dir := filepath.Join(cwd, ".crw", "goalplans", "ship-the-export-feature")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(dir, "goalplan.json")
	broken := "{\"objective\": \"Ship the export feature\""
	if err := os.WriteFile(damaged, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	wantPrefix := "loop init: a plan file for slug 'ship-the-export-feature' already exists but could not be read (invalid-json)"
	if result.Code != 1 || !strings.HasPrefix(result.Output, wantPrefix) || !strings.HasSuffix(result.Output, "Nothing was written.") {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	if got, err := os.ReadFile(damaged); err != nil || string(got) != broken {
		t.Fatalf("the damaged plan was replaced: %q %v", got, err)
	}
}

// TestLoopInitRefusesToReplaceUnreadableSessionState is the other data-loss fix: ReadState answers a
// fresh IDLE state for a file it cannot decode, so binding a slug through it would replace it.
func TestLoopInitRefusesToReplaceUnreadableSessionState(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	if err := state.WriteState(cwd, state.DefaultState("rec-s1", "")); err != nil {
		t.Fatal(err)
	}
	path := state.StatePath(cwd, "rec-s1")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := loopRun(t, cwd, "init", "--objective", "Bound objective", "--session", "rec-s1")
	wantPrefix := "loop init: session rec-s1 has unreadable state; refusing to overwrite it"
	if result.Code != 1 || !strings.HasPrefix(result.Output, wantPrefix) {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "not json" {
		t.Fatalf("the damaged state was replaced: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "goalplans")); !os.IsNotExist(err) {
		t.Fatalf("a refused init wrote a plan: %v", err)
	}
}

// TestLoopSessionVerbsRefuseANonCanonicalSession is the security fix: the oracle guards the ready verb
// alone, so show and validate would resolve a non-canonical id to another session state file.
func TestLoopSessionVerbsRefuseANonCanonicalSession(t *testing.T) {
	cwd := loopReadWorkspace(t)
	plan := loopReadyFixturePlan(t, cwd)
	loopSession(t, cwd, "a-b")
	bound := state.ReadState(cwd, "a-b")
	bound.Slug = plan.Slug
	if err := state.WriteState(cwd, bound); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"show", "validate", "ready"} {
		t.Run(verb, func(t *testing.T) {
			result := loopRun(t, cwd, verb, "--session", "a/b")
			if result.Code != 1 || result.Output != "loop "+verb+": session id is not canonical" {
				t.Fatalf("got %d %q", result.Code, result.Output)
			}
			if strings.Contains(result.Output, plan.Slug) {
				t.Fatalf("the refusal leaked the plan slug: %q", result.Output)
			}
		})
	}
}

// TestLoopReadyJSONKeepsJSONStringifyBytes covers the escaping difference a review found: json.Marshal
// would print the escape forms of < and of U+2028 where the oracle prints the characters.
func TestLoopReadyJSONKeepsJSONStringifyBytes(t *testing.T) {
	cwd := loopReadWorkspace(t)
	plan := loopReadyFixturePlan(t, cwd)
	plan.WorkPhases[1].Title = "<build> & ship end"
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	result := loopRun(t, cwd, "ready", "--slug", plan.Slug, "--json")
	if result.Code != 0 {
		t.Fatalf("ready: %d %q", result.Code, result.Output)
	}
	for _, want := range []string{"<build> & ship", " "} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("ready --json escaped %q: %q", want, result.Output)
		}
	}
	for _, escaped := range []string{"\\u003c", "\\u2028"} {
		if strings.Contains(result.Output, escaped) {
			t.Fatalf("ready --json used Go escaping %q: %q", escaped, result.Output)
		}
	}
}
