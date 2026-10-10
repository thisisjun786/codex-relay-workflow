package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1086: the per-phase budget latches when it is spent. Another Stop evaluator (bg-wake, a completion gate) can keep the
// host turn alive after the PABCD leg released, so the host calls Stop again in the same turn at the same phase; the oracle
// reset the counter on the release and blocked again at the fifth Stop (18 of the first 24). The latch holds for the same user
// turn, phase and active work phase, and only real progress or a new user turn rearms it.

const crw1086Turn = "t1"

// crw1086Stop runs the PABCD Stop leg of the stamped turn crw1086Turn, as the host does while some evaluator blocks.
func crw1086Stop(cwd string, env host.LookupEnv) StopAnswer {
	return StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: crw1086Turn}, "linux", env)
}

func crw1086IsBlock(a StopAnswer) bool { return strings.Contains(a.Stdout, `"decision":"block"`) }

// crw1086Exhaust spends the per-phase budget of the stamped turn: three blocks, then a release.
func crw1086Exhaust(t *testing.T, cwd string, env host.LookupEnv) {
	t.Helper()
	for i := 1; i <= StopMaxBlocks; i++ {
		if a := crw1086Stop(cwd, env); !crw1086IsBlock(a) {
			t.Fatalf("Stop %d must block: %+v", i, a)
		}
	}
	if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("the fourth Stop must release: %+v", a)
	}
}

// End condition 1: at a fixed B in one turn, while a fake second evaluator keeps blocking (so the host keeps calling Stop),
// the PABCD contribution after its first three blocks stays empty until the turn's total cap announces itself once.
func TestCRW1086PhaseCapLatchHoldsWhileAnotherEvaluatorContinues(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTurnID = ptr(crw1086Turn) })
	secondEvaluatorBlocks := func() bool { return true } // a bg-wake completion that is never delivered
	var pabcd []string
	for stop := 1; stop <= StopMaxBlocksTotal+1; stop++ {
		a := crw1086Stop(cwd, env)
		switch {
		case crw1086IsBlock(a):
			pabcd = append(pabcd, "block")
		case a == (StopAnswer{}):
			pabcd = append(pabcd, "")
		case a.Stdout == stopSystemMessage(stopTotalCapMessage):
			pabcd = append(pabcd, "cap")
		default:
			t.Fatalf("Stop %d: unexpected answer %+v", stop, a)
		}
		if !crw1086IsBlock(a) && !secondEvaluatorBlocks() {
			break
		}
	}
	for i, got := range pabcd {
		want := ""
		switch {
		case i < StopMaxBlocks:
			want = "block"
		case i == StopMaxBlocksTotal:
			want = "cap"
		}
		if got != want {
			t.Errorf("Stop %d of the same turn at B: got %q, want %q (all: %q)", i+1, got, want, pabcd)
		}
	}
	s := state.ReadState(cwd, stopSID)
	if s.StopBlockTotal != StopMaxBlocksTotal+1 || !s.StopBlockCapNotified {
		t.Errorf("the total still counts every Stop of the turn: total %v notified %v", s.StopBlockTotal, s.StopBlockCapNotified)
	}
}

// End condition 2: a phase transition, an active work-phase switch, a new improving metric and a new user turn each rearm the
// spent budget; until then the latch holds.
func TestCRW1086RealProgressRearmsTheLatch(t *testing.T) {
	plan := func(t *testing.T, cwd string) {
		stopWritePlan(t, cwd, "two", func(p *goalplan.Goalplan) {
			p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress), stopWorkPhase("wp2", "Two", goalplan.WorkPhasePending)}
		})
	}
	record := func(t *testing.T, cwd string, value float64) {
		t.Helper()
		if _, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{SessionID: stopSID, MetricName: "score", Value: value, Source: metric.OperatorEntered}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		rearm  func(t *testing.T, cwd string)
		turnID string
	}{
		{"phase transition", func(t *testing.T, cwd string) {
			s := state.ReadState(cwd, stopSID)
			s.Phase = state.PhaseC
			if err := state.WriteState(cwd, s); err != nil {
				t.Fatal(err)
			}
		}, crw1086Turn},
		{"active work-phase switch", func(t *testing.T, cwd string) {
			p := goalplan.ReadGoalplan(cwd, "two")
			p.WorkPhases[0].Status, p.WorkPhases[1].Status = goalplan.WorkPhaseDone, goalplan.WorkPhaseInProgress
			if err := goalplan.WriteGoalplan(cwd, p); err != nil {
				t.Fatal(err)
			}
		}, crw1086Turn},
		{"new improving metric", func(t *testing.T, cwd string) { record(t, cwd, 2) }, crw1086Turn},
		{"new user turn", func(t *testing.T, cwd string) {
			// what UserPromptSubmit's turn stamp writes (prompt_submit.go)
			s := state.ReadState(cwd, stopSID)
			s.StopBlockTotal, s.StopBlockTurnID, s.StopBlockCapNotified = 0, ptr("t2"), false
			if err := state.WriteState(cwd, s); err != nil {
				t.Fatal(err)
			}
		}, "t2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd, env := stopRig(t, "active")
			stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug, s.StopBlockTurnID = "two", ptr(crw1086Turn) })
			plan(t, cwd)
			record(t, cwd, 1)
			crw1086Exhaust(t, cwd, env)
			for i := 0; i < 3; i++ {
				if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
					t.Fatalf("the latch did not hold at release %d: %+v", i+2, a)
				}
			}
			c.rearm(t, cwd)
			if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: c.turnID}, "linux", env); !crw1086IsBlock(a) {
				t.Fatalf("%s did not rearm: %+v", c.name, a)
			}
			if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 {
				t.Errorf("%s: the rearmed count is %v, want 1", c.name, s.StopBlockCount)
			}
		})
	}
}

// End condition 3: a pause, an approval wait, a compaction's recovery window and a stale turn still release while the budget
// is latched, write nothing, and leave the latch as it was.
func TestCRW1086ReleasesStillReleaseWhileLatched(t *testing.T) {
	type world struct {
		cwd string
		env host.LookupEnv
	}
	cases := []struct {
		name string
		run  func(t *testing.T, w world) StopAnswer
		undo func(t *testing.T, w world)
	}{
		{"paused goal", func(t *testing.T, w world) StopAnswer {
			stopSetGoalStatus(t, w.env, "paused")
			return crw1086Stop(w.cwd, w.env)
		}, func(t *testing.T, w world) { stopSetGoalStatus(t, w.env, "active") }},
		{"approval wait", func(t *testing.T, w world) StopAnswer {
			stopAskDecision(t, w.cwd, "plan")
			return crw1086Stop(w.cwd, w.env)
		}, func(t *testing.T, w world) {
			stopWritePlan(t, w.cwd, "plan", func(p *goalplan.Goalplan) {
				p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress)}
			})
		}},
		{"context pressure", func(t *testing.T, w world) StopAnswer {
			transcript := filepath.Join(w.cwd, "transcript.jsonl")
			if err := os.WriteFile(transcript, []byte(`{"type":"compacted","payload":{"message":"","replacement_history":[]}}`+"\n"), 0o666); err != nil {
				t.Fatal(err)
			}
			return StopHandle(StopPayload{Cwd: w.cwd, SessionID: stopSID, TurnID: crw1086Turn, TranscriptPath: transcript}, "linux", w.env)
		}, func(t *testing.T, w world) { compactionRecoveryEnd(w.cwd, stopSID) }},
		{"stale turn", func(t *testing.T, w world) StopAnswer {
			return StopHandle(StopPayload{Cwd: w.cwd, SessionID: stopSID, TurnID: "t0"}, "linux", w.env)
		}, func(t *testing.T, w world) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd, env := stopRig(t, "active")
			stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug, s.StopBlockTurnID = "plan", ptr(crw1086Turn) })
			stopWritePlan(t, cwd, "plan", func(p *goalplan.Goalplan) {
				p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress)}
			})
			crw1086Exhaust(t, cwd, env)
			w := world{cwd, env}
			before := stopStateBytes(t, cwd)
			if a := c.run(t, w); a != (StopAnswer{}) {
				t.Errorf("%s while latched: %+v", c.name, a)
			}
			if stopStateBytes(t, cwd) != before {
				t.Errorf("%s while latched wrote the state", c.name)
			}
			c.undo(t, w)
			if a := crw1086Stop(cwd, env); a != (StopAnswer{}) {
				t.Errorf("after %s the latch no longer holds: %+v", c.name, a)
			}
		})
	}
}
