package doctor_test

import (
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CRW-1377: doctor reads the Codex home the hook, the launcher and register-mcp read: CODEX_HOME trimmed of the whitespace around it (the
// value internal/pluginwiring's CRW1377PaddedCodexHome spells); a blank CODEX_HOME names none.
func TestCRW1377DoctorTrimsCodexHome(t *testing.T) {
	const want = "/crw1377/codex-home"
	home := t.TempDir()
	if got, err := doctor.CodexHomeOf(scope.Env{"HOME=" + home, "CODEX_HOME= \t" + want + " \n"}); err != nil || got != want {
		t.Errorf("a padded CODEX_HOME: %q, %v; want %q", got, err, want)
	}
	if got, err := doctor.CodexHomeOf(scope.Env{"HOME=" + home, "CODEX_HOME= \t "}); err != nil || got != filepath.Join(home, ".codex") {
		t.Errorf("a blank CODEX_HOME: %q, %v; want HOME's .codex", got, err)
	}
}
