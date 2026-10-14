package planinput

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the process homes: this test binary links internal/relay/store (through internal/flow/compile), so
// without testsupport a test could open the live relay state.
func TestMain(m *testing.M) { testsupport.Main(m) }
