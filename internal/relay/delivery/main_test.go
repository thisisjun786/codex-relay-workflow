package delivery

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain removes the crw binary the CLI tests build once per package run.
func TestMain(m *testing.M) {
	code := m.Run()
	if crwPath != "" {
		if err := os.RemoveAll(filepath.Dir(crwPath)); err != nil {
			code = 1
		}
	}
	os.Exit(code)
}
