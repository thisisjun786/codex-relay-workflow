package cli

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain isolates the process homes: this test binary links internal/relay/store, so without testsupport a test could open
// the live relay state (TestEvery_test_binary_that_links_the_store_links_testsupport). Started as the fake crw-dev of
// TestCILocalIntegrationVerifiesTheMergedTree, the binary answers as that and runs no test.
func TestMain(m *testing.M) {
	if os.Getenv(labFakeCrwDev) == "1" {
		os.Exit(fakeCrwDevMain())
	}
	testsupport.Main(m)
}
