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
)

// Runner executes one scenario and returns its observation (see evaluate.go for its shape).
// It returns an error wrapping ErrNotPorted when the scenario needs a surface the Go build
// does not have yet; the caller turns that into an explicit, counted skip.
type Runner func(t *testing.T, scenario Scenario) (map[string]any, error)

// ErrNotPorted marks a scenario whose Go surface does not exist yet.
var ErrNotPorted = errors.New("not ported")

// Kinds is every run.kind contract/README.md defines. A fixture with any other kind is a
// corpus defect and fails, whether or not its domain is ported.
var Kinds = map[RunKind]bool{
	"cli": true, "mcp": true, "appserver": true, "git": true, "release": true, "stop": true,
	"agreement": true, "entry": true, "hook": true, "status": true, "install": true, "ledger": true,
}

// runners maps a run kind to its Go runner. Only kinds the built crw can already answer have
// one; the rest arrive with the todo that ports their surface.
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
}

// ported lists the domains whose Go implementation is registered. A domain joins this set in
// the todo that ports it (for example cli-shape once the relay commands its fixtures call are
// registered in internal/relay/cli); until then every scenario in it is skipped and counted.
var ported = map[string]bool{"hook": true, "sqlite-ddl": true, "appserver": true, "ledger-fingerprint": true, "git": true, "mcp-tools": true, "cli-shape": true}

// crwBinary is the crw under test: CRW_TEST_BINARY, or ./cmd/crw built once per package run
// into buildDir, which TestMain creates and removes. crwDevBinary is the development binary the
// hook corpus's `verify` steps run (`crw-dev stop-events`): CRW_TEST_DEV_BINARY, or ./cmd/crw-dev
// built with -tags dev once, on its first use.
var (
	buildDir     string
	crwBinary    = sync.OnceValues(func() (string, error) { return build("CRW_TEST_BINARY", "crw", "./cmd/crw") })
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
