package managed

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var sharedCRW string

func TestMain(m *testing.M) {
	// The lock helper inherits its parent's isolated environment and never runs CLI
	// scenarios. Rebuilding there would use the isolated HOME as a module cache.
	if os.Getenv("CRW_LOCK_HELPER_STORE") != "" {
		os.Exit(m.Run())
	}
	testsupport.Main(m, testsupport.TempDirInRoot, func(string) (cleanup func() error, err error) {
		sharedCRW, err = testsupport.CRWPath()
		return nil, err
	})
}
