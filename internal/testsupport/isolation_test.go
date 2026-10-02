package testsupport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateTMPDIR gives the test a TMPDIR of its own with no isolation inherited and no keep request,
// and has t restore every variable isolation moves; it returns the TMPDIR.
func privateTMPDIR(t *testing.T) string {
	t.Helper()
	restoreIsolationEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, key := range []string{IsolationRootEnv, KeepRootEnv} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

// isolateForTest isolates and has t remove the root it made.
func isolateForTest(t *testing.T) string {
	t.Helper()
	root, err := isolate()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveTempTree(root) })
	return root
}

func TestIsolate_makes_a_root_in_TMPDIR_and_exports_it(t *testing.T) {
	tmp := privateTMPDIR(t)
	root := isolateForTest(t)
	if filepath.Dir(root) != tmp || !strings.HasPrefix(filepath.Base(root), rootPrefix) {
		t.Fatalf("root %s is not a crw-relay-test-* directory of TMPDIR %s", root, tmp)
	}
	if got := os.Getenv(IsolationRootEnv); got != root {
		t.Fatalf("%s=%q, want the root %s", IsolationRootEnv, got, root)
	}
	if got, want := os.Getenv("HOME"), filepath.Join(root, "home"); got != want {
		t.Fatalf("HOME=%q, want %q", got, want)
	}
}

// A process started by a test process that isolated makes its root inside the starter's; so does the
// process it starts in turn, and the variable keeps naming the first root.
func TestIsolate_nests_in_the_root_a_starter_exported(t *testing.T) {
	tmp := privateTMPDIR(t)
	top := isolateForTest(t)
	for _, generation := range []string{"helper", "helper's helper"} {
		nested := isolateForTest(t)
		if filepath.Dir(nested) != top {
			t.Fatalf("the %s's root %s is not directly inside %s", generation, nested, top)
		}
		if got := os.Getenv(IsolationRootEnv); got != top {
			t.Fatalf("after the %s isolated, %s=%q, want %s", generation, IsolationRootEnv, got, top)
		}
		if got, want := os.Getenv("HOME"), filepath.Join(nested, "home"); got != want {
			t.Fatalf("the %s's HOME=%q, want its own %q", generation, got, want)
		}
	}
	if roots := isolationRoots(t, tmp); len(roots) != 1 {
		t.Fatalf("TMPDIR holds %v, want only the first root", roots)
	}
}

func TestIsolate_ignores_an_exported_value_that_is_not_a_root(t *testing.T) {
	for name, value := range map[string]func(tmp string) string{
		"relative":      func(string) string { return rootPrefix + "relative" },
		"missing":       func(tmp string) string { return filepath.Join(tmp, rootPrefix+"missing") },
		"another name":  func(tmp string) string { return filepath.Join(tmp, "elsewhere") },
		"a file":        func(tmp string) string { return filepath.Join(tmp, rootPrefix+"file") },
		"set but empty": func(string) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			tmp := privateTMPDIR(t)
			given := value(tmp)
			if err := os.Mkdir(filepath.Join(tmp, "elsewhere"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tmp, rootPrefix+"file"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(IsolationRootEnv, given)
			root := isolateForTest(t)
			if filepath.Dir(root) != tmp || !strings.HasPrefix(filepath.Base(root), rootPrefix) {
				t.Fatalf("with %s=%q the root is %s, want a new crw-relay-test-* directory directly in %s", IsolationRootEnv, given, root, tmp)
			}
			if got := os.Getenv(IsolationRootEnv); got != root {
				t.Fatalf("%s=%q, want the new root %s", IsolationRootEnv, got, root)
			}
		})
	}
}

// The cleanup IsolateRelayState returns removes the root, unless KeepRootEnv asks to keep it.
func TestKeepRootEnv_keeps_the_root_only_when_asked(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		kept        bool
	}{
		{"unset", "", false},
		{"zero", "0", false},
		{"one", "1", true},
		{"any other value", "yes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			privateTMPDIR(t)
			if tc.value != "" {
				t.Setenv(KeepRootEnv, tc.value)
			}
			cleanup, err := IsolateRelayState()
			if err != nil {
				t.Fatal(err)
			}
			root := os.Getenv(IsolationRootEnv)
			t.Cleanup(func() { _ = RemoveTempTree(root) })
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(root)
			if kept := statErr == nil; kept != tc.kept {
				t.Fatalf("with %s=%q the root %s kept=%v, want %v (stat: %v)", KeepRootEnv, tc.value, root, kept, tc.kept, statErr)
			}
		})
	}
}
