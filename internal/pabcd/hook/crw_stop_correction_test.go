package hook

import (
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The post-evaluation corrections of the lane's Stop work (CRW-1086, CRW-1088, CRW-1091, CRW-1107, evaluation of 54c3bf98).

// crwTurnlessStop is a Stop whose payload carries no turn_id, an input the payload parser accepts.
func crwTurnlessStop(cwd string, env host.LookupEnv) StopAnswer { return stopRun(cwd, env) }

// crwTurnlessPrompt is an ordinary user prompt whose payload carries no turn_id.
func crwTurnlessPrompt(cwd string, env host.LookupEnv, prompt string) {
	PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: stopSID, Prompt: prompt, PabcdEnabled: true}, "linux", env)
}

// A user prompt without turn_id is a new user turn like any other: the spent per-phase budget and an announced total cap are
// rearmed by the prompt itself, not only by a prompt that carries a turn id (CRW-1086 d1, CRW-1088 d1, CRW-1091 d2, CRW-1107 d1).
func TestCRW1086TurnlessPromptRearmsTheBudget(t *testing.T) {
	t.Run("phase budget", func(t *testing.T) {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB)
		for i := 1; i <= StopMaxBlocks; i++ {
			if a := crwTurnlessStop(cwd, env); !crw1086IsBlock(a) {
				t.Fatalf("Stop %d must block: %+v", i, a)
			}
		}
		if a := crwTurnlessStop(cwd, env); a != (StopAnswer{}) {
			t.Fatalf("the fourth Stop must release: %+v", a)
		}
		if a := crwTurnlessStop(cwd, env); a != (StopAnswer{}) {
			t.Fatalf("the latch must hold inside the turn: %+v", a)
		}
		crwTurnlessPrompt(cwd, env, "Summarise the README.")
		if a := crwTurnlessStop(cwd, env); !crw1086IsBlock(a) {
			t.Fatalf("the Stop after a new user prompt without turn_id released: %+v", a)
		}
		if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
			t.Errorf("the new turn's count %v total %v, want 1 and 1", s.StopBlockCount, s.StopBlockTotal)
		}
	})
	t.Run("goal-idle block", func(t *testing.T) {
		cwd, env := stopRig(t, "active")
		sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "export" })
		stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
			p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "Exporter", goalplan.WorkPhasePending)}
		})
		for i := 1; i <= StopMaxBlocks; i++ {
			if a := crwTurnlessStop(cwd, env); !crw1086IsBlock(a) {
				t.Fatalf("idle Stop %d must block: %+v", i, a)
			}
		}
		if a := crwTurnlessStop(cwd, env); a != (StopAnswer{}) {
			t.Fatalf("the fourth idle Stop must release: %+v", a)
		}
		crwTurnlessPrompt(cwd, env, "Continue this task.")
		if a := crwTurnlessStop(cwd, env); !crw1086IsBlock(a) {
			t.Fatalf("the idle Stop after a new user prompt without turn_id released: %+v", a)
		}
	})
	t.Run("announced total cap", func(t *testing.T) {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTotal = StopMaxBlocksTotal })
		if a := crwTurnlessStop(cwd, env); a.Stdout != stopSystemMessage(stopTotalCapMessage) {
			t.Fatalf("the cap must be announced: %+v", a)
		}
		if a := crwTurnlessStop(cwd, env); a != (StopAnswer{}) {
			t.Fatalf("after the announcement the turn is over: %+v", a)
		}
		crwTurnlessPrompt(cwd, env, "keep going")
		if a := crwTurnlessStop(cwd, env); !crw1086IsBlock(a) {
			t.Fatalf("a new user prompt without turn_id left the announced cap standing: %+v", a)
		}
	})
	t.Run("a stamped turn id is dropped with the stamp", func(t *testing.T) {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTurnID, s.StopBlockTotal = ptr("t1"), 3 })
		crwTurnlessPrompt(cwd, env, "carry on")
		if s := state.ReadState(cwd, stopSID); s.StopBlockTurnID != nil || s.StopBlockTotal != 0 {
			t.Errorf("turn %v total %v, want none and 0", s.StopBlockTurnID, s.StopBlockTotal)
		}
		if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: "t2"}, "linux", env); !crw1086IsBlock(a) {
			t.Errorf("the next turn's Stop released: %+v", a)
		}
	})
	t.Run("a prompt with nothing to rearm writes nothing", func(t *testing.T) {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB)
		before := stopStateBytes(t, cwd)
		crwTurnlessPrompt(cwd, env, "hello")
		if stopStateBytes(t, cwd) != before {
			t.Errorf("a turnless prompt rewrote a state with nothing to rearm")
		}
	})
}

// An unreadable bound goalplan is not a work-phase switch: the spent budget stays spent while the plan cannot be read and
// when it reads again (CRW-1086 d2).
func TestCRW1086UnreadablePlanDoesNotRechargeTheBudget(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug, s.StopBlockTurnID = "two", ptr(crw1086Turn) })
	stopWritePlan(t, cwd, "two", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress)}
	})
	crw1086Exhaust(t, cwd, env)
	dir, err := goalplan.GoalplanDir(cwd, "two")
	if err != nil {
		t.Fatal(err)
	}
	away := dir + ".away"
	if err := os.Rename(dir, away); err != nil {
		t.Fatal(err)
	}
	if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("an unreadable plan recharged the budget: %+v", a)
	}
	if err := os.Rename(away, dir); err != nil {
		t.Fatal(err)
	}
	if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("the plan reading again recharged the budget: %+v", a)
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockCount != StopMaxBlocks+1 {
		t.Errorf("count %v, want the spent latch %d", s.StopBlockCount, StopMaxBlocks+1)
	}
}

// One evaluation window asks for divergence once even when the active work phase leaves it and comes back (CRW-1088 d2).
func TestCRW1088WindowRevisitedAfterAWorkPhaseSwitchIsNotAskedAgain(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "phases" })
	stopWritePlan(t, cwd, "phases", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp-a", "A", goalplan.WorkPhaseInProgress), stopWorkPhase("wp-b", "B", goalplan.WorkPhaseInProgress)}
	})
	if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
		t.Fatal(err)
	}
	for _, wp := range []string{"wp-a", "wp-a", "wp-b", "wp-b"} {
		crw1088Record(t, cwd, "score", 1, ptr(wp))
	}
	activate := func(id string) {
		t.Helper()
		p := goalplan.ReadGoalplan(cwd, "phases")
		p.ActiveWorkPhaseID = ptr(id)
		if err := goalplan.WriteGoalplan(cwd, p); err != nil {
			t.Fatal(err)
		}
	}
	kind := func() string {
		t.Helper()
		if strings.HasPrefix(stopBlockReason(t, stopRun(cwd, env)), "[crw — objective plateau]") {
			return "plateau"
		}
		return "continue"
	}
	var got []string
	for _, id := range []string{"wp-a", "wp-b", "wp-a", "wp-b"} {
		activate(id)
		got = append(got, kind())
	}
	if want := []string{"plateau", "plateau", "continue", "continue"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("answers over the visits wp-a, wp-b, wp-a, wp-b: %v, want %v", got, want)
	}
	activate("wp-a")
	crw1088Record(t, cwd, "score", 1, ptr("wp-a")) // new evaluation evidence for wp-a: a new window
	if k := kind(); k != "plateau" {
		t.Errorf("a new row of the revisited window: %s, want plateau", k)
	}
}

// A capped turn's Stop still finishes the ledger events and plan-audit cleanups an earlier writer left pending, as it did before the
// cap fast path, and still reads no plan and no ledger when nothing is pending (CRW-1091 d1).
func TestCRW1091CappedStopStillRecoversPendingEvents(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) {
		s.StopBlockTotal, s.StopBlockCapNotified, s.StopBlockTurnID = StopMaxBlocksTotal+1, true, ptr(crw1086Turn)
	})
	post := state.ReadState(cwd, stopSID)
	pre := post
	pre.Phase = state.PhaseA
	ev, err := state.NewLedgerEvent(cwd, pre, post, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, stopSID); len(pending) != 1 {
		t.Fatalf("setup: %d pending events", len(pending))
	}
	before := stopStateBytes(t, cwd)
	if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("a capped Stop answered %+v", a)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, stopSID); len(pending) != 0 {
		t.Errorf("the capped Stop left %d events pending", len(pending))
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("recovery rewrote the state of the capped turn")
	}
}

// The same under the lock: a cap that another Stop of the turn announced between this Stop's first reading and the lock does not
// keep this Stop from finishing a pending event (the recovery runs before the fresh cap is judged), and it still announces and
// advances nothing (CRW-1091 d3).
func TestCRW1091ACapAnnouncedBeforeTheLockStillRecoversPendingEvents(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTotal, s.StopBlockTurnID = StopMaxBlocksTotal, ptr(crw1086Turn) })
	var announced string
	lock := beforeLock(func(cwd, sessionID string) {
		s := state.ReadState(cwd, sessionID)
		s.StopBlockTotal, s.StopBlockCapNotified = StopMaxBlocksTotal+1, true
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		post := state.ReadState(cwd, sessionID)
		pre := post
		pre.Phase = state.PhaseA
		ev, err := state.NewLedgerEvent(cwd, pre, post, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.PrepareLedgerEvent(cwd, ev); err != nil {
			t.Fatal(err)
		}
		announced = stopStateBytes(t, cwd)
	})
	if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: crw1086Turn}, "linux", env, lock); a != (StopAnswer{}) {
		t.Errorf("a cap announced before the lock: %+v", a)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, stopSID); len(pending) != 0 {
		t.Errorf("the Stop left %d events pending", len(pending))
	}
	if stopStateBytes(t, cwd) != announced {
		t.Errorf("the Stop wrote over the announced cap")
	}
}
