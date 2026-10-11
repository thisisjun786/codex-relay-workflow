package hook

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-145 (verification round 1): the source-identity investigation behind the goal-idle block (git status, hashing the dirty
// files) is slow work, so it is never done while the session lock is held, and it is bounded by the hook's budget.

// The investigation runs outside the session lock: another writer of the same session takes the lock while it is running.
func TestStopGoalIdleSourceCheckRunsOutsideTheSessionLock(t *testing.T) {
	cwd, env := stopIdleRig(t, true)
	var heldDuring error
	called := 0
	old := stopCheckBound
	stopCheckBound = func(ctx context.Context, c, s string) sourcesession.GateResult {
		called++
		heldDuring = state.WithSessionLock(c, s, func() error { return nil })
		return old(ctx, c, s)
	}
	defer func() { stopCheckBound = old }()
	got := stopBlockReason(t, stopRun(cwd, env))
	if called != 1 {
		t.Fatalf("the source check ran %d times, want once", called)
	}
	if heldDuring != nil {
		t.Errorf("another writer of the session could not take the lock while the source was investigated: %v", heldDuring)
	}
	if !strings.Contains(got, "orchestrate P --session") {
		t.Errorf("a git workspace keeps the arming option: %q", got)
	}
}

// A source investigation that outlasts the budget is no evidence that arming is impossible: the Stop answers within the budget
// with the oracle's arming option, and the investigation is cancelled at the deadline and has ended when the Stop answers (its
// git is not left running behind the hook).
func TestStopGoalIdleSourceCheckIsBoundedByTheHookBudget(t *testing.T) {
	cwd, env := stopIdleRig(t, true)
	var sawCancel, returned atomic.Bool
	oldCheck, oldBudget := stopCheckBound, stopSourceBudget
	stopCheckBound = func(ctx context.Context, _, _ string) sourcesession.GateResult {
		<-ctx.Done() // a probe that honours its context ends with it
		sawCancel.Store(true)
		time.Sleep(50 * time.Millisecond) // reaping its git takes a moment
		returned.Store(true)
		return sourcesession.GateResult{Cause: sourcesession.CauseProbeFailed, Probe: ctx.Err()}
	}
	stopSourceBudget = 100 * time.Millisecond
	defer func() { stopCheckBound, stopSourceBudget = oldCheck, oldBudget }()
	start := time.Now()
	got := stopBlockReason(t, stopRun(cwd, env))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the Stop took %v with a stuck source investigation", d)
	}
	if !sawCancel.Load() || !returned.Load() {
		t.Errorf("the investigation must be cancelled at the deadline and have ended when the Stop answers: cancelled=%v ended=%v", sawCancel.Load(), returned.Load())
	}
	if !strings.Contains(got, "orchestrate P --session") || strings.Contains(got, "cannot be armed") {
		t.Errorf("an unfinished investigation must not claim arming is impossible: %q", got)
	}
}

// An investigation that ignores its cancellation cannot hold the Stop past the grace it is given.
func TestStopGoalIdleSourceCheckGraceIsBounded(t *testing.T) {
	cwd, env := stopIdleRig(t, true)
	release := make(chan struct{})
	oldCheck, oldBudget, oldGrace := stopCheckBound, stopSourceBudget, stopSourceGrace
	stopCheckBound = func(context.Context, string, string) sourcesession.GateResult {
		<-release
		return sourcesession.GateResult{}
	}
	stopSourceBudget, stopSourceGrace = 100*time.Millisecond, 100*time.Millisecond
	defer func() {
		close(release)
		stopCheckBound, stopSourceBudget, stopSourceGrace = oldCheck, oldBudget, oldGrace
	}()
	start := time.Now()
	got := stopBlockReason(t, stopRun(cwd, env))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the Stop took %v with an investigation that ignores its cancellation", d)
	}
	if !strings.Contains(got, "orchestrate P --session") {
		t.Errorf("the oracle's arming option is offered: %q", got)
	}
}

// A verdict that arrives as the deadline passes is not a verdict: a cancelled probe reports its cancellation as a refusal.
func TestStopGoalIdleSourceCheckVerdictAfterTheDeadlineIsIgnored(t *testing.T) {
	cwd, env := stopIdleRig(t, true)
	oldCheck, oldBudget := stopCheckBound, stopSourceBudget
	stopCheckBound = func(ctx context.Context, _, _ string) sourcesession.GateResult {
		<-ctx.Done()
		return sourcesession.GateResult{Cause: sourcesession.CauseBindingError, Reason: "Cannot resolve source Git worktree identity."}
	}
	stopSourceBudget = 50 * time.Millisecond
	defer func() { stopCheckBound, stopSourceBudget = oldCheck, oldBudget }()
	if got := stopBlockReason(t, stopRun(cwd, env)); strings.Contains(got, "cannot be armed") || !strings.Contains(got, "orchestrate P --session") {
		t.Errorf("a refusal that is a cancellation must not be worded: %q", got)
	}
}
