package hook

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestTaskSkillNames(t *testing.T) {
	for _, name := range []string{"crw-work", "crw-pabcd"} {
		for _, prompt := range []string{"Use " + name, "Use `" + name + "`", "Use $crw:" + name, "Use [$" + name + "](skill://test/task)"} {
			t.Run(prompt, func(t *testing.T) {
				if phase, ok := DetectTrigger(prompt); !ok || phase != state.PhaseP {
					t.Errorf("got phase %q, %v; want P", phase, ok)
				}
				if DetectLoopArmRequest(prompt) {
					t.Error("task skill request alone must not arm a loop")
				}
			})
		}
		for _, prompt := range []string{"Do not use " + name, "Explain how to use " + name, "Use " + name + " 쓰지 말고 그냥 고쳐줘", "Use " + name + "flow", "```\nUse " + name + "\n```"} {
			if phase, ok := DetectTrigger(prompt); ok {
				t.Errorf("%q must stay silent, got %s", prompt, phase)
			}
		}
	}
	for _, name := range []string{"crw-continue", "crw-loop", "cxc-loop", "CXC Loop"} {
		for _, prompt := range []string{"Run " + name, "Run `" + name + "`", name + "으로 끝까지 해줘"} {
			if !DetectLoopArmRequest(prompt) {
				t.Errorf("%q must name the completion loop", prompt)
			}
		}
		for _, prompt := range []string{"Do not run " + name, "Explain how to run " + name, "Run " + name + " 돌리지 마", "Run " + name + "back", "\"Run " + name + "\"", "~~~\nRun " + name + "\n~~~"} {
			if DetectLoopArmRequest(prompt) {
				t.Errorf("%q must stay silent", prompt)
			}
		}
	}
}
