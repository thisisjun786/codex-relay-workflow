package contracttest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// crwBinary is the crw under test (testsupport.CRW: CRW_TEST_BINARY, or ./cmd/crw built once).
// crwDevBinary is the development binary the hook corpus's `verify` steps run (`crw-dev
// stop-events`): CRW_TEST_DEV_BINARY, or ./cmd/crw-dev built with -tags dev once into buildDir,
// which TestMain creates and removes. TestMain builds both before it isolates HOME.
var (
	buildDir     string
	crwBinary    = testsupport.CRWPath
	crwDevBinary = sync.OnceValues(func() (string, error) {
		return build("CRW_TEST_DEV_BINARY", "crw-dev", "-tags", "dev", "./cmd/crw-dev")
	})
	errNoBuild = errors.New("contracttest: build directory not set (TestMain did not run)")
)

func build(override, name string, args ...string) (string, error) {
	if path := os.Getenv(override); path != "" {
		return filepath.Abs(path)
	}
	if buildDir == "" {
		return "", errNoBuild
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	out := filepath.Join(buildDir, name)
	command := exec.Command(goBinary, append([]string{"build", "-buildvcs=false", "-o", out}, args...)...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("contracttest: go build %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	return out, nil
}
