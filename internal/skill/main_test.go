package skill

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

// recordedCRW is the crw under test for a test whose goldens name its temporary directories by
// number. Each such test built its own crw into its first t.TempDir when its answers were first
// recorded from Python; that directory is still taken, so the numbers the goldens carry stay the
// same.
func recordedCRW(t *testing.T) string {
	t.Helper()
	_ = t.TempDir()
	return testsupport.CRW(t)
}
