package affordance

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

func TestLongInvocationPreservesSessionAndLoopGuidance(t *testing.T) {
	pad := strings.Repeat("x", 9000)
	env := func(key string) (string, bool) {
		if key == "CRW_BIN" {
			return "env CRW_CONTEXT_PAD=" + pad + " crw", true
		}
		return "", false
	}
	for _, output := range []string{
		RunMapAffordanceSessionStart(`{"session_id":"rec-s1"}`, t.TempDir(), env),
		RenderSessionBinding("rec-s1", env) + RenderLoopAffordance(env),
	} {
		body := output
		if strings.HasPrefix(output, `{"hookSpecificOutput"`) {
			var p struct {
				HookSpecificOutput struct{ AdditionalContext string }
			}
			if err := json.Unmarshal([]byte(output), &p); err != nil {
				t.Fatal(err)
			}
			body = p.HookSpecificOutput.AdditionalContext
		}
		if len(utf16.Encode([]rune(body))) > harness.MaxContext || !strings.Contains(body, "--session rec-s1") || !strings.Contains(body, "Loop contract:") || !strings.Contains(body, "do not bypass guards") || strings.Contains(body, pad) {
			t.Fatal("long invocation lost required instructions or exceeded context limit")
		}
	}
}
