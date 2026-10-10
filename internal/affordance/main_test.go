package affordance

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the process homes and refuses any guidance record in the account's real Codex home: the session-start
// leg records what it gave a session, and a test lookup without a temporary CODEX_HOME would otherwise reach ~/.codex.
func TestMain(m *testing.M) { testsupport.Main(m, guidancerecord.RefuseAccountHome) }
