package cli

// CRW-922: the D close honours the first SIGINT at every durable effect that can still be its first one,
// and a goalplan write lock that gives up after the invocation was cancelled answers 130 with no output
// instead of the busy, unreadable or pending text. CRW-871 (PR #796) put the check at the goalplan
// callback's entry only, and the close then reads and parses the whole PABCD ledger before its first
// append. The timing of every case below is pinned by a seam or by the lock the case takes, never a sleep.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateDcloseCancelAllDoneAtC seeds the all-done bound session of the ledger-read case: every
// work-phase is already done, so the close takes the all-done branch, whose first durable effect is the
// PABCD done row it appends after it has read and parsed the ledger.
func orchestrateDcloseCancelAllDoneAtC(t *testing.T, cwd, id, slug string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all phases done"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "closed", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseC, &epoch, true
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)
}

// TestOrchestrateDcloseCancelBeforeTheIdleStateWriteWritesNothing pins the check that guards the IDLE
// state write when the goalplan lock wrote nothing: the all-done branch whose PABCD row is already
// recorded. The read that immediately precedes that write is the recovery state read that follows the
// first lock, so the cancellation is fired from that read's own seam; a check moved in front of it cannot
// see the cancellation and writes IDLE, which fails here.
func TestOrchestrateDcloseCancelBeforeTheIdleStateWriteWritesNothing(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "dclose-cancel-idlewrite", "dclose-cancel-idlewrite-plan"
	orchestrateDcloseCancelAllDoneAtC(t, cwd, id, slug)
	cur := state.ReadState(cwd, id)
	if err := orchestrateDcloseAppendPabcdRow(cwd, cur, cur.CheckEpoch, nil, orchestrateDcloseAttest(id)); err != nil {
		t.Fatal(err)
	}
	before := statusTree(t, cwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	seam := orchestrateDcloseSeam{afterRecoveryStateRead: func() {
		reads++
		cancel()
	}}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
	after := statusTree(t, cwd)
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
		t.Fatalf("a close cancelled before its IDLE state write answered (%+v, %v) with phase %s; want context.Canceled with the zero result and nothing written",
			got, err, state.ReadState(cwd, id).Phase)
	}
	if reads != 1 {
		t.Fatalf("the close ran the recovery-read seam %d time(s); want the read that precedes the IDLE write once", reads)
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseC {
		t.Fatalf("phase = %s; the cancelled close must not have published IDLE", s.Phase)
	}
}

// TestOrchestrateDcloseCancelBeforeTheFinalizationRowWritesNothing pins the check that guards the
// finalization pass's own PABCD row, which is that pass's first durable effect on a recovery retry that
// owes only that row. The first lock writes nothing and the state is already IDLE, so the finalization
// callback is entered with wrote false. The cancellation is fired by the seam that runs immediately after
// that pass's ledger read returns, not by a count of the invocation's checks, so a check moved in front of
// the read - where it cannot see this cancellation - writes the row and fails here.
func TestOrchestrateDcloseCancelBeforeTheFinalizationRowWritesNothing(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "dclose-cancel-finrow", "dclose-cancel-finrow-plan"
	orchestrateDcloseCancelIdleRetrySeed(t, cwd, id, slug)
	cur := state.ReadState(cwd, id)
	if !state.MatchesDcloseRecovery(cur, "wp-1") {
		t.Fatalf("the seeded retry does not match the request: %+v", cur.DcloseRecovery)
	}
	before := statusTree(t, cwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	seam := orchestrateDcloseSeam{afterLedgerRead: func() {
		reads++
		cancel()
	}}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", cur, orchestrateDcloseAttest(id), true, seam)
	after := statusTree(t, cwd)
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
		t.Fatalf("a recovery retry cancelled after the finalization pass read the ledger answered (%+v, %v) after %d read(s); want context.Canceled with the zero result and nothing written",
			got, err, reads)
	}
	if reads != 1 {
		t.Fatalf("the close ran the after-read seam %d time(s); want the finalization pass's ledger read once", reads)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 0 {
		t.Fatalf("the cancelled finalization wrote %d PABCD close row(s)", n)
	}
}

// orchestrateDcloseCancelIdleRetrySeed seeds the recovery retry that owes only the PABCD close row: the
// target work-phase is already closed and its goalplan row already recorded, the state is already IDLE
// with the marker that names it, and the C -> IDLE row the finalization pass still owes was never
// appended. Nothing in the invocation that follows writes, so a goalplan lock that gives up is its first
// and only failure and the close has no first write of its own to have started.
func orchestrateDcloseCancelIdleRetrySeed(t *testing.T, cwd, id, slug string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "cycle completion gate"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "second", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	if err := goalplan.AppendGoalplanLedger(cwd, slug, goalplan.GoalplanLedgerEntry{
		Ts: "2026-01-01T00:00:00.000Z", Slug: slug, Event: goalplan.EventWorkphaseDone, Detail: "closed wp-1",
	}); err != nil {
		t.Fatal(err)
	}
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseIdle, &epoch, true
	s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: id, CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1"}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

// orchestrateDcloseCancelLockDir is the lock directory this close's two goalplan write locks contend
// with, so a case can make a lock wait and then give up without sleeping on a real holder.
func orchestrateDcloseCancelLockDir(t *testing.T, cwd, slug string) string {
	t.Helper()
	dir, err := goalplan.GoalplanWriteLockDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// orchestrateDcloseCancelPlanRel and orchestrateDcloseCancelLockRel name the two artefacts a case may
// take or remove itself, relative to the case root.
func orchestrateDcloseCancelPlanRel(slug string) string {
	return filepath.Join(".crw", "goalplans", slug, goalplan.GoalplanFile)
}

func orchestrateDcloseCancelLockRel(slug string) string {
	return filepath.Join(".crw", "goalplans", slug, goalplan.GoalplanLockDir)
}

// orchestrateDcloseCancelTreeExcept is statusTree without those two artefacts. They belong to the case -
// the lock directory it makes a lock contend with, and the plan file it removes to make a lock answer
// unreadable - so they say nothing about whether the close under test wrote.
func orchestrateDcloseCancelTreeExcept(t *testing.T, root, slug string) map[string]string {
	t.Helper()
	tree := statusTree(t, root)
	plan, lock := orchestrateDcloseCancelPlanRel(slug), orchestrateDcloseCancelLockRel(slug)
	for path := range tree {
		if path == plan || path == lock || strings.HasPrefix(path, lock+string(filepath.Separator)) {
			delete(tree, path)
		}
	}
	return tree
}

// orchestrateDcloseCancelLockSeam pins a cancellation inside the goalplan write lock a case names. take
// runs as the close enters that lock, which is where a case takes the lock directory so the lock has to
// wait at all; sleep, which the lock calls only once its wait has begun, fires the cancellation for the
// shapes that need it inside the wait rather than before it. The options give that lock zero retry
// delays, so a lock that has to wait reaches its give-up branch at once with no real sleep; the other
// lock keeps a single attempt and no hook, because the case is about exactly one of the two.
func orchestrateDcloseCancelLockSeam(wantFinalize bool, take func(), sleep func(int)) func(bool) *goalplan.GoalplanWriteLockOptions {
	return func(finalize bool) *goalplan.GoalplanWriteLockOptions {
		if finalize != wantFinalize {
			return &goalplan.GoalplanWriteLockOptions{RetryDelaysMs: []int{0}}
		}
		take()
		return &goalplan.GoalplanWriteLockOptions{RetryDelaysMs: []int{0}, Sleep: sleep}
	}
}

// orchestrateDcloseCancelLockCases is the two shapes a goalplan write lock gives up in: locked, which
// this case makes by taking the lock directory, and unreadable, which it makes by releasing that
// directory and removing the plan file so the attempt that follows acquires the lock and then finds the
// plan gone.
func orchestrateDcloseCancelLockCases() []struct {
	name   string
	mutate func(t *testing.T, cwd, slug, lockDir string)
	want   string
} {
	return []struct {
		name   string
		mutate func(t *testing.T, cwd, slug, lockDir string)
		want   string
	}{
		{"locked", func(t *testing.T, cwd, slug, lockDir string) {}, "is busy."},
		{"unreadable", func(t *testing.T, cwd, slug, lockDir string) {
			if err := os.Remove(lockDir); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(cwd, orchestrateDcloseCancelPlanRel(slug))); err != nil {
				t.Fatal(err)
			}
		}, "could not be read (CYCLE-COMPLETION-01)"},
	}
}

// TestOrchestrateDcloseCancelAfterTheLedgerReadWritesNothing is the F2 case: a cancellation that lands
// after the all-done close has read and parsed the PABCD ledger and before its first append leaves the
// ledger, the state and the plan untouched and ends the close with 130 (the context's own error and no
// answer). On the pre-fix code the close appended the done row, wrote IDLE and answered 0.
//
// The cancellation is fired by a seam that runs immediately after the ledger read returns, not by a count
// of the close's checks, so an implementation that ran the check before that read - where the check cannot
// see this cancellation - completes the close and fails here.
func TestOrchestrateDcloseCancelAfterTheLedgerReadWritesNothing(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "dclose-cancel-read", "dclose-cancel-read-plan"
	orchestrateDcloseCancelAllDoneAtC(t, cwd, id, slug)
	before := statusTree(t, cwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	seam := orchestrateDcloseSeam{afterLedgerRead: func() {
		reads++
		cancel()
	}}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
	after := statusTree(t, cwd)
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
		t.Fatalf("a close cancelled after the ledger read answered (%+v, %v), leaving %d done row(s) and phase %s; want context.Canceled with the zero result and the ledger, the state and the plan unchanged",
			got, err, orchestrateDcloseDoneRows(t, cwd, id), state.ReadState(cwd, id).Phase)
	}
	if reads != 1 {
		t.Fatalf("the close ran the after-read seam %d time(s); want the all-done branch's ledger read once", reads)
	}
}

// TestOrchestrateDcloseCancelWhileTheGoalplanLockGivesUpAnswersInterrupted is the F3 case for the close's
// first goalplan write lock: the lock gives up with locked or unreadable while the invocation's context is
// cancelled from inside that lock's wait, and the close answers 130 with no output instead of the busy or
// unreadable text with code 1. On the pre-fix code both answered that text with code 1, because the
// branches never read ctx.Err().
func TestOrchestrateDcloseCancelWhileTheGoalplanLockGivesUpAnswersInterrupted(t *testing.T) {
	for _, tc := range orchestrateDcloseCancelLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			cwd := orchestrateDcloseTestCwd(t)
			id, slug := "dclose-cancel-"+tc.name, "dclose-cancel-"+tc.name+"-plan"
			orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
			lockDir := orchestrateDcloseCancelLockDir(t, cwd, slug)
			before := orchestrateDcloseCancelTreeExcept(t, cwd, slug)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var takeErr error
			seam := orchestrateDcloseSeam{goalplanLockSeam: orchestrateDcloseCancelLockSeam(false,
				func() { takeErr = os.Mkdir(lockDir, 0o700) },
				func(int) {
					cancel()
					tc.mutate(t, cwd, slug, lockDir)
				})}
			got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
			after := orchestrateDcloseCancelTreeExcept(t, cwd, slug)
			if takeErr != nil {
				t.Fatal(takeErr)
			}
			if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
				t.Fatalf("a close whose goalplan lock answered %s after the context ended during its wait answered (%+v, %v); want context.Canceled with the zero result and no write of the close",
					tc.name, got, err)
			}
		})
	}
}

// TestOrchestrateDcloseCancelWhileTheFinalizationLockGivesUpAnswersInterrupted is the F1 case: on a
// recovery retry that wrote nothing in this invocation, a cancellation that lands while the finalization
// lock waits and that lock then gives up answers 130 with no output, not the pending text with code 0.
// On the pre-fix code it answered code 0 with the pending text, because the finalization branch never read
// ctx.Err() and answered the lock's failure as a code-0 pending result.
func TestOrchestrateDcloseCancelWhileTheFinalizationLockGivesUpAnswersInterrupted(t *testing.T) {
	for _, tc := range orchestrateDcloseCancelLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			cwd := orchestrateDcloseTestCwd(t)
			id, slug := "dclose-cancel-finalize-"+tc.name, "dclose-cancel-finalize-"+tc.name+"-plan"
			orchestrateDcloseCancelIdleRetrySeed(t, cwd, id, slug)
			lockDir := orchestrateDcloseCancelLockDir(t, cwd, slug)
			before := orchestrateDcloseCancelTreeExcept(t, cwd, slug)

			cur := state.ReadState(cwd, id)
			if !state.MatchesDcloseRecovery(cur, "wp-1") {
				t.Fatalf("the seeded retry does not match the request: %+v", cur.DcloseRecovery)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var takeErr error
			seam := orchestrateDcloseSeam{goalplanLockSeam: orchestrateDcloseCancelLockSeam(true,
				func() { takeErr = os.Mkdir(lockDir, 0o700) },
				func(int) {
					cancel()
					tc.mutate(t, cwd, slug, lockDir)
				})}
			got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", cur, orchestrateDcloseAttest(id), true, seam)
			after := orchestrateDcloseCancelTreeExcept(t, cwd, slug)
			if takeErr != nil {
				t.Fatal(takeErr)
			}
			if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
				t.Fatalf("a recovery retry that wrote nothing and was cancelled during the finalization lock wait answered (%+v, %v); want context.Canceled with the zero result and no write of the close",
					got, err)
			}
		})
	}
}

// TestOrchestrateDcloseCancelAfterTheFirstWriteAnswersAsToday is the first control: a cancellation that
// lands once the close's first durable effect is on disk does not undo it, and the close answers the same
// success text it answered before.
func TestOrchestrateDcloseCancelAfterTheFirstWriteAnswersAsToday(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "dclose-cancel-late", "dclose-cancel-late-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The seam cancels immediately after the close's first durable effect - the recovery marker - is
	// published, so every later effect runs under an ended context and must still happen.
	seam := orchestrateDcloseSeam{afterRecoveryMarkerWrite: func() error {
		cancel()
		return nil
	}}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
	if err != nil {
		t.Fatalf("a close whose first write started before the cancel returned %v", err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "close target wp-1 is complete") {
		t.Fatalf("close after the first write: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the close whose first write had started did not land")
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows = %d, want 1", n)
	}
}

// TestOrchestrateDcloseCancelAfterTheFirstWriteKeepsThePendingAnswer is F1's other half: when this
// invocation has already written, a cancellation that lands as the finalization lock waits does not
// change the answer. The close still reports the pending finalization with code 0, because its effects
// are visible and the marker is what the next request resumes from.
func TestOrchestrateDcloseCancelAfterTheFirstWriteKeepsThePendingAnswer(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "dclose-cancel-written", "dclose-cancel-written-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	lockDir := orchestrateDcloseCancelLockDir(t, cwd, slug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seam := orchestrateDcloseSeam{goalplanLockSeam: orchestrateDcloseCancelLockSeam(true,
		func() {
			cancel()
			if err := os.Mkdir(lockDir, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		func(int) {})}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
	if err != nil {
		t.Fatalf("a close whose first write had started returned %v; want the pending answer", err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "finalization is pending") ||
		!strings.Contains(got.Output, "The recovery marker is still on the session") {
		t.Fatalf("pending finalization under a cancelled context: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseIdle || after.DcloseRecovery == nil {
		t.Fatalf("state: %+v", after)
	}
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
}

// TestOrchestrateDcloseLockGiveUpWithALiveContextAnswersAsToday is the second control: the answers a live
// invocation gets are unchanged by this issue. The first lock answers the busy or unreadable text with
// code 1, and the finalization lock keeps the section 39 Y3 code-0 pending answer, because the state write
// that precedes it already moved the FSM to IDLE outside the lock.
func TestOrchestrateDcloseLockGiveUpWithALiveContextAnswersAsToday(t *testing.T) {
	for _, tc := range orchestrateDcloseCancelLockCases() {
		t.Run("first-lock-"+tc.name, func(t *testing.T) {
			cwd := orchestrateDcloseTestCwd(t)
			id, slug := "dclose-live-"+tc.name, "dclose-live-"+tc.name+"-plan"
			orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
			lockDir := orchestrateDcloseCancelLockDir(t, cwd, slug)
			var takeErr error
			seam := orchestrateDcloseSeam{goalplanLockSeam: orchestrateDcloseCancelLockSeam(false,
				func() { takeErr = os.Mkdir(lockDir, 0o700) },
				func(int) { tc.mutate(t, cwd, slug, lockDir) })}
			got, err := orchestrateDcloseRun(t, cwd, id, seam)
			if takeErr != nil {
				t.Fatal(takeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Code != 1 || !strings.Contains(got.Output, tc.want) || !strings.Contains(got.Output, "Nothing was written.") {
				t.Fatalf("a live close whose first lock answered %s gave %+v; want that text with code 1", tc.name, got)
			}
		})
	}
	t.Run("finalization-lock-pending", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "dclose-live-pending", "dclose-live-pending-plan"
		orchestrateDcloseCancelIdleRetrySeed(t, cwd, id, slug)
		lockDir := orchestrateDcloseCancelLockDir(t, cwd, slug)
		cur := state.ReadState(cwd, id)
		var takeErr error
		seam := orchestrateDcloseSeam{goalplanLockSeam: orchestrateDcloseCancelLockSeam(true,
			func() { takeErr = os.Mkdir(lockDir, 0o700) },
			func(int) {})}
		got, err := orchestrateDcloseContext(context.Background(), cwd, id, "wp-1", cur, orchestrateDcloseAttest(id), true, seam)
		if takeErr != nil {
			t.Fatal(takeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 || !strings.Contains(got.Output, "finalization is pending") ||
			!strings.Contains(got.Output, "The recovery marker is still on the session") {
			t.Fatalf("a live retry whose finalization lock is busy answered %+v; want the code-0 pending text", got)
		}
	})
}
