package hook

import (
	"strings"
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
	stopCheckBound = func(c, s string) sourcesession.GateResult {
		called++
		heldDuring = state.WithSessionLock(c, s, func() error { return nil })
		return old(c, s)
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
// with the oracle's arming option.
func TestStopGoalIdleSourceCheckIsBoundedByTheHookBudget(t *testing.T) {
	cwd, env := stopIdleRig(t, true)
	release := make(chan struct{})
	oldCheck, oldBudget := stopCheckBound, stopSourceBudget
	stopCheckBound = func(string, string) sourcesession.GateResult { <-release; return sourcesession.GateResult{} }
	stopSourceBudget = 100 * time.Millisecond
	defer func() { close(release); stopCheckBound, stopSourceBudget = oldCheck, oldBudget }()
	start := time.Now()
	got := stopBlockReason(t, stopRun(cwd, env))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the Stop took %v with a stuck source investigation", d)
	}
	if !strings.Contains(got, "orchestrate P --session") || strings.Contains(got, "cannot be armed") {
		t.Errorf("an unfinished investigation must not claim arming is impossible: %q", got)
	}
}
