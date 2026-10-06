package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptSubmitFailingLock stands in for a session lock that cannot be taken, which the oracle has no
// counterpart of: its write is unlocked and either lands or throws.
func promptSubmitFailingLock(cwd, sessionID string, fn func() error) error {
	return errors.New("lock unavailable")
}

// promptSubmitHost is the environment the handler reads: a pinned CRW invocation, so an injected
// directive is byte-comparable, and a goal database directory that does not exist, which reads as
// "no goal", as the oracle's resolveGoalsDbPath plus existsSync does.
func promptSubmitHost(cwd string) host.LookupEnv {
	return func(key string) (string, bool) {
		switch key {
		case "CRW_BIN":
			return "{CRW}", true
		case "CODEX_SQLITE_HOME":
			return filepath.Join(cwd, "codex-home"), true
		}
		return "", false
	}
}

// promptSubmitAnswer runs the handler as the leg does. The empty platform selects this host's.
func promptSubmitAnswer(t *testing.T, cwd, sessionID, turn, prompt string, enabled bool) string {
	t.Helper()
	return PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: sessionID, Prompt: prompt, TurnID: turn, PabcdEnabled: enabled}, "", promptSubmitHost(cwd))
}

// promptSubmitStateFile writes a state for the session, so the cases that need an existing file reach
// the branches that read one.
func promptSubmitStateFile(t *testing.T, cwd, sessionID string, mutate func(*state.State)) {
	t.Helper()
	if _, err := state.EnsureState(cwd, sessionID); err != nil {
		t.Fatal(err)
	}
	s := state.ReadState(cwd, sessionID)
	mutate(&s)
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

// TestPromptSubmitNonTriggerStaysSilentAndWritesNoState is hook.test.ts "handleUserPromptSubmit:
// non-trigger -> ” and writes no state" (:477-486): a prompt that asks for nothing writes nothing,
// not even the state directory.
func TestPromptSubmitNonTriggerStaysSilentAndWritesNoState(t *testing.T) {
	cwd := t.TempDir()
	if answer := promptSubmitAnswer(t, cwd, "s1", "t1", "hello there", true); answer != "" {
		t.Errorf("answer %q", answer)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
		t.Error("a non-trigger prompt wrote state")
	}
}

// TestPromptSubmitMemoryMarkerPrecedesEveryEarlyReturn is the MEMORY-WRITE-GATE-01 placement rule
// (hook.ts:665-684): the remember request is recorded before the pabcd-off guard, before the
// already-injected-turn guard, and before the loop-arm return, because PreToolUse carries no prompt
// and this write is the only place the gate's evidence can come from.
func TestPromptSubmitMemoryMarkerPrecedesEveryEarlyReturn(t *testing.T) {
	const remember = "Remember this: the deploy key lives in the vault."
	t.Run("before the pabcd-off guard", func(t *testing.T) {
		cwd := t.TempDir()
		if answer := promptSubmitAnswer(t, cwd, "rec-s1", "rec-t1", remember, false); answer != "" {
			t.Errorf("answer %q", answer)
		}
		s := state.ReadState(cwd, "rec-s1")
		if !s.MemoryWriteRequested || s.MemoryWriteTurn == nil || *s.MemoryWriteTurn != "rec-t1" {
			t.Errorf("the marker was not written: %+v", s)
		}
	})
	t.Run("before the already-injected-turn guard", func(t *testing.T) {
		cwd := t.TempDir()
		promptSubmitStateFile(t, cwd, "rec-s1", func(s *state.State) { s.InjectedTurns = []string{"rec-t1"} })
		if answer := promptSubmitAnswer(t, cwd, "rec-s1", "rec-t1", remember, true); answer != "" {
			t.Errorf("answer %q", answer)
		}
		s := state.ReadState(cwd, "rec-s1")
		if !s.MemoryWriteRequested || s.MemoryWriteTurn == nil || *s.MemoryWriteTurn != "rec-t1" {
			t.Errorf("the marker was not written: %+v", s)
		}
		if len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "rec-t1" {
			t.Errorf("the guarded turn changed injectedTurns: %+v", s.InjectedTurns)
		}
	})
	t.Run("before the loop-arm return", func(t *testing.T) {
		cwd := t.TempDir()
		answer := promptSubmitAnswer(t, cwd, "rec-s1", "rec-t1", "Remember this for later and run crw-loop for this task.", true)
		if answer == "" {
			t.Fatal("the loop-arm branch answered nothing")
		}
		// The loop-arm branch spreads the state it read; the marker survives only because it was
		// written before that read.
		s := state.ReadState(cwd, "rec-s1")
		if !s.MemoryWriteRequested || s.MemoryWriteTurn == nil || *s.MemoryWriteTurn != "rec-t1" {
			t.Errorf("the loop-arm write dropped the marker: %+v", s)
		}
		if !s.LoopArmSeen {
			t.Errorf("the loop-arm branch did not record loopArmSeen: %+v", s)
		}
	})
	t.Run("an empty turn records a null turn", func(t *testing.T) {
		cwd := t.TempDir()
		if answer := promptSubmitAnswer(t, cwd, "rec-s1", "", remember, false); answer != "" {
			t.Errorf("answer %q", answer)
		}
		if s := state.ReadState(cwd, "rec-s1"); !s.MemoryWriteRequested || s.MemoryWriteTurn != nil {
			t.Errorf("a turnless payload did not record a null turn: %+v", s)
		}
	})
}

// TestPromptSubmitAlreadyInjectedTurnIsSilent is hook.test.ts "handleUserPromptSubmit: idempotent
// within same (session,turn)" (:440-451) at this unit's boundary: the guard at hook.ts:691 answers
// before the command parse and before every trigger branch, and a new turn re-injects.
func TestPromptSubmitAlreadyInjectedTurnIsSilent(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.InjectedTurns = []string{"t1"} })
	if answer := promptSubmitAnswer(t, cwd, "s1", "t1", "Use crw-pabcd to start Plan phase", true); answer != "" {
		t.Errorf("an already-injected turn answered %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); len(s.InjectedTurns) != 1 || s.LoopArmSeen {
		t.Errorf("the guard changed the state: %+v", s)
	}
	// A different turn is not guarded: the loop-arm branch records it.
	if answer := promptSubmitAnswer(t, cwd, "s1", "t2", "Run crw-loop for this task", true); answer == "" {
		t.Fatal("a new turn answered nothing")
	}
	if s := state.ReadState(cwd, "s1"); len(s.InjectedTurns) != 2 || s.InjectedTurns[1] != "t2" {
		t.Errorf("a new turn was not recorded: %+v", s.InjectedTurns)
	}
}

// TestPromptSubmitPabcdOffIsSilent is the options.pabcdEnabled guard (hook.ts:685), the silent half
// of the corpus fixture hook__user-prompt-submit-checking-pabcd-trigger__pabcd_off_keeps_memory_marker:
// the hook answers nothing, and an ordinary prompt still writes nothing at all.
func TestPromptSubmitPabcdOffIsSilent(t *testing.T) {
	cwd := t.TempDir()
	if answer := promptSubmitAnswer(t, cwd, "s1", "t1", "Use crw-pabcd to start Plan phase", false); answer != "" {
		t.Errorf("answer %q", answer)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
		t.Error("a pabcd-off prompt wrote state")
	}
}

// TestPromptSubmitStopBudgetTurnStamp is the turn stamp at hook.ts:687-690: the Stop budget is reset
// once per genuine prompt, and a second prompt of the same turn does not rewrite the file.
func TestPromptSubmitStopBudgetTurnStamp(t *testing.T) {
	cwd := t.TempDir()
	previous := "t0"
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.StopBlockTotal, s.StopBlockTurnID, s.StopBlockCapNotified = 3, &previous, true
	})
	if answer := promptSubmitAnswer(t, cwd, "s1", "t1", "Summarise the README.", true); answer != "" {
		t.Errorf("answer %q", answer)
	}
	s := state.ReadState(cwd, "s1")
	if s.StopBlockTotal != 0 || s.StopBlockTurnID == nil || *s.StopBlockTurnID != "t1" || s.StopBlockCapNotified {
		t.Errorf("the budget was not stamped for the new turn: %+v", s)
	}
	// The same turn again: stopBlockTurnId already names it, so the file is not written.
	before, err := os.ReadFile(state.StatePath(cwd, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	if answer := promptSubmitAnswer(t, cwd, "s1", "t1", "Summarise the README.", true); answer != "" {
		t.Errorf("answer %q", answer)
	}
	after, err := os.ReadFile(state.StatePath(cwd, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the same turn rewrote the state file")
	}
}

// TestPromptSubmitLoopArmText is hook.test.ts "ORCH-MANDATE-01: loop request against un-armed FSM
// injects the arming mandate" (:628-647) and "wp3: arming limits precede recipes on both platforms"
// (:707-746): the mandate is injected byte for byte on both platforms, and it never arms the FSM by
// itself.
func TestPromptSubmitLoopArmText(t *testing.T) {
	const prompt = "이 유닛 crw-loop로 알아서 끝까지 해줘"
	for _, platform := range []string{"linux", "win32"} {
		t.Run(platform, func(t *testing.T) {
			cwd := t.TempDir()
			answer := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: prompt, TurnID: "t1", PabcdEnabled: true}, platform, promptSubmitHost(cwd))
			want := ResolveCRWInDirective(LoopArmDirective(platform), promptSubmitHost(cwd))
			if answer != want {
				t.Errorf("answer\n%q\nwant\n%q", answer, want)
			}
			for _, marker := range []string{"orchestrate arming mandate (ORCH-MANDATE-01)", "explicit interview-only, plan-only", "mention or quoted example alone is not authorization", "HOTL does not grant push, merge, release, deploy or external-message permission", "do not bypass a gate or fabricate an attestation/receipt"} {
				if !strings.Contains(answer, marker) {
					t.Errorf("the mandate is missing %q", marker)
				}
			}
			if platform == "win32" {
				if !strings.Contains(answer, "Set-Content -Encoding utf8") || strings.Contains(answer, "--attest <json>") {
					t.Error("the win32 mandate does not teach the file flag")
				}
			} else if !strings.Contains(answer, "--attest <json>") {
				t.Error("the posix mandate does not teach inline --attest")
			}
			if s := state.ReadState(cwd, "s1"); s.OrchestrationActive || s.Phase != state.PhaseIdle || s.LastInjectedPhase != nil || !s.LoopArmSeen {
				t.Errorf("the mandate armed the FSM: %+v", s)
			}
		})
	}
}

// TestPromptSubmitLoopArmRecordsTheTurnAndSurvivesATurnlessPayload is hook.test.ts "260714 wp3:
// loop-arm prompt persists loopArmSeen on the un-armed branch (even turnless)" (:587-598): the flag
// is persisted outside the turn guard, and injectedTurns stays turn-guarded.
func TestPromptSubmitLoopArmRecordsTheTurnAndSurvivesATurnlessPayload(t *testing.T) {
	cwd := t.TempDir()
	if answer := promptSubmitAnswer(t, cwd, "la1", "t1", "pabcd 여러 번 돌려서 해결해", true); answer == "" {
		t.Fatal("the loop-arm branch answered nothing")
	}
	s := state.ReadState(cwd, "la1")
	if !s.LoopArmSeen || s.OrchestrationActive {
		t.Errorf("state after the arming prompt: %+v", s)
	}
	if len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "t1" {
		t.Errorf("the turn was not recorded: %+v", s.InjectedTurns)
	}
	if answer := promptSubmitAnswer(t, cwd, "la2", "", "crw-loop로 알아서 끝까지 해줘", true); answer == "" {
		t.Fatal("a turnless loop-arm prompt answered nothing")
	}
	s = state.ReadState(cwd, "la2")
	if !s.LoopArmSeen {
		t.Errorf("a turnless payload lost the flag: %+v", s)
	}
	if len(s.InjectedTurns) != 0 {
		t.Errorf("a turnless payload recorded a turn: %+v", s.InjectedTurns)
	}
}

// TestPromptSubmitLoopArmComposesWithAgbrowse is hook.test.ts "ORCH-MANDATE-01: loop-arm and agbrowse
// directives compose when both are requested" (:669-680): the mandate comes first and the search
// guidance second, one blank line apart.
func TestPromptSubmitLoopArmComposesWithAgbrowse(t *testing.T) {
	cwd := t.TempDir()
	answer := promptSubmitAnswer(t, cwd, "s1", "t1", "agbrowse로 검증하면서 crw-loop 돌려줘", true)
	want := ResolveCRWInDirective(LoopArmDirective(""), promptSubmitHost(cwd)) + "\n\n" + AgbrowseSearchDirective
	if answer != want {
		t.Errorf("answer\n%q\nwant\n%q", answer, want)
	}
	if i, j := strings.Index(answer, "arming mandate"), strings.Index(answer, "[crw: SEARCH"); i < 0 || j < 0 || i > j {
		t.Errorf("the directives are out of order: %q", answer)
	}
}

// TestPromptSubmitAppendTurnKeepsTheLastFifty is hook-continuation.test.ts "hybrid: injectedTurns is
// bounded to 50 (audit blocker #2)" (:325-337) at appendTurn (hook.ts:577-581): the state file's
// growth is bounded and the most recent turn survives.
func TestPromptSubmitAppendTurnKeepsTheLastFifty(t *testing.T) {
	turns := []string{}
	for n := 0; n < 60; n++ {
		turns = promptSubmitAppendTurn(turns, "turn-"+string(rune('a'+n%26))+string(rune('0'+n/26)))
	}
	if len(turns) != promptSubmitMaxInjectedTurns {
		t.Fatalf("%d turns kept, want %d", len(turns), promptSubmitMaxInjectedTurns)
	}
	if last := turns[len(turns)-1]; last != "turn-h2" {
		t.Errorf("the last turn was dropped: %q", last)
	}
	if turns[0] != "turn-k0" {
		t.Errorf("the window starts at %q, want turn-k0", turns[0])
	}
	short := promptSubmitAppendTurn([]string{"a"}, "b")
	if len(short) != 2 || short[0] != "a" || short[1] != "b" {
		t.Errorf("a short list was not appended in order: %+v", short)
	}
}

// TestPromptSubmitOrchestrateCommandSeamIsNotHandled is the L3b seam (hook.ts:693-702): the chat
// command is parsed here, its handler belongs to the successor unit, and an unhandled command falls
// through to the loose path, which this unit leaves silent. The corpus command fixtures stay pending
// until that unit lands.
func TestPromptSubmitOrchestrateCommandSeamIsNotHandled(t *testing.T) {
	if command := fsm.ParseOrchestrateCommand("orchestrate A"); command == nil {
		t.Fatal("the recorded command does not parse")
	}
	cwd := t.TempDir()
	if answer := promptSubmitAnswer(t, cwd, "s1", "t1", "orchestrate A", true); answer != "" {
		t.Errorf("an unhandled command answered %q", answer)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
		t.Error("an unhandled command wrote state")
	}
}

// TestPromptSubmitTriggerAndGoalFirewallAreSilentHere pins what this unit does with the branches it
// hands to the successor: a phase trigger injects nothing yet, and an interview trigger under an
// active goal is suppressed (hook.ts:720-725) rather than injected. The discriminating assertion
// belongs to the successor unit, which owns the trigger branch.
func TestPromptSubmitTriggerAndGoalFirewallAreSilentHere(t *testing.T) {
	for _, prompt := range []string{"Use crw-pabcd to start Plan phase", "Start crw-pabcd interview for the onboarding flow"} {
		cwd := t.TempDir()
		if answer := promptSubmitAnswer(t, cwd, "s1", "t1", prompt, true); answer != "" {
			t.Errorf("%q answered %q", prompt, answer)
		}
		if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
			t.Errorf("%q wrote state", prompt)
		}
	}
}

// TestPromptSubmitMarkerKeepsAParticipatingWritersUpdate is the data-loss fix on the marker write:
// the oracle reads the state, sets the marker and writes the whole state back with no lock, so an
// update a participating writer lands between the read and the write is overwritten and lost. The
// seam stands in for the writer, which the handler cannot be timed against.
func TestPromptSubmitMarkerKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
	writer := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.MemoryWriteGrant = true
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "rec-s1", Prompt: "Remember this: the deploy key lives in the vault.", TurnID: "rec-t1"}, "", promptSubmitHost(cwd), writer)
	if answer != "" {
		t.Errorf("answer %q", answer)
	}
	s := state.ReadState(cwd, "rec-s1")
	if !s.MemoryWriteGrant {
		t.Errorf("the participating writer's update was lost: %+v", s)
	}
	if !s.MemoryWriteRequested || s.MemoryWriteTurn == nil || *s.MemoryWriteTurn != "rec-t1" {
		t.Errorf("the marker was not written: %+v", s)
	}
}

// TestPromptSubmitStampKeepsAParticipatingWritersUpdate is the same fix on the Stop-budget stamp.
func TestPromptSubmitStampKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
	writer := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.MemoryWriteGrant = true
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "rec-s1", Prompt: "Summarise the README.", TurnID: "rec-t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), writer)
	if answer != "" {
		t.Errorf("answer %q", answer)
	}
	s := state.ReadState(cwd, "rec-s1")
	if !s.MemoryWriteGrant {
		t.Errorf("the participating writer's update was lost: %+v", s)
	}
	if s.StopBlockTurnID == nil || *s.StopBlockTurnID != "rec-t1" {
		t.Errorf("the budget was not stamped: %+v", s)
	}
}

// TestPromptSubmitLeavesAnUnreadableStateAlone is the other half of the fix: the oracle's readState
// answers a default for a file it cannot read, so its write would replace whatever the file held
// with that default; the port writes nothing and the file is left as it was.
func TestPromptSubmitLeavesAnUnreadableStateAlone(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
	path := state.StatePath(cwd, "rec-s1")
	const corrupt = "{ this is not a state\n"
	if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	if answer := promptSubmitAnswer(t, cwd, "rec-s1", "rec-t1", "Remember this: the deploy key lives in the vault.", true); answer != "" {
		t.Errorf("answer %q", answer)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != corrupt {
		t.Errorf("the unreadable state was rewritten: %q", after)
	}
}

// TestPromptSubmitLoopArmKeepsAParticipatingWritersUpdate is the same fix on the loop-arm write.
func TestPromptSubmitLoopArmKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
	writer := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.MemoryWriteGrant = true
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "rec-s1", Prompt: "Run crw-loop for this task", TurnID: "rec-t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), writer)
	if answer == "" {
		t.Fatal("the loop-arm branch answered nothing")
	}
	s := state.ReadState(cwd, "rec-s1")
	if !s.MemoryWriteGrant {
		t.Errorf("the participating writer's update was lost: %+v", s)
	}
	if !s.LoopArmSeen {
		t.Errorf("loopArmSeen was not recorded: %+v", s)
	}
}

// TestPromptSubmitLoopArmStoresTheTurnOnce is the concurrent case the unlocked guard cannot separate
// (hook.ts:691 is read before the lock in both invocations): both answer the mandate, as the oracle's
// do, and the turn is stored once, as the oracle's own write also ends up storing it.
func TestPromptSubmitLoopArmStoresTheTurnOnce(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
	writer := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.InjectedTurns = []string{"rec-t1"}
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "rec-s1", Prompt: "Run crw-loop for this task", TurnID: "rec-t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), writer)
	if answer == "" {
		t.Fatal("the mandate was not answered")
	}
	if s := state.ReadState(cwd, "rec-s1"); len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "rec-t1" {
		t.Errorf("the turn was stored %d times: %+v", len(s.InjectedTurns), s.InjectedTurns)
	}
}

// TestPromptSubmitLoopArmFailsSilentOnAWriteFailure is one half of the write decision: a write that
// failed ends the hook in silence, as the oracle's own writeState throwing does, so an unrecorded
// mandate is never answered.
func TestPromptSubmitLoopArmFailsSilentOnAWriteFailure(t *testing.T) {
	cwd := t.TempDir()
	promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
	calls := 0
	lock := func(cwd, sessionID string, fn func() error) error {
		calls++
		if calls == 2 {
			return errors.New("lock unavailable")
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "rec-s1", Prompt: "Run crw-loop for this task", TurnID: "rec-t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), lock)
	if answer != "" {
		t.Errorf("a failed loop-arm write answered %q", answer)
	}
	if calls != 2 {
		t.Errorf("the lock was taken %d times, want 2", calls)
	}
}

// TestPromptSubmitLoopArmAnswersWhenTheStateCannotBeRewritten is the other half: the port's rewrite
// guard skips the write of a state the reader would not keep whole, and the mandate is still
// answered, because the oracle has no such guard and would have written and answered there.
func TestPromptSubmitLoopArmAnswersWhenTheStateCannotBeRewritten(t *testing.T) {
	cwd := t.TempDir()
	if _, err := state.EnsureState(cwd, "rec-s1"); err != nil {
		t.Fatal(err)
	}
	path := state.StatePath(cwd, "rec-s1")
	const corrupt = "{ not a state\n"
	if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	answer := promptSubmitAnswer(t, cwd, "rec-s1", "rec-t1", "Run crw-loop for this task", true)
	if !strings.HasPrefix(answer, "[crw: LOOP — orchestrate arming mandate (ORCH-MANDATE-01)]") {
		t.Errorf("the mandate was not answered: %q", answer)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != corrupt {
		t.Errorf("the unreadable state was rewritten: %q, %v", after, err)
	}
}

// TestPromptSubmitLoopArmIsSilentWhenTheWriteFails: the mandate is not emitted unrecorded, so a lock
// this port cannot take, or a write that fails, answers nothing - which is what the oracle's own
// throwing writeState produced, since cli.ts catches it as silence. A write the port's rewrite guard
// only skips is the other half, in TestPromptSubmitLoopArmAnswersWhenTheStateCannotBeRewritten.
func TestPromptSubmitLoopArmIsSilentWhenTheWriteFails(t *testing.T) {
	cwd := t.TempDir()
	if answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "Run crw-loop for this task", TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), promptSubmitFailingLock); answer != "" {
		t.Errorf("a lock that cannot be taken answered %q", answer)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
		t.Error("a failed write created state")
	}
}

// TestPromptSubmitMarkerFailureIsDropped: the marker write is the one the oracle wraps in try/catch,
// so a lock that cannot be taken records nothing and the prompt still works.
func TestPromptSubmitMarkerFailureIsDropped(t *testing.T) {
	cwd := t.TempDir()
	answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "Remember this: the deploy key lives in the vault.", TurnID: "t1"}, "", promptSubmitHost(cwd), promptSubmitFailingLock)
	if answer != "" {
		t.Errorf("answer %q", answer)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
		t.Error("a failed marker write created state")
	}
}
