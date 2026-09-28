package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "crw-ledger-tests-")
	if err != nil {
		panic(err)
	}
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
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
