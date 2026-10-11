package harness

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-386 (post-evaluation round 3, d1), through the wired Stop leg and a seeded registry: a registered parent or supervisor that
// took an explicit request to implement a task in this session (loopArmSeen) is released by neither the registry role nor the
// goal-idle block, between the work-phases of its own goalplan; the same seat without the request is.
func TestStopLegGoalIdleKeepsTheBlockForAnExplicitImplementer(t *testing.T) {
	for _, role := range []string{"parent", "supervisor"} {
		cwd, env := stopIdleSetup(t)
		seedStopRoles(t, false, role)
		s := state.ReadState(cwd, "s1")
		s.LoopArmSeen = true
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := stopHook(Legs(), payload("Stop", cwd, `,"turn_id":"t1","stop_hook_active":false,"last_assistant_message":"Done.","transcript_path":null`), env)
		if code != 0 || errOut != "" || !strings.HasPrefix(out, `{"decision":"block","reason":"[crw — goal continuation] A host goal is ACTIVE but no PABCD cycle is in flight.`) {
			t.Errorf("%s that implements a task here: want the goal-idle block, got %d %q %q", role, code, out, errOut)
		}
	}
}
