package affordance

import (
	"strings"
	"testing"
)

// CRW-1377: the SessionStart hook's Codex home is the one the launcher, register-mcp and doctor pick from the same padded CODEX_HOME
// (internal/pluginwiring, internal/runtime/install and internal/runtime/doctor test the same value: CRW1377PaddedCodexHome).
func TestCRW1377TheHookTrimsCodexHomeAsTheLauncherDoes(t *testing.T) {
	const want = "/crw1377/codex-home"
	env := func(key string) (string, bool) {
		switch key {
		case "CODEX_HOME":
			return " \t" + want + " \n", true
		case "HOME":
			return "/crw1377/home", true
		}
		return "", false
	}
	if home, ok := codexHomeOf(env); !ok || home != want {
		t.Errorf("a padded CODEX_HOME: %q, %v; want %q", home, ok, want)
	}
}

// CRW-1377 d2: the digest override's value is printed shell-quoted in the warning, as the policy path override's is.
func TestCRW1377TheDigestOverrideIsShellQuoted(t *testing.T) {
	ws := t.TempDir()
	const policy = `{"a":1}`
	env, _ := policyHost(t, policy, digestText(policy), true)
	got := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, layered(env, map[string]string{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": "not a digest; x"})), "SessionStart")
	if want := "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST='not a digest; x'"; !strings.Contains(got, want) {
		t.Errorf("the warning lacks %q:\n%s", want, got)
	}
}
