package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Cases from CXC v0.2.40 review-binding.test.ts:96-122,230-262 and review-deadlock.test.ts:50-345 (3c1459ac), run through the
// registered leg subagent-stop-observing-review, the way the host runs it.

const (
	reviewObsUnit  = "devlog/_plan/260817_probe"
	reviewObsEpoch = "e-probe-1"
)

type reviewObsEnv struct {
	cwd, slug, session string
}

// reviewObsSeed is the oracle's seedAtA: a goalplan with one in-progress work-phase wp0 and a session at A bound to a plan unit.
func reviewObsSeed(t *testing.T, session string, tasks []goalplan.GoalplanTask) reviewObsEnv {
	t.Helper()
	cwd := subagentStopWorkspace(t)
	subagentStopPut(t, filepath.Join(cwd, reviewObsUnit, "000_plan.md"), "# probe\n")
	slug := "deadlock-probe"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "deadlock"})
	plan.Slug = slug
	if tasks == nil {
		tasks = []goalplan.GoalplanTask{}
	}
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp0", Title: "probe", Status: goalplan.WorkPhaseInProgress, Tasks: tasks, CriteriaIDs: []string{}}}
	active := "wp0"
	plan.ActiveWorkPhaseID = &active
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	s := state.DefaultState(session, slug)
	unit, epoch := reviewObsUnit, reviewObsEpoch
	s.Phase, s.PlanUnit, s.PlanEpoch = state.PhaseA, &unit, &epoch
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	return reviewObsEnv{cwd, slug, session}
}

// open runs `review-round open` and returns the launch id, the first line of its packet.
func (e reviewObsEnv) open(t *testing.T) string {
	t.Helper()
	parsed := cli.ParseReviewRoundCliArgs([]string{"open", "--session", e.session, "--cwd", e.cwd, "--plan-path", reviewObsUnit + "/000_plan.md"}, e.cwd)
	if parsed.Args == nil {
		t.Fatal(parsed.Error)
	}
	res, err := cli.RunReviewRoundCli(*parsed.Args, nil)
	if err != nil || res.Code != 0 {
		t.Fatalf("open: %v %d %s", err, res.Code, res.Output)
	}
	return strings.SplitN(res.Output, "\n", 2)[0]
}

func (e reviewObsEnv) round(t *testing.T) goalplan.ReviewRoundState {
	t.Helper()
	plan := goalplan.ReadGoalplan(e.cwd, e.slug)
	if plan == nil {
		t.Fatal("goalplan unreadable")
	}
	r := review.LatestRound(plan, goalplan.PurposePlanAudit)
	if r == nil {
		t.Fatal("no plan_audit round")
	}
	return *r
}

func (e reviewObsEnv) ledger(t *testing.T) string {
	t.Helper()
	dir, err := goalplan.GoalplanDir(e.cwd, e.slug)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, goalplan.GoalplanLedgerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(raw)
}

// stop delivers a SubagentStop to the leg. A nil agentType leaves the key out, the v1 spawn surface.
func (e reviewObsEnv) stop(t *testing.T, agentType *string, agentID, message string) string {
	t.Helper()
	p := map[string]any{"hook_event_name": "SubagentStop", "cwd": e.cwd, "session_id": e.session, "last_assistant_message": message}
	if agentType != nil {
		p["agent_type"] = *agentType
	}
	if agentID != "" {
		p["agent_id"] = agentID
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-observing-review"},
		bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	return out.String()
}

func reviewObsType(s string) *string { return &s }

func reviewObsSignoff(launch, verdict string) string {
	return fmt.Sprintf("reviewed\n\nLAUNCH: %s\nVERDICT: %s", launch, verdict)
}

func TestReviewObserverOnlyAClosingSignoffRecordsAVerdict(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	status := func() goalplan.ReviewRoundStatus { return e.round(t).Status }

	e.stop(t, reviewObsType("worker"), "a1", "LAUNCH: "+launch+"\nVERDICT: PASS")
	if status() != goalplan.ReviewInFlight {
		t.Fatal("a worker exit belongs to the receipt gate")
	}
	e.stop(t, reviewObsType("explorer"), "a1", "quoting LAUNCH: "+launch+" / VERDICT: PASS in the packet\n\nstill working")
	if status() != goalplan.ReviewInFlight {
		t.Fatal("a mid-message mention is not a sign-off")
	}
	e.stop(t, reviewObsType("explorer"), "a1", "LAUNCH: wrong-id\nVERDICT: PASS")
	if status() != goalplan.ReviewInFlight {
		t.Fatal("a foreign launch id is ignored")
	}
	if !strings.Contains(e.ledger(t), "belongs to no plan_audit round") {
		t.Fatalf("a sign-off for an unknown launch is recorded as ignored: %q", e.ledger(t))
	}
	if out := e.stop(t, reviewObsType("explorer"), "a1", "looks fine\n\nLAUNCH: "+launch+"\nVERDICT: PASS"); out != "" {
		t.Fatalf("the observer answers nothing: %q", out)
	}
	if r := e.round(t); r.Status != goalplan.ReviewApproved || r.Lane.Verdict != goalplan.VerdictPass {
		t.Fatalf("round %+v", r)
	}
}

func TestReviewObserverRoles(t *testing.T) {
	cases := []struct {
		name      string
		agentType *string
		approved  bool
	}{
		{"no agent_type (v1 surface)", nil, true},
		{"default role", reviewObsType("default"), true},
		{"explorer", reviewObsType("explorer"), true},
		{"reviewer", reviewObsType("reviewer"), true},
		{"empty", reviewObsType(""), true},
		{"worker", reviewObsType("worker"), false},
		{"executor", reviewObsType("executor"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := reviewObsSeed(t, "roles", nil)
			launch := e.open(t)
			e.stop(t, c.agentType, "d1", reviewObsSignoff(launch, "PASS"))
			got := e.round(t).Status == goalplan.ReviewApproved
			if got != c.approved {
				t.Fatalf("approved=%v, want %v", got, c.approved)
			}
		})
	}
}

func TestReviewObserverSecondChildCannotOverwriteTheVerdict(t *testing.T) {
	e := reviewObsSeed(t, "two", nil)
	launch := e.open(t)
	e.stop(t, nil, "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	if r := e.round(t); r.Lane.Verdict != goalplan.VerdictFail || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("round %+v", r)
	}
	e.stop(t, nil, "bystander", reviewObsSignoff(launch, "PASS"))
	r := e.round(t)
	if r.Lane.Verdict != goalplan.VerdictFail || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("a bystander flipped a recorded verdict: %+v", r)
	}
	if !strings.Contains(e.ledger(t), "already signed by reviewer-1") {
		t.Fatal(e.ledger(t))
	}
}

func TestReviewObserverIgnoredSignoffsSayWhy(t *testing.T) {
	t.Run("re-planned", func(t *testing.T) {
		e := reviewObsSeed(t, "late", nil)
		launch := e.open(t)
		s, _ := state.ReadStateStrict(e.cwd, e.session)
		epoch := "e-probe-2"
		s.PlanEpoch = &epoch
		if err := state.WriteState(e.cwd, s); err != nil {
			t.Fatal(err)
		}
		e.stop(t, reviewObsType("explorer"), "e1", reviewObsSignoff(launch, "PASS"))
		ledger := e.ledger(t)
		if !strings.Contains(ledger, "review_signoff_ignored") || !strings.Contains(ledger, "re-planned") {
			t.Fatal(ledger)
		}
		if e.round(t).Status != goalplan.ReviewInFlight {
			t.Fatal("the round must stay in flight")
		}
	})
	t.Run("session left A", func(t *testing.T) {
		e := reviewObsSeed(t, "left", nil)
		launch := e.open(t)
		s, _ := state.ReadStateStrict(e.cwd, e.session)
		s.Phase = state.PhaseB
		if err := state.WriteState(e.cwd, s); err != nil {
			t.Fatal(err)
		}
		e.stop(t, nil, "e1", reviewObsSignoff(launch, "PASS"))
		if !strings.Contains(e.ledger(t), "the session left A before the reviewer finished") || e.round(t).Status != goalplan.ReviewInFlight {
			t.Fatal(e.ledger(t))
		}
	})
	t.Run("another session", func(t *testing.T) {
		e := reviewObsSeed(t, "own", nil)
		launch := e.open(t)
		plan := goalplan.ReadGoalplan(e.cwd, e.slug)
		plan.ReviewRounds[len(plan.ReviewRounds)-1].OwnerSessionID = "someone-else"
		if err := goalplan.WriteGoalplan(e.cwd, plan); err != nil {
			t.Fatal(err)
		}
		e.stop(t, nil, "e1", reviewObsSignoff(launch, "PASS"))
		if !strings.Contains(e.ledger(t), "the round belongs to another session") || e.round(t).Status != goalplan.ReviewInFlight {
			t.Fatal(e.ledger(t))
		}
	})
	t.Run("work-phase moved on", func(t *testing.T) {
		e := reviewObsSeed(t, "wp", nil)
		launch := e.open(t)
		plan := goalplan.ReadGoalplan(e.cwd, e.slug)
		plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
		plan.WorkPhases = append(plan.WorkPhases, goalplan.GoalplanWorkPhase{ID: "wp1", Title: "next", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		next := "wp1"
		plan.ActiveWorkPhaseID = &next
		if err := goalplan.WriteGoalplan(e.cwd, plan); err != nil {
			t.Fatal(err)
		}
		e.stop(t, nil, "e1", reviewObsSignoff(launch, "PASS"))
		if !strings.Contains(e.ledger(t), "the round audited work-phase wp0, but wp1 is active") || e.round(t).Status != goalplan.ReviewInFlight {
			t.Fatal(e.ledger(t))
		}
	})
	t.Run("no usable sign-off while a plan_audit round is in flight", func(t *testing.T) {
		e := reviewObsSeed(t, "noparse", nil)
		e.open(t)
		e.stop(t, reviewObsType("default"), "n1", "I reviewed the plan and it looks fine to me.")
		if !strings.Contains(e.ledger(t), "review_signoff_unparsed") || e.round(t).Status != goalplan.ReviewInFlight {
			t.Fatal(e.ledger(t))
		}
	})
}

func TestReviewObserverUnparsedExitIsSilentWithoutAWaitingRound(t *testing.T) {
	e := reviewObsSeed(t, "quiet", nil)
	e.stop(t, nil, "n1", "I reviewed the plan and it looks fine to me.")
	if got := e.ledger(t); got != "" {
		t.Fatalf("no round was in flight, yet the ledger has %q", got)
	}
	// Outside A the same exit is silent too.
	launch := e.open(t)
	s, _ := state.ReadStateStrict(e.cwd, e.session)
	s.Phase = state.PhaseB
	if err := state.WriteState(e.cwd, s); err != nil {
		t.Fatal(err)
	}
	before := e.ledger(t)
	e.stop(t, nil, "n1", "no sign-off")
	if e.ledger(t) != before {
		t.Fatalf("outside A the unparsed exit wrote %q", e.ledger(t))
	}
	_ = launch
}

func TestReviewObserverFailsOpenWhenTheLockIsHeld(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	dir, err := goalplan.GoalplanDir(e.cwd, e.slug)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, goalplan.GoalplanLockDir)
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	subagentStopPut(t, filepath.Join(lock, "owner.json"), `{"pid":4242}`+"\n")
	before, err := os.ReadFile(filepath.Join(dir, goalplan.GoalplanFile))
	if err != nil {
		t.Fatal(err)
	}
	if out := e.stop(t, reviewObsType("explorer"), "reviewer-1", "LAUNCH: "+launch+"\nVERDICT: PASS"); out != "" {
		t.Fatal(out)
	}
	after, _ := os.ReadFile(filepath.Join(dir, goalplan.GoalplanFile))
	if !bytes.Equal(before, after) {
		t.Fatal("a held lock must leave the plan untouched")
	}
	if r := e.round(t); r.Status != goalplan.ReviewInFlight || r.Lane.Verdict != "" {
		t.Fatalf("round %+v", r)
	}
}

func TestReviewObserverKeepsTaskDependenciesAndOutcomes(t *testing.T) {
	tasks := []goalplan.GoalplanTask{
		{ID: "t-1", Title: "first", Status: goalplan.TaskDone, DependsOn: []string{}, Outcome: "first task verified"},
		{ID: "t-2", Title: "second", Status: goalplan.TaskDone, DependsOn: []string{"t-1"}, Outcome: "second task verified"},
	}
	e := reviewObsSeed(t, "wp7", tasks)
	launch := e.open(t)
	if out := e.stop(t, reviewObsType("explorer"), "reviewer-wp7", reviewObsSignoff(launch, "PASS")); out != "" {
		t.Fatal(out)
	}
	r := e.round(t)
	if r.Status != goalplan.ReviewApproved || r.Lane.Verdict != goalplan.VerdictPass || *r.Lane.ReviewerSession != "reviewer-wp7" {
		t.Fatalf("round %+v", r)
	}
	saved := goalplan.ReadGoalplan(e.cwd, e.slug).WorkPhases[0].Tasks
	if len(saved) != 2 || saved[0].Outcome != "first task verified" || saved[1].Outcome != "second task verified" ||
		len(saved[1].DependsOn) != 1 || saved[1].DependsOn[0] != "t-1" {
		t.Fatalf("tasks %+v", saved)
	}
}

func TestReviewObserverNearPassAndFail(t *testing.T) {
	for _, c := range []struct {
		verdict string
		status  goalplan.ReviewRoundStatus
		want    goalplan.Verdict
	}{
		{"GO-WITH-FIXES", goalplan.ReviewApproved, goalplan.VerdictNearPass},
		{"FAIL", goalplan.ReviewChangesRequested, goalplan.VerdictFail},
	} {
		e := reviewObsSeed(t, "v", nil)
		launch := e.open(t)
		e.stop(t, nil, "r", reviewObsSignoff(launch, c.verdict))
		if r := e.round(t); r.Status != c.status || r.Lane.Verdict != c.want {
			t.Fatalf("%s: %+v", c.verdict, r)
		}
	}
}

func TestReviewObserverIsQuietOnUnusablePayloads(t *testing.T) {
	e := reviewObsSeed(t, "q", nil)
	launch := e.open(t)
	for _, raw := range []string{
		`{`, `[]`, `null`, `{"hook_event_name":"Stop","cwd":"` + e.cwd + `","session_id":"q"}`,
		`{"hook_event_name":"SubagentStop","session_id":"q","last_assistant_message":"LAUNCH: ` + launch + `\nVERDICT: PASS"}`,
		`{"hook_event_name":"SubagentStop","cwd":"` + e.cwd + `","last_assistant_message":"LAUNCH: ` + launch + `\nVERDICT: PASS"}`,
	} {
		var out, stderr bytes.Buffer
		code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-observing-review"},
			strings.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
		if code != 0 || out.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("%q: exit=%d out=%q err=%q", raw, code, &out, &stderr)
		}
	}
	if e.round(t).Status != goalplan.ReviewInFlight {
		t.Fatal("an unusable payload recorded a verdict")
	}
}

func TestReviewObserverBaselineEmptyWorkspace(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	e := reviewObsEnv{cwd: cwd, session: "rec-s1"}
	if out := e.stop(t, reviewObsType("explorer"), "rec-a1", "Done."); out != "" {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
		t.Fatalf("unexpected state: %v", err)
	}
}

// A stored round that revival cannot read would be deleted by the write that follows. The write lock refuses such a plan, so the
// observer leaves the file as it is and records nothing (the oracle drops the round silently; known-defects CRW-564).
func TestReviewObserverLeavesAPlanWithAnUnreadableRoundAlone(t *testing.T) {
	e := reviewObsSeed(t, "loss", nil)
	launch := e.open(t)
	dir, err := goalplan.GoalplanDir(e.cwd, e.slug)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, goalplan.GoalplanFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]any{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	stored["reviewRounds"] = append(stored["reviewRounds"].([]any), map[string]any{"roundId": "r0", "status": "bogus"})
	edited, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	subagentStopPut(t, path, string(edited))
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
	if after, _ := os.ReadFile(path); string(after) != string(edited) {
		t.Fatal("the observer rewrote a plan whose stored round it cannot keep")
	}
}

// stopRaw delivers a payload built by the caller, for the agent_id shapes stop cannot express (an empty text, a number).
func (e reviewObsEnv) stopRaw(t *testing.T, extra map[string]any) {
	t.Helper()
	p := map[string]any{"hook_event_name": "SubagentStop", "cwd": e.cwd, "session_id": e.session, "agent_type": "explorer"}
	for k, v := range extra {
		p[k] = v
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-observing-review"},
		bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs()); code != 0 || stderr.Len() != 0 || out.Len() != 0 {
		t.Fatalf("exit=%d out=%q stderr=%s", code, &out, &stderr)
	}
}

// CRW-564 d2 (port: fixed). The oracle reads `payload.agent_id ?? ""` and records a sign-off from a child it cannot name, so the
// round turns terminal with an empty reviewerSession and the real reviewer's later answer is refused as a second signer
// (review-observer.ts:117-118,133). The A>B check reads the verdict only, so such a PASS would be spent as an honest approval.
func TestReviewObserverIgnoresASignoffFromAChildItCannotName(t *testing.T) {
	for name, id := range map[string]any{"missing": nil, "empty": "", "number": 7, "object": map[string]any{"a": 1}} {
		t.Run(name, func(t *testing.T) {
			e := reviewObsSeed(t, "rb", nil)
			launch := e.open(t)
			extra := map[string]any{"last_assistant_message": reviewObsSignoff(launch, "PASS")}
			if id != nil {
				extra["agent_id"] = id
			}
			e.stopRaw(t, extra)
			if r := e.round(t); r.Status != goalplan.ReviewInFlight || r.Lane.Verdict != "" || r.Lane.ReviewerSession != nil {
				t.Fatalf("a nameless sign-off must record nothing: %+v", r)
			}
			if !strings.Contains(e.ledger(t), "no agent id") {
				t.Fatalf("the ignored sign-off says why: %q", e.ledger(t))
			}
			// The real reviewer is still the first signer.
			e.stopRaw(t, map[string]any{"agent_id": "reviewer-1", "last_assistant_message": reviewObsSignoff(launch, "FAIL")})
			if r := e.round(t); r.Lane.Verdict != goalplan.VerdictFail || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
				t.Fatalf("the named reviewer records its verdict: %+v", r)
			}
		})
	}
}

// CRW-564 d1 (port: fixed). The oracle writes the verdict under the goalplan lock only and reads the session state before it, so a
// FAIL can land between the A>B transition's review check and its publication, which holds the session lock for that whole span.
// The observer takes the session lock first (the order every writer of this tree follows: session, then goalplan), and judges the
// state it reads under it.
func TestReviewObserverWaitsForTheSessionLockAndJudgesTheStateItLeaves(t *testing.T) {
	for _, verdict := range []string{"PASS", "FAIL"} {
		t.Run(verdict, func(t *testing.T) {
			e := reviewObsSeed(t, "rb", nil)
			launch := e.open(t)
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- state.WithSessionLock(e.cwd, e.session, func() error {
					close(entered)
					<-release
					st := state.ReadState(e.cwd, e.session)
					st.Phase = state.PhaseB // the transition publishes B before it lets go
					return state.WriteState(e.cwd, st)
				})
			}()
			<-entered
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				e.stopRaw(t, map[string]any{"agent_id": "reviewer-1", "last_assistant_message": reviewObsSignoff(launch, verdict)})
			}()
			select {
			case <-stopped:
				t.Fatal("the observer must wait for the session lock the transition holds")
			case <-time.After(60 * time.Millisecond):
			}
			if r := e.round(t); r.Lane.Verdict != "" {
				t.Fatalf("no verdict may land while the transition holds the session: %+v", r)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			<-stopped
			if r := e.round(t); r.Status != goalplan.ReviewInFlight || r.Lane.Verdict != "" {
				t.Fatalf("the session left A before the observer judged: %+v", r)
			}
			if !strings.Contains(e.ledger(t), "the session left A before the reviewer finished") {
				t.Fatalf("ledger %q", e.ledger(t))
			}
		})
	}
}
