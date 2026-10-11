package affordance

import (
	"strings"
	"testing"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-386 (post-evaluation round 3, d1): a coordinating seat that took an explicit request to implement a task in the current
// session (loopArmSeen, set by the loop-arm recipe UserPromptSubmit keeps for it) is that task's implementer, so SessionStart
// and the restore after a compaction give it the loop pointer as they give every implementer; a seat without the request does not.
func TestImplementingSeatKeepsTheLoopPointer(t *testing.T) {
	for _, role := range []pabcdhook.PromptRole{pabcdhook.PromptRoleParent, pabcdhook.PromptRoleSupervisor} {
		for _, implements := range []bool{true, false} {
			ws := t.TempDir()
			seed(t, ws, 40)
			if implements {
				s := state.DefaultState("SESSION", "")
				s.LoopArmSeen = true
				if err := state.WriteState(ws, s); err != nil {
					t.Fatal(err)
				}
			}
			spy := &roleSpy{role: role}
			raw := payload(ws, "SessionStart", "SESSION", nil)
			start := contextOf(t, RunMapAffordanceSessionStartWithRole(raw, ws, testEnv, spy.read), "SessionStart")
			if has := strings.Contains(start, RenderLoopAffordance(testEnv)); has != implements {
				t.Errorf("%s implements=%v: SessionStart loop pointer present=%v", role, implements, has)
			}
			prompt := payload(ws, "UserPromptSubmit", "SESSION", nil)
			RunPostCompactAffordance(payload(ws, "PostCompact", "SESSION", nil))
			restore := contextOf(t, RunUserPromptAffordanceWithRole(prompt, testEnv, spy.read), "UserPromptSubmit")
			if has := strings.Contains(restore, RenderLoopAffordance(testEnv)); has != implements {
				t.Errorf("%s implements=%v: compaction restore loop pointer present=%v", role, implements, has)
			}
		}
	}
}
