// prompt_trigger_test.go is the Go form of the trigger-injection, re-injection and status-line cases
// of CXC v0.2.40 pabcd-state/test/hook.test.ts and hook-continuation.test.ts for the unit that owns
// hook.ts:755-847 and renderStatusLine (hook.ts:849-853). Expected texts are built from the directive
// helpers directives_test.go already holds to the recorded oracle table, or compared as full
// literals, so every comparison is byte for byte.
package hook

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptTriggerAnswer runs the handler the way the host runs the leg, with the transcript path the
// R-11 cases need.
func promptTriggerAnswer(t *testing.T, cwd, sessionID, turn, prompt, transcript string, enabled bool) string {
	t.Helper()
	return PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: sessionID, Prompt: prompt,
		TurnID: turn, TranscriptPath: transcript, PabcdEnabled: enabled}, "", promptSubmitHost(cwd))
}

// promptTriggerAdvice is the loose trigger branch's answer: the directive the phase and the policy
// select, the TRIGGER_AUTHORITY_NOTE resolved at emission, and the phase footer.
func promptTriggerAdvice(env host.LookupEnv, directive string, phase state.Phase) string {
	return WithFooter(directive+"\n\n"+ResolveCRWInDirective(TriggerAuthorityNote, env), phase)
}

// promptTriggerGoalEnv is promptSubmitHost with the goals database at home, so a case can make the
// host's goal mode active (the L17 firewall).
func promptTriggerGoalEnv(home string) host.LookupEnv {
	return func(key string) (string, bool) {
		switch key {
		case "CRW_BIN":
			return "{CRW}", true
		case "CODEX_SQLITE_HOME":
			return home, true
		}
		return "", false
	}
}

// TestPromptTriggerInjectsTheAdviceOncePerTurn is hook.test.ts "handleUserPromptSubmit: idempotent
// within same (session,turn)" (:440-452) and "new turn re-injects" (:453-464) on the loose path: the
// advice is injected once, a re-fired hook for the same turn answers nothing, and a new turn injects
// again.
func TestPromptTriggerInjectsTheAdviceOncePerTurn(t *testing.T) {
	cwd := t.TempDir()
	const prompt = "Use crw-pabcd to start Plan phase"
	env := promptSubmitHost(cwd)
	want := promptTriggerAdvice(env, InterviewDirective(env), state.PhaseIdle)
	if first := promptSubmitAnswer(t, cwd, "s1", "t1", prompt, true); first != want {
		t.Errorf("the first turn\n got %q\nwant %q", first, want)
	}
	if second := promptSubmitAnswer(t, cwd, "s1", "t1", prompt, true); second != "" {
		t.Errorf("a re-fired turn answered %q", second)
	}
	if third := promptSubmitAnswer(t, cwd, "s1", "t2", prompt, true); third != want {
		t.Errorf("a new turn\n got %q\nwant %q", third, want)
	}
	if s := state.ReadState(cwd, "s1"); len(s.InjectedTurns) != 2 || s.InjectedTurns[0] != "t1" || s.InjectedTurns[1] != "t2" {
		t.Errorf("the recorded turns: %+v", s.InjectedTurns)
	}
}

// TestPromptTriggerSessionsAreIndependent is hook.test.ts "different sessions are independent"
// (:465-476).
func TestPromptTriggerSessionsAreIndependent(t *testing.T) {
	cwd := t.TempDir()
	const prompt = "Use crw-pabcd to start Plan phase"
	env := promptSubmitHost(cwd)
	want := promptTriggerAdvice(env, InterviewDirective(env), state.PhaseIdle)
	for _, session := range []string{"alpha", "beta"} {
		if got := promptSubmitAnswer(t, cwd, session, "t1", prompt, true); got != want {
			t.Errorf("%s\n got %q\nwant %q", session, got, want)
		}
	}
}

// TestPromptTriggerAgbrowseOnlyInjectsTheSearchDirective is hook.test.ts "agbrowse request injects
// search directive without activating PABCD" (:488-519): the un-armed, un-triggered search request
// answers the search directive and leaves the phase alone.
func TestPromptTriggerAgbrowseOnlyInjectsTheSearchDirective(t *testing.T) {
	cwd := t.TempDir()
	got := promptSubmitAnswer(t, cwd, "s1", "t1", "agbrowse를 통해서 질문해줘", true)
	if got != AgbrowseSearchDirective {
		t.Errorf("answer\n got %q\nwant %q", got, AgbrowseSearchDirective)
	}
	for _, marker := range []string{"[crw: SEARCH", "crw-search", "agbrowse fetch", "Never use plain", "dev/references/browser-routing.md", "optional", "diagnosed CDP connection failure", "task-owned"} {
		if !strings.Contains(got, marker) {
			t.Errorf("the search directive is missing %q", marker)
		}
	}
	s := state.ReadState(cwd, "s1")
	if s.OrchestrationActive || s.LastInjectedPhase != nil || s.Phase != state.PhaseIdle {
		t.Errorf("the search branch activated PABCD: %+v", s)
	}
}

// TestPromptTriggerAgbrowseIsIdempotentWithinATurn is hook.test.ts "agbrowse request is idempotent
// within same turn" (:748-759), including the accepted agbrowe typo.
func TestPromptTriggerAgbrowseIsIdempotentWithinATurn(t *testing.T) {
	cwd := t.TempDir()
	const prompt = "agbrowe를 통해서 질문해줘"
	if first := promptSubmitAnswer(t, cwd, "s1", "t1", prompt, true); first != AgbrowseSearchDirective {
		t.Errorf("the first search request\n got %q\nwant %q", first, AgbrowseSearchDirective)
	}
	if second := promptSubmitAnswer(t, cwd, "s1", "t1", prompt, true); second != "" {
		t.Errorf("the second search request answered %q", second)
	}
}

// TestPromptTriggerHintWinsOverAgbrowse is hook.test.ts "PABCD hint wins over agbrowse without phase
// entry" (:760-775): the trigger branch answers the advice and the search directive is not appended.
func TestPromptTriggerHintWinsOverAgbrowse(t *testing.T) {
	cwd := t.TempDir()
	env := promptSubmitHost(cwd)
	got := promptSubmitAnswer(t, cwd, "s1", "t1", "Use crw-pabcd to start Plan phase with agbrowse", true)
	want := promptTriggerAdvice(env, InterviewDirective(env), state.PhaseIdle)
	if got != want {
		t.Errorf("answer\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "agbrowse fetch") {
		t.Error("the search directive leaked into the trigger branch")
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle || s.OrchestrationActive || s.LastInjectedPhase != nil {
		t.Errorf("the trigger branch moved the phase: %+v", s)
	}
}

// TestPromptTriggerInterviewPolicyOffSelectsPlanAdvice is hook.test.ts "wp4: interview policy off
// selects PLAN advice without phase entry" (:776-791).
func TestPromptTriggerInterviewPolicyOffSelectsPlanAdvice(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, projectcfg.ConfigFilename), []byte(`{"interview":"off"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := promptSubmitHost(cwd)
	got := promptSubmitAnswer(t, cwd, "s1off", "t1", "Use crw-pabcd to start Plan phase with agbrowse", true)
	want := promptTriggerAdvice(env, PhaseDirective(state.PhaseP, nil), state.PhaseIdle)
	if got != want {
		t.Errorf("answer\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "agbrowse fetch") {
		t.Error("the search directive leaked in")
	}
}

// TestPromptTriggerFailClosedFreshSessionStaysSilent is hook.test.ts "hybrid FAIL-CLOSED: fresh
// session, non-trigger prompt -> ” (no I-phase leak)" (:812-822).
func TestPromptTriggerFailClosedFreshSessionStaysSilent(t *testing.T) {
	cwd := t.TempDir()
	if got := promptSubmitAnswer(t, cwd, "s1", "t1", "hello, can you help me", true); got != "" {
		t.Errorf("a non-trigger answered %q", got)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
		t.Error("a non-trigger wrote state")
	}
}

// TestRenderStatusLine is hook.test.ts "L5: chat 'orchestrate status' returns the one-line status with
// flags" (:954-965) at renderStatusLine (hook.ts:849-853): the phase, its label and the three flags.
func TestRenderStatusLine(t *testing.T) {
	for _, c := range []struct {
		phase                    state.Phase
		interview                bool
		auditPassed, checkPassed bool
		want                     string
	}{
		{state.PhaseP, false, false, false, "[crw status] IPABCD: P (PLAN) · interview=false auditPassed=false checkPassed=false"},
		{state.PhaseI, true, true, true, "[crw status] IPABCD: I (INTERVIEW) · interview=true auditPassed=true checkPassed=true"},
		{state.PhaseIdle, false, true, false, "[crw status] IPABCD: IDLE (IDLE) · interview=false auditPassed=true checkPassed=false"},
		{state.Phase("X"), false, false, false, "[crw status] IPABCD: X (X) · interview=false auditPassed=false checkPassed=false"},
	} {
		if got := RenderStatusLine(c.phase, c.interview, c.auditPassed, c.checkPassed); got != c.want {
			t.Errorf("RenderStatusLine(%q)\n got %q\nwant %q", c.phase, got, c.want)
		}
	}
}

// TestPromptTriggerLeavesThePhaseAloneFromIdle is hook.test.ts "040: a natural-language build trigger
// from IDLE leaves the phase alone" (:1059-1072).
func TestPromptTriggerLeavesThePhaseAloneFromIdle(t *testing.T) {
	cwd := t.TempDir()
	got := promptSubmitAnswer(t, cwd, "ta1", "t1", "crw-pabcd로 구현 진행해줘", true)
	for _, marker := range []string{"BUILD", "TRIGGER-AUTHORITY-01", "orchestrate"} {
		if !strings.Contains(got, marker) {
			t.Errorf("the advice is missing %q: %q", marker, got)
		}
	}
	s := state.ReadState(cwd, "ta1")
	if s.Phase != state.PhaseIdle || s.OrchestrationActive || s.LastInjectedPhase != nil {
		t.Errorf("the trigger moved the phase: %+v", s)
	}
}

// TestPromptTriggerCannotMoveAMidCyclePhase is hook.test.ts "040: a mid-cycle trigger cannot move the
// phase and the footer reports the real one" (:1110-1128).
func TestPromptTriggerCannotMoveAMidCyclePhase(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseP
	promptSubmitStateFile(t, cwd, "ta3", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, &phase
	})
	got := promptSubmitAnswer(t, cwd, "ta3", "t1", "crw-pabcd로 구현 진행해줘", true)
	if !strings.Contains(got, "TRIGGER-AUTHORITY-01") || !strings.Contains(got, "IPABCD: P") {
		t.Errorf("the footer must name the persisted phase: %q", got)
	}
	s := state.ReadState(cwd, "ta3")
	if s.Phase != state.PhaseP || !s.OrchestrationActive || s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseP {
		t.Errorf("the trigger moved the phase: %+v", s)
	}
}

// TestPromptTriggerLoopArmSeenSurvivesTheStageMarkerBranch is hook.test.ts "040: an armed session's
// loop request records loopArmSeen through the stage-marker branch" (:1129-1137); the transcript
// makes the R-11 branch the one that writes.
func TestPromptTriggerLoopArmSeenSurvivesTheStageMarkerBranch(t *testing.T) {
	cwd := t.TempDir()
	transcript := filepath.Join(cwd, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("[crw — B: BUILD]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phase := state.PhaseB
	promptSubmitStateFile(t, cwd, "ar1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	})
	if got := promptTriggerAnswer(t, cwd, "ar1", "t1", "pabcd 여러 번 돌려줘", transcript, true); got != "" {
		t.Errorf("the stage-marker branch answered %q", got)
	}
	if s := state.ReadState(cwd, "ar1"); !s.LoopArmSeen {
		t.Errorf("loopArmSeen was lost through the stage-marker branch: %+v", s)
	}
}

// TestPromptTriggerLoopArmSeenSurvivesMode2 is hook.test.ts "040: same through mode 2 (phase changed
// since last inject)" (:1138-1148).
func TestPromptTriggerLoopArmSeenSurvivesMode2(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseB
	promptSubmitStateFile(t, cwd, "ar2", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseC, true, &phase
	})
	promptSubmitAnswer(t, cwd, "ar2", "t1", "pabcd 여러 번 돌려줘", true)
	s := state.ReadState(cwd, "ar2")
	if !s.LoopArmSeen {
		t.Errorf("loopArmSeen was lost through mode 2: %+v", s)
	}
	if s.Phase != state.PhaseC {
		t.Errorf("mode 2 changed the phase: %+v", s)
	}
}

// TestPromptTriggerLoopArmSeenSurvivesMode3 is hook.test.ts "040: same through mode 3 (same phase,
// header only)" (:1149-1160).
func TestPromptTriggerLoopArmSeenSurvivesMode3(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseC
	promptSubmitStateFile(t, cwd, "ar3", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseC, true, &phase
	})
	promptSubmitAnswer(t, cwd, "ar3", "t1", "pabcd 여러 번 돌려줘", true)
	if s := state.ReadState(cwd, "ar3"); !s.LoopArmSeen {
		t.Errorf("loopArmSeen was lost through mode 3: %+v", s)
	}
}

// TestPromptTriggerTurnlessHintsPreserveThePhase is hook.test.ts "040: turnless hints preserve phase
// while loop requests persist bookkeeping" (:1161-1200).
func TestPromptTriggerTurnlessHintsPreserveThePhase(t *testing.T) {
	t.Run("a turnless hint writes nothing", func(t *testing.T) {
		cwd := t.TempDir()
		env := promptSubmitHost(cwd)
		got := promptSubmitAnswer(t, cwd, "tl1", "", "Use crw-pabcd to start Plan phase", true)
		if want := promptTriggerAdvice(env, InterviewDirective(env), state.PhaseIdle); got != want {
			t.Errorf("answer\n got %q\nwant %q", got, want)
		}
		if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); err == nil {
			t.Error("a turnless hint wrote state")
		}
		if s := state.ReadState(cwd, "tl1"); s.Phase != state.PhaseIdle || s.OrchestrationActive || s.LastInjectedPhase != nil || len(s.InjectedTurns) != 0 {
			t.Errorf("a turnless hint changed the state: %+v", s)
		}
	})
	t.Run("a turnless loop request persists the flag", func(t *testing.T) {
		cwd := t.TempDir()
		if got := promptSubmitAnswer(t, cwd, "tl2", "", "pabcd 여러 번 돌려줘", true); got == "" {
			t.Fatal("a turnless loop request answered nothing")
		}
		if s := state.ReadState(cwd, "tl2"); !s.LoopArmSeen || s.Phase != state.PhaseIdle {
			t.Errorf("the turnless loop request: %+v", s)
		}
	})
	t.Run("a turnless loop request in an armed phase keeps the phase", func(t *testing.T) {
		cwd := t.TempDir()
		phase := state.PhaseC
		promptSubmitStateFile(t, cwd, "tl3", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseC, true, &phase
		})
		if got := promptSubmitAnswer(t, cwd, "tl3", "", "pabcd 여러 번 돌려줘", true); got == "" {
			t.Fatal("a turnless armed loop request answered nothing")
		}
		if s := state.ReadState(cwd, "tl3"); !s.LoopArmSeen || s.Phase != state.PhaseC {
			t.Errorf("the turnless armed loop request: %+v", s)
		}
	})
}

// TestPromptTriggerMode2InjectsTheFullDirective is hook-continuation.test.ts "hybrid mode 2: active +
// phase changed -> full directive for new phase" (:220-234).
func TestPromptTriggerMode2InjectsTheFullDirective(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseP
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
	})
	got := promptSubmitAnswer(t, cwd, "s1", "t2", "here is my work", true)
	want := WithFooter(PhaseDirective(state.PhaseA, nil), state.PhaseA)
	if got != want {
		t.Errorf("answer\n got %q\nwant %q", got, want)
	}
	for _, marker := range []string{"$crw:crw-pabcd", "$crw:crw-dev-code-reviewer"} {
		if !strings.Contains(got, marker) {
			t.Errorf("the directive is missing %q", marker)
		}
	}
	if s := state.ReadState(cwd, "s1"); s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseA {
		t.Errorf("lastInjectedPhase: %+v", s.LastInjectedPhase)
	}
}

// TestPromptTriggerMode3InjectsTheStageHeader is hook-continuation.test.ts "hybrid mode 3: active +
// same phase -> short stage header every new turn" (:300-311).
func TestPromptTriggerMode3InjectsTheStageHeader(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseA
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
	})
	got := promptSubmitAnswer(t, cwd, "s1", "t3", "more work", true)
	want := WithFooter(BuildStageHeader(state.PhaseA), state.PhaseA)
	if got != want {
		t.Errorf("answer\n got %q\nwant %q", got, want)
	}
}

// TestPromptTriggerIsIdempotentAcrossModes is hook-continuation.test.ts "hybrid: idempotent within
// same (session,turn) across modes" (:312-324).
func TestPromptTriggerIsIdempotentAcrossModes(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseA
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
	})
	if first := promptSubmitAnswer(t, cwd, "s1", "tDup", "x", true); first == "" {
		t.Fatal("the first prompt answered nothing")
	}
	if second := promptSubmitAnswer(t, cwd, "s1", "tDup", "x", true); second != "" {
		t.Errorf("the re-fired turn answered %q", second)
	}
}

// TestPromptTriggerInjectedTurnsAreBoundedToFifty is hook-continuation.test.ts "hybrid: injectedTurns
// is bounded to 50 (audit blocker #2)" (:325-338) driven through the handler.
func TestPromptTriggerInjectedTurnsAreBoundedToFifty(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseA
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
	})
	for n := 0; n < 60; n++ {
		promptSubmitAnswer(t, cwd, "s1", "turn-"+strconv.Itoa(n), "work", true)
	}
	s := state.ReadState(cwd, "s1")
	if len(s.InjectedTurns) > promptSubmitMaxInjectedTurns {
		t.Errorf("%d turns kept, want at most %d", len(s.InjectedTurns), promptSubmitMaxInjectedTurns)
	}
	if !slices.Contains(s.InjectedTurns, "turn-59") {
		t.Errorf("the newest turn was dropped: %+v", s.InjectedTurns)
	}
	if slices.Contains(s.InjectedTurns, "turn-0") {
		t.Errorf("the oldest turn survived the bound: %+v", s.InjectedTurns)
	}
}

// TestPromptTriggerStageMarkerInTranscriptSuppressesReinjection is hook-continuation.test.ts "R-11:
// passive re-fire with phase marker already in transcript -> no re-inject" (:339-358).
func TestPromptTriggerStageMarkerInTranscriptSuppressesReinjection(t *testing.T) {
	cwd := t.TempDir()
	transcript := filepath.Join(cwd, "transcript.jsonl")
	line := `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"[crw — B: BUILD]"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	phase := state.PhaseB
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	})
	if got := promptTriggerAnswer(t, cwd, "s1", "fresh-turn-after-compaction", "keep going", transcript, true); got != "" {
		t.Errorf("a marked transcript answered %q", got)
	}
	s := state.ReadState(cwd, "s1")
	if s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseB {
		t.Errorf("the stage-marker branch did not record lastInjectedPhase: %+v", s.LastInjectedPhase)
	}
	if !slices.Contains(s.InjectedTurns, "fresh-turn-after-compaction") {
		t.Errorf("the stage-marker branch did not record the turn: %+v", s.InjectedTurns)
	}
}

// TestPromptTriggerContextPressureSuppressesReinjection is hook-continuation.test.ts "R-11:
// context-pressure transcript suppresses passive injection" (:359-377).
func TestPromptTriggerContextPressureSuppressesReinjection(t *testing.T) {
	cwd := t.TempDir()
	transcript := filepath.Join(cwd, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("# Compacted Session Handoff\nsummary...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phase := state.PhaseB
	promptSubmitStateFile(t, cwd, "s2", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseC, true, &phase
	})
	if got := promptTriggerAnswer(t, cwd, "s2", "t-after-compact", "continue", transcript, true); got != "" {
		t.Errorf("a context-pressure tail answered %q", got)
	}
}

// TestPromptTriggerExplicitTriggerIgnoresTheTranscriptMarker is hook-continuation.test.ts "R-11:
// explicit trigger still injects even if a marker is present" (:378-397).
func TestPromptTriggerExplicitTriggerIgnoresTheTranscriptMarker(t *testing.T) {
	cwd := t.TempDir()
	transcript := filepath.Join(cwd, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("[crw — B: BUILD]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phase := state.PhaseB
	promptSubmitStateFile(t, cwd, "s3", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	})
	got := promptTriggerAnswer(t, cwd, "s3", "t-trigger", "Use crw-pabcd to start Check phase", transcript, true)
	if !strings.Contains(got, "CHECK") {
		t.Errorf("the explicit trigger was suppressed: %q", got)
	}
}

// TestPromptTriggerPassiveInterviewFirewallSuppressesUnderAGoal is hook-continuation.test.ts "L17
// firewall: active goal suppresses PASSIVE I re-injection (phase=I already armed)" (:103-116).
func TestPromptTriggerPassiveInterviewFirewallSuppressesUnderAGoal(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	home := sessionHookGoalsDB(t, filepath.Join(dir, "codex"),
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, goal_id TEXT NOT NULL, objective TEXT NOT NULL, status TEXT NOT NULL)",
		"INSERT INTO thread_goals (thread_id, goal_id, objective, status) VALUES ('g-int', 'g', 'obj', 'active')")
	phase := state.PhaseP
	promptSubmitStateFile(t, cwd, "g-int", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseI, true, &phase
	})
	got := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "g-int", Prompt: "continue", TurnID: "t9", PabcdEnabled: true}, "", promptTriggerGoalEnv(home))
	if got != "" {
		t.Errorf("an active goal did not suppress the passive I re-injection: %q", got)
	}
}

// TestPromptTriggerPassiveInterviewReinjectsWithoutAGoal is hook-continuation.test.ts "L17 firewall:
// with NO goal, passive I phase still re-injects the interview directive" (:117-130).
func TestPromptTriggerPassiveInterviewReinjectsWithoutAGoal(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseP
	promptSubmitStateFile(t, cwd, "ni", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseI, true, &phase
	})
	got := promptSubmitAnswer(t, cwd, "ni", "t9", "continue", true)
	if !strings.Contains(got, "INTERVIEW") {
		t.Errorf("without a goal the interview must still drive: %q", got)
	}
}

// TestPromptTriggerKeepsAParticipatingWritersUpdate is the data-loss fix on this unit's writes: the
// oracle reads the state, changes fields and writes the whole state back with no lock, so an update a
// participating writer lands between the read and the write is overwritten and lost. The lock stands in
// for the writer, which the handler cannot be timed against.
func TestPromptTriggerKeepsAParticipatingWritersUpdate(t *testing.T) {
	for _, c := range []struct {
		name    string
		prompt  string
		state   func(*state.State)
		loopArm bool
		turn    string
	}{
		{name: "the trigger branch", prompt: "Use crw-pabcd to start Plan phase", turn: "rec-t1"},
		{name: "the fail-closed agbrowse branch", prompt: "agbrowse를 통해서 질문해줘", turn: "rec-t1"},
		{name: "mode 2", prompt: "here is my work", turn: "rec-t1",
			state: func(s *state.State) {
				phase := state.PhaseP
				s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
			}},
		{name: "mode 3", prompt: "more work", turn: "rec-t1",
			state: func(s *state.State) {
				phase := state.PhaseA
				s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := t.TempDir()
			if c.state != nil {
				promptSubmitStateFile(t, cwd, "rec-s1", c.state)
			} else {
				promptSubmitStateFile(t, cwd, "rec-s1", func(*state.State) {})
			}
			writer := func(cwd, sessionID string, fn func() error) error {
				s := state.ReadState(cwd, sessionID)
				s.MemoryWriteGrant = true
				if err := state.WriteState(cwd, s); err != nil {
					return err
				}
				return state.WithSessionLock(cwd, sessionID, fn)
			}
			answer := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "rec-s1", Prompt: c.prompt,
				TurnID: c.turn, PabcdEnabled: true}, "", promptSubmitHost(cwd), writer)
			if answer == "" {
				t.Fatal("the branch answered nothing")
			}
			s := state.ReadState(cwd, "rec-s1")
			if !s.MemoryWriteGrant {
				t.Errorf("the participating writer's update was lost: %+v", s)
			}
			if !slices.Contains(s.InjectedTurns, c.turn) {
				t.Errorf("the turn was not recorded: %+v", s.InjectedTurns)
			}
		})
	}
}

// TestPromptTriggerReinjectionNamesThePhaseInTheCursor is the other half of the mode 2 and
// stage-marker writes: the oracle stores `lastInjectedPhase: state.phase`, the phase the handler read,
// not the phase a fresh read might hold.
func TestPromptTriggerReinjectionNamesThePhaseInTheCursor(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseP
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseA, true, &phase
	})
	promptSubmitAnswer(t, cwd, "s1", "t2", "here is my work", true)
	s := state.ReadState(cwd, "s1")
	if s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseA {
		t.Errorf("lastInjectedPhase = %v, want A", s.LastInjectedPhase)
	}
}
