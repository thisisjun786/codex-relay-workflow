package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-871: the first SIGINT cancels an orchestrate mutation before its first write. cmd/crw serve
// turns the signal into the invocation context; the orchestrate row now takes it, and the
// transition checks it once more immediately before each branch writes. Once the first write has
// started the command finishes and answers as it did before, because a published change is never
// relabelled as interrupted.

// TestOrchestrateInterruptCancelledAfterTheLockWritesNothing pins the check after the lock: the
// seam cancels the invocation once the lock is held, and the pre-write check then writes nothing.
func TestOrchestrateInterruptCancelledAfterTheLockWritesNothing(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "interrupt-lock"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","sessionId":"interrupt-lock"}`)
	before := statusTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seams := &orchestrateCommitSeams{interrupt: cancel}
	got, err := orchestrateCommitRunContext(ctx, OrchestrateCliArgs{Verb: fsm.VerbReset, Cwd: cwd}, id, seams)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reset returned %v, want context.Canceled", err)
	}
	if got != (CliResult{}) {
		t.Fatalf("cancelled reset answered %+v, want the zero result", got)
	}
	if !reflect.DeepEqual(before, statusTree(t, cwd)) {
		t.Fatal("the cancelled reset changed the workspace")
	}
	if state.ReadState(cwd, id).Phase != state.PhaseB {
		t.Fatal("the cancelled reset moved the phase")
	}
}

// TestOrchestrateInterruptAfterTheFirstWriteAnswersAsToday pins the other side: a cancellation
// that lands once the state write has begun does not undo it, and the command answers the same
// success text it answered before.
func TestOrchestrateInterruptAfterTheFirstWriteAnswersAsToday(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "interrupt-write"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","sessionId":"interrupt-write"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seams := &orchestrateCommitSeams{writeState: func(cwd string, next state.State) error {
		err := state.WriteState(cwd, next)
		cancel()
		return err
	}}
	got, err := orchestrateCommitRunContext(ctx, OrchestrateCliArgs{Verb: fsm.VerbReset, Cwd: cwd}, id, seams)
	if err != nil {
		t.Fatalf("reset whose first write started before the cancel returned %v", err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "orchestrate reset: current=B -> IDLE") {
		t.Fatalf("reset after the first write: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the reset whose write had started did not land")
	}
}

// TestOrchestrateInterruptPreCancelledContextWritesNothing pins the lock helper through the
// library entry: a context that is already cancelled returns its own error and creates nothing.
func TestOrchestrateInterruptPreCancelledContextWritesNothing(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "interrupt-pre"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","sessionId":"interrupt-pre"}`)
	before := statusTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := orchestrateCommitRunContext(ctx, OrchestrateCliArgs{Verb: fsm.VerbReset, Cwd: cwd}, id, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled reset returned %v, want context.Canceled", err)
	}
	if got != (CliResult{}) {
		t.Fatalf("pre-cancelled reset answered %+v", got)
	}
	if !reflect.DeepEqual(before, statusTree(t, cwd)) {
		t.Fatal("the pre-cancelled reset changed the workspace")
	}
}

// TestOrchestrateInterruptLiveContextStillWrites is the control: a live context changes nothing.
func TestOrchestrateInterruptLiveContextStillWrites(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "interrupt-live"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","sessionId":"interrupt-live"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := orchestrateCommitRunContext(ctx, OrchestrateCliArgs{Verb: fsm.VerbReset, Cwd: cwd}, id, nil)
	if err != nil {
		t.Fatalf("live reset returned %v", err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "orchestrate reset: current=B -> IDLE") {
		t.Fatalf("live reset: %+v", got)
	}
}

// TestOrchestrateInterruptCancelledDuringTheGoalplanWait is the reviewer finding on the goalplan
// wait: the pre-write check runs before orchestrateCommitPublish, but a bound gated transition then
// waits in the goalplan lock, and a signal during that wait must still leave nothing written.
// The seam cancels the context inside the lock callback, immediately before the callback's first
// write, which is the window a real signal lands in.
func TestOrchestrateInterruptCancelledDuringTheGoalplanWait(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "interrupt-plan"
	orchestrateCommitPlan(t, cwd, id)
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seams := &orchestrateCommitSeams{lockGoalplan: orchestrateInterruptGoalplanLock(cancel)}
	got, err := orchestrateCommitRunContext(ctx, OrchestrateCliArgs{
		Verb: fsm.VerbC, Cwd: cwd,
		Attest: &attest.Attestation{From: state.PhaseB, To: state.PhaseC, Did: "built it", WorkPhaseID: "wp1"},
	}, id, seams)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a transition cancelled during the goalplan wait returned %v, want context.Canceled", err)
	}
	if got != (CliResult{}) {
		t.Fatalf("a transition cancelled during the goalplan wait answered %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseB {
		t.Fatal("the cancelled transition moved the session")
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the cancelled transition wrote a ledger row: %+v", rows)
	}
}

// orchestrateInterruptGoalplanLock is the real goalplan write lock with the invocation cancelled
// inside the callback: a signal that arrives while this process waited for the plan lock and lands
// before the callback writes.
func orchestrateInterruptGoalplanLock(cancel context.CancelFunc) orchestrateCommitLockFunc {
	return func(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
		return goalplan.WithGoalplanWriteLock(cwd, slug, func(plan *goalplan.Goalplan) (orchestrateCommitOutcome, error) {
			cancel()
			return fn(plan)
		}, nil)
	}
}

// TestOrchestrateInterruptCancelledDuringTheDcloseGoalplanWait is the D-close half of the same
// finding: the close takes its own goalplan lock, so a signal that arrives while the process waits
// there must leave the marker, the plan and the ledger untouched.
func TestOrchestrateInterruptCancelledDuringTheDcloseGoalplanWait(t *testing.T) {
	cwd, id := orchestrateDcloseTestCwd(t), "interrupt-dclose"
	orchestrateDcloseSeedAtC(t, cwd, id, "interrupt-dclose", goalplan.TaskDone)
	before := statusTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seam := orchestrateDcloseSeam{interrupt: cancel}
	got, err := orchestrateDcloseContext(ctx, cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a D close cancelled during the goalplan wait returned %v, want context.Canceled", err)
	}
	if got != (CliResult{}) {
		t.Fatalf("a D close cancelled during the goalplan wait answered %+v", got)
	}
	if !reflect.DeepEqual(before, statusTree(t, cwd)) {
		t.Fatal("the cancelled D close changed the workspace")
	}
}
