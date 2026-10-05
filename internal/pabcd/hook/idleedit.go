package hook

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// NudgeEvery is the 0-indexed interval: calls 1, 6, 11, ... of eligible edits advise.
const NudgeEvery = 5

// IdleEditAdvisory resolves the command only when the advisory is emitted.
func IdleEditAdvisory(sessionID string, env host.LookupEnv) string {
	inv, err := host.Invocation(env)
	if err != nil {
		inv = "crw"
	}
	return strings.Join([]string{
		"[crw IDLE-EDIT] You are editing files while the PABCD FSM is un-armed",
		"but this session expects loop/goal work. If this edit belongs to the loop,",
		"arm first: `" + inv + " pabcd orchestrate status --session " + sessionID + "` -> enter P ->",
		"advance edges with --attest (one work-phase = one full PABCD cycle).",
		"C0 edits need no automatic devlog record. C1 edits record only in an existing owning unit.",
		"Do not create a unit just for a fast-path edit; explicit user/release record requirements remain controlling",
		"(UNIT-RESIDENCE-01, dev §0.1).",
	}, " ")
}

// unsafeCounterWrite marks states whose read-back value cannot preserve their records.
func unsafeCounterWrite(s state.State) bool {
	return s.UnverifiedCorrupt || (s.DcloseRecovery != nil && s.DcloseRecovery.Legacy)
}

// rewriteGuardLossy says whether writing fresh's unverified-subagent list over the session file would change a record the file
// stores (a receipt past 256 units, a field of the wrong type; state.RewriteKeepsUnverified), which the reader's flag does not show.
// It runs only on the state read inside the lock: the state read before the goal lookup may be older than the file, and comparing it
// would refuse a record a participating writer stored meanwhile. A file that does not exist stores nothing; one that cannot be read
// now is refused.
func rewriteGuardLossy(cwd, sessionID string, kept []state.UnverifiedSubagent) bool {
	raw, err := os.ReadFile(state.StatePath(cwd, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return len(kept) != 0
	}
	return err != nil || !state.RewriteKeepsUnverified(raw, kept)
}

// HandleIdleEditAdvisory never denies. Like idle-edit.ts it neither trims JSON nor checks its event.
// The counter write intentionally differs: unreadable/lossy records are preserved and a fresh read
// under the existing lock prevents overwriting a participating writer's intervening records.
func HandleIdleEditAdvisory(raw string, env host.LookupEnv) string {
	p := editObject(raw)
	tool, _ := p["tool_name"].(string)
	sid, _ := p["session_id"].(string)
	cwd, _ := p["cwd"].(string)
	if !editTool(tool) || sid == "" || cwd == "" {
		return ""
	}
	s, unreadable := state.ReadStateStrict(cwd, sid)
	if unreadable || s.Phase != state.PhaseIdle || s.OrchestrationActive {
		return ""
	}
	armed := s.LoopArmSeen
	if !armed {
		if db, err := host.GoalsDBPath(env); err == nil {
			armed = host.GoalActiveStatus(sid, db) == host.GoalActive
		}
	}
	if !armed {
		return ""
	}
	count := s.IdleEditNudges
	stillIdle := true
	if !unsafeCounterWrite(s) {
		// Cosmetic failures stay fail-open; an unavailable lock uses the first read's count.
		_ = state.WithSessionLock(cwd, sid, func() error {
			fresh, bad := state.ReadStateStrict(cwd, sid)
			if bad || unsafeCounterWrite(fresh) || rewriteGuardLossy(cwd, sid, fresh.UnverifiedSubagents) {
				return nil
			}
			if fresh.Phase != state.PhaseIdle || fresh.OrchestrationActive {
				stillIdle = false
				return nil
			}
			count = fresh.IdleEditNudges
			fresh.IdleEditNudges++
			return state.WriteState(cwd, fresh)
		})
	}
	if !stillIdle || math.Mod(count, NudgeEvery) != 0 {
		return ""
	}
	return editAnswer("allow", "", IdleEditAdvisory(sid, env))
}
