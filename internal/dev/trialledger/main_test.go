//go:build dev

package trialledger

import (
	"fmt"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// The ledger reads stores and writes nothing; the guard holds the package to that for the account's real
// home too (CRW-1186): a test that reaches it, or changes the real hook switch, fails the package.
func TestMain(m *testing.M) {
	cleanup, _ := homeguard.RefuseAccountHome("")
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, "trialledger:", err)
		code = 1
	}
	os.Exit(code)
}
