package faults

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Fence the entire package, including in-process CLI tests and child oracles.
func TestMain(m *testing.M) {
	testsupport.Main(m, func(root string) (func() error, error) {
		// The sweep reports the relay's installation location, which EvalSymlinks must resolve:
		// a directory of this run, which the goldens name by placeholder (runPathsOf).
		relayPackageLocation = filepath.Join(root, "codex_session_relay")
		sweepInstallation = func() (Installation, error) {
			return Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: relayPackageLocation}, nil
		}
		return nil, os.MkdirAll(relayPackageLocation, 0o700)
	})
}

// relayPackageLocation is where the sweep's relay package is installed: a directory of this run
// standing in for the installation.
var relayPackageLocation string

func testHostRecordPath() string {
	return filepath.Join(os.Getenv("XDG_STATE_HOME"), "codex-relay-workflow", "host-record.json")
}

// f1Root is the repository checkout this package's tests run from.
func f1Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
