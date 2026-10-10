package configguard

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain refuses any guidance record in the account's real Codex home: the session-start hook of this package records what it
// said to a resumed session, and a test lookup without a temporary CODEX_HOME would otherwise reach ~/.codex.
func TestMain(m *testing.M) { testsupport.Main(m, guidancerecord.RefuseAccountHome) }
