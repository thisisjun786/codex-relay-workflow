package hook

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-386 (post-evaluation round 3, d1): a registry role is what a session is bound as, not what it is doing now. An explicit
// request to implement a task in the current session (goal-mode.md; UserPromptSubmit keeps the recipe for it and sets loopArmSeen)
// makes a registered parent or supervisor the implementer of that task, so the goal-idle block that chains its next work-phase
// is the oracle's for it, between the cycles of its own goalplan as much as for any session. Only a coordinating seat that took
// no such request is released.
func TestStopGoalIdleBlockKeepsTheLoopForAnExplicitImplementer(t *testing.T) {
	oracleCwd, oracleEnv := stopIdleRig(t, true)
	oracle := stopBlockReason(t, stopRun(oracleCwd, oracleEnv))
	for _, role := range []PromptRole{PromptRoleParent, PromptRoleSupervisor} {
		// A session that took the current-task request: the oracle's block, and the budget is spent as for any session.
		cwd, env := stopIdleRig(t, true)
		sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.LoopArmSeen = true })
		a := StopHandleWithRole(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, (&roleSpy{role: role}).read)
		if got := stopBlockReason(t, a); got != oracle {
			t.Errorf("%s that implements a task here: not the oracle's block:\n got %q\nwant %q", role, got, oracle)
		}
		if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
			t.Errorf("%s that implements a task here: the block spends the budget once: %+v", role, s)
		}
		// The same seat without the request is still released, with no state write.
		cwd, env = stopIdleRig(t, true)
		before := stopStateBytes(t, cwd)
		if a := StopHandleWithRole(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, (&roleSpy{role: role}).read); a != (StopAnswer{}) {
			t.Errorf("%s without the request: want a release, got %+v", role, a)
		}
		if stopStateBytes(t, cwd) != before {
			t.Errorf("%s without the request: the release wrote the state", role)
		}
	}
}
