package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain removes the crw binary the CLI tests may build (testsupport.CRW) and the
// process-lifetime parity trees (processParityTree), once per package run.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "crw-delivery-tests-")
	if err != nil {
		panic(err)
	}
	// A copy of its own: tests name the binary beside it (selection_test's alias).
	crwPath = filepath.Join(root, "crw")
	if err = testsupport.CopyCRW(crwPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if cleanupErr := testsupport.RemoveTempTree(root); cleanupErr != nil {
			fmt.Fprintln(os.Stderr, cleanupErr)
		}
		os.Exit(1)
	}
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
		if err := os.Setenv(key, filepath.Join(root, key)); err != nil {
			panic(err)
		}
	}
	// SQLite-heavy parity scenarios use tmpfs rather than the runner's disk.
	if err := os.Setenv("TMPDIR", root); err != nil {
		panic(err)
	}
	code := m.Run()
	releaseParityTrees()
	if err := errors.Join(testsupport.RemoveTempTree(root), testsupport.RemoveCRW()); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup isolated state:", err)
		code = 1
	}
	os.Exit(code)
}
