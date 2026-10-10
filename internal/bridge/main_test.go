package bridge

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points CRW_HOME at a temporary directory for the whole package: every resume a test
// drives through SendMessageToThread records the thread's state-root anchor under the CRW home
// (CRW-1140), and the real ~/.crw must never receive one. A test that needs its own CRW home still
// sets it with t.Setenv.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "crw-bridge-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: a temporary CRW home:", err)
		os.Exit(1)
	}
	code := 1
	if err := os.Setenv("CRW_HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: set CRW_HOME:", err)
	} else {
		code = m.Run()
	}
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintln(os.Stderr, "bridge tests: remove the temporary CRW home:", err)
		code = 1
	}
	os.Exit(code)
}
