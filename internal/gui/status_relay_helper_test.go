package gui

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The manage reads run the relay command as a child of this executable (os.Executable), so the
// capacity reading of a status request starts "relay ..." as a child of this test binary. Given
// those arguments the test binary would run its own tests, so TestMain replaces the process with
// the crw binary the test built (testsupport.CRW), the binary an operator runs. A relay child with
// no such binary exits 4 rather than running the suite again. testsupport.Main removes the binary the
// process built when the tests end.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "relay" {
		crw := os.Getenv(testsupport.CRWBinaryEnv)
		if crw == "" {
			fmt.Fprintln(os.Stderr, "gui tests: a relay child has no crw binary to run")
			os.Exit(4)
		}
		err := syscall.Exec(crw, os.Args, os.Environ())
		fmt.Fprintln(os.Stderr, "gui tests: exec the crw binary:", err)
		os.Exit(4)
	}
	testsupport.Main(m)
}
