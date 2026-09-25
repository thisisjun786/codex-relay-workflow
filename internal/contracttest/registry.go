package contracttest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	"agreement": true, "entry": true, "hook": true, "status": true, "install": true,
}

// runners maps a run kind to its Go runner. Only kinds the built crw can already answer have
// one; the rest arrive with the todo that ports their surface.
var runners = map[RunKind]Runner{
	"cli": runCLI,
}

// ported lists the domains whose Go implementation is registered. A domain joins this set in
// the todo that ports it (for example cli-shape once the relay commands its fixtures call are
// registered in internal/relay/cli); until then every scenario in it is skipped and counted.
var ported = map[string]bool{"sqlite-ddl": true}

// crwBinary is the crw under test: CRW_TEST_BINARY, or ./cmd/crw built once per package run
// into buildDir, which TestMain creates and removes.
var (
	buildDir   string
	crwBinary  = sync.OnceValues(buildCRW)
	errNoBuild = errors.New("contracttest: build directory not set (TestMain did not run)")
)

func buildCRW() (string, error) {
	if path := os.Getenv("CRW_TEST_BINARY"); path != "" {
		return filepath.Abs(path)
	}
	if buildDir == "" {
		return "", errNoBuild
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	out := filepath.Join(buildDir, "crw")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", out, "./cmd/crw")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("contracttest: go build ./cmd/crw: %w\n%s", err, output)
	}
	return out, nil
}
