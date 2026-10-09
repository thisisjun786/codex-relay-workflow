package hook

import (
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
