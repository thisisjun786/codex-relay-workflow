package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var isolationRoot string

// hostEnviron is the environment this test binary started with, before TestMain isolated the
// relay's state from it: a `go build` (relayCLI) keeps the host's Go build and module caches.
var hostEnviron []string

func TestMain(m *testing.M) {
	// SIGKILL children inherit the parent's isolation and cannot run cleanup.
	if os.Getenv("CRW_CRASH_DB") != "" {
		os.Exit(m.Run())
	}
	root, err := os.MkdirTemp("", "crw-store-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	isolationRoot = root
	hostEnviron = os.Environ()
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
		if err := os.Setenv(key, filepath.Join(root, key)); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	if err := testsupport.RemoveTempTree(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
