package spawn

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1265: the managed-spawn selection asks the sandbox-root rule where the parent works, so the tests of this package run under a fake
// account home, a fake Codex home and a fake crw configuration whose sandbox base is the temporary directory, never the host's.
func TestMain(m *testing.M) {
	testsupport.Main(m, sandboxroottest.SetupTemp)
}
