package hook

import (
	"strings"
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

// CRW-1151: what the hook injects for the skill's hook-hint examples in a fresh session that has not
// armed PABCD. DetectTrigger's phase hint is not the whole answer: the un-armed loop-arm branch runs
// first, so a request that DetectLoopArmRequest reads as a loop request (a bare pabcd word plus an
// action word) answers the arming mandate instead of the hint, and a prompt with no hint stays silent.
func TestPromptSubmitSkillHintExamplesInAFreshSession(t *testing.T) {
	cases := []struct {
		prompt string
		want   string // "hint", "mandate" or "silent"
	}{
		{"Use crw-pabcd", "hint"},
		{"Use crw-pabcd to start the interview", "hint"},
		{"pabcd로 시작해줘", "mandate"},
		{"pabcd로 인터뷰 해줘", "mandate"},
		{"Start pabcd phase", "mandate"},
		{"Start pabcd phase i", "mandate"},
		{"인터뷰 먼저 해줘", "silent"},
		{"interview me first", "silent"},
	}
	for _, tc := range cases {
		cwd := t.TempDir()
		env := promptSubmitHost(cwd)
		got := promptSubmitAnswer(t, cwd, "s1", "t1", tc.prompt, true)
		mandate := ResolveCRWInDirective(LoopArmDirective(""), env)
		switch tc.want {
		case "silent":
			if got != "" {
				t.Errorf("%q: answered %.80q, want silence", tc.prompt, got)
			}
		case "mandate":
			if got != mandate {
				t.Errorf("%q: answered %.80q, want the arming mandate", tc.prompt, got)
			}
		case "hint":
			if got == "" || got == mandate || !strings.Contains(got, ResolveCRWInDirective(TriggerAuthorityNote, env)) {
				t.Errorf("%q: answered %.80q, want a phase hint with the trigger-authority note", tc.prompt, got)
			}
		}
	}
}
