package faults

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
	// The replay tests compare fault-sweep's facts with the live-Python oracle's, which runs
	// from this checkout and names its package directory there.
	sweepInstallation = func() (Installation, error) {
		return Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: filepath.Join(f1Root(), "packages", "codex-session-relay", "src", "codex_session_relay")}, nil
	}
	code := m.Run()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func testHostRecordPath() string {
	return filepath.Join(os.Getenv("XDG_STATE_HOME"), "codex-relay-workflow", "host-record.json")
}

// f1Root is the repository checkout this package's tests run from.
func f1Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
