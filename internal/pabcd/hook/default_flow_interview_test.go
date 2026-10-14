// default_flow_interview_test.go pins CRW-1340: the CRW default flow never enters the PABCD I phase
// and never asks the user a synchronous question. The I phase, its state and its hooks stay only
// for the explicit paths the CXC contract fixtures pin (`orchestrate I`, the explicit `interview`
// hint, a project crw.json that opts into an interview policy); none of them is on the default path.
package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// unroutedQuestionSkills are the skills no I-phase hook text routes to: crw-interview, which CRW-1340
// removed, and crw-define, which stays (Jun's 10-14 correction) for the registration of a milestone and
// its issues by the top session and the parent, never for a task session's own question.
var unroutedQuestionSkills = []string{"crw-interview", "crw-define"}

func TestCRWDefaultFlowNeverEntersInterview(t *testing.T) {
	t.Run("session start bootstraps IDLE", func(t *testing.T) {
		cwd := t.TempDir()
		if got := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: cwd, SessionID: "df-start"}, promptSubmitHost(cwd)); got != "" {
			t.Errorf("SessionStart answered %q", got)
		}
		if s := state.ReadState(cwd, "df-start"); s.Phase != state.PhaseIdle {
			t.Errorf("SessionStart left phase %s, want IDLE", s.Phase)
		}
	})

	t.Run("the default interview policy never advises the Interview", func(t *testing.T) {
		if projectcfg.DefaultPolicy != projectcfg.PolicyOff {
			t.Errorf("DefaultPolicy = %q, want %q", projectcfg.DefaultPolicy, projectcfg.PolicyOff)
		}
		if got := projectcfg.ReadPolicy(t.TempDir()); got != projectcfg.PolicyOff {
			t.Errorf("a project without crw.json reads policy %q, want off", got)
		}
		for _, active := range []bool{false, true} {
			if d := projectcfg.DecideEntry(projectcfg.EntryInput{Trigger: "P", Policy: projectcfg.DefaultPolicy, OrchestrationActive: active}); d.AdviseInterview {
				t.Errorf("the default policy advises the Interview (orchestration active %v)", active)
			}
		}
	})

	t.Run("a plan request in a project without crw.json gets the plan advice and keeps IDLE", func(t *testing.T) {
		cwd := t.TempDir()
		env := promptSubmitHost(cwd)
		got := promptSubmitAnswer(t, cwd, "df-plan", "t1", "Use crw-pabcd to start Plan phase", true)
		if want := promptTriggerAdvice(env, PhaseDirective(state.PhaseP, nil), state.PhaseIdle); got != want {
			t.Errorf("answer\n got %q\nwant %q", got, want)
		}
		if strings.Contains(got, "[crw: INTERVIEW") {
			t.Errorf("the plan request got the Interview directive: %q", got)
		}
		if s := state.ReadState(cwd, "df-plan"); s.Phase != state.PhaseIdle || s.OrchestrationActive {
			t.Errorf("the plan request moved the session: %+v", s)
		}
	})

	t.Run("the default transitions never lead into I", func(t *testing.T) {
		for _, phase := range []state.Phase{state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC, state.PhaseD} {
			if next, ok := fsm.NextPhase(state.State{Phase: phase}); !ok || next == state.PhaseI {
				t.Errorf("NextPhase(%s) = %s, %v", phase, next, ok)
			}
			if next, ok := stopNextCommands(phase, stopBinding{}); ok && strings.Contains(next, "orchestrate I") {
				t.Errorf("the Stop continuation of %s names I: %s", phase, next)
			}
		}
		if _, ok := fsm.NextPhase(state.State{Phase: state.PhaseIdle}); ok {
			t.Error("NextPhase chose an entry from IDLE")
		}
		for _, platform := range []string{"linux", "win32"} {
			recipe := LoopArmDirective(platform)
			if !strings.Contains(recipe, "`crw pabcd orchestrate P --session <id>`") || strings.Contains(recipe, "orchestrate I") {
				t.Errorf("the %s loop recipe does not enter at P alone", platform)
			}
		}
	})

	t.Run("the explicit I directive routes ambiguity up and names no question skill", func(t *testing.T) {
		for name, text := range map[string]string{
			"InterviewDirective":             InterviewDirective(promptSubmitHost(t.TempDir())),
			"rescan directive":               sessionHookRescanReinjectDirective,
			"PhaseDirective(I)":              PhaseDirective(state.PhaseI, nil),
			"BuildStageHeader(I)":            BuildStageHeader(state.PhaseI),
			"the interview hint's directive": promptSubmitAnswer(t, t.TempDir(), "df-i", "t1", "Use crw-pabcd to start the interview", true),
		} {
			for _, skill := range unroutedQuestionSkills {
				if strings.Contains(text, skill) {
					t.Errorf("%s routes to %s: %q", name, skill, text)
				}
			}
		}
		directive := InterviewDirective(promptSubmitHost(t.TempDir()))
		for _, want := range []string{"decision request", "the session above the project session", "flow report-up`", "OPEN ASSUMPTIONS"} {
			if !strings.Contains(directive, want) {
				t.Errorf("the I directive lacks %q: %q", want, directive)
			}
		}
		if strings.Contains(directive, "request_user_input") || strings.Contains(directive, "Mind dispatch") {
			t.Errorf("the I directive still opens a synchronous interview: %q", directive)
		}
		rescan := ResolveCRWInDirective(sessionHookRescanReinjectDirective, promptSubmitHost(t.TempDir()))
		for _, want := range []string{"decision request", "OPEN ASSUMPTIONS"} {
			if !strings.Contains(rescan, want) {
				t.Errorf("the post-answer directive lacks %q: %q", want, rescan)
			}
		}
		if strings.Contains(rescan, "Mind") || strings.Contains(rescan, "user questions") {
			t.Errorf("the post-answer directive still drives an interview round: %q", rescan)
		}
	})
}
