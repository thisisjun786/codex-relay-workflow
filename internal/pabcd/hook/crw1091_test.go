package hook

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1091: a total cap another Stop of the same turn announced between this Stop's first reading and the lock is read under
// the lock: the Stop releases silently and writes nothing (no second announcement, no advanced total).
func TestCRW1091ACapAnnouncedBeforeTheLockIsNotAnnouncedAgain(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTotal, s.StopBlockTurnID = StopMaxBlocksTotal, ptr(crw1086Turn) })
	var announced string
	lock := beforeLock(func(cwd, sessionID string) {
		s := state.ReadState(cwd, sessionID)
		s.StopBlockTotal, s.StopBlockCapNotified = StopMaxBlocksTotal+1, true
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		announced = stopStateBytes(t, cwd)
	})
	if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: crw1086Turn}, "linux", env, lock); a != (StopAnswer{}) {
		t.Errorf("a cap announced before the lock: %+v", a)
	}
	if stopStateBytes(t, cwd) != announced {
		t.Errorf("the Stop wrote over the announced cap")
	}
}
