package delivery

import (
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain keeps the tests' temporary directories in the isolation root, gives the CLI tests a
// copy of crw of their own there (selection_test names the binary beside it) and releases the
// process-lifetime parity trees (processParityTree) after the tests.
func TestMain(m *testing.M) {
	testsupport.Main(m, testsupport.TempDirInRoot, func(root string) (func() error, error) {
		crwPath = filepath.Join(root, "crw")
		return func() error { releaseParityTrees(); return nil }, testsupport.CopyCRW(crwPath)
	})
}
