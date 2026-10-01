package store

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var isolationRoot string

func TestMain(m *testing.M) {
	// SIGKILL children inherit the parent's isolation and cannot run cleanup.
	if os.Getenv("CRW_CRASH_DB") != "" {
		os.Exit(m.Run())
	}
	testsupport.Main(m, func(root string) (func() error, error) {
		isolationRoot = root
		return nil, nil
	})
}
