package delivery

import (
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain keeps the tests' temporary directories in the isolation root and gives the CLI tests a
// copy of crw of their own there (selection_test names the binary beside it).
func TestMain(m *testing.M) {
	testsupport.Main(m, testsupport.TempDirInRoot, func(root string) (func() error, error) {
		crwPath = filepath.Join(root, "crw")
		return nil, testsupport.CopyCRW(crwPath)
	})
}
