package skill

import (
	"fmt"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain removes the crw the tests may have built (testsupport.CRW), once per package run.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := testsupport.RemoveCRW(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// recordedCRW is the crw under test for a test whose goldens name its temporary directories by
// number. Each such test built its own crw into its first t.TempDir when its answers were first
// recorded from Python; that directory is still taken, so the numbers the goldens carry stay the
// same.
func recordedCRW(t *testing.T) string {
	t.Helper()
	_ = t.TempDir()
	return testsupport.CRW(t)
}
