package install

import (
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CRW-1377: register-mcp (and every install command that takes its options) writes under the Codex home the hook and the launcher read: CODEX_HOME
// trimmed of the whitespace around it (the value internal/pluginwiring's CRW1377PaddedCodexHome spells), never a relative directory named with the
// spaces under the working directory. A blank CODEX_HOME names none and HOME's .codex is used.
func TestCRW1377RegisterMCPTrimsCodexHome(t *testing.T) {
	const want = "/crw1377/codex-home"
	home := t.TempDir()
	env := scope.Env{"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "state"), "CODEX_HOME= \t" + want + " \n"}
	o, err := options(env, "", "", "", "", "")
	if err != nil || o.CodexHome != want {
		t.Errorf("a padded CODEX_HOME: %q, %v; want %q", o.CodexHome, err, want)
	}
	o, err = options(env.With("CODEX_HOME", " \t "), "", "", "", "", "")
	if err != nil || o.CodexHome != filepath.Join(home, ".codex") {
		t.Errorf("a blank CODEX_HOME: %q, %v; want HOME's .codex", o.CodexHome, err)
	}
}
