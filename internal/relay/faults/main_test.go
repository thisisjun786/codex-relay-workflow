package faults

import (
	"fmt"
	"os"
	"path/filepath"
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
