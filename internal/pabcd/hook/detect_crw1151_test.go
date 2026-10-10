package hook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1151: every example the crw-pabcd skill's hook-hint paragraph states
// (plugins/crw/skills/crw-pabcd/SKILL.md, "Hook hint (narrow)") holds for
// DetectTrigger. The P fallback applies to a crw-pabcd or pabcd로 request only;
// the marker form `pabcd phase` needs its own phase pattern.
func TestDetectTriggerSkillHintExamples(t *testing.T) {
	cases := []struct {
		prompt string
		phase  state.Phase
		ok     bool
	}{
		{"Use crw-pabcd", state.PhaseP, true},
		{"pabcd로 시작해줘", state.PhaseP, true},
		{"Use crw-pabcd to start the interview", state.PhaseI, true},
		{"Start pabcd phase", "", false},
		{"Use pabcd phase", "", false},
		{"Start pabcd phase i", state.PhaseI, true},
		{"인터뷰 먼저 해줘", "", false},
		{"interview me first", "", false},
		{"pabcd로 인터뷰 해줘", "", false},
	}
	for _, tc := range cases {
		phase, ok := DetectTrigger(tc.prompt)
		if phase != tc.phase || ok != tc.ok {
			t.Errorf("DetectTrigger(%q) = %q, %v; want %q, %v", tc.prompt, phase, ok, tc.phase, tc.ok)
		}
	}
}

// CRW-1151: what the hook injects for the skill's hook-hint examples. DetectTrigger's phase hint is not
// the whole answer. The order in PromptSubmitHandle is: the trigger and the interview-entry decision
// (an active or unreadable goal never promotes P to the interview and drops an I hint), then the
// un-armed loop-arm branch (a request DetectLoopArmRequest reads as a loop request answers the arming
// mandate instead of a hint), then the hint. A prompt with no hint and no loop request stays silent.
const (
	skillAnswerHint    = "hint"    // the default-policy P hint: interview directive, trigger-authority note, footer
	skillAnswerPlanP   = "plan-p"  // the P hint without the interview promotion: the P phase directive, note, footer
	skillAnswerMandate = "mandate" // the ORCH-MANDATE-01 arming mandate
	skillAnswerSilent  = "silent"
)

type skillAnswerCase struct {
	prompt string
	want   string
}

func checkSkillAnswers(t *testing.T, name string, cases []skillAnswerCase, answer func(prompt string) (got, cwd string, env func(string) (string, bool))) {
	t.Helper()
	for _, tc := range cases {
		got, cwd, env := answer(tc.prompt)
		note := ResolveCRWInDirective(TriggerAuthorityNote, env)
		var want string
		switch tc.want {
		case skillAnswerSilent:
			want = ""
		case skillAnswerMandate:
			want = ResolveCRWInDirective(LoopArmDirective(""), env)
		case skillAnswerHint:
			want = WithFooter(InterviewDirective(env)+"\n\n"+note, state.PhaseIdle)
		case skillAnswerPlanP:
			want = WithFooter(PhaseDirective(state.PhaseP, ActiveWorkPhaseOpts(cwd, ""))+"\n\n"+note, state.PhaseIdle)
		}
		if got != want {
			t.Errorf("%s %q: answered %.100q, want %s (%.100q)", name, tc.prompt, got, tc.want, want)
		}
	}
}

// A fresh session with no goal and the default interview-entry policy that has not armed PABCD.
func TestPromptSubmitSkillHintExamplesInAFreshSession(t *testing.T) {
	cases := []skillAnswerCase{
		{"Use crw-pabcd", skillAnswerHint},
		{"Use crw-pabcd to start the interview", skillAnswerHint},
		{"pabcd로 시작해줘", skillAnswerMandate},
		{"pabcd로 인터뷰 해줘", skillAnswerMandate},
		{"Start pabcd phase", skillAnswerMandate},
		{"Start pabcd phase i", skillAnswerMandate},
		{"인터뷰 먼저 해줘", skillAnswerSilent},
		{"interview me first", skillAnswerSilent},
	}
	checkSkillAnswers(t, "no goal", cases, func(prompt string) (string, string, func(string) (string, bool)) {
		cwd := t.TempDir()
		return promptSubmitAnswer(t, cwd, "s1", "t1", prompt, true), cwd, promptSubmitHost(cwd)
	})
}

// The same session while a goal is active, or while the goals database cannot be read (both suppress):
// DecideEntry then never promotes P to the interview, and the I suppression at the trigger runs before
// the un-armed loop-arm branch, so an I hint is dropped (silence) instead of answering the mandate.
func TestPromptSubmitSkillHintExamplesUnderAGoal(t *testing.T) {
	databases := map[string][]string{
		"active goal": {
			"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, goal_id TEXT NOT NULL, objective TEXT NOT NULL, status TEXT NOT NULL)",
			"INSERT INTO thread_goals (thread_id, goal_id, objective, status) VALUES ('s1', 'g', 'obj', 'active')",
		},
		"unreadable goals database": {"CREATE TABLE thread_goals (thread_id TEXT)"},
	}
	cases := []skillAnswerCase{
		{"Use crw-pabcd", skillAnswerPlanP},                         // P is kept, but as the P directive, not the interview
		{"Use crw-pabcd to start the interview", skillAnswerSilent}, // I hint dropped
		{"Start pabcd phase i", skillAnswerSilent},                  // I hint dropped before the loop-arm check
		{"pabcd로 시작해줘", skillAnswerMandate},                         // P hint, but the loop-arm branch comes first
		{"pabcd로 인터뷰 해줘", skillAnswerMandate},                       // no hint: loop-arm
		{"Start pabcd phase", skillAnswerMandate},                   // no hint: loop-arm
		{"인터뷰 먼저 해줘", skillAnswerSilent},
		{"interview me first", skillAnswerSilent},
	}
	for name, statements := range databases {
		checkSkillAnswers(t, name, cases, func(prompt string) (string, string, func(string) (string, bool)) {
			dir := t.TempDir()
			cwd := filepath.Join(dir, "ws")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			env := promptTriggerGoalEnv(sessionHookGoalsDB(t, filepath.Join(dir, "codex"), statements...))
			got := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: prompt, TurnID: "t1", PabcdEnabled: true}, "", env)
			return got, cwd, env
		})
	}
}
