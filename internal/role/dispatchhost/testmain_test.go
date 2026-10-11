package dispatchhost

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the test process (testsupport.Main): its homes and its crw configuration, whose
// sandbox_root is the temporary directory the tests make their workspaces in (CRW-1265), so the
// sandbox-root rule judges them the same on every host, whatever TMPDIR is and whatever the host's own
// configuration says.
func TestMain(m *testing.M) { testsupport.Main(m) }
