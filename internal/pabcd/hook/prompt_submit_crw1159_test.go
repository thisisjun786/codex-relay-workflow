// prompt_submit_crw1159_test.go holds CRW-1159: an advisory answer is emitted only when the session lock that records
// it finds the turn not yet recorded and the decision's inputs (phase, orchestration, binding, injection cursor, the
// bound work phase) as the handler read them. The oracle judged the turn and chose the directive on a read before any
// lock (hook.ts:691), so two invocations of one turn both answered and a phase that moved meanwhile got a stale directive
// and an old cursor.
package hook

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// crw1159Lock is the session lock whose first acquisition first runs before (another handler, a participating
// writer), so the handler under test has made its unlocked read when before lands.
func crw1159Lock(before func()) func(cwd, sessionID string, fn func() error) error {
	return crw1159LockAt(1, before)
}

// crw1159LockAt runs before at the nth acquisition instead: with a state file the first is the Stop-budget
// stamp, which comes before the handler builds its answer, and the second is the answer's own recording write.
func crw1159LockAt(n int, before func()) func(cwd, sessionID string, fn func() error) error {
	calls := 0
	return func(cwd, sessionID string, fn func() error) error {
		if calls++; calls == n {
			before()
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
}

// TestCRW1159OneTurnIsAnsweredOnce is end condition 1: two handlers that both read the turn as unrecorded answer one
// non-empty context between them, on every path that records the turn.
func TestCRW1159OneTurnIsAnsweredOnce(t *testing.T) {
	cursor := state.PhaseP
	for _, c := range []struct {
		name, prompt string
		setup        func(*state.State)
	}{
		{"loop-arm mandate", "Run crw-loop for this task", nil},
		{"project scope pointer", "Run crw-loop for the migration project", nil},
		{"trigger advice", "Use crw-pabcd to start Plan phase", nil},
		{"agbrowse fail-closed", "agbrowse search for the release notes", nil},
		{"mode 2 directive", "keep going", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true }},
		{"mode 3 header", "keep going", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, &cursor
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := t.TempDir()
			if c.setup != nil {
				promptSubmitStateFile(t, cwd, "s1", c.setup)
			}
			payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: c.prompt, TurnID: "t1", PabcdEnabled: true}
			var second string
			first := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159Lock(func() {
				second = promptSubmitHandle(payload, "", promptSubmitHost(cwd), state.WithSessionLock)
			}))
			answered := 0
			for _, a := range []string{first, second} {
				if a != "" {
					answered++
				}
			}
			if answered != 1 {
				t.Errorf("%d answers for one turn:\nfirst  %.120q\nsecond %.120q", answered, first, second)
			}
		})
	}
}

// TestCRW1159AStaleDecisionIsDropped is end condition 2: a phase, binding, cursor or bound work phase that moved between
// the handler's read and its injection lock neither answers the stale directive nor writes the old cursor.
func TestCRW1159AStaleDecisionIsDropped(t *testing.T) {
	cursor := state.PhaseP
	move := func(t *testing.T, cwd string, mutate func(*state.State)) func() {
		return func() {
			s := state.ReadState(cwd, "s1")
			mutate(&s)
			if err := state.WriteState(cwd, s); err != nil {
				t.Error(err)
			}
		}
	}
	for _, c := range []struct {
		name  string
		setup func(*state.State)
		moved func(*state.State)
	}{
		{"P to B before the mode 2 directive", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true },
			func(s *state.State) { s.Phase = state.PhaseB }},
		{"P to B before the mode 3 header", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, &cursor
		},
			func(s *state.State) { s.Phase = state.PhaseB }},
		{"a PostCompact reset before the mode 3 header", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, &cursor
		},
			func(s *state.State) { s.LastInjectedPhase = nil }},
		{"a new binding before the mode 2 directive", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true },
			func(s *state.State) { s.Slug = "other" }},
		{"a closed cycle before the mode 2 directive", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true },
			func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseIdle, false }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := t.TempDir()
			promptSubmitStateFile(t, cwd, "s1", c.setup)
			payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "keep going", TurnID: "t1", PabcdEnabled: true}
			var moved state.State
			got := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159Lock(func() {
				move(t, cwd, c.moved)()
				moved = state.ReadState(cwd, "s1")
			}))
			if strings.Contains(got, "PLAN") {
				t.Errorf("a stale PLAN context was answered: %.160q", got)
			}
			if s := state.ReadState(cwd, "s1"); !promptSamePhase(s.LastInjectedPhase, moved.LastInjectedPhase) || slices.Contains(s.InjectedTurns, "t1") {
				t.Errorf("the stale decision was recorded: cursor %v, turns %v", s.LastInjectedPhase, s.InjectedTurns)
			}
		})
	}

	t.Run("a new active work phase before the B directive", func(t *testing.T) {
		cwd := t.TempDir()
		wp1, wp2 := "wp1", "wp2"
		write := func(active *string) {
			stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
				p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "Exporter", goalplan.WorkPhasePending), stopWorkPhase("wp2", "Importer", goalplan.WorkPhasePending)}
				p.ActiveWorkPhaseID = active
			})
		}
		write(&wp1)
		promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.Phase, s.OrchestrationActive, s.Slug = state.PhaseB, true, "export" })
		payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "keep going", TurnID: "t1", PabcdEnabled: true}
		got := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159LockAt(2, func() { write(&wp2) }))
		if strings.Contains(got, "Exporter") {
			t.Errorf("the directive named the work phase the plan has left: %.200q", got)
		}
		if s := state.ReadState(cwd, "s1"); s.LastInjectedPhase != nil {
			t.Errorf("a stale directive's cursor was written: %v", *s.LastInjectedPhase)
		}
	})
}

// TestCRW1159ACompactionAfterTheDecisionDropsTheAnswer is fix round 1, finding 2: the injection cursor is not the only
// witness of the context generation (PostCompact leaves a cursor that is already nil as it is), so a compaction the
// transcript records between the handler's read and its recording lock drops the answer and the cursor write, whatever
// the cursor held. An append that is no compaction does not.
func TestCRW1159ACompactionAfterTheDecisionDropsTheAnswer(t *testing.T) {
	cursor := state.PhaseP
	for _, c := range []struct {
		name   string
		last   *state.Phase
		append func(*testing.T) string
		stale  bool
	}{
		{"mode 2, cursor already reset, compacted", nil, func(t *testing.T) string { return codexCompaction(t, "[crw: PLAN]") }, true},
		{"mode 3, compacted", &cursor, func(t *testing.T) string { return codexCompaction(t, "[crw: PLAN]") }, true},
		{"mode 2, a huge compacted record", nil, func(t *testing.T) string {
			return codexCompaction(t, strings.Repeat("x", 200_000))
		}, true},
		{"mode 2, an ordinary append", nil, func(t *testing.T) string { return codexMessage(t, "assistant", "", "working") }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := t.TempDir()
			transcript := writeTranscript(t, cwd, codexUserTurn(t, "plan it"))
			promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
				s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, c.last
			})
			payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "keep going", TurnID: "t1", TranscriptPath: transcript, PabcdEnabled: true}
			got := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159LockAt(2, func() {
				f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Error(err)
					return
				}
				defer f.Close()
				if _, err := f.WriteString(c.append(t)); err != nil {
					t.Error(err)
				}
				SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
			}))
			s := state.ReadState(cwd, "s1")
			if c.stale && (got != "" || slices.Contains(s.InjectedTurns, "t1") || !promptSamePhase(s.LastInjectedPhase, nil)) {
				t.Errorf("a compaction after the decision: answered %.120q, cursor %v, turns %v", got, s.LastInjectedPhase, s.InjectedTurns)
			}
			if !c.stale && (got == "" || !slices.Contains(s.InjectedTurns, "t1")) {
				t.Errorf("an ordinary append dropped the answer: %.120q, turns %v", got, s.InjectedTurns)
			}
		})
	}
}

// TestCRW1159ARepeatedTurnStillRecordsTheRememberRequest is end condition 3: a turn that is already recorded, or that
// a concurrent handler records first, still records the remember request before its early return.
func TestCRW1159ARepeatedTurnStillRecordsTheRememberRequest(t *testing.T) {
	const remember = "Remember this: the deploy key lives in the vault. Use crw-pabcd to start Plan phase"
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.InjectedTurns = []string{"t1"} })
	if got := promptSubmitAnswer(t, cwd, "s1", "t1", remember, true); got != "" {
		t.Errorf("a recorded turn answered %q", got)
	}
	if s := state.ReadState(cwd, "s1"); !s.MemoryWriteRequested || s.MemoryWriteTurn == nil || *s.MemoryWriteTurn != "t1" {
		t.Errorf("the repeated turn's remember request was lost: %+v", s)
	}

	cwd = t.TempDir()
	payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: remember, TurnID: "t2", PabcdEnabled: true}
	var second string
	first := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159Lock(func() {
		second = promptSubmitHandle(payload, "", promptSubmitHost(cwd), state.WithSessionLock)
		s := state.ReadState(cwd, "s1")
		s.MemoryWriteRequested, s.MemoryWriteTurn = false, nil // the gate consumed the first request
		if err := state.WriteState(cwd, s); err != nil {
			t.Error(err)
		}
	}))
	if (first == "") == (second == "") {
		t.Errorf("want exactly one answer: %q / %q", first, second)
	}
	if s := state.ReadState(cwd, "s1"); !s.MemoryWriteRequested {
		t.Errorf("the concurrent repeat did not record its remember request: %+v", s)
	}
}

// appendTo appends text to the file at path, reporting a failure on t.
func appendTo(t *testing.T, path, text string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Error(err)
	}
}

// TestCRW1159ACompactionCompletedAcrossTheMarkDropsTheAnswer is fix round 2, finding 1: the compacted record was still
// landing when the handler marked the transcript (its type already on disk), and its remainder lands before the
// recording lock, ahead of the ContextCompaction item. The cursor is already nil, so PostCompact changes nothing: only
// the mark can tell, and the PLAN directive is neither answered nor recorded.
func TestCRW1159ACompactionCompletedAcrossTheMarkDropsTheAnswer(t *testing.T) {
	// Codex writes the timestamp, the type and then the payload (codexLine sorts the keys, which puts the type last).
	history, err := json.Marshal(map[string]any{"message": "", "replacement_history": []map[string]any{{"type": "message", "role": "developer",
		"content": []map[string]any{{"type": "input_text", "text": "[crw: PLAN] " + strings.Repeat("h", 4000)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	// The ContextCompaction item that follows the record has not landed yet when the lock reads.
	compaction := `{"timestamp":"2026-10-09T20:49:26.924Z","type":"compacted","payload":` + string(history) + "}\n"
	split := strings.Index(compaction, `"type":"compacted"`) + 200
	cwd := t.TempDir()
	transcript := writeTranscript(t, cwd, codexUserTurn(t, "plan it")+compaction[:split])
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true })
	payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "keep going", TurnID: "t1", TranscriptPath: transcript, PabcdEnabled: true}
	got := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159LockAt(2, func() {
		appendTo(t, transcript, compaction[split:])
		SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
	}))
	s := state.ReadState(cwd, "s1")
	if got != "" || slices.Contains(s.InjectedTurns, "t1") || s.LastInjectedPhase != nil {
		t.Errorf("a compaction completed across the mark: answered %.120q, cursor %v, turns %v", got, s.LastInjectedPhase, s.InjectedTurns)
	}
}

// TestCRW1159ATurnlessAnswerIsDecidedAgain is fix round 2, finding 2: a turnless payload records no turn and no cursor,
// but its answer is still decided again on the state and the transcript as they stand before it goes out, so a phase
// that moved or a compaction that landed meanwhile drops the stale directive. The loop-arm bookkeeping write of an armed
// session is the window: the move lands just before its lock. A turnless answer whose inputs held still writes nothing.
func TestCRW1159ATurnlessAnswerIsDecidedAgain(t *testing.T) {
	cursor := state.PhaseP
	for _, c := range []struct {
		name  string
		last  *state.Phase
		moved func(t *testing.T, cwd, transcript string)
	}{
		{"P to B before the mode 2 directive", nil, func(t *testing.T, cwd, _ string) {
			s := state.ReadState(cwd, "s1")
			s.Phase = state.PhaseB
			if err := state.WriteState(cwd, s); err != nil {
				t.Error(err)
			}
		}},
		{"P to B before the mode 3 header", &cursor, func(t *testing.T, cwd, _ string) {
			s := state.ReadState(cwd, "s1")
			s.Phase = state.PhaseB
			if err := state.WriteState(cwd, s); err != nil {
				t.Error(err)
			}
		}},
		{"a compaction before the mode 2 directive", nil, func(t *testing.T, cwd, transcript string) {
			appendTo(t, transcript, codexCompaction(t, "[crw: PLAN]"))
			SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := t.TempDir()
			transcript := writeTranscript(t, cwd, codexUserTurn(t, "plan it"))
			promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
				s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, c.last
			})
			payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "Run crw-loop for this task", TranscriptPath: transcript, PabcdEnabled: true}
			got := promptSubmitHandle(payload, "", promptSubmitHost(cwd), crw1159Lock(func() { c.moved(t, cwd, transcript) }))
			if strings.Contains(got, "PLAN") {
				t.Errorf("a stale turnless PLAN context was answered: %.160q", got)
			}
			if s := state.ReadState(cwd, "s1"); !promptSamePhase(s.LastInjectedPhase, c.last) || len(s.InjectedTurns) != 0 {
				t.Errorf("the turnless payload recorded a cursor or a turn: %v, %v", s.LastInjectedPhase, s.InjectedTurns)
			}
		})
	}

	t.Run("inputs that held still answer and write nothing", func(t *testing.T) {
		cwd := t.TempDir()
		promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true })
		before, err := os.ReadFile(state.StatePath(cwd, "s1"))
		if err != nil {
			t.Fatal(err)
		}
		payload := PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "keep going", PabcdEnabled: true}
		if got := promptSubmitHandle(payload, "", promptSubmitHost(cwd), state.WithSessionLock); !strings.Contains(got, "PLAN") {
			t.Errorf("a turnless mode 2 prompt lost its directive: %.160q", got)
		}
		if after, err := os.ReadFile(state.StatePath(cwd, "s1")); err != nil || string(after) != string(before) {
			t.Errorf("a turnless answer wrote the state (%v)", err)
		}
	})
}
