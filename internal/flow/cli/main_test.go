package cli

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the process homes: this test binary links internal/relay/store, so without testsupport a test could open
// the live relay state (TestEvery_test_binary_that_links_the_store_links_testsupport). Started as the fake crw-dev of
// TestCILocalIntegrationVerifiesTheMergedTree, the binary answers as that and runs no test; started as a run process in the env-probe
// mode (CRW_TEST_ENV_PROBE, runenv_launch_test.go), it writes its environment and waits to be killed.
func TestMain(m *testing.M) {
	if file := os.Getenv(testEnvProbe); file != "" {
		os.Exit(envProbeMain(file))
	}
	if os.Getenv(labFakeCrwDev) == "1" {
		os.Exit(fakeCrwDevMain())
	}
	testsupport.Main(m)
}
