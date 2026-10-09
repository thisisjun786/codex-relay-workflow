package hook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The Stop continuation cases: the normal cases of CXC v0.2.40 pabcd-state hook.ts handleStop (3c1459ac, the
// stop-checking-pabcd-continuation corpus fixtures and hook-continuation.test.ts), and the CRW counterexamples
// the leg answers for: a stalled loop, a recovery tail, a goal the user stopped, a session without a goal, a
// counter that cannot be written. Every case runs against a temporary cwd and a temporary goals database; the
// block texts are compared byte for byte after the declared name substitution, with the crw invocation fixed
// to the word CRW (CRW_BIN).

const stopSID = "rec-s1"

// stopRig is one case's world: the workspace and the host environment whose goals database holds status for
// the session ("" builds no database).
func stopRig(t *testing.T, status string) (string, host.LookupEnv) {
	t.Helper()
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"CRW_BIN": "CRW", "HOME": filepath.Join(dir, "home")}
	if status != "" {
		home := sessionHookGoalsDB(t, filepath.Join(dir, "codex"),
			"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
			"INSERT INTO thread_goals (thread_id, status, objective) VALUES ('"+stopSID+"', '"+status+"', 'ship the feature')")
		vars["CODEX_SQLITE_HOME"] = home
	} else {
		vars["CODEX_SQLITE_HOME"] = filepath.Join(dir, "codex-empty")
	}
	return cwd, sessionHookEnv(vars)
}

// stopInFlight writes a session state in flight at phase.
func stopInFlight(t *testing.T, cwd string, phase state.Phase, more ...func(*state.State)) {
	t.Helper()
	sessionHookStateFile(t, cwd, stopSID, func(s *state.State) {
		s.Phase, s.OrchestrationActive = phase, true
		for _, m := range more {
			m(s)
		}
	})
}

func stopRun(cwd string, env host.LookupEnv) StopAnswer {
	return StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env)
}

// stopBlockReason decodes a block answer, failing the case on anything else.
func stopBlockReason(t *testing.T, a StopAnswer) string {
	t.Helper()
	if !strings.HasSuffix(a.Stdout, "\n") || a.Context != "" {
		t.Fatalf("not a finished line: %+v", a)
	}
	var out struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(a.Stdout), &out); err != nil || out.Decision != "block" || out.Reason == "" {
		t.Fatalf("not a block: %q (%v)", a.Stdout, err)
	}
	return out.Reason
}

func stopStateBytes(t *testing.T, cwd string) string {
	t.Helper()
	b, err := os.ReadFile(state.StatePath(cwd, stopSID))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const stopTail = "\nC→D requires checkOutput+exitCode. D is not a resting state; close the cycle back to IDLE."

// The block text of every phase, byte for byte (hook.ts:1523-1537, 1577-1631 after R23, R33 and the cli table).
func TestStopBlockTextOfEveryPhase(t *testing.T) {
	head := func(p, label string) string {
		return "[crw — continue PABCD] You are mid-cycle at " + p + " (" + label + ") with an active goal.\n" +
			"Do the real work of this phase, then self-advance with the concrete next command:\n"
	}
	want := map[state.Phase]string{
		state.PhaseP: head("P", "PLAN") + "`CRW pabcd orchestrate A --session rec-s1 --attest '{\"from\":\"P\",\"to\":\"A\",\"did\":\"diff-level plan written with files and acceptance criteria\",\"planUnit\":\"devlog/_plan/YYMMDD_slug\",\"workPhaseId\":\"<bound goalplan only>\"}'`" + stopTail,
		state.PhaseA: head("A", "AUDIT") + "`CRW pabcd orchestrate B --session rec-s1 --attest '{\"from\":\"A\",\"to\":\"B\",\"did\":\"audit loop closed: blockers folded into plan\",\"auditOutput\":\"<reviewer verdict tail>\",\"auditVerdict\":\"pass|near-pass\",\"auditResidual\":\"<near-pass only: residual blockers + disposition>\",\"workPhaseId\":\"<bound goalplan only>\"}'`" + stopTail,
		state.PhaseB: head("B", "BUILD") + "`CRW pabcd orchestrate C --session rec-s1 --attest '{\"from\":\"B\",\"to\":\"C\",\"did\":\"implementation completed and verifier reviewed it\",\"workPhaseId\":\"<bound goalplan only>\"}'`" + stopTail,
		state.PhaseC: head("C", "CHECK") + "`CRW pabcd orchestrate D --session rec-s1 --attest '{\"from\":\"C\",\"to\":\"D\",\"did\":\"checks passed\",\"checkOutput\":\"<test tail>\",\"exitCode\":0,\"testReceiptPath\":\"<bound goalplan only: crw pabcd receipt test output path>\",\"workPhaseId\":\"<bound goalplan only>\"}'`" + stopTail,
		state.PhaseD: head("D", "DONE") + "`CRW pabcd orchestrate reset --session rec-s1` after the DONE summary is recorded" + stopTail,
	}
	for phase, text := range want {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, phase)
		if got := stopBlockReason(t, stopRun(cwd, env)); got != text {
			t.Errorf("phase %s:\n got %q\nwant %q", phase, got, text)
		}
	}
}

// The win32 spelling of the continuation command points at the attest file (hook.ts:1539-1553, 002 B1).
func TestStopWin32CommandsUseTheAttestFile(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	reason := stopBlockReason(t, StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "win32", env))
	want := "`'{\"from\":\"B\",\"to\":\"C\",\"did\":\"implementation completed and verifier reviewed it\",\"workPhaseId\":\"<bound goalplan only>\"}' | Set-Content -Encoding utf8 .crw/attest.json` then `CRW pabcd orchestrate C --session rec-s1 --attest-file .crw/attest.json`"
	if !strings.Contains(reason, "\n"+want+"\n") {
		t.Errorf("win32 reason %q", reason)
	}
}

// The counter is persisted with every block (the corpus fixture in_flight_with_active_goal_blocks).
func TestStopBlockPersistsTheCounter(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	stopBlockReason(t, stopRun(cwd, env))
	s := state.ReadState(cwd, stopSID)
	if s.StopBlockPhase == nil || *s.StopBlockPhase != state.PhaseB || s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
		t.Errorf("counter after one block: %+v %v %v", s.StopBlockPhase, s.StopBlockCount, s.StopBlockTotal)
	}
	if s.Phase != state.PhaseB || !s.OrchestrationActive {
		t.Errorf("Stop moved the cycle: %s %v", s.Phase, s.OrchestrationActive)
	}
}

// Stall: three consecutive blocks at one phase, then a release that recharges the budget (stop_budget_three_per_phase).
func TestStopStallReleasesAfterThreeBlocksPerPhase(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseA)
	for i := 1; i <= 3; i++ {
		stopBlockReason(t, stopRun(cwd, env))
		if s := state.ReadState(cwd, stopSID); s.StopBlockCount != float64(i) {
			t.Fatalf("block %d: count %v", i, s.StopBlockCount)
		}
	}
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Fatalf("the fourth Stop must release, got %+v", a)
	}
	s := state.ReadState(cwd, stopSID)
	if s.StopBlockPhase != nil || s.StopBlockCount != 0 || s.StopBlockTotal != 4 {
		t.Errorf("after the release: phase %v count %v total %v", s.StopBlockPhase, s.StopBlockCount, s.StopBlockTotal)
	}
}

// Progress recharges the per-phase budget: a transition or a work-phase switch between two Stops (hook.ts:1465-1490).
func TestStopProgressRechargesTheBudget(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseA)
	for i := 0; i < 3; i++ {
		stopBlockReason(t, stopRun(cwd, env))
	}
	// the cycle advances to B: the next Stop is progress and blocks with a fresh budget
	s := state.ReadState(cwd, stopSID)
	s.Phase = state.PhaseB
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	if got := stopBlockReason(t, stopRun(cwd, env)); !strings.Contains(got, "mid-cycle at B (BUILD)") {
		t.Errorf("after progress: %q", got)
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopBlockTotal != 4 {
		t.Errorf("recharged count %v total %v (the total is never recharged)", s.StopBlockCount, s.StopBlockTotal)
	}
}

// No endless resume: whatever progress does, 24 blocks end the turn's loop; the cap is announced once.
func TestStopTotalCapEndsTheLoopAndIsAnnouncedOnce(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	blocks, announced, silent := 0, 0, 0
	for i := 0; i < 80; i++ {
		// every Stop is progress (the phase alternates), so only the total can end the loop
		s := state.ReadState(cwd, stopSID)
		if s.Phase == state.PhaseB {
			s.Phase = state.PhaseC
		} else {
			s.Phase = state.PhaseB
		}
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		switch a := stopRun(cwd, env); {
		case strings.Contains(a.Stdout, `"decision":"block"`):
			blocks++
		case a.Stdout == `{"systemMessage":"CRW Stop continuation cap (24) reached for this user turn; releasing."}`+"\n":
			announced++
		case a == (StopAnswer{}):
			silent++
		default:
			t.Fatalf("unexpected answer %+v", a)
		}
	}
	if blocks != StopMaxBlocksTotal || announced != 1 || silent != 80-StopMaxBlocksTotal-1 {
		t.Errorf("blocks %d announced %d silent %d", blocks, announced, silent)
	}
	if s := state.ReadState(cwd, stopSID); !s.StopBlockCapNotified {
		t.Errorf("the announcement is not recorded")
	}
}

// The total is the turn's: the UserPromptSubmit stamp resets it, so the next turn's loop starts again (hook.ts:1409).
func TestStopTotalCapAtTheBoundary(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTotal = StopMaxBlocksTotal - 1 })
	stopBlockReason(t, stopRun(cwd, env)) // the 24th block
	if a := stopRun(cwd, env); a.Stdout != stopSystemMessage(stopTotalCapMessage) {
		t.Errorf("the 25th Stop must announce the cap: %+v", a)
	}
}

// User stop: a goal that is not active (paused, budget limited, complete, blocked) never blocks, and Stop writes nothing.
func TestStopReleasesForAGoalTheUserOrTheHostStopped(t *testing.T) {
	for _, status := range []string{"paused", "budget_limited", "usage_limited", "complete", "blocked", "cancelled", "ACTIVE"} {
		cwd, env := stopRig(t, status)
		stopInFlight(t, cwd, state.PhaseB)
		before := stopStateBytes(t, cwd)
		if a := stopRun(cwd, env); a != (StopAnswer{}) {
			t.Errorf("goal %q: %+v", status, a)
		}
		if stopStateBytes(t, cwd) != before {
			t.Errorf("goal %q: Stop wrote the state", status)
		}
	}
}

// A parent without a goal (interactive, or driven by relay events) is never blocked, mid-cycle or at IDLE with a bound plan.
func TestStopLeavesASessionWithoutAGoalAlone(t *testing.T) {
	for name, status := range map[string]string{"no database": "", "no row for the session": "other"} {
		cwd, env := stopRig(t, status)
		if status == "other" {
			// a goal row of another thread
			home, _ := env("CODEX_SQLITE_HOME")
			os.RemoveAll(home)
			sessionHookGoalsDB(t, home, "CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
				"INSERT INTO thread_goals (thread_id, status, objective) VALUES ('another', 'active', 'x')")
		}
		stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "ship" })
		stopWritePlan(t, cwd, "ship", func(p *goalplan.Goalplan) {
			p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "Exporter", goalplan.WorkPhasePending)}
		})
		before := stopStateBytes(t, cwd)
		if a := stopRun(cwd, env); a != (StopAnswer{}) {
			t.Errorf("%s, in flight: %+v", name, a)
		}
		sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "ship" }) // IDLE with a bound plan
		idle := stopStateBytes(t, cwd)
		if a := stopRun(cwd, env); a != (StopAnswer{}) {
			t.Errorf("%s, idle: %+v", name, a)
		}
		if stopStateBytes(t, cwd) != idle || before == "" {
			t.Errorf("%s: Stop wrote the state", name)
		}
	}
}

// Phase I is never driven, an unreadable goals database is not an active goal, and a state the reader cannot trust is IDLE.
func TestStopReleasesForInterviewUnreadableGoalAndCorruptState(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseI)
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("phase I: %+v", a)
	}

	cwd, _ = stopRig(t, "active")
	dir := t.TempDir()
	home := sessionHookGoalsDB(t, dir, "CREATE TABLE unrelated (x INTEGER)")
	stopInFlight(t, cwd, state.PhaseB)
	before := stopStateBytes(t, cwd)
	if a := stopRun(cwd, sessionHookEnv(map[string]string{"CODEX_SQLITE_HOME": home, "CRW_BIN": "CRW"})); a != (StopAnswer{}) {
		t.Errorf("unreadable goals database: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("an unreadable goals database changed the state")
	}

	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	path := state.StatePath(cwd, stopSID)
	if err := os.WriteFile(path, []byte("{not json"), 0o666); err != nil {
		t.Fatal(err)
	}
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("corrupt state: %+v", a)
	}
	if b, _ := os.ReadFile(path); string(b) != "{not json" {
		t.Errorf("a corrupt state was rewritten: %q", b)
	}
}

// Recovery: a transcript whose tail shows compaction or a context-pressure recovery releases without a counter
// write, so the recovery turn is not piled on and the budget is not spent.
func TestStopContextPressureTailReleasesWithoutSpendingTheBudget(t *testing.T) {
	for _, marker := range []string{"Your context window has been compacted.", "Compacted session handoff", "conversation history has been summarized"} {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB)
		transcript := filepath.Join(cwd, "transcript.jsonl")
		if err := os.WriteFile(transcript, []byte(`{"type":"event_msg","payload":{"message":"`+marker+`"}}`+"\n"), 0o666); err != nil {
			t.Fatal(err)
		}
		before := stopStateBytes(t, cwd)
		if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TranscriptPath: transcript}, "linux", env); a != (StopAnswer{}) {
			t.Errorf("%q: %+v", marker, a)
		}
		if stopStateBytes(t, cwd) != before {
			t.Errorf("%q: the release spent the budget", marker)
		}
		// the same Stop on a transcript that is gone or unreadable is not a recovery: it blocks
		if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TranscriptPath: filepath.Join(cwd, "gone.jsonl")}, "linux", env); !strings.Contains(a.Stdout, `"decision":"block"`) {
			t.Errorf("%q: a missing transcript must not release: %+v", marker, a)
		}
	}
}

// IDLE with an active goal and a bound plan: the goal-idle arming block, with the remaining work named
// (hook.ts:1694-1738, GOAL-IDLE-CONTINUE-01).
func TestStopGoalIdleBlockNamesTheWork(t *testing.T) {
	cwd, env := stopRig(t, "active")
	sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "export" })
	stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "Exporter", goalplan.WorkPhasePending)}
		p.Criteria = []goalplan.GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "green", Status: goalplan.CriterionOpen}}
	})
	got := stopBlockReason(t, stopRun(cwd, env))
	want := "[crw — goal continuation] A host goal is ACTIVE but no PABCD cycle is in flight.\n" +
		"GOAL-IDLE-CONTINUE-01: IDLE is not the end while the goal is active (LOOP-CONTINUE-01). Do not end the turn here.\n" +
		"Either start the next work-phase now: `CRW pabcd orchestrate P --session rec-s1 --attest '{\"from\":\"IDLE\",\"to\":\"P\",\"did\":\"<diff-level plan for the next work-phase>\"}'`\n" +
		"or close the goal honestly: `update_goal` status \"complete\" (only when the recorded criteria are proven — the E8 gate checks a bound goalplan) or status \"blocked\" for an external blocker.\n" +
		"LOOP-UNIT-CHAIN-01: work-phases chain HETEROGENEOUS units in one session — an independent feature/plan discovered mid-loop is simply the NEXT work-phase (append it to the goalplan, then orchestrate P). \"Needs its own PABCD\" is a plan statement, not a session boundary; do not close the goal while naming remaining features that fit the objective.\n" +
		"Ready work phases: wp1 (Exporter)\n" +
		"Required evidence: green\n" +
		"Record progress in: .crw/goalplans/export/ledger.jsonl"
	if got != want {
		t.Errorf("goal-idle block:\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockPhase == nil || *s.StopBlockPhase != state.PhaseIdle || s.StopBlockCount != 1 || s.Phase != state.PhaseIdle || s.OrchestrationActive {
		t.Errorf("the idle block is counted at IDLE and starts no cycle: %+v %v", s.StopBlockPhase, s.StopBlockCount)
	}
	// win32 writes the JSON first
	win := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "win32", env)
	if r := stopBlockReason(t, win); !strings.Contains(r, "write the JSON with `'{\"from\":\"IDLE\",\"to\":\"P\",\"did\":\"<diff-level plan for the next work-phase>\"}' | Set-Content -Encoding utf8 .crw/attest.json` then run `CRW pabcd orchestrate P --session rec-s1 --attest-file .crw/attest.json`") {
		t.Errorf("win32 goal-idle block: %q", r)
	}
}

// The goal-idle block's guidance when the plan has nothing to name: an empty bound plan, and its release when a slug
// names no plan or names one that cannot be read safely.
func TestStopGoalIdleGuidanceAndReleases(t *testing.T) {
	cwd, env := stopRig(t, "active")
	sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "shell" })
	stopWritePlan(t, cwd, "shell", nil)
	got := stopBlockReason(t, stopRun(cwd, env))
	if !strings.HasSuffix(got, "The bound goalplan 'shell' is EMPTY: register workPhases[]/criteria[] in .crw/goalplans/shell/goalplan.json (schema in $crw-loop) so remaining work is durable and the E8 gate can pass.") {
		t.Errorf("empty plan: %q", got)
	}
	for _, slug := range []string{"", "no-such-plan", ".", "..", "../x", "a b"} {
		cwd, env := stopRig(t, "active")
		sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = slug })
		before := stopStateBytes(t, cwd)
		if a := stopRun(cwd, env); a != (StopAnswer{}) {
			t.Errorf("slug %q: %+v", slug, a)
		}
		if stopStateBytes(t, cwd) != before {
			t.Errorf("slug %q: a release without a plan wrote the state", slug)
		}
	}
}

// Approval wait: remaining work that waits only on open decisions releases, at IDLE and in flight; Stop does not
// push the agent past a question the user has to answer.
func TestStopReleasesWhileTheRemainingWorkAwaitsADecision(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		cwd, env := stopRig(t, "active")
		if inFlight {
			stopInFlight(t, cwd, state.PhaseA, func(s *state.State) { s.Slug = "ask" })
		} else {
			sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "ask" })
		}
		stopWritePlan(t, cwd, "ask", func(p *goalplan.Goalplan) {
			wp := stopWorkPhase("wp1", "Pick a store", goalplan.WorkPhasePending)
			wp.AwaitsDecision = []string{"d-1"}
			p.WorkPhases = []goalplan.GoalplanWorkPhase{wp}
			p.Decisions = []goalplan.GoalplanDecision{{ID: "d-1", Question: "Which store?", Status: goalplan.DecisionOpen, AskedAt: "2026-01-01T00:00:00.000Z"}}
		})
		before := stopStateBytes(t, cwd)
		if a := stopRun(cwd, env); a != (StopAnswer{}) {
			t.Errorf("in flight %v: %+v", inFlight, a)
		}
		if stopStateBytes(t, cwd) != before {
			t.Errorf("in flight %v: the wait spent the budget", inFlight)
		}
	}
}

// A bound goalplan enriches the in-flight block with the ready work (hook.ts:1639-1692); an invalid graph is named.
func TestStopInFlightBlockCarriesTheWorkContext(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "export" })
	stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
		wp := stopWorkPhase("wp1", "Exporter", goalplan.WorkPhaseInProgress)
		wp.Tasks = []goalplan.GoalplanTask{{ID: "t1", Title: "Write it", Status: goalplan.TaskPending}}
		p.WorkPhases = []goalplan.GoalplanWorkPhase{wp}
	})
	got := stopBlockReason(t, stopRun(cwd, env))
	for _, line := range []string{"\nReady work phases: wp1 (Exporter)\n", "\nReady tasks: wp1/t1 (Write it)\n", "\nRecord progress in: .crw/goalplans/export/ledger.jsonl\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in %q", line, got)
		}
	}
	if !strings.HasSuffix(got, stopTail[1:]) {
		t.Errorf("the closing line must stay last: %q", got)
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockWorkPhaseID == nil || *s.StopBlockWorkPhaseID != "wp1" {
		t.Errorf("the work phase is part of the progress key: %v", s.StopBlockWorkPhaseID)
	}

	// two work phases with one id: not a valid graph, named instead of listed twice
	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "dup" })
	stopWritePlan(t, cwd, "dup", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "A", goalplan.WorkPhasePending), stopWorkPhase("wp1", "B", goalplan.WorkPhasePending)}
	})
	got = stopBlockReason(t, stopRun(cwd, env))
	if !strings.Contains(got, "\nWaiting on: the plan is not a valid graph: ") || strings.Contains(got, "Ready work phases") {
		t.Errorf("invalid graph: %q", got)
	}
}

// A work-phase switch is progress: the block counter recharges when the active work phase changes between Stops.
func TestStopWorkPhaseSwitchIsProgress(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "two" })
	stopWritePlan(t, cwd, "two", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhaseInProgress), stopWorkPhase("wp2", "Two", goalplan.WorkPhasePending)}
	})
	for i := 0; i < 3; i++ {
		stopBlockReason(t, stopRun(cwd, env))
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 3 {
		t.Fatalf("count %v", s.StopBlockCount)
	}
	plan := goalplan.ReadGoalplan(cwd, "two")
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	plan.WorkPhases[1].Status = goalplan.WorkPhaseInProgress
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	stopBlockReason(t, stopRun(cwd, env))
	if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopBlockWorkPhaseID == nil || *s.StopBlockWorkPhaseID != "wp2" {
		t.Errorf("after the switch: count %v work phase %v", s.StopBlockCount, s.StopBlockWorkPhaseID)
	}
}

func stopWorkPhase(id, title string, status goalplan.WorkPhaseStatus) goalplan.GoalplanWorkPhase {
	return goalplan.GoalplanWorkPhase{ID: id, Title: title, Status: status, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}
}

func stopWritePlan(t *testing.T, cwd, slug string, mutate func(*goalplan.Goalplan)) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: slug, Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	plan.Slug = slug
	if mutate != nil {
		mutate(plan)
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// A maximize objective whose latest two metric values do not improve gets the plateau block instead of the
// continuation (hook.ts:1740-1771); a satisfy objective, or a rising window, keeps the continuation.
func TestStopPlateauBlock(t *testing.T) {
	record := func(t *testing.T, cwd string, values ...float64) {
		t.Helper()
		for _, v := range values {
			if _, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{SessionID: stopSID, MetricName: "score", Value: v, Source: metric.OperatorEntered}); err != nil {
				t.Fatal(err)
			}
		}
	}
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
		t.Fatal(err)
	}
	record(t, cwd, 0.5, 0.5)
	got := stopBlockReason(t, stopRun(cwd, env))
	if !strings.HasPrefix(got, "[crw — objective plateau] You are mid-cycle at B (BUILD) with an active maximize goal.\n"+
		"The latest 2 score metric value(s) are non-improving: 0.5 -> 0.5.\n"+
		"Step back and re-plan with divergence: record at least two grounded candidate approaches, choose the collapse point, then continue PABCD.\n"+
		"Do not ask the user while the goal is active; record assumptions or an unresolved-tie note for later review.\n"+
		"Anchor rule (LOOP-CANDIDATE-ANCHOR-01):") || strings.Contains(got, "orchestrate") {
		t.Errorf("plateau block: %q", got)
	}
	if s := state.ReadState(cwd, stopSID); s.StopMetricCursor != 2 {
		t.Errorf("the cursor is the high-water mark of the rows seen: %v", s.StopMetricCursor)
	}

	// a rising window keeps the continuation, and the new row counts as progress once
	record(t, cwd, 0.9)
	if got := stopBlockReason(t, stopRun(cwd, env)); !strings.Contains(got, "[crw — continue PABCD]") {
		t.Errorf("a rising window: %q", got)
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopMetricCursor != 3 {
		t.Errorf("improvement is progress: count %v cursor %v", s.StopBlockCount, s.StopMetricCursor)
	}
	// the same row is not progress twice: the budget is spent, not refilled
	stopBlockReason(t, stopRun(cwd, env))
	stopBlockReason(t, stopRun(cwd, env))
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("a recorded improvement refilled the budget: %+v", a)
	}

	// a satisfy objective ignores the plateau
	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Satisfy); err != nil {
		t.Fatal(err)
	}
	record(t, cwd, 0.5, 0.5)
	if got := stopBlockReason(t, stopRun(cwd, env)); !strings.Contains(got, "[crw — continue PABCD]") {
		t.Errorf("satisfy objective: %q", got)
	}
}

// The plateau block escalates after three discarded candidates of one change class.
func TestStopPlateauBlockNamesDiscardedCandidates(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{SessionID: stopSID, MetricName: "score", Value: 1, Source: metric.OperatorEntered}); err != nil {
			t.Fatal(err)
		}
	}
	class, discarded := metric.ChangeParameterTweak, metric.StatusDiscarded
	for _, title := range []string{"raise the threshold", "lower the threshold", "widen the guard"} {
		if _, err := metric.RecordDivergenceCandidate(cwd, metric.CandidateInput{SessionID: stopSID, Kind: metric.KindStrong1, Title: title,
			Rationale: "r", SourceURLs: []string{"https://example.com/a"}, Status: &discarded, ChangeClass: &class}); err != nil {
			t.Fatalf("the candidate ledger refused the fixture row %q: %v", title, err)
		}
	}
	got := stopBlockReason(t, stopRun(cwd, env))
	for _, want := range []string{"FORBIDDEN: another parameter-tweak candidate — 3 consecutive parameter-tweak candidates were discarded.", "\nRecent discarded candidates:\n", "raise the threshold [parameter-tweak]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// A plateau block at C keeps the render and native observation advisories of the same Stop (CRW port deviation: the oracle's
// plateau branch returns before the advisory is appended, hook.ts:1861-1863, so the soft warning was lost for the one case that
// asks the model to re-plan; the block text itself is unchanged and the advisory follows it after a blank line).
func TestStopPlateauBlockKeepsTheObservationAdvisories(t *testing.T) {
	plateau := func(t *testing.T, more ...func(*state.State)) (string, host.LookupEnv) {
		t.Helper()
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseC, more...)
		if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{SessionID: stopSID, MetricName: "score", Value: 0.5, Source: metric.OperatorEntered}); err != nil {
				t.Fatal(err)
			}
		}
		return cwd, env
	}
	const anchor = "Check whether evaluation instances are fixed/enumerable (LOOP-INSTANCE-CHECK-01)."
	modified := func(cwd string) {
		appendRow(cwd, RenderObsRow{TS: "2026-01-01T00:00:00.000Z", Kind: ArtifactModified, Detail: "page.html", SessionID: stopSID})
	}

	t.Run("render", func(t *testing.T) {
		cwd, env := plateau(t)
		modified(cwd)
		got := stopBlockReason(t, stopRun(cwd, env))
		if !strings.HasPrefix(got, "[crw — objective plateau] You are mid-cycle at C (") || !strings.HasSuffix(got, anchor+"\n\n"+RenderGroundingAdvisory()) {
			t.Errorf("plateau block without the render advisory: %q", got)
		}
	})
	t.Run("observed", func(t *testing.T) {
		cwd, env := plateau(t)
		modified(cwd)
		appendRow(cwd, RenderObsRow{TS: "2026-01-01T00:00:01.000Z", Kind: Observation, Detail: "shot", SessionID: stopSID})
		got := stopBlockReason(t, stopRun(cwd, env))
		if !strings.HasSuffix(got, anchor) || strings.Contains(got, "advisory") {
			t.Errorf("an observed render grew an advisory: %q", got)
		}
	})
	t.Run("native and render", func(t *testing.T) {
		cwd, env := plateau(t, func(s *state.State) { s.Slug = "native" })
		stopWritePlan(t, cwd, "native", func(p *goalplan.Goalplan) {
			p.Criteria = []goalplan.GoalplanCriterion{{ID: "c1", Scenario: "s", ExpectedEvidence: "e", Status: goalplan.CriterionOpen, Surface: goalplan.SurfaceDesktop, Presented: goalplan.PresentedNative}}
		})
		modified(cwd)
		got := stopBlockReason(t, stopRun(cwd, env))
		i := strings.Index(got, "[crw advisory — D5.2] The active desktop criteria c1 declare presented: \"native\"")
		if !strings.HasPrefix(got, "[crw — objective plateau]") || i < 0 || !strings.Contains(got[:i], anchor+"\n\n") || !strings.HasSuffix(got, "\n\n"+RenderGroundingAdvisory()) {
			t.Errorf("plateau block without both advisories: %q", got)
		}
	})
	t.Run("not at C", func(t *testing.T) {
		cwd, env := stopRig(t, "active")
		stopInFlight(t, cwd, state.PhaseB)
		if err := metric.WriteObjectiveKind(cwd, stopSID, metric.Maximize); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := metric.RecordObjectiveMetric(cwd, metric.RecordInput{SessionID: stopSID, MetricName: "score", Value: 0.5, Source: metric.OperatorEntered}); err != nil {
				t.Fatal(err)
			}
		}
		modified(cwd)
		if got := stopBlockReason(t, stopRun(cwd, env)); !strings.HasSuffix(got, anchor) {
			t.Errorf("phase B grew an advisory: %q", got)
		}
	})
}

// The render advisory (C-RENDER-GROUNDING-01): at C, an interactive session gets additionalContext and never a
// block; a goal session gets the advisory appended to the continuation.
func TestStopRenderAdvisory(t *testing.T) {
	seed := func(t *testing.T, cwd string) {
		t.Helper()
		appendRow(cwd, RenderObsRow{TS: "2026-01-01T00:00:00.000Z", Kind: ArtifactModified, Detail: "page.html", SessionID: stopSID})
		if !HasRenderArtifactModified(cwd, stopSID) {
			t.Fatal("the artifact row was not recorded")
		}
	}
	cwd, env := stopRig(t, "")
	stopInFlight(t, cwd, state.PhaseC)
	seed(t, cwd)
	before := stopStateBytes(t, cwd)
	a := stopRun(cwd, env)
	if a.Stdout != "" || a.Context != RenderGroundingAdvisory() {
		t.Errorf("interactive session: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("an advisory spent the budget")
	}

	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseC)
	seed(t, cwd)
	got := stopBlockReason(t, stopRun(cwd, env))
	if !strings.HasSuffix(got, stopTail+"\n\n"+RenderGroundingAdvisory()) {
		t.Errorf("goal session: %q", got)
	}

	// not at C: no advisory
	cwd, env = stopRig(t, "")
	stopInFlight(t, cwd, state.PhaseB)
	seed(t, cwd)
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("phase B: %+v", a)
	}
}

// Counter safety: a block the counter could not record is never answered, and a Stop judged on a state that has
// moved is released instead of blocked.
func TestStopNeverBlocksWithoutRecordingTheCounter(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	before := stopStateBytes(t, cwd)
	held := func(cwd, sessionID string, fn func() error) error { return errors.New("a held lock") }
	if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, held); a != (StopAnswer{}) {
		t.Errorf("a lock that cannot be taken: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("a held lock wrote the file")
	}

	// a state file the reader would not keep whole is not rewritten, so it cannot carry a counter: release
	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) {
		for i := 0; i <= state.MaxUnverifiedSubagents; i++ {
			s.UnverifiedSubagents = append(s.UnverifiedSubagents, state.UnverifiedSubagent{AgentID: "a" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
				TurnID: "t", AgentType: "worker", Attempts: 1, ReceiptClaimed: "v", RecordedAt: "2026-01-01T00:00:00.000Z", Resolvable: true})
		}
	})
	before = stopStateBytes(t, cwd)
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("a lossy state: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("a lossy state was rewritten")
	}

	// the cycle advances to IDLE between the decision and the lock: the Stop is stale and releases
	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	moved := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.Phase, s.OrchestrationActive = state.PhaseIdle, false
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, moved); a != (StopAnswer{}) {
		t.Errorf("a stale Stop: %+v", a)
	}
	if s := state.ReadState(cwd, stopSID); s.Phase != state.PhaseIdle || s.StopBlockCount != 0 || s.StopBlockTotal != 0 {
		t.Errorf("a stale Stop counted: %+v", s)
	}
}

// A participating writer's update between the first read and the lock is kept (the oracle's unlocked write loses it).
func TestStopKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	writer := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.MemoryWriteGrant = true
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	stopBlockReason(t, stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, writer))
	if s := state.ReadState(cwd, stopSID); !s.MemoryWriteGrant || s.StopBlockCount != 1 {
		t.Errorf("the participating writer's update was lost: %+v", s)
	}
}

// No duplicate transition: whatever Stop answers, it never moves the cycle or the binding, and the same event run
// again only spends the bounded budget.
func TestStopNeverTransitionsTheCycle(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseA, func(s *state.State) {
		s.Slug = "plan"
		s.InjectedTurns = []string{"t1"}
		s.PlanUnit = ptr("devlog/_plan/x")
	})
	stopWritePlan(t, cwd, "plan", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhasePending)}
	})
	start := state.ReadState(cwd, stopSID)
	var answered []bool
	for i := 0; i < 10; i++ {
		answered = append(answered, strings.Contains(stopRun(cwd, env).Stdout, `"decision":"block"`))
		now := state.ReadState(cwd, stopSID)
		if now.Phase != start.Phase || now.Slug != start.Slug || now.OrchestrationActive != start.OrchestrationActive || now.Flags != start.Flags ||
			now.PlanUnit == nil || *now.PlanUnit != *start.PlanUnit || len(now.InjectedTurns) != 1 || now.SessionID != start.SessionID {
			t.Fatalf("Stop %d moved the cycle: %+v", i, now)
		}
	}
	// the same event again only spends the budget: three blocks, a release, and a fresh budget for the next turn's loop
	if want := []bool{true, true, true, false, true, true, true, false, true, true}; !slices.Equal(answered, want) {
		t.Errorf("answers of a repeated event: %v, want %v", answered, want)
	}
	plan := goalplan.ReadGoalplan(cwd, "plan")
	if plan.WorkPhases[0].Status != goalplan.WorkPhasePending {
		t.Errorf("Stop changed the goalplan: %+v", plan.WorkPhases[0])
	}
}

func ptr[T any](v T) *T { return &v }

// The answer is one line a host reads as JSON, and the cap text carries the CRW name.
func TestStopAnswersAreOneLineOfJSON(t *testing.T) {
	if got := stopSystemMessage(stopTotalCapMessage); got != "{\"systemMessage\":\"CRW Stop continuation cap (24) reached for this user turn; releasing.\"}\n" {
		t.Errorf("system message %q", got)
	}
	if got := stopEnvelope("a\n<b> & \u2028"); got != "{\"decision\":\"block\",\"reason\":\"a\\n<b> & \u2028\"}\n" {
		t.Errorf("envelope %q", got)
	}
}

// beforeLock runs change where a participating writer or the user could land between Stop's first reading and the
// session lock, then takes the real lock.
func beforeLock(change func(cwd, sessionID string)) func(cwd, sessionID string, fn func() error) error {
	return func(cwd, sessionID string, fn func() error) error {
		change(cwd, sessionID)
		return state.WithSessionLock(cwd, sessionID, fn)
	}
}

func stopSetGoalStatus(t *testing.T, env host.LookupEnv, status string) {
	t.Helper()
	path, err := host.GoalsDBPath(env)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE thread_goals SET status = ? WHERE thread_id = ?", status, stopSID); err != nil {
		t.Fatal(err)
	}
}

// stopAskDecision rewrites the bound plan so that its only remaining work waits on an open decision.
func stopAskDecision(t *testing.T, cwd, slug string) {
	t.Helper()
	stopWritePlan(t, cwd, slug, func(p *goalplan.Goalplan) {
		wp := stopWorkPhase("wp1", "Pick a store", goalplan.WorkPhasePending)
		wp.AwaitsDecision = []string{"d1"}
		p.WorkPhases = []goalplan.GoalplanWorkPhase{wp}
		p.Decisions = []goalplan.GoalplanDecision{{ID: "d1", Question: "Which store?", Status: goalplan.DecisionOpen, AskedAt: "2026-01-01T00:00:00.000Z"}}
	})
}

// CRW-192 verification round 1: what the Stop decision stood on is judged again on what the lock found. A pause by
// the user, a question that now awaits an answer, a plan that is gone, or a new user turn that lands between the
// first reading and the lock releases the event without spending the budget.
func TestStopReleasesWhenTheWorldMovedBeforeTheLock(t *testing.T) {
	readyPlan := func(t *testing.T, cwd string) {
		stopWritePlan(t, cwd, "plan", func(p *goalplan.Goalplan) {
			p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "One", goalplan.WorkPhasePending)}
		})
	}
	for _, inFlight := range []bool{false, true} {
		setup := func(t *testing.T) (string, host.LookupEnv) {
			cwd, env := stopRig(t, "active")
			if inFlight {
				stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "plan" })
			} else {
				sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "plan" })
			}
			readyPlan(t, cwd)
			return cwd, env
		}
		cases := []struct {
			name   string
			change func(t *testing.T, cwd string, env host.LookupEnv)
		}{
			{"the user pauses the goal", func(t *testing.T, cwd string, env host.LookupEnv) { stopSetGoalStatus(t, env, "paused") }},
			{"the goal database goes", func(t *testing.T, cwd string, env host.LookupEnv) {
				path, _ := host.GoalsDBPath(env)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}},
			{"a decision opens", func(t *testing.T, cwd string, env host.LookupEnv) { stopAskDecision(t, cwd, "plan") }},
			{"a new user turn is stamped", func(t *testing.T, cwd string, env host.LookupEnv) {
				s := state.ReadState(cwd, stopSID)
				s.StopBlockTurnID, s.StopBlockTotal = ptr("t1"), 0
				if err := state.WriteState(cwd, s); err != nil {
					t.Fatal(err)
				}
			}},
		}
		if !inFlight {
			cases = append(cases, struct {
				name   string
				change func(t *testing.T, cwd string, env host.LookupEnv)
			}{"the bound plan is gone", func(t *testing.T, cwd string, env host.LookupEnv) {
				if err := os.RemoveAll(filepath.Join(cwd, ".crw", "goalplans", "plan")); err != nil {
					t.Fatal(err)
				}
			}})
		}
		for _, c := range cases {
			cwd, env := setup(t)
			if c.name == "a new user turn is stamped" {
				sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.StopBlockTurnID = ptr("t0"); s.StopBlockTotal = 5 })
			}
			ran := false
			lock := beforeLock(func(cwd, sessionID string) { c.change(t, cwd, env); ran = true })
			a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, lock)
			if !ran {
				t.Fatalf("in flight %v, %s: the lock seam did not run", inFlight, c.name)
			}
			if a != (StopAnswer{}) {
				t.Errorf("in flight %v, %s: %+v", inFlight, c.name, a)
			}
			if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 0 || (s.StopBlockTotal != 0 && c.name != "a new user turn is stamped") || s.StopBlockPhase != nil {
				t.Errorf("in flight %v, %s: the released event spent the budget: %+v", inFlight, c.name, s)
			}
			if c.name == "a new user turn is stamped" {
				if s := state.ReadState(cwd, stopSID); s.StopBlockTotal != 0 || s.StopBlockTurnID == nil || *s.StopBlockTurnID != "t1" {
					t.Errorf("in flight %v: the new turn's stamp was changed: %+v", inFlight, s)
				}
			}
		}
	}
}

// An event of an earlier user turn that reaches Stop after the next turn was stamped does not spend that turn's
// budget either: the payload's turn_id is held against the stamp.
func TestStopReleasesAnEventOfAnEarlierTurn(t *testing.T) {
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.StopBlockTurnID = ptr("t1") })
	before := stopStateBytes(t, cwd)
	if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: "t0"}, "linux", env); a != (StopAnswer{}) {
		t.Errorf("a stale turn: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("a stale turn wrote the state")
	}
	// the current turn, and a host whose state carries no stamp yet, keep the loop
	if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: "t1"}, "linux", env); !strings.Contains(a.Stdout, `"decision":"block"`) {
		t.Errorf("the current turn: %+v", a)
	}
	cwd, env = stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TurnID: "t0"}, "linux", env); !strings.Contains(a.Stdout, `"decision":"block"`) {
		t.Errorf("an unstamped state: %+v", a)
	}
}

// A write of the counter that fails before the state is published is not a block, and the file stays as it was; a failure after the
// publication (state.PublishedError: the directory sync) still has the counter on disk, so the block it was written for is answered.
func TestStopStateWriteFailures(t *testing.T) {
	failing := func(t *testing.T, write func(cwd string, next state.State) error) {
		t.Helper()
		old := stopWriteState
		stopWriteState = write
		t.Cleanup(func() { stopWriteState = old })
	}

	// before the publication: nothing on disk, no block
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	before := stopStateBytes(t, cwd)
	failing(t, func(string, state.State) error { return errors.New("no space left on device") })
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("a write that failed before the publication: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Errorf("a failed write changed the file")
	}

	// the write is the real one and the directory refuses the temp file (read-only): same answer, file kept
	if os.Geteuid() != 0 {
		failing(t, state.WriteState)
		dir := filepath.Dir(state.StatePath(cwd, stopSID))
		readOnly := func(cwd, sessionID string, fn func() error) error {
			return state.WithSessionLock(cwd, sessionID, func() error {
				if err := os.Chmod(dir, 0o500); err != nil {
					return err
				}
				defer os.Chmod(dir, 0o700)
				return fn()
			})
		}
		if a := stopHandle(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, readOnly); a != (StopAnswer{}) {
			t.Errorf("a read-only sessions directory: %+v", a)
		}
		if stopStateBytes(t, cwd) != before {
			t.Errorf("a read-only sessions directory changed the file")
		}
	}

	// after the publication: the counter is on disk, so the block is answered
	published := func(cwd string, next state.State) error {
		if err := state.WriteState(cwd, next); err != nil {
			return err
		}
		return &state.PublishedError{Err: errors.New("fsync the directory")}
	}
	failing(t, published)
	a := stopRun(cwd, env)
	if reason := stopBlockReason(t, a); reason == "" {
		t.Errorf("a failure after the publication: %+v", a)
	}
	if s := state.ReadState(cwd, stopSID); s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
		t.Errorf("the published counter: %+v", s)
	}
}

// A session id that is not canonical shares its file and lock with the canonical id it sanitises to ("s/1" and "s-1"): the Stop of
// the one must neither spend the other's budget nor rewrite its sessionId. It releases, and the file is untouched.
func TestStopReleasesANonCanonicalSessionID(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	home := sessionHookGoalsDB(t, filepath.Join(dir, "codex"),
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
		"INSERT INTO thread_goals (thread_id, status, objective) VALUES ('s/1', 'active', 'ship the feature')")
	env := sessionHookEnv(map[string]string{"CRW_BIN": "CRW", "HOME": filepath.Join(dir, "home"), "CODEX_SQLITE_HOME": home})
	sessionHookStateFile(t, cwd, "s-1", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseB, true })
	path := state.StatePath(cwd, "s-1")
	if state.StatePath(cwd, "s/1") != path {
		t.Fatalf("the ids no longer share a file")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s/1", "s 1", "../s-1", ""} {
		if a := StopHandle(StopPayload{Cwd: cwd, SessionID: id}, "linux", env); a != (StopAnswer{}) {
			t.Errorf("session id %q: %+v", id, a)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Errorf("session id %q rewrote the file of s-1", id)
		}
	}
	// the canonical id of the file, with a goal of its own, still drives its loop
	sessionHookGoalsDB(t, filepath.Join(dir, "codex"), "INSERT INTO thread_goals (thread_id, status, objective) VALUES ('s-1', 'active', 'ship the feature')")
	if a := StopHandle(StopPayload{Cwd: cwd, SessionID: "s-1"}, "linux", env); stopBlockReason(t, a) == "" {
		t.Errorf("the canonical id: %+v", a)
	}
}
