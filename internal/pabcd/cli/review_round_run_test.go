package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// B-class review-binding.test.ts:35-83,114-163,208-233 and review-deadlock.test.ts:41-49 (openRoundFor): the part of each case that
// calls runReviewRoundCli. The cases that need the SubagentStop observer or the orchestrate CLI belong to the issues that port those.
const (
	reviewRoundRunUnit = "devlog/_plan/260815_probe"
	reviewRoundRunDoc  = reviewRoundRunUnit + "/000_plan.md"
	reviewRoundRunSlug = "review-binding-probe"
)

func reviewRoundRunMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func reviewRoundRunPut(t *testing.T, path, content string) {
	t.Helper()
	reviewRoundRunMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
	reviewRoundRunMust(t, os.WriteFile(path, []byte(content), 0o644))
}

// reviewRoundRunSeed is seedAtA of review-binding.test.ts: a plan unit with one document, a goalplan with one in-progress
// work-phase and a session "rb" at A bound to the unit. HOME, CODEX_HOME and CRW_HOME point into temporary directories.
func reviewRoundRunSeed(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, t.TempDir())
	}
	cwd := t.TempDir()
	reviewRoundRunPut(t, filepath.Join(cwd, reviewRoundRunDoc), "# probe\n")
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "review binding"})
	plan.Slug, plan.ActiveWorkPhaseID = reviewRoundRunSlug, new("wp1")
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "probe", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	reviewRoundRunMust(t, goalplan.WriteGoalplan(cwd, plan))
	st := state.DefaultState("rb", reviewRoundRunSlug)
	st.Phase, st.PlanUnit, st.PlanEpoch = state.PhaseA, new(reviewRoundRunUnit), new("e-probe-1")
	reviewRoundRunMust(t, state.WriteState(cwd, st))
	return cwd
}

func reviewRoundRunTry(cwd string, o *ReviewRoundRunOptions, argv ...string) (ReviewRoundCliResult, error) {
	return RunReviewRoundCli(*ParseReviewRoundCliArgs(argv, cwd).Args, o)
}

func reviewRoundRunDo(t *testing.T, cwd string, argv ...string) ReviewRoundCliResult {
	t.Helper()
	res, err := reviewRoundRunTry(cwd, nil, argv...)
	reviewRoundRunMust(t, err)
	return res
}

func reviewRoundRunOpenDoc(t *testing.T, cwd string) ReviewRoundCliResult {
	t.Helper()
	return reviewRoundRunDo(t, cwd, "open", "--session", "rb", "--plan-path", reviewRoundRunDoc)
}

func reviewRoundRunPlan(t *testing.T, cwd string) *goalplan.Goalplan {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, reviewRoundRunSlug)
	if plan == nil {
		t.Fatal("the goalplan could not be read")
	}
	return plan
}

func reviewRoundRunRound(t *testing.T, cwd string) goalplan.ReviewRoundState {
	t.Helper()
	round := review.LatestRound(reviewRoundRunPlan(t, cwd), goalplan.PurposePlanAudit)
	if round == nil {
		t.Fatal("no plan_audit round")
	}
	return *round
}

func reviewRoundRunBytes(t *testing.T, cwd string) string {
	t.Helper()
	dir, err := goalplan.GoalplanDir(cwd, reviewRoundRunSlug)
	reviewRoundRunMust(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, goalplan.GoalplanFile))
	reviewRoundRunMust(t, err)
	return string(raw)
}

func reviewRoundRunSetState(t *testing.T, cwd string, mutate func(*state.State)) {
	t.Helper()
	st := state.ReadState(cwd, "rb")
	mutate(&st)
	reviewRoundRunMust(t, state.WriteState(cwd, st))
}

func TestReviewRoundRunRefusals(t *testing.T) {
	const tail = " — enter A through `crw pabcd orchestrate A` so P>A records the unit it validated"
	cwd := reviewRoundRunSeed(t)
	reviewRoundRunPut(t, filepath.Join(cwd, "package.json"), "{}\n")
	before := reviewRoundRunBytes(t, cwd)
	rawState, err := os.ReadFile(state.StatePath(cwd, "rb"))
	reviewRoundRunMust(t, err)
	for _, c := range []struct {
		name   string
		mutate func(*state.State)
		argv   []string
		want   ReviewRoundCliResult
	}{
		{"no session", nil, []string{"open"}, ReviewRoundCliResult{1, "review-round: --session <id> is required"}},
		{"blank session", nil, []string{"show", "--session", " \u00a0\ufeff "}, ReviewRoundCliResult{1, "review-round: --session <id> is required"}},
		{"not at A", func(s *state.State) { s.Phase = state.PhaseP }, []string{"open", "--session", "rb"}, ReviewRoundCliResult{1, "review-round open: session is at P, not A — a plan audit is opened during Audit"}},
		{"no goalplan", func(s *state.State) { s.Slug = "" }, []string{"open", "--session", "rb"}, ReviewRoundCliResult{1, "review-round open: this session has no bound goalplan"}},
		{"no unit", func(s *state.State) { s.PlanUnit = nil }, []string{"open", "--session", "rb"}, ReviewRoundCliResult{1, "review-round open: no plan binding on this session" + tail}},
		{"empty epoch", func(s *state.State) { s.PlanEpoch = new("") }, []string{"open", "--session", "rb"}, ReviewRoundCliResult{1, "review-round open: no plan binding on this session" + tail}},
		{"no plan path", nil, []string{"open", "--session", "rb"}, ReviewRoundCliResult{1, "review-round open: --plan-path is required at least once: a round with no files audits nothing"}},
		{"foreign path", nil, []string{"open", "--session", "rb", "--plan-path", "package.json"}, ReviewRoundCliResult{1, "review-round open: plan path package.json is outside the bound plan unit " + reviewRoundRunUnit}},
		{"abort without goalplan", func(s *state.State) { s.Slug = "" }, []string{"abort", "--session", "rb"}, ReviewRoundCliResult{1, "review-round abort: this session has no bound goalplan"}},
		{"show without goalplan", func(s *state.State) { s.Slug = "" }, []string{"show", "--session", "rb"}, ReviewRoundCliResult{1, "review-round show: this session has no bound goalplan"}},
		{"abort with no round", nil, []string{"abort", "--session", "rb"}, ReviewRoundCliResult{1, "review-round abort: no open plan_audit round"}},
		{"show with no round", nil, []string{"show", "--session", "rb"}, ReviewRoundCliResult{0, "review-round show: no plan_audit round yet"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.mutate != nil {
				reviewRoundRunSetState(t, cwd, c.mutate)
				defer func() { reviewRoundRunMust(t, os.WriteFile(state.StatePath(cwd, "rb"), rawState, 0o600)) }()
			}
			if res := reviewRoundRunDo(t, cwd, c.argv...); res != c.want {
				t.Errorf("got %+v, want %+v", res, c.want)
			}
			if reviewRoundRunBytes(t, cwd) != before {
				t.Error("a refusal changed the goalplan")
			}
		})
	}
	if help := reviewRoundRunDo(t, cwd, "help"); help.Code != 0 || help.Output != RenderReviewRoundHelp() {
		t.Errorf("help: %+v", help)
	}
}

func TestReviewRoundRunOpenRecordsWhatItWasOpenedAgainst(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	stateBefore, err := os.ReadFile(state.StatePath(cwd, "rb"))
	reviewRoundRunMust(t, err)
	opened := reviewRoundRunOpenDoc(t, cwd)
	if opened.Code != 0 {
		t.Fatalf("open: %+v", opened)
	}
	round := reviewRoundRunRound(t, cwd)
	lines := strings.Split(opened.Output, "\n")
	if lines[0] != round.Lane.LaunchID || lines[1] != "" || lines[2] != "Round r1 is in flight over 1 file(s)." ||
		!strings.Contains(opened.Output, "CRW-ROLE: reviewer") || strings.Contains(opened.Output, "CXC") || !strings.HasSuffix(opened.Output, "There is no way to write it here.") {
		t.Errorf("packet: %q", opened.Output)
	}
	if round.Status != goalplan.ReviewInFlight || round.OwnerSessionID != "rb" || round.WorkPhaseID != "wp1" || round.PlanEpoch != "e-probe-1" ||
		round.PlanUnit != reviewRoundRunUnit || round.PlanPath != reviewRoundRunUnit || len(round.PlanFiles) != 1 || round.PlanFiles[0].Path != reviewRoundRunDoc ||
		round.PlanSha256 != PlanFilesHash(round.PlanFiles) || round.Lane.WorkspaceRoot != nil {
		t.Errorf("round: %+v", round)
	}
	if stateAfter, err := os.ReadFile(state.StatePath(cwd, "rb")); err != nil || string(stateAfter) != string(stateBefore) {
		t.Error("the session state is only read")
	}
	// the session id is trimmed before the state is read and before it is recorded as the owner
	if res := reviewRoundRunDo(t, cwd, "open", "--session", " rb\t", "--plan-path", reviewRoundRunDoc); res.Code != 0 || reviewRoundRunRound(t, cwd).OwnerSessionID != "rb" {
		t.Errorf("padded session: %+v", res)
	}
}

func TestReviewRoundRunAbortNeverApproves(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"reason", []string{"--reason", "reviewer died"}, "aborted: reviewer died"},
		{"default reason", nil, "aborted: aborted by the agent"},
		{"empty reason", []string{"--reason", ""}, "aborted: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := reviewRoundRunSeed(t)
			reviewRoundRunOpenDoc(t, cwd)
			res := reviewRoundRunDo(t, cwd, append([]string{"abort", "--session", "rb"}, c.argv...)...)
			round := reviewRoundRunRound(t, cwd)
			if res != (ReviewRoundCliResult{Output: "review-round abort: r1 closed as inconclusive"}) || round.Status != goalplan.ReviewInconclusive ||
				round.Lane.Verdict != "" || round.Lane.ReviewerSession == nil || *round.Lane.ReviewerSession != c.want {
				t.Errorf("%+v %+v", res, round)
			}
			if again := reviewRoundRunDo(t, cwd, "abort", "--session", "rb"); again.Code != 1 {
				t.Errorf("a closed round cannot be aborted again: %+v", again)
			}
		})
	}
}

func TestReviewRoundRunFailsClosedWhenTheLockIsHeld(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	reviewRoundRunOpenDoc(t, cwd)
	dir, err := goalplan.GoalplanDir(cwd, reviewRoundRunSlug)
	reviewRoundRunMust(t, err)
	lock := filepath.Join(dir, goalplan.GoalplanLockDir)
	reviewRoundRunPut(t, filepath.Join(lock, goalplan.GoalplanLockOwnerFile), "{\"pid\":4242}\n")
	before := reviewRoundRunBytes(t, cwd)
	once := &ReviewRoundRunOptions{Lock: &goalplan.GoalplanWriteLockOptions{RetryDelaysMs: []int{}}}
	for _, argv := range [][]string{{"abort", "--session", "rb", "--reason", "reviewer died"}, {"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}} {
		res, err := reviewRoundRunTry(cwd, once, argv...)
		if err != nil || res.Code != 1 || !strings.HasPrefix(res.Output, "review-round "+argv[0]+": ") || !strings.Contains(res.Output, ".goalplan.lock") || !strings.HasSuffix(res.Output, "; retry") {
			t.Errorf("%s: %+v %v", argv[0], res, err)
		}
	}
	if reviewRoundRunBytes(t, cwd) != before || reviewRoundRunRound(t, cwd).Status != goalplan.ReviewInFlight {
		t.Error("a held lock must leave the goalplan alone")
	}
	reviewRoundRunMust(t, os.RemoveAll(lock))
	if res, err := reviewRoundRunTry(cwd, once, "abort", "--session", "rb"); err != nil || res.Code != 0 {
		t.Errorf("a released lock: %+v %v", res, err)
	}
}

// wp7 preservation: open and abort keep the tasks' dependsOn and outcome.
func TestReviewRoundRunKeepsTheRestOfThePlan(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	plan := reviewRoundRunPlan(t, cwd)
	plan.WorkPhases[0].Tasks = []goalplan.GoalplanTask{
		{ID: "t-1", Title: "first", Status: "done", DependsOn: []string{}, Outcome: "first task verified"},
		{ID: "t-2", Title: "second", Status: "done", DependsOn: []string{"t-1"}, Outcome: "second task verified"},
	}
	reviewRoundRunMust(t, goalplan.WriteGoalplan(cwd, plan))
	want := plan.WorkPhases[0].Tasks
	for _, argv := range [][]string{{"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}, {"abort", "--session", "rb", "--reason", "reviewer stopped"}} {
		if res := reviewRoundRunDo(t, cwd, argv...); res.Code != 0 {
			t.Fatalf("%v: %+v", argv, res)
		}
		if got := reviewRoundRunPlan(t, cwd).WorkPhases[0].Tasks; !reflect.DeepEqual(got, want) {
			t.Errorf("after %s: %+v, want %+v", argv[0], got, want)
		}
	}
}

func TestReviewRoundRunShow(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	launch := strings.Split(reviewRoundRunOpenDoc(t, cwd).Output, "\n")[0]
	asJSON := func() string { return reviewRoundRunDo(t, cwd, "show", "--session", "rb", "--json").Output }
	asText := func() string { return reviewRoundRunDo(t, cwd, "show", "--session", "rb").Output }
	wantJSON := func(status, staleness string) string {
		return `{"roundId":"r1","status":"` + status + `","staleness":"` + staleness + `","launchId":"` + launch + `","verdict":null,"workPhaseId":"wp1","planEpoch":"e-probe-1"}`
	}
	if got := asJSON(); got != wantJSON("in_flight", "open") {
		t.Errorf("json: %s", got)
	}
	if got, want := asText(), "review-round r1: status=in_flight staleness=open verdict=- launch="+launch; got != want {
		t.Errorf("text: %s", got)
	}
	reviewRoundRunDo(t, cwd, "abort", "--session", "rb")
	if got := asJSON(); got != wantJSON("inconclusive", "fresh") {
		t.Errorf("fresh: %s", got)
	}
	reviewRoundRunPut(t, filepath.Join(cwd, reviewRoundRunDoc), "# probe amended\n")
	if got := asJSON(); got != wantJSON("inconclusive", "stale") {
		t.Errorf("stale: %s", got)
	}
	reviewRoundRunMust(t, os.Remove(filepath.Join(cwd, reviewRoundRunDoc)))
	if got := asJSON(); got != wantJSON("inconclusive", "stale") {
		t.Errorf("missing file: %s", got)
	}
	plan := reviewRoundRunPlan(t, cwd)
	plan.ReviewRounds[0].Lane.Verdict, plan.ReviewRounds[0].PlanFiles = goalplan.VerdictFail, nil
	reviewRoundRunMust(t, goalplan.WriteGoalplan(cwd, plan))
	if got, want := asText(), "review-round r1: status=inconclusive staleness=open verdict=fail launch="+launch; got != want {
		t.Errorf("a pre-binding round is never rehashed: %s", got)
	}
}

func TestReviewRoundRunGoalplanProblems(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	reviewRoundRunSetState(t, cwd, func(s *state.State) { s.Slug = "gone" })
	for _, argv := range [][]string{{"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}, {"abort", "--session", "rb"}} {
		if res := reviewRoundRunDo(t, cwd, argv...); res != (ReviewRoundCliResult{Code: 1, Output: "review-round " + argv[0] + ": goalplan 'gone' does not exist; retry"}) {
			t.Errorf("%s: %+v", argv[0], res)
		}
	}
	if res := reviewRoundRunDo(t, cwd, "show", "--session", "rb"); res != (ReviewRoundCliResult{Code: 1, Output: "review-round show: the bound goalplan could not be read"}) {
		t.Errorf("show: %+v", res)
	}
	reviewRoundRunSetState(t, cwd, func(s *state.State) { s.Slug = "../escape" })
	for _, argv := range [][]string{{"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}, {"abort", "--session", "rb"}} {
		if res, err := reviewRoundRunTry(cwd, nil, argv...); err == nil {
			t.Errorf("%s with a slug that is no slug: %+v, want an error", argv[0], res)
		}
	}
	if res := reviewRoundRunDo(t, cwd, "show", "--session", "rb"); res.Code != 1 {
		t.Errorf("show: %+v", res)
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		reviewRoundRunSetState(t, cwd, func(s *state.State) { s.Slug = reviewRoundRunSlug })
		reviewRoundRunMust(t, os.Chmod(filepath.Join(cwd, reviewRoundRunDoc), 0))
		if res, err := reviewRoundRunTry(cwd, nil, "open", "--session", "rb", "--plan-path", reviewRoundRunDoc); err == nil || (res != ReviewRoundCliResult{}) {
			t.Errorf("an unreadable plan file is an error where the oracle throws: %+v %v", res, err)
		}
	}
}

func TestReviewRoundRunNeedsAnActiveWorkPhase(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	plan := reviewRoundRunPlan(t, cwd)
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	reviewRoundRunMust(t, goalplan.WriteGoalplan(cwd, plan))
	before := reviewRoundRunBytes(t, cwd)
	if res := reviewRoundRunOpenDoc(t, cwd); res != (ReviewRoundCliResult{Code: 1, Output: "review-round open: the bound goalplan has no active work-phase"}) || reviewRoundRunBytes(t, cwd) != before {
		t.Errorf("%+v", res)
	}
}

// The packet is rendered after the write: a home the CODEX_HOME probe cannot resolve fails the command with the round already in flight.
func TestReviewRoundRunPacketErrorComesAfterTheWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a removed working directory is not an error there")
	}
	cwd := reviewRoundRunSeed(t)
	gone := t.TempDir()
	t.Chdir(gone)
	reviewRoundRunMust(t, os.Remove(gone))
	env := func(key string) (string, bool) { return "", key == "CODEX_HOME" || key == "HOME" }
	if res, err := reviewRoundRunTry(cwd, &ReviewRoundRunOptions{Env: env}, "open", "--session", "rb", "--plan-path", reviewRoundRunDoc); err == nil || (res != ReviewRoundCliResult{}) {
		t.Errorf("want an error and no result: %+v %v", res, err)
	}
	if reviewRoundRunRound(t, cwd).Status != goalplan.ReviewInFlight {
		t.Error("the round must be open")
	}
}

// A stored plan whose cursor names a pending r1 of the same document while an r2 exists: OpenRound refreshes r1, and MarkLaunching
// answers that r1 was superseded. Consecutive CLI calls never store that shape; a hand edit or an older build can.
func TestReviewRoundRunRefusesAStaleReusedRound(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	plan := reviewRoundRunPlan(t, cwd)
	round := func(id string, status goalplan.ReviewRoundStatus) goalplan.ReviewRoundState {
		return goalplan.ReviewRoundState{RoundID: id, Purpose: goalplan.PurposePlanAudit, PlanPath: reviewRoundRunUnit, PlanSha256: "x", Status: status,
			Lane: goalplan.ReviewLane{LaunchID: id + "-20260101000000"}, OpenedAt: "2026-01-01T00:00:00.000Z"}
	}
	plan.ReviewRounds = []goalplan.ReviewRoundState{round("r1", goalplan.ReviewPending), round("r2", goalplan.ReviewInconclusive)}
	plan.ActivePlanAuditRoundID = new("r1")
	reviewRoundRunMust(t, goalplan.WriteGoalplan(cwd, plan))
	before := reviewRoundRunBytes(t, cwd)
	if res := reviewRoundRunOpenDoc(t, cwd); res != (ReviewRoundCliResult{Code: 1, Output: "review-round open: round r1 was superseded before this verdict arrived"}) || reviewRoundRunBytes(t, cwd) != before {
		t.Errorf("%+v", res)
	}
}

// A stored round that revival cannot read would be deleted by the write that follows; the oracle does it silently (a record data-loss
// defect, fixed here by refusing and leaving the file as it is). Show only reads, so it still answers.
func TestReviewRoundRunRefusesToDropAStoredRound(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	reviewRoundRunOpenDoc(t, cwd)
	dir, err := goalplan.GoalplanDir(cwd, reviewRoundRunSlug)
	reviewRoundRunMust(t, err)
	stored := map[string]any{}
	reviewRoundRunMust(t, json.Unmarshal([]byte(reviewRoundRunBytes(t, cwd)), &stored))
	stored["reviewRounds"] = append(stored["reviewRounds"].([]any), map[string]any{"roundId": "r0", "status": "bogus"})
	edited, err := json.Marshal(stored)
	reviewRoundRunMust(t, err)
	reviewRoundRunPut(t, filepath.Join(dir, goalplan.GoalplanFile), string(edited))
	for _, verb := range []string{"open", "abort"} {
		res := reviewRoundRunDo(t, cwd, verb, "--session", "rb", "--plan-path", reviewRoundRunDoc)
		if want := (ReviewRoundCliResult{Code: 1, Output: "review-round " + verb + ": the goalplan holds review rounds that this build cannot read; refusing to rewrite it"}); res != want {
			t.Errorf("%s: %+v", verb, res)
		}
		if reviewRoundRunBytes(t, cwd) != string(edited) {
			t.Errorf("%s changed the file", verb)
		}
	}
	if res := reviewRoundRunDo(t, cwd, "show", "--session", "rb"); res.Code != 0 || !strings.Contains(res.Output, "review-round r1: status=in_flight") {
		t.Errorf("show: %+v", res)
	}
}

// The stored rounds are counted under the reader's exact key: a differently cased key neither hides an unreadable round (the reader
// reads "reviewRounds" only) nor counts as one.
func TestReviewRoundRunCountsTheExactKey(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	dir, err := goalplan.GoalplanDir(cwd, reviewRoundRunSlug)
	reviewRoundRunMust(t, err)
	base := reviewRoundRunBytes(t, cwd)
	put := func(extra string) {
		reviewRoundRunPut(t, filepath.Join(dir, goalplan.GoalplanFile), strings.Replace(base, "\"schemaVersion\"", extra+", \"schemaVersion\"", 1))
	}
	hidden := "\"reviewRounds\": [{\"roundId\": \"r0\", \"status\": \"bogus\"}], \"ReviewRounds\": []"
	put(hidden)
	if res := reviewRoundRunDo(t, cwd, "abort", "--session", "rb"); res.Code != 1 || !strings.Contains(res.Output, "refusing to rewrite") {
		t.Errorf("a cased twin must not hide an unreadable round: %+v", res)
	}
	put("\"ReviewRounds\": [1]")
	if res := reviewRoundRunDo(t, cwd, "abort", "--session", "rb"); res.Code != 1 || strings.Contains(res.Output, "refusing") {
		t.Errorf("a key the reader ignores holds no round: %+v", res)
	}
}

// The count reads the file through a handle that follows no link and waits on no special file; whatever it cannot read is not intact.
func TestReviewRoundRunCountRefusesLinksAndSpecialFiles(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	plan := reviewRoundRunPlan(t, cwd)
	dir, err := goalplan.GoalplanDir(cwd, reviewRoundRunSlug)
	reviewRoundRunMust(t, err)
	file := filepath.Join(dir, goalplan.GoalplanFile)
	if !reviewRoundRunRoundsIntact(cwd, reviewRoundRunSlug, plan) {
		t.Fatal("a regular file with no rounds is intact")
	}
	reviewRoundRunMust(t, os.Rename(file, file+".real"))
	reviewRoundRunMust(t, os.Symlink(file+".real", file))
	if reviewRoundRunRoundsIntact(cwd, reviewRoundRunSlug, plan) {
		t.Error("a link was followed")
	}
	reviewRoundRunMust(t, os.Remove(file))
	reviewRoundRunMust(t, syscall.Mkfifo(file, 0o600))
	if reviewRoundRunRoundsIntact(cwd, reviewRoundRunSlug, plan) {
		t.Error("a FIFO was read")
	}
}

// Two stored entries share the round id r1; the binding lands on the entry selected by purpose and launch id and on no other
// (review-round-cli.ts:246 matches by roundId alone and rewrites both).
func TestReviewRoundRunBindsOnlyTheSelectedRound(t *testing.T) {
	entry := func(purpose goalplan.ReviewPurpose, launch string) goalplan.ReviewRoundState {
		return goalplan.ReviewRoundState{RoundID: "r1", Purpose: purpose, PlanPath: "p", PlanSha256: "x", Status: goalplan.ReviewApproved, OwnerSessionID: "other",
			WorkPhaseID: "wp9", PlanFiles: []goalplan.PlanFileHash{{Path: "old", Sha256: "y"}}, Lane: goalplan.ReviewLane{LaunchID: launch}}
	}
	plan := &goalplan.Goalplan{ReviewRounds: []goalplan.ReviewRoundState{
		entry(goalplan.PurposeFinalGate, "r1-A"), entry(goalplan.PurposePlanAudit, "r1-B"), entry(goalplan.PurposePlanAudit, "r1-C")}}
	before := slices.Clone(plan.ReviewRounds)
	files := []goalplan.PlanFileHash{{Path: reviewRoundRunDoc, Sha256: "z"}}
	bound, ok := reviewRoundRunBound(plan, "r1-C", "rb", "wp1", reviewRoundRunUnit, "e1", files)
	if !ok || !reflect.DeepEqual(plan.ReviewRounds, before) || !reflect.DeepEqual(bound.ReviewRounds[:2], before[:2]) {
		t.Fatalf("only the selected entry may change, and not in the plan given: %+v", bound)
	}
	if got := bound.ReviewRounds[2]; got.OwnerSessionID != "rb" || got.WorkPhaseID != "wp1" || got.PlanUnit != reviewRoundRunUnit || got.PlanEpoch != "e1" || !reflect.DeepEqual(got.PlanFiles, files) || got.Lane.LaunchID != "r1-C" {
		t.Errorf("selected entry: %+v", got)
	}
	if _, ok := reviewRoundRunBound(plan, "r1-A", "rb", "wp1", reviewRoundRunUnit, "e1", files); ok {
		t.Error("a final_gate launch is no plan_audit round")
	}
}

// effectiveActiveWorkPhaseId (goalplan.ts:2249-2267).
func TestReviewRoundRunActiveWorkPhase(t *testing.T) {
	phase := func(id string, status goalplan.WorkPhaseStatus, deps ...string) goalplan.GoalplanWorkPhase {
		return goalplan.GoalplanWorkPhase{ID: id, Status: status, DependsOn: deps, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}
	}
	type phases = []goalplan.GoalplanWorkPhase
	for _, c := range []struct {
		name   string
		cursor *string
		phases phases
		want   string
	}{
		{"a runnable cursor wins", new("b"), phases{phase("a", "in_progress"), phase("b", "pending")}, "b"},
		{"a done cursor falls through", new("a"), phases{phase("a", "done"), phase("b", "pending")}, "b"},
		{"a blocked cursor falls through", new("a"), phases{phase("a", "blocked"), phase("b", "pending")}, "b"},
		{"a cursor with an unmet dependency falls through", new("b"), phases{phase("a", "pending"), phase("b", "in_progress", "a")}, "a"},
		{"a ghost cursor falls through", new("zz"), phases{phase("a", "pending")}, "a"},
		{"an empty cursor is no cursor", new(""), phases{phase("a", "pending"), phase("b", "in_progress")}, "b"},
		{"in progress before pending", nil, phases{phase("a", "pending"), phase("b", "in_progress")}, "b"},
		{"the first pending", nil, phases{phase("a", "done"), phase("b", "pending"), phase("c", "pending")}, "b"},
		{"nothing open", nil, phases{phase("a", "done"), phase("b", "blocked")}, ""},
	} {
		if got := reviewRoundRunActiveWorkPhase(&goalplan.Goalplan{ActiveWorkPhaseID: c.cursor, WorkPhases: c.phases}); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// reviewRoundRunOracle is testdata/review_round_run/oracle.json: what the Node build of CXC v0.2.40 (3c1459ac) answered. Files are
// the workspace as the oracle's own functions wrote it (under its state directory), steps are CLI runs or file writes between them.
type reviewRoundRunOracle struct {
	Cases []struct {
		ID    string
		Files map[string]string
		Steps []struct {
			Argv   []string
			Write  map[string]string
			Remove []string
			Want   *struct {
				Code   int
				Output string
			}
		}
		After json.RawMessage
	}
}

// reviewRoundRunChanged lists the recorded cases the port answers differently on purpose: the refusal the first CLI step gets, with the
// file left as it was seeded (the oracle drops the unreadable round and goes on).
var reviewRoundRunChanged = map[string]string{
	"open_drops_an_unreadable_round":  "review-round open: the goalplan holds review rounds that this build cannot read; refusing to rewrite it",
	"abort_drops_an_unreadable_round": "review-round abort: the goalplan holds review rounds that this build cannot read; refusing to rewrite it",
}

var (
	reviewRoundRunStamp = regexp.MustCompile(`r(\d+)-\d{14}`)
	reviewRoundRunTime  = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z`)
)

// reviewRoundRunMask hides what the clock makes: launch stamps and every timestamp except the pinned one the seeds carry, so a seeded
// createdAt or openedAt that a write altered still shows.
func reviewRoundRunMask(s string) string {
	return reviewRoundRunTime.ReplaceAllStringFunc(reviewRoundRunStamp.ReplaceAllString(s, "r$1-<STAMP>"), func(ts string) string {
		if ts == "2026-01-01T00:00:00.000Z" {
			return ts
		}
		return "<TS>"
	})
}

func TestReviewRoundRunOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/review_round_run/oracle.json")
	reviewRoundRunMust(t, err)
	var o reviewRoundRunOracle
	reviewRoundRunMust(t, json.Unmarshal(raw, &o))
	if len(o.Cases) < 25 {
		t.Fatalf("recorded cases missing: %d", len(o.Cases))
	}
	rename := strings.NewReplacer("CXC-ROLE:", "CRW-ROLE:", "cxc orchestrate A", "crw pabcd orchestrate A", "cxc review-round", "crw pabcd review-round")
	for _, c := range o.Cases {
		t.Run(c.ID, func(t *testing.T) {
			cwd := reviewRoundRunSeed(t)
			reviewRoundRunMust(t, os.RemoveAll(filepath.Join(cwd, ".crw")))
			put := func(files map[string]string) {
				for name, content := range files {
					reviewRoundRunPut(t, filepath.Join(cwd, strings.Replace(name, ".codexclaw/", ".crw/", 1)), content)
				}
			}
			put(c.Files)
			for i, step := range c.Steps {
				put(step.Write)
				for _, rel := range step.Remove {
					reviewRoundRunMust(t, os.Remove(filepath.Join(cwd, rel)))
				}
				if step.Want == nil {
					continue
				}
				res, err := reviewRoundRunTry(cwd, nil, step.Argv...)
				if refusal, changed := reviewRoundRunChanged[c.ID]; changed {
					seeded := c.Files[".codexclaw/goalplans/"+reviewRoundRunSlug+"/goalplan.json"]
					if err != nil || res != (ReviewRoundCliResult{Code: 1, Output: refusal}) || reviewRoundRunBytes(t, cwd) != seeded {
						t.Errorf("changed case: %+v %v", res, err)
					}
					return
				}
				if want := rename.Replace(strings.ReplaceAll(step.Want.Output, "${WS}", cwd)); err != nil || res.Code != step.Want.Code || reviewRoundRunMask(res.Output) != want {
					t.Errorf("step %d %v: %d %q %v, want %d %q", i, step.Argv, res.Code, res.Output, err, step.Want.Code, want)
				}
			}
			if string(c.After) == "null" {
				return
			}
			var want, got any
			reviewRoundRunMust(t, json.Unmarshal([]byte(reviewRoundRunMask(string(c.After))), &want))
			reviewRoundRunMust(t, json.Unmarshal([]byte(reviewRoundRunMask(reviewRoundRunBytes(t, cwd))), &got))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("goalplan after the steps:\n got %v\nwant %v", got, want)
			}
		})
	}
}
