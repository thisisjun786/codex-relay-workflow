package sessionhost_test

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the process homes: the tests read a flow-bound directory of their own and must never reach the real one.
func TestMain(m *testing.M) { testsupport.Main(m) }
