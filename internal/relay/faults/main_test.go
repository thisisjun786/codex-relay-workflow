package faults

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Fence the entire package, including in-process CLI tests and child oracles.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "faults-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Keep child Go builds on the existing toolchain cache rather than downloading
	// read-only module trees into the disposable host-state home.
	cache, err := os.UserCacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	env := map[string]string{"HOME": home, "XDG_STATE_HOME": filepath.Join(home, "state"), "XDG_CONFIG_HOME": filepath.Join(home, "config"), "CODEX_HOME": filepath.Join(home, "codex"), "CODEX_SESSION_RELAY_STATE": filepath.Join(home, "relay"), "CODEX_SESSION_RELAY_SCOPE_DIR": filepath.Join(home, "scope")}
	if os.Getenv("GOPATH") == "" {
		env["GOPATH"] = filepath.Join(os.Getenv("HOME"), "go")
	}
	if os.Getenv("GOCACHE") == "" {
		env["GOCACHE"] = filepath.Join(cache, "go-build")
	}
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	// The sweep reports the relay's installation location, which EvalSymlinks must resolve: a
	// directory of this run, which the goldens name by placeholder (runPathsOf).
	relayPackageLocation = filepath.Join(home, "codex_session_relay")
	if err := os.MkdirAll(relayPackageLocation, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sweepInstallation = func() (Installation, error) {
		return Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: relayPackageLocation}, nil
	}
	code := m.Run()
	if err := errors.Join(os.RemoveAll(home), testsupport.RemoveCRW()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
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
