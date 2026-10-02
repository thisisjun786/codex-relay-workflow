package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killedHelperEnv makes TestIsolationHelperProcess act as the helper; without it that test does nothing.
const killedHelperEnv = "CRW_TEST_ISOLATION_KILLED_HELPER"

// isolationEnvKeys are the variables isolate sets, the ones the tests below move, and the two
// that carry the isolation between processes; every test restores them (as livestate_test.go does).
var isolationEnvKeys = []string{"HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CODEX_HOME",
	"CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR", "CODEX_SESSION_RELAY_MARKER_ROOT",
	"GOPATH", "GOMODCACHE", "GOCACHE", RefuseLiveStateEnv, "TMPDIR", "CRW_TEST_ISOLATION_ROOT", "CRW_TEST_KEEP_ROOT"}

// restoreIsolationEnv has t put every isolationEnvKeys variable back as it is now when the test ends.
func restoreIsolationEnv(t *testing.T) {
	t.Helper()
	for _, key := range isolationEnvKeys {
		t.Setenv(key, os.Getenv(key))
	}
}

// isolationRoots lists the crw-relay-test-* directories directly in dir.
func isolationRoots(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "crw-relay-test-") {
			roots = append(roots, entry.Name())
		}
	}
	return roots
}

// TestIsolationHelperProcess is the child of TestAHelperKilledAfterIsolatingLeavesNoRoot. It
// isolates as a TestMain does, then dies of SIGKILL, so nothing it made is removed by itself.
func TestIsolationHelperProcess(t *testing.T) {
	if os.Getenv(killedHelperEnv) == "" {
		return
	}
	if _, err := isolate(); err != nil {
		fmt.Fprintln(os.Stderr, "isolate:", err)
		os.Exit(2)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	time.Sleep(time.Minute)
	t.Fatal("the helper outlived its own SIGKILL")
}

// A test process isolates, then starts a helper of its own binary that isolates too and is killed
// before any cleanup. The helper's tree must go with its starter's, so after the starter has
// removed its tree no crw-relay-test-* directory is left in TMPDIR (before this change the
// helper's stayed: relay/cli, relay/dag, relay/registry and runtime/install left six per run).
func TestAHelperKilledAfterIsolatingLeavesNoRoot(t *testing.T) {
	restoreIsolationEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	// Given: the starter's own isolation, in a TMPDIR nothing else uses.
	root, err := isolate()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveTempTree(root) })
	// When: a helper that isolates and is killed.
	child := exec.Command(os.Args[0], "-test.run=^TestIsolationHelperProcess$", "-test.count=1")
	child.Env = append(os.Environ(), killedHelperEnv+"=1")
	out, err := child.CombinedOutput()
	if status, ok := child.ProcessState.Sys().(syscall.WaitStatus); err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper was not killed: %v\n%s", err, out)
	}
	// Then: the helper's tree is not left beside the starter's, and the starter's removal takes both.
	if roots := isolationRoots(t, tmp); len(roots) != 1 {
		t.Fatalf("a killed helper left an isolation root in TMPDIR: %v", roots)
	}
	if err := RemoveTempTree(root); err != nil {
		t.Fatal(err)
	}
	if roots := isolationRoots(t, tmp); len(roots) != 0 {
		t.Fatalf("isolation roots remain after the starter removed its own: %v", roots)
	}
}
