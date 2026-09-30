package delivery

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestMain removes the crw binary the CLI tests build and the Python capture trees, once per
// package run.
func TestMain(m *testing.M) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root, err := os.MkdirTemp("", "crw-delivery-tests-")
	if err != nil {
		panic(err)
	}
	// Build once with the caller's cache environment before isolating HOME.
	crwPath = filepath.Join(root, "crw")
	command := exec.Command(goBinary, "build", "-buildvcs=false", "-o", crwPath, "./cmd/crw")
	command.Dir, err = filepath.Abs("../../..")
	if err != nil {
		panic(err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build shared crw: %v: %s\n", err, output)
		if cleanupErr := testsupport.RemoveTempTree(root); cleanupErr != nil {
			fmt.Fprintln(os.Stderr, cleanupErr)
		}
		os.Exit(1)
	}
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
		if err := os.Setenv(key, filepath.Join(root, key)); err != nil {
			panic(err)
		}
	}
	// SQLite-heavy parity scenarios use tmpfs rather than the runner's disk.
	if err := os.Setenv("TMPDIR", root); err != nil {
		panic(err)
	}
	code := m.Run()
	releaseParityTrees()
	for _, path := range captureCleanups {
		if err := testsupport.RemoveTempTree(path); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup Python capture:", err)
			code = 1
		}
	}
	if err := testsupport.RemoveTempTree(root); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup isolated state:", err)
		code = 1
	}
	os.Exit(code)
}
