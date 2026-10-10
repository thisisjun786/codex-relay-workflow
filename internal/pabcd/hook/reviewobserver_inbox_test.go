package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// A kept CRW-1116 verdict must retain its count and findings across CRW-1113's deferred drain.
func TestReviewObserverInboxKeepsTheBlockerCountAndFindings(t *testing.T) {
	for name, hold := range map[string]func(reviewObsEnv, *testing.T) func(){
		"session lock":  reviewObsEnv.holdSessionLock,
		"goalplan lock": reviewObsEnv.holdGoalplanLock,
	} {
		t.Run(name, func(t *testing.T) {
			e := reviewObsSeed(t, "counted-inbox", nil)
			launch := e.open(t)
			release := hold(e, t)
			t.Cleanup(func() {
				if release != nil {
					release()
				}
			})
			e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "GO-WITH-FIXES (blockers=2; findings=c1,r2)"))
			files := e.inboxFiles(t)
			if len(files) != 1 {
				t.Fatalf("one kept verdict: %v", files)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var kept struct {
				Blockers int
				Findings []string
			}
			if err := json.Unmarshal(raw, &kept); err != nil {
				t.Fatal(err)
			}
			if kept.Blockers != 2 || strings.Join(kept.Findings, ",") != "c1,r2" {
				t.Fatalf("kept verdict lost its metadata: %s", raw)
			}
			release()
			release = nil
			e.stop(t, nil, "other", "nothing")
			r := e.round(t)
			if r.Status != goalplan.ReviewApproved || r.Lane.Verdict != goalplan.VerdictNearPass || r.Lane.Blockers != 2 || strings.Join(r.Lane.Findings, ",") != "c1,r2" {
				t.Fatalf("drained verdict lost its metadata: %+v", r)
			}
			if len(e.inboxFiles(t)) != 0 {
				t.Fatal("the recorded verdict stayed in the inbox")
			}
		})
	}
}

// CRW-1113 (A3-06, port: fixed). A sign-off that meets a held session or goalplan lock used to be dropped: the observer kept it in a
// local variable only, released the child, and the round stayed in_flight after the lock was gone. The sign-off is now kept in an
// idempotent inbox first and drained at the next legitimate hook or transition, under the session lock then the goalplan lock,
// against the binding, launch, agent and work-phase as they are then.

// reviewObsInboxFiles lists the inbox entries a session has, the layout the observer documents.
func (e reviewObsEnv) inboxFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(e.cwd, ".crw", "review-inbox", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// holdGoalplanLock takes the goalplan write lock the way a live writer would and returns the release.
func (e reviewObsEnv) holdGoalplanLock(t *testing.T) (release func()) {
	t.Helper()
	dir, err := goalplan.GoalplanDir(e.cwd, e.slug)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, goalplan.GoalplanLockDir)
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	subagentStopPut(t, filepath.Join(lock, "owner.json"), `{"pid":4242}`+"\n")
	return func() {
		if err := os.RemoveAll(lock); err != nil {
			t.Fatal(err)
		}
	}
}

// holdSessionLock holds the same kernel-owned lock as a live writer until release, including on a failed assertion.
func (e reviewObsEnv) holdSessionLock(t *testing.T) (release func()) {
	t.Helper()
	const bound = 30 * time.Second
	entered, unlock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var lockErr error
	go func() {
		lockErr = state.WithSessionLock(e.cwd, e.session, func() error {
			close(entered)
			<-unlock
			return nil
		})
		close(done)
	}()
	release = func() {
		t.Helper()
		once.Do(func() { close(unlock) })
		select {
		case <-done:
			if lockErr != nil {
				t.Fatal(lockErr)
			}
		case <-time.After(bound):
			t.Fatal("the session lock holder never finished after release")
		}
	}
	t.Cleanup(release)
	select {
	case <-entered:
	case <-done:
		t.Fatalf("the session lock holder never entered: %v", lockErr)
	case <-time.After(bound):
		t.Fatal("the session lock holder never took the lock")
	}
	return release
}

func TestReviewObserverKeepsASignoffThatMetAHeldLockAndApprovesItAfterwards(t *testing.T) {
	for name, hold := range map[string]func(reviewObsEnv, *testing.T) func(){
		"session lock":  reviewObsEnv.holdSessionLock,
		"goalplan lock": reviewObsEnv.holdGoalplanLock,
	} {
		t.Run(name, func(t *testing.T) {
			e := reviewObsSeed(t, "rb", nil)
			launch := e.open(t)
			release := hold(e, t)
			if out := e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS")); out != "" {
				t.Fatalf("the child is released with an empty answer: %q", out)
			}
			if r := e.round(t); r.Status != goalplan.ReviewInFlight {
				t.Fatalf("a held lock records nothing yet: %+v", r)
			}
			if got := len(e.inboxFiles(t)); got != 1 {
				t.Fatalf("the sign-off is kept: %d inbox entries", got)
			}
			release()
			// An unrelated exit of the same session is the next legitimate hook: it drains.
			e.stop(t, reviewObsType("explorer"), "other-1", "no sign-off in this one")
			r := e.round(t)
			if r.Status != goalplan.ReviewApproved || r.Lane.Verdict != goalplan.VerdictPass || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
				t.Fatalf("the kept sign-off is approved without a new dispatch: %+v", r)
			}
			if got := len(e.inboxFiles(t)); got != 0 {
				t.Fatalf("a drained entry is gone: %d", got)
			}
			if plan := goalplan.ReadGoalplan(e.cwd, e.slug); len(plan.ReviewRounds) != 1 {
				t.Fatalf("no new reviewer round: %d rounds", len(plan.ReviewRounds))
			}
		})
	}
}

func TestReviewObserverInboxDrainIsIdempotent(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	if got := len(e.inboxFiles(t)); got != 1 {
		t.Fatalf("the same child and launch keep one entry: %d", got)
	}
	release()
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	first := e.round(t)
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	e.stop(t, reviewObsType("explorer"), "other-2", "nothing")
	if second := e.round(t); second.Status != goalplan.ReviewChangesRequested || second.ClosedAt == nil || first.ClosedAt == nil || *second.ClosedAt != *first.ClosedAt {
		t.Fatalf("a repeated drain changes nothing: %+v then %+v", first, second)
	}
}

func TestReviewObserverInboxEntryIsRevalidatedAtDrain(t *testing.T) {
	setup := func(t *testing.T) (reviewObsEnv, string, func()) {
		e := reviewObsSeed(t, "rb", nil)
		launch := e.open(t)
		release := e.holdGoalplanLock(t)
		e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
		if len(e.inboxFiles(t)) != 1 {
			t.Fatal("the sign-off is kept")
		}
		return e, launch, release
	}
	drain := func(e reviewObsEnv, t *testing.T) { e.stop(t, reviewObsType("explorer"), "other-1", "nothing") }
	notApproved := func(t *testing.T, e reviewObsEnv, why string) {
		t.Helper()
		if r := e.round(t); r.Status == goalplan.ReviewApproved || r.Lane.Verdict == goalplan.VerdictPass {
			t.Fatalf("%s: approved %+v", why, r)
		}
		if got := len(e.inboxFiles(t)); got != 0 {
			t.Fatalf("%s: a refused entry is dropped, not retried forever: %d", why, got)
		}
	}

	t.Run("superseded epoch", func(t *testing.T) {
		e, _, release := setup(t)
		s := state.ReadState(e.cwd, e.session)
		next := "e-replanned"
		s.PlanEpoch = &next
		if err := state.WriteState(e.cwd, s); err != nil {
			t.Fatal(err)
		}
		release()
		drain(e, t)
		notApproved(t, e, "epoch")
		if !strings.Contains(e.ledger(t), "re-planned") {
			t.Fatalf("the refusal says why: %q", e.ledger(t))
		}
	})
	t.Run("session left A", func(t *testing.T) {
		e, _, release := setup(t)
		s := state.ReadState(e.cwd, e.session)
		s.Phase = state.PhaseP
		if err := state.WriteState(e.cwd, s); err != nil {
			t.Fatal(err)
		}
		release()
		drain(e, t)
		notApproved(t, e, "phase")
	})
	t.Run("other work-phase active", func(t *testing.T) {
		e, _, release := setup(t)
		release()
		plan := goalplan.ReadGoalplan(e.cwd, e.slug)
		other := "wp-other"
		plan.WorkPhases = append(plan.WorkPhases, goalplan.GoalplanWorkPhase{ID: other, Title: "other", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		plan.ActiveWorkPhaseID = &other
		plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
		if err := goalplan.WriteGoalplan(e.cwd, plan); err != nil {
			t.Fatal(err)
		}
		drain(e, t)
		notApproved(t, e, "work-phase")
	})
	t.Run("earlier terminal verdict", func(t *testing.T) {
		e, launch, release := setup(t)
		release()
		// A second child signs the same launch FAIL first, directly; the kept PASS arrives after a terminal verdict.
		e.closeRoundFail(t, launch)
		if r := e.round(t); r.Lane.Verdict != goalplan.VerdictFail {
			t.Fatalf("setup: %+v", r)
		}
		drain(e, t)
		if r := e.round(t); r.Lane.Verdict != goalplan.VerdictFail || r.Status != goalplan.ReviewChangesRequested {
			t.Fatalf("a terminal verdict is never overwritten: %+v", r)
		}
		if got := len(e.inboxFiles(t)); got != 0 {
			t.Fatalf("refused entry dropped: %d", got)
		}
	})
	t.Run("other child", func(t *testing.T) {
		e, launch, release := setup(t)
		release()
		// The round is already signed by a different child, so the kept PASS of reviewer-1 cannot overwrite it.
		plan := goalplan.ReadGoalplan(e.cwd, e.slug)
		signed := "reviewer-2"
		plan.ReviewRounds[0].Lane.ReviewerSession = &signed
		if err := goalplan.WriteGoalplan(e.cwd, plan); err != nil {
			t.Fatal(err)
		}
		_ = launch
		drain(e, t)
		notApproved(t, e, "agent")
	})
}

// closeRoundFail closes the round FAIL, signed by reviewer-0, the way an earlier observer run would have left it.
func (e reviewObsEnv) closeRoundFail(t *testing.T, launch string) {
	t.Helper()
	plan := goalplan.ReadGoalplan(e.cwd, e.slug)
	r := &plan.ReviewRounds[0]
	v := "reviewer-0"
	r.Lane.ReviewerSession, r.Lane.Verdict, r.Status = &v, goalplan.VerdictFail, goalplan.ReviewChangesRequested
	plan.ActivePlanAuditRoundID = nil
	if err := goalplan.WriteGoalplan(e.cwd, plan); err != nil {
		t.Fatal(err)
	}
	_ = launch
}

func TestReviewObserverKeepsNothingForWorkersExecutorsOrUnparsedExits(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	defer release()
	e.stop(t, reviewObsType("worker"), "w1", reviewObsSignoff(launch, "PASS"))
	e.stop(t, reviewObsType("executor"), "x1", reviewObsSignoff(launch, "PASS"))
	e.stop(t, reviewObsType("explorer"), "u1", "no closing lines")
	e.stopRaw(t, map[string]any{"last_assistant_message": reviewObsSignoff(launch, "PASS")})
	if got := e.inboxFiles(t); len(got) != 0 {
		t.Fatalf("only a named reviewer-shaped child's sign-off is kept: %v", got)
	}
}

// An inbox that cannot keep the sign-off is a bounded diagnostic and never an approval: with the locks busy nothing is recorded, and
// with them free the sign-off is still judged once in memory, as the observer always did.
func TestReviewObserverInboxFailureIsADiagnosticNotAnApproval(t *testing.T) {
	broken := func(e reviewObsEnv, t *testing.T) {
		t.Helper()
		// A file where the inbox directory belongs: nothing can be created under it.
		if err := os.WriteFile(filepath.Join(e.cwd, ".crw", "review-inbox"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("locks free", func(t *testing.T) {
		e := reviewObsSeed(t, "rb", nil)
		launch := e.open(t)
		broken(e, t)
		e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
		if r := e.round(t); r.Status != goalplan.ReviewApproved {
			t.Fatalf("a legitimate sign-off is still judged in memory: %+v", r)
		}
		ledger := e.ledger(t)
		if !strings.Contains(ledger, "review_signoff_inbox_failed") || len(ledger) > 2000 {
			t.Fatalf("one bounded diagnostic row: %q", ledger)
		}
	})
	// The diagnostic does not wait for a lock: with the sign-off lost to a busy lock and an inbox that cannot keep it, the failure is
	// still said once, bounded, before the child is released.
	for name, hold := range map[string]func(reviewObsEnv, *testing.T) func(){
		"goalplan lock held": reviewObsEnv.holdGoalplanLock,
		"session lock held":  reviewObsEnv.holdSessionLock,
	} {
		t.Run(name, func(t *testing.T) {
			e := reviewObsSeed(t, "rb", nil)
			launch := e.open(t)
			broken(e, t)
			release := hold(e, t)
			e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
			ledger := e.ledger(t)
			if got := strings.Count(ledger, "review_signoff_inbox_failed"); got != 1 || len(ledger) > 2000 {
				t.Fatalf("one bounded diagnostic row although no lock was free: %d rows, %q", got, ledger)
			}
			release()
			e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
			if r := e.round(t); r.Status != goalplan.ReviewInFlight || r.Lane.Verdict != "" {
				t.Fatalf("a sign-off the inbox could not keep is not read as an approval: %+v", r)
			}
			if got := strings.Count(e.ledger(t), "review_signoff_inbox_failed"); got != 1 {
				t.Fatalf("the diagnostic is written once: %d", got)
			}
		})
	}
}

// A plan the first, unlocked read could not open (a writer's rename window, a transient permission error) is not a plan whose
// work-phase differs: the sign-off keeps no work-phase then, and the drain, which reads the plan under the locks, binds it to the
// round's own work-phase. It used to be kept with an empty one and refused for that, so a legitimate sign-off was deleted.
func TestReviewObserverTransientlyUnreadablePlanDoesNotDestroyASignoff(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	dir, err := goalplan.GoalplanDir(e.cwd, e.slug)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, goalplan.GoalplanFile)
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })
	if goalplan.ReadGoalplan(e.cwd, e.slug) != nil {
		t.Fatal("setup: the plan must be unreadable for the first read")
	}
	// The plan is readable again by the time the session lock is taken.
	restore := hook.SetReviewObserverSessionLock(func(cwd, sessionID string, fn func() error) error {
		if err := os.Chmod(file, 0o644); err != nil {
			t.Fatal(err)
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	})
	defer restore()
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
	r := e.round(t)
	if r.Status != goalplan.ReviewApproved || r.Lane.Verdict != goalplan.VerdictPass || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("a legitimate sign-off survives one unreadable look at the plan: %+v\n%s", r, e.ledger(t))
	}
	if strings.Contains(e.ledger(t), "sign-off arrived for work-phase") {
		t.Fatalf("no work-phase mismatch is claimed: %q", e.ledger(t))
	}
}

// The same kept entry still refuses a sign-off that belongs to a work-phase other than the one active, so an unknown work-phase is
// resolved from the round and never waves a stale one through.
func TestReviewObserverUnknownWorkPhaseEntryStillFollowsTheRound(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
	release()
	files := e.inboxFiles(t)
	if len(files) != 1 {
		t.Fatal("the sign-off is kept")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], []byte(strings.Replace(string(raw), `"workPhaseId":"wp0"`, `"workPhaseId":""`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := goalplan.ReadGoalplan(e.cwd, e.slug)
	other := "wp-other"
	plan.WorkPhases = append(plan.WorkPhases, goalplan.GoalplanWorkPhase{ID: other, Title: "other", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	plan.ActiveWorkPhaseID = &other
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	if err := goalplan.WriteGoalplan(e.cwd, plan); err != nil {
		t.Fatal(err)
	}
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	if r := e.round(t); r.Status == goalplan.ReviewApproved || r.Lane.Verdict == goalplan.VerdictPass {
		t.Fatalf("an entry for the old work-phase approves nothing: %+v", r)
	}
	if got := len(e.inboxFiles(t)); got != 0 {
		t.Fatalf("refused entry dropped: %d", got)
	}
}

// A file with an entry's name that cannot be read as an entry cannot be judged: it is removed with one row, and the entries after it
// still drain. (A file with any other name is not the observer's and stays: TestReviewObserverInboxRemovesOnlyItsOwnFileNames.)
func TestReviewObserverInboxDropsAnUnreadableEntryAndDrainsTheRest(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
	release()
	dir := filepath.Dir(e.inboxFiles(t)[0])
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("0", 32)+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	if r := e.round(t); r.Status != goalplan.ReviewApproved {
		t.Fatalf("%+v", r)
	}
	if got := e.inboxFiles(t); len(got) != 0 {
		t.Fatalf("both entries are gone: %v", got)
	}
	if !strings.Contains(e.ledger(t), "unreadable review inbox entry") {
		t.Fatalf("one row says so: %q", e.ledger(t))
	}
}

// Every finding reference the dev parser accepts must fit in a kept inbox entry.
func TestReviewObserverInboxDrainsTheMaximumFindingReferences(t *testing.T) {
	e := reviewObsSeed(t, "maximum-findings", nil)
	launch := e.open(t)
	findings := make([]string, goalplan.MaxFindingRefs)
	for i := range findings {
		findings[i] = "c" + strings.Repeat("x", goalplan.MaxFindingRefLength-1)
	}
	release := e.holdGoalplanLock(t)
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	e.stop(t, nil, "reviewer-1", reviewObsSignoff(launch, "GO-WITH-FIXES (blockers=9999; findings="+strings.Join(findings, ",")+")"))
	files := e.inboxFiles(t)
	if len(files) != 1 {
		t.Fatalf("one kept verdict: %v", files)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 4096 {
		t.Fatalf("the maximum parsed references must exceed the old inbox limit: %d", info.Size())
	}
	release()
	release = nil
	e.stop(t, nil, "other", "nothing")
	r := e.round(t)
	if r.Status != goalplan.ReviewApproved || r.Lane.Blockers != 9999 || strings.Join(r.Lane.Findings, ",") != strings.Join(findings, ",") {
		t.Fatalf("the valid maximum verdict was lost: %+v", r)
	}
	if len(e.inboxFiles(t)) != 0 {
		t.Fatal("the recorded verdict stayed in the inbox")
	}
}
