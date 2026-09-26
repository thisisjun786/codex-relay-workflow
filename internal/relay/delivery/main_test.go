package delivery

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain removes the crw binary the CLI tests build and the Python capture trees, once per
// package run.
func TestMain(m *testing.M) {
	code := m.Run()
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
