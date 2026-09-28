package delivery

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain removes the crw binary the CLI tests build and the Python capture trees, once per
// package run.
func TestMain(m *testing.M) {
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	for _, path := range captureCleanups {
		if err := os.RemoveAll(path); err != nil {
			code = 1
		}
	}
	if crwPath != "" {
		if err := os.RemoveAll(filepath.Dir(crwPath)); err != nil {
			code = 1
		}
	}
	os.Exit(code)
}
