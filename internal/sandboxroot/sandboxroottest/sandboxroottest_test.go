package sandboxroottest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// helperEnv makes the test binary a helper that installs the rule and exits without its cleanup, as a crash
// child or a hook helper of a test package does.
const helperEnv = "CRW_SANDBOXROOTTEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		if _, err := InstallTemp(); err != nil {
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

// CRW-1579 (R1-7): a relative TMPDIR is tmploc's refusal at install, for InstallTemp and for Install(base) alike. Before, the
// fakes were made by os.MkdirTemp("", ...) below the relative directory, that is below the working directory.
func TestInstall_refusesARelativeTMPDIR(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("rel", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "rel")
	t.Setenv(testsupport.IsolationRootEnv, "")
	t.Setenv(parentEnv, "")
	for name, install := range map[string]func() (func() error, error){
		"InstallTemp": InstallTemp,
		"Install":     func() (func() error, error) { return Install("rel") },
	} {
		t.Run(name, func(t *testing.T) {
			cleanup, err := install()
			if err == nil {
				_ = cleanup()
				t.Fatal("a relative TMPDIR was accepted")
			}
			if !strings.Contains(err.Error(), "tmpdir_not_absolute") {
				t.Errorf("error %q is not tmploc's tmpdir_not_absolute", err)
			}
			if entries, _ := os.ReadDir("rel"); len(entries) != 0 {
				t.Errorf("fakes were made below the relative TMPDIR: %v", entries)
			}
		})
	}
}

// CRW-1579 (R1-7): the directory of the fakes is exported to helper processes only when Install chose it (TMPDIR); a
// directory the process tree named (the isolation root) is theirs already.
func TestInstall_exportsItsParentOnlyWhenItChoseIt(t *testing.T) {
	for name, c := range map[string]struct {
		isolationRoot bool
		exported      bool
	}{"no isolation root": {false, true}, "isolation root": {true, false}} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv(parentEnv, "")
			root := ""
			if c.isolationRoot {
				root = t.TempDir()
			}
			t.Setenv(testsupport.IsolationRootEnv, root)
			cleanup, err := Install(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cleanup() })
			dir := fakesDir(t)
			got := os.Getenv(parentEnv)
			if c.exported && got != dir {
				t.Errorf("%s = %q, want the fakes %s", parentEnv, got, dir)
			}
			if !c.exported && got != "" {
				t.Errorf("%s = %q, want none: the parent was named, not chosen", parentEnv, got)
			}
		})
	}
}

// CRW-1579 (D7): the installed crw configuration names the sandbox base only: no project roots, no socket root.
func TestInstall_documentIsTheBaseOnly(t *testing.T) {
	base := t.TempDir()
	cleanup, err := Install(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	data, err := os.ReadFile(os.Getenv("CRW_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(base)
	if want := map[string]any{"paths": map[string]any{"sandbox_root": real}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("document = %v, want exactly %v", got, want)
	}
}
