package contracttest

import (
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
)
