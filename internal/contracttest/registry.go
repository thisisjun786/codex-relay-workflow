package contracttest

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Runner executes one scenario and returns its observation (see evaluate.go for its shape).
type Runner func(t *testing.T, scenario Scenario) (map[string]any, error)

// runners maps each run kind the corpus uses to its Go runner. A fixture of any other kind is a
// corpus defect and fails (TestDomain).
var runners = map[RunKind]Runner{
	"cli":       runCLI,
	"appserver": runAppServer,
	"ledger":    runLedger,
	"git":       runGit,
	"mcp":       runMCP,
	"hook":      runHook,
	"entry":     runHook,
	"stop":      runHook,
	"status":    runHook,
	"agreement": runAgreement,
	"release":   runRelease,
}

// crwBinary is the crw under test and crwDevBinary the development binary the hook corpus's
// `verify` steps run (`crw-dev stop-events`): testsupport.CRWPath and CRWDevPath, each
// $CRW_TEST_BINARY or $CRW_TEST_DEV_BINARY when set, else built once. TestMain builds both before
// the first scenario.
var (
	crwBinary    = testsupport.CRWPath
	crwDevBinary = testsupport.CRWDevPath

	// cxcRecallBinary is the crw the cxc corpus replays against: the release-shaped binary plus
	// the recorder's frozen clock linked into the two seams that read the wall clock and print a
	// value derived from it (cmd/crw/recall_clock.go for recall's recency scores, and
	// internal/runtime/doctor/cli.go for the retrust backup name). 1767225600000 is
	// 2026-01-01T00:00:00Z, the instant the oracle recorded under before its clock advanced 1 ms
	// per Date read (contract/notes/cxc/README.md "Seams the replay does not provide").
	cxcRecallBinary = func() (string, error) {
		return testsupport.BuildCRWPath("-trimpath", "-ldflags="+strings.Join([]string{
			"-X main.recallTestClock=1767225600000",
			"-X github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor.retrustTestClock=1767225600000",
		}, " "))
	}
)
