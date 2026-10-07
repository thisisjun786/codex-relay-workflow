package cli

// CRW-922: the D close honours the first SIGINT at every durable effect that can still be its first one,
// and a goalplan write lock that gives up after the invocation was cancelled answers 130 with no output
// instead of the busy or unreadable text with code 1. CRW-871 (PR #796) put the check at the goalplan
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

// orchestrateDcloseCancelAllDoneAtC seeds the all-done bound session of the first case: every work-phase
// is already done, so the close takes the all-done branch, whose first durable effect is the PABCD done
// row it appends after it has read and parsed the ledger.
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

// TestOrchestrateDcloseCancelAfterTheLedgerReadWritesNothing is the first red case: a cancellation that
// lands after the all-done close has read and parsed the PABCD ledger and before its first append leaves
// the ledger, the state and the plan untouched and ends the close with 130 (the context's own error and
// no answer). On the baseline the close appended the done row, wrote IDLE and answered 0.
//
// The check count pins the window without a sleep. The close's pre-write checks run in order: the goalplan
// callback's entry check first (CRW-871), then the check this issue adds immediately before the all-done
// row append, which is the first one to run after the ledger read. Cancelling at the second is exactly
// "after the read, before the first write"; on the baseline that second check does not exist, so the hook
// never cancels the close and it completes.
func TestOrchestrateDcloseCancelAfterTheLedgerReadWritesNothing(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "dclose-cancel-read", "dclose-cancel-read-plan"
	orchestrateDcloseCancelAllDoneAtC(t, cwd, id, slug)
	before := statusTree(t, cwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	seam := orchestrateDcloseSeam{interrupt: func() {
		checks++
		if checks == 2 {
			cancel()
		}
	}}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
	after := statusTree(t, cwd)
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
		t.Fatalf("a close cancelled after the ledger read answered (%+v, %v) after %d check(s), leaving %d done row(s) and phase %s; want context.Canceled with the zero result and the ledger, the state and the plan unchanged",
			got, err, checks, orchestrateDcloseDoneRows(t, cwd, id), state.ReadState(cwd, id).Phase)
	}
	if checks != 2 {
		t.Fatalf("the close ran %d pre-write check(s); want the goalplan callback's entry check and the check before its first append", checks)
	}
}

// TestOrchestrateDcloseCancelWhileTheGoalplanLockGivesUpAnswersInterrupted is the second red case: the
// close's goalplan write lock gives up while the invocation's context is cancelled - the state the close
// sees when a SIGINT lands during that wait - and the close answers 130 with no output instead of the
// busy or unreadable text with code 1. On the baseline both answered the busy or unreadable text with
// code 1, because those branches never read ctx.Err().
//
// Neither case needs a sleep: "locked" is a lock directory this test takes, so the real lock exhausts its
// retry budget and gives up; "unreadable" is the plan file removed, which the real lock reports at once.
// The lock takes no context by design, so the close can only observe the cancellation after it gives up,
// which is why the context is cancelled before the call.
func TestOrchestrateDcloseCancelWhileTheGoalplanLockGivesUpAnswersInterrupted(t *testing.T) {
	cases := []struct {
		name  string
		given func(t *testing.T, cwd, slug string)
	}{
		{"locked", func(t *testing.T, cwd, slug string) {
			dir, err := goalplan.GoalplanWriteLockDir(cwd, slug)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", func(t *testing.T, cwd, slug string) {
			if err := os.Remove(filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := orchestrateDcloseTestCwd(t)
			id, slug := "dclose-cancel-"+tc.name, "dclose-cancel-"+tc.name+"-plan"
			orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
			tc.given(t, cwd, slug)
			before := statusTree(t, cwd)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, orchestrateDcloseSeam{})
			after := statusTree(t, cwd)
			if !errors.Is(err, context.Canceled) || got != (CliResult{}) || !reflect.DeepEqual(before, after) {
				t.Fatalf("a close whose goalplan lock answered %s after the context ended answered (%+v, %v); want context.Canceled with the zero result and the workspace unchanged", tc.name, got, err)
			}
		})
	}
}

// TestOrchestrateDcloseCancelAfterTheFirstWriteAnswersAsToday is the control: a cancellation that lands
// once the close's first durable effect is on disk does not undo it, and the close answers the same
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
