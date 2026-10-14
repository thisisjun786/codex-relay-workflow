package pluginwiring

import "testing"

// CRW1377PaddedCodexHome is the CODEX_HOME of the CRW-1377 tests: a home spelled with whitespace around it. The hook, the bridge launcher,
// register-mcp and doctor all pick CRW1377CodexHome from it (one rule: trim the surrounding whitespace; a blank value names no home).
const (
	CRW1377CodexHome       = "/crw1377/codex-home"
	CRW1377PaddedCodexHome = " \t" + CRW1377CodexHome + " \n"
)

// CRW-1377: the bridge launcher reads its record under the Codex home CODEX_HOME names, trimmed as the SessionStart hook trims it; a blank
// CODEX_HOME names none and the launcher falls back as before.
func TestCRW1377TheLauncherTrimsCodexHome(t *testing.T) {
	if home, how := codexHome(map[string]string{"CODEX_HOME": CRW1377PaddedCodexHome, "HOME": "/crw1377/home"}); home != CRW1377CodexHome || how != "the CODEX_HOME environment variable" {
		t.Errorf("a padded CODEX_HOME: %q (%s); want %q", home, how, CRW1377CodexHome)
	}
	if home, how := codexHome(map[string]string{"CODEX_HOME": " \t ", "HOME": "/crw1377/home"}); home == " \t " || how == "the CODEX_HOME environment variable" {
		t.Errorf("a blank CODEX_HOME: %q (%s); want the fallback", home, how)
	}
}
