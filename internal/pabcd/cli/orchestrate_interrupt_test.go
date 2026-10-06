package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
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
