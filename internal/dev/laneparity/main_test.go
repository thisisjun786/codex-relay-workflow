//go:build dev

package laneparity

import (
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contracttest"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// crwUnderTest is the crw the fire tests start: the release-shaped build with the recorder's frozen
// clock linked in, as TestDomain/cxc builds it.
var crwUnderTest = func() (string, error) {
	return testsupport.BuildCRWPath("-trimpath", "-ldflags="+strings.Join([]string{
		"-X main.recallTestClock=1767225600000",
		"-X github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor.retrustTestClock=1767225600000",
	}, " "))
}

func TestMain(m *testing.M) {
	// A replay case links its stub programs and its git wrapper to this test binary.
	if dir := os.Getenv(contracttest.RecDirEnv); dir != "" {
		os.Exit(contracttest.RunStubHelper(dir))
	}
	testsupport.Main(m, testsupport.TempDirInRoot)
}
