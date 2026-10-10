package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1091 as the third verification round of 5888f791 judged it: the new-turn race of a prompt without turn_id, and the
// recovery a capped Stop still runs, checked on a real ledger row and plan-audit cleanup.

// A user prompt without turn_id that lands between an earlier Stop's first reading and its lock starts a new turn that Stop was
// not judged in: the earlier Stop releases and spends nothing of the new turn's budget, as a Stop of a stamped turn does when the
// stamp changes under it (CRW-1091 c2, the new-turn race). Red on 5888f791: the prompt cleared the total and left the stamp nil,
// so the earlier Stop read the cleared total inside the lock as the first Stop of the new turn and blocked. (A turn with no Stop
// counted yet is not rewritten by such a prompt, CRW-1159, so there the earlier Stop and the new turn's first read the same state.)
func TestCRW1091TurnlessPromptBeforeTheLockReleasesTheEarlierStop(t *testing.T) {
	phaseB := state.PhaseB
	for name, setup := range map[string]func(*state.State){
		"a spent budget": func(s *state.State) {
			s.StopBlockPhase, s.StopBlockCount, s.StopBlockTotal = &phaseB, StopMaxBlocks+1, 4
		},
		"a budget still open": func(s *state.State) {
			s.StopBlockPhase, s.StopBlockCount, s.StopBlockTotal = &phaseB, 2, 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			cwd, env := stopRig(t, "active")
			stopInFlight(t, cwd, state.PhaseB, setup)
			lock := beforeLock(func(cwd, _ string) { crwTurnlessPrompt(cwd, env, "next question") })
			if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, lock); a != (StopAnswer{}) {
				t.Fatalf("the earlier turn's Stop answered in the new turn: %+v", a)
			}
			if s := state.ReadState(cwd, stopSID); s.StopBlockTotal != 0 {
				t.Fatalf("the earlier turn's Stop spent the new turn's budget: total %v", s.StopBlockTotal)
			}
			if a := crwTurnlessStop(cwd, env); !crw1086IsBlock(a) {
				t.Fatalf("the new turn's first Stop released: %+v", a)
			}
			if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
				t.Errorf("the new turn's count %v total %v, want 1 and 1", s.StopBlockCount, s.StopBlockTotal)
			}
		})
	}
}

// crwCapEvent prepares, for the session's current state, the published A>B event of a bound session with its ledger row and the
// plan-audit cleanup of the open rounds r1 and r2 of plan demo, as orchestrate leaves it when it stops before the drain.
func crwCapEvent(t *testing.T, cwd string) {
	t.Helper()
	post := state.ReadState(cwd, stopSID)
	pre := post
	pre.Phase = state.PhaseA
	from := state.PhaseA
	row := &state.LedgerEntry{TS: cleanupStamp, SessionID: stopSID, From: &from, To: state.PhaseB, Reason: "chat", Actor: "human"}
	payload, err := json.Marshal(PlanAuditCleanup{Kind: PlanAuditCleanupKind, ID: "pac-cap", Slug: "demo", Epoch: "e-new", Rounds: []string{"r1", "r2"}, ClosedAt: cleanupStamp})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := state.NewLedgerEvent(cwd, pre, post, row, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
}

// crwCapPlan writes plan demo with the session's open plan_audit rounds r1 and r2 of an earlier epoch.
func crwCapPlan(t *testing.T, cwd string) {
	t.Helper()
	plan := cleanupPlan(t, cwd, "demo")
	for i := range plan.ReviewRounds {
		plan.ReviewRounds[i].OwnerSessionID = stopSID
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// crwSessionLedgerRows is the session ledger's lines that record the A>B row.
func crwSessionLedgerRows(t *testing.T, cwd string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, state.LedgerFile))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"sessionId":"`+stopSID+`"`) && strings.Contains(line, `"to":"B"`) {
			n++
		}
	}
	return n
}

// crwBlockPlanLedger makes the plan ledger of demo a directory, so a cleanup's row append fails; the returned func undoes it.
func crwBlockPlanLedger(t *testing.T, cwd string) func() {
	t.Helper()
	dir, err := goalplan.GoalplanDir(cwd, "demo")
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, goalplan.GoalplanLedgerFile)
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(ledger); err != nil {
			t.Fatal(err)
		}
	}
}

// A capped turn's Stop finishes what an earlier writer left pending, really: the transition's ledger row is recorded exactly once
// and the plan-audit rounds are closed with their superseded rows, also when the first capped Stop's cleanup fails and the next
// capped Stop retries it (CRW-1091 d1, strengthening TestCRW1091CappedStopStillRecoversPendingEvents).
func TestCRW1091CappedStopRecordsTheRowAndTheCleanupOfAPendingEvent(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) {
		s.StopBlockTotal, s.StopBlockCapNotified, s.StopBlockTurnID = StopMaxBlocksTotal+1, true, ptr(crw1086Turn)
	})
	crwCapPlan(t, cwd)
	crwCapEvent(t, cwd)
	before := stopStateBytes(t, cwd)
	unblock := crwBlockPlanLedger(t, cwd)
	if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("a capped Stop answered %+v", a)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, stopSID); len(pending) != 1 {
		t.Fatalf("a failed cleanup retired the event: %d pending", len(pending))
	}
	unblock()
	if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("the retrying capped Stop answered %+v", a)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, stopSID); len(pending) != 0 {
		t.Errorf("the capped Stop left %d events pending", len(pending))
	}
	if n := crwSessionLedgerRows(t, cwd); n != 1 {
		t.Errorf("the session ledger holds the transition row %d times, want once", n)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 2 {
		t.Errorf("superseded rows %v, want r1 and r2", rows)
	}
	for _, r := range goalplan.ReadGoalplan(cwd, "demo").ReviewRounds {
		if r.Status != goalplan.ReviewInconclusive {
			t.Errorf("round %s is %s, want closed by the cleanup", r.RoundID, r.Status)
		}
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("recovery rewrote the state of the capped turn")
	}
}

// The same under the lock: a cap announced between this Stop's first reading and its lock (CRW-1091 d3, strengthening
// TestCRW1091ACapAnnouncedBeforeTheLockStillRecoversPendingEvents).
func TestCRW1091ACapAnnouncedBeforeTheLockRecordsTheRowAndTheCleanup(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTotal, s.StopBlockTurnID = StopMaxBlocksTotal, ptr(crw1086Turn) })
	crwCapPlan(t, cwd)
	var announced string
	lock := beforeLock(func(cwd, sessionID string) {
		s := state.ReadState(cwd, sessionID)
		s.StopBlockTotal, s.StopBlockCapNotified = StopMaxBlocksTotal+1, true
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		crwCapEvent(t, cwd)
		announced = stopStateBytes(t, cwd)
	})
	if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: crw1086Turn}, "linux", env, lock); a != (StopAnswer{}) {
		t.Errorf("a cap announced before the lock: %+v", a)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, stopSID); len(pending) != 0 {
		t.Errorf("the Stop left %d events pending", len(pending))
	}
	if n := crwSessionLedgerRows(t, cwd); n != 1 {
		t.Errorf("the session ledger holds the transition row %d times, want once", n)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 2 {
		t.Errorf("superseded rows %v, want r1 and r2", rows)
	}
	if stopStateBytes(t, cwd) != announced {
		t.Errorf("the Stop wrote over the announced cap")
	}
}
