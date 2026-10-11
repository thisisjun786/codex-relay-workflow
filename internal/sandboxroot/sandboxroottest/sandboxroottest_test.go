package sandboxroottest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// helperEnv makes the test binary a helper that installs the rule and exits without its cleanup, as a crash
// child or a hook helper of a test package does.
const helperEnv = "CRW_SANDBOXROOTTEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		if _, err := Install(os.TempDir()); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	testsupport.Main(m)
}

// fakesDir is the directory the installed rule's fake crw configuration lies in.
func fakesDir(t *testing.T) string {
	t.Helper()
	config := os.Getenv("CRW_CONFIG")
	if config == "" {
		t.Fatal("the installed rule exported no CRW_CONFIG")
	}
	return filepath.Dir(config)
}

func within(dir, root string) bool { return strings.HasPrefix(dir, root+string(filepath.Separator)) }

// CRW-1265: the fakes of a rule installed through testsupport.Main lie in the isolation root the setup is
// given, so the root's removal covers them however the test binary ends. Before, they were made in TMPDIR.
func TestSetupMakesItsFakesInTheIsolationRoot(t *testing.T) {
	for name, setup := range map[string]func(string) (func() error, error){"Setup": Setup, "SetupTemp": SetupTemp} {
		t.Run(name, func(t *testing.T) {
			// Given: an isolation root
			root := t.TempDir()

			// When: the setup installs the rule
			cleanup, err := setup(root)
			if err != nil {
				t.Fatal(err)
			}
			dir := fakesDir(t)

			// Then: its fakes lie in the root, and the cleanup removes them
			if !within(dir, root) {
				t.Errorf("the fakes are in %s, outside the isolation root %s", dir, root)
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the cleanup left %s (%v)", dir, err)
			}
		})
	}
}

// CRW-1265: Install called from a TestMain of its own makes its fakes in the isolation root of the test
// process tree (testsupport.IsolationRootEnv) when there is one.
func TestInstallMakesItsFakesInTheIsolationRootOfTheProcessTree(t *testing.T) {
	// Given: an isolation root the process tree names
	root := t.TempDir()
	t.Setenv(testsupport.IsolationRootEnv, root)

	// When: the rule is installed
	cleanup, err := Install(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })

	// Then: its fakes lie in that root
	if dir := fakesDir(t); !within(dir, root) {
		t.Errorf("the fakes are in %s, outside the isolation root %s", dir, root)
	}
}

// CRW-1265: a test binary that installs the rule without testsupport.Main (internal/bridge) starts helper
// processes of itself that install it too and may end without their cleanup (a killed crash child). The helper
// makes its fakes in the directory of the binary that started it, whose cleanup removes them, so a run
// leaves nothing in TMPDIR. Before, each such helper left a crw-sandboxroot-test-* directory there.
func TestAHelperThatEndsWithoutItsCleanupLeavesNothingInTMPDIR(t *testing.T) {
	// Given: a TMPDIR of the test's own and a rule installed with no isolation root named
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(testsupport.IsolationRootEnv, "")
	cleanup, err := Install(tmp)
	if err != nil {
		t.Fatal(err)
	}
	dir := fakesDir(t)

	// When: a helper process of the test binary installs the rule and exits without its cleanup
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), helperEnv+"=1")
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}

	// Then: TMPDIR holds only the starter's fakes, and nothing once the starter cleaned up
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || filepath.Join(tmp, entries[0].Name()) != dir {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("TMPDIR holds %v after the helper, want only the starter's %s", names, filepath.Base(dir))
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR holds %d entries after the cleanup (%v)", len(entries), err)
	}
}
