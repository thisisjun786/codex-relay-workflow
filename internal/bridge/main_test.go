package bridge

import (
	"fmt"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// TestMain points CRW_HOME at a temporary directory for the whole package: every resume a test
// drives through SendMessageToThread records the thread's state-root anchor under the CRW home
// (CRW-1140), and the real ~/.crw must never receive one. A test that needs its own CRW home still
// sets it with t.Setenv.
func TestMain(m *testing.M) {
	home, err := tmploc.MkdirTemp("crw-bridge-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: a temporary CRW home:", err)
		os.Exit(1)
	}
	code := 1
	// CRW-1265: the tests run threads in temporary directories, so the sandbox base of the rule they
	// read is the temporary directory, with a fake account home; a test of the rule uses
	// sandboxroottest.Fake.
	removeRule, err := sandboxroottest.InstallTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: the sandbox-root rule:", err)
	} else if err := os.Setenv("CRW_HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: set CRW_HOME:", err)
	} else {
		code = m.Run()
	}
	if removeRule != nil {
		if err := removeRule(); err != nil {
			fmt.Fprintln(os.Stderr, "bridge tests: remove the sandbox-root rule:", err)
			code = 1
		}
	}
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: remove the temporary CRW home:", err)
		code = 1
	}
	os.Exit(code)
}
