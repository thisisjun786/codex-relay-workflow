package managed

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var sharedCRW string

func TestMain(m *testing.M) {
	// The lock helper inherits its parent's isolated environment and never runs CLI
	// scenarios. Rebuilding there would use the isolated HOME as a module cache.
	if os.Getenv("CRW_LOCK_HELPER_STORE") != "" {
		os.Exit(m.Run())
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root, err := os.MkdirTemp("", "crw-managed-tests-")
	if err != nil {
		panic(err)
	}
	sharedCRW = filepath.Join(root, "crw")
	command := exec.Command(goBinary, "build", "-buildvcs=false", "-o", sharedCRW, "./cmd/crw")
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
	if err := os.Setenv("TMPDIR", root); err != nil {
		panic(err)
	}
	code := m.Run()
	if err := testsupport.RemoveTempTree(root); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup isolated state:", err)
		code = 1
	}
	os.Exit(code)
}
