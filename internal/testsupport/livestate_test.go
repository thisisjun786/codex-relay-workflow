package testsupport

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This binary links testsupport, so its init has already set the refusal, before any test ran.
func TestInit_sets_the_live_state_refusal_in_a_test_binary(t *testing.T) {
	if got := os.Getenv(RefuseLiveStateEnv); got != "1" {
		t.Fatalf("%s=%q in a test binary that links testsupport", RefuseLiveStateEnv, got)
	}
}

// IsolateRelayState keeps the refusal in force even where something cleared it before.
func TestIsolateRelayState_sets_the_live_state_refusal(t *testing.T) {
	// Given: every variable the isolation moves is restored after the test, and no refusal is set.
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CODEX_HOME",
		"CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR", "CODEX_SESSION_RELAY_MARKER_ROOT",
		"CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST",
		"GOPATH", "GOMODCACHE", "GOCACHE", RefuseLiveStateEnv, IsolationRootEnv, KeepRootEnv} {
		t.Setenv(key, os.Getenv(key))
	}
	if err := os.Unsetenv(RefuseLiveStateEnv); err != nil {
		t.Fatal(err)
	}
	// When: the relay state is isolated.
	cleanup, err := IsolateRelayState()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	// Then: the refusal is set for this process and whatever it starts.
	if got := os.Getenv(RefuseLiveStateEnv); got != "1" {
		t.Fatalf("%s=%q after IsolateRelayState", RefuseLiveStateEnv, got)
	}
}

// Every test binary that links the relay store links testsupport, whose init sets the refusal:
// the product's store opens the live state, and only this variable keeps a test off it
// (decisions.md 46). A test package that reaches the store without linking testsupport fails
// here; a blank import of testsupport in one of its test files (livestate_test.go) mends it.
func TestEvery_test_binary_that_links_the_store_links_testsupport(t *testing.T) {
	const module = "github.com/thisisjun786/codex-relay-workflow"
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not two directories above the package: %v", err)
	}
	for _, tags := range []string{"", "dev", "parity", "integration"} {
		cmd := exec.Command("go", "list", "-test", "-tags", tags, "-f", "{{.ImportPath}}{{range .Deps}}\t{{.}}{{end}}", "./...")
		cmd.Dir = root
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -tags %q: %v", tags, err)
		}
		binaries := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Split(line, "\t")
			if !strings.HasSuffix(fields[0], ".test") {
				continue
			}
			binaries++
			linked := map[string]bool{}
			for _, dep := range fields[1:] {
				path, _, _ := strings.Cut(dep, " [")
				linked[path] = true
			}
			if linked[module+"/internal/relay/store"] && !linked[module+"/internal/testsupport"] {
				t.Errorf("-tags %q: %s links internal/relay/store but not internal/testsupport, so its tests would open the live state", tags, fields[0])
			}
		}
		if binaries == 0 {
			t.Fatalf("go list -tags %q named no test binary", tags)
		}
	}
}
