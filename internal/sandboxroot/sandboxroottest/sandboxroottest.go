// Package sandboxroottest gives a test a sandbox-root rule that reads nothing of the real host
// (CRW-1265): a fake account home, a fake Codex home, a fake crw configuration and a sandbox base
// the test chooses. The real account home, the real ~/.codex and the real crw configuration never
// decide a verdict in a test.
package sandboxroottest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// World is a fake host: the directories the rule reads.
type World struct {
	Root      string // the temporary directory everything lives in (a real path)
	Home      string // the fake account home, also the fake $HOME
	Sandbox   string // sandbox_root
	Worktrees string // worktree_root
	CodexHome string // the fake CODEX_HOME
	Config    string // the fake crw config.json
	env       map[string]string
}

// Fake builds a World under t.TempDir() and points the rule at it until the test ends. Its sandbox
// base, account home and worktree base are three directories below that temporary root (see the
// World fields); both bases and the home exist.
func Fake(t testing.TB) *World {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &World{Root: root, Home: filepath.Join(root, "home", "jun"), Sandbox: filepath.Join(root, "scratch", "crw"), Worktrees: filepath.Join(root, "data", "worktrees"), CodexHome: filepath.Join(root, "codex-home"), Config: filepath.Join(root, "crw-config.json")}
	for _, dir := range []string{w.Home, w.Sandbox, w.Worktrees, w.CodexHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.WriteConfig(t, nil)
	w.env = map[string]string{"HOME": w.Home, "CODEX_HOME": w.CodexHome, "CRW_CONFIG": w.Config}
	t.Cleanup(sandboxroot.Inject(func(name string) string { return w.env[name] }, w.Home))
	return w
}

// WriteConfig rewrites the fake crw configuration: sandbox_root and worktree_root as the World has
// them, and project roots as sandbox.project_roots.
func (w *World) WriteConfig(t testing.TB, projectRoots []string) {
	t.Helper()
	doc := map[string]any{"paths": map[string]string{"sandbox_root": w.Sandbox, "worktree_root": w.Worktrees}}
	if projectRoots != nil {
		doc["sandbox"] = map[string]any{"project_roots": projectRoots}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.Config, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Project makes dir (and its parents) a Codex project: a [projects."dir"] key in the fake config.toml.
func (w *World) Project(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.CodexHome, "config.toml")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "[projects.%q]\ntrust_level = \"trusted\"\n", dir); err != nil {
		t.Fatal(err)
	}
}

// Warnings is the sandbox-root warning record of the fake Codex home (CRW-1308), in the order written.
func (w *World) Warnings() []sandboxroot.Record { return sandboxroot.ReadRecords(w.CodexHome) }

// ClearWarnings empties the warning record, so that a subtest sees only its own.
func (w *World) ClearWarnings(t testing.TB) {
	t.Helper()
	if err := os.Remove(sandboxroot.RecordPath(w.CodexHome)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// Warned is the last warning record that names caller and rule, and path unless path is "". It fails the test when there is
// none.
func (w *World) Warned(t testing.TB, caller, rule, path string) sandboxroot.Record {
	t.Helper()
	records := w.Warnings()
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.Caller == caller && r.Rule == rule && (path == "" || r.Path == path) && r.Decision == sandboxroot.DecisionWarn {
			return r
		}
	}
	t.Fatalf("no %s warning by %s for %q; the record holds %+v", rule, caller, path, records)
	return sandboxroot.Record{}
}

// Mkdir makes a directory under Sandbox (or any path) and returns it.
func (w *World) Mkdir(t testing.TB, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// FakeAccountHome is the account home Install names: a directory that does not exist.
const FakeAccountHome = "/crw-test-account-home"

// Install points the rule at a sandbox base for a whole test binary, from TestMain: base is the
// sandbox_root, the account home is FakeAccountHome and the Codex home and configuration are made in a
// directory of their own. The configuration names the base and nothing else (no project roots). The
// returned cleanup removes it. A test that needs particular roots uses Fake instead, which replaces
// this until the test ends.
//
// That directory lies where the process that started the test binary removes it however the binary ends
// (fakesParent): a helper process of a test binary (a crash child, a hook helper) installs the rule too and
// may end without running its cleanup. A relative or unusable TMPDIR is an error and nothing is made.
func Install(base string) (cleanup func() error, err error) {
	parent, exportParent, err := fakesParent()
	if err != nil {
		return nil, err
	}
	return install(parent, exportParent, base)
}

// InstallTemp is Install with the temporary directory (tmploc.Dir) as the sandbox base, for a test
// binary whose tests make their directories with t.TempDir().
func InstallTemp() (cleanup func() error, err error) {
	dir, err := tmploc.Dir()
	if err != nil {
		return nil, err
	}
	return Install(dir)
}

// isolationRootEnv is testsupport.IsolationRootEnv, the isolation root of a tree of test processes. It is not
// imported: a test binary that installs the rule without testsupport.Main (internal/bridge) does not link
// testsupport.
const isolationRootEnv = "CRW_TEST_ISOLATION_ROOT"

// parentEnv names the directory of fakes of the first Install of a tree of test processes that has no
// isolation root; a helper process that installs the rule makes its own inside it, so the first one's cleanup
// removes both (CRW-1265).
const parentEnv = "CRW_SANDBOXROOT_TEST_DIR"

// fakesParent is the directory Install makes its fakes in: the isolation root of the test process tree, else
// the fakes of the test binary that started this process (both named by the tree, so exportParent is false),
// else the temporary directory (tmploc.Dir), which Install chose and exports to its helpers. It is never "":
// a relative TMPDIR is tmploc's error.
func fakesParent() (parent string, exportParent bool, err error) {
	for _, name := range []string{isolationRootEnv, parentEnv} {
		dir := os.Getenv(name)
		if !filepath.IsAbs(dir) {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, false, nil
		}
	}
	dir, err := tmploc.Dir()
	if err != nil {
		return "", false, err
	}
	return dir, true, nil
}

// install is Install with the directory its fakes are made in, which is absolute (tmploc.MkdirTempIn
// refuses "" and a relative one). exportParent says that the process chose it, so helper processes are
// told (parentEnv).
func install(parent string, exportParent bool, base string) (cleanup func() error, err error) {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	fakes, err := tmploc.MkdirTempIn(parent, "crw-sandboxroot-test-")
	if err != nil {
		return nil, err
	}
	// Every failure below removes the directory it made.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(fakes)
		}
	}()
	// The fake account home is a path that does not exist and lies above nothing, so it protects
	// nothing a test makes and the sandbox base cannot be its ancestor.
	home := FakeAccountHome
	codex := filepath.Join(fakes, "codex-home")
	if err := os.MkdirAll(codex, 0o755); err != nil {
		return nil, err
	}
	config := filepath.Join(fakes, "crw-config.json")
	data, _ := json.Marshal(map[string]any{"paths": map[string]string{"sandbox_root": realBase}})
	if err := os.WriteFile(config, data, 0o644); err != nil {
		return nil, err
	}
	env := map[string]string{"HOME": home, "CODEX_HOME": codex, "CRW_CONFIG": config}
	restore := sandboxroot.Inject(func(name string) string { return env[name] }, home)
	// A process the tests start reads the rule from its own environment, so the test process exports the
	// fake crw configuration to it (the rule's account home there is the passwd home), and the fake Codex home
	// when the test binary has none of its own.
	// When Install chose the fakes' directory (TMPDIR), it exports it too (parentEnv): a helper process of the
	// test binary nests its own there.
	exported := map[string]string{"CODEX_HOME": codex, "CRW_CONFIG": config}
	if exportParent {
		exported[parentEnv] = fakes
	}
	previous := map[string]*string{}
	for _, name := range []string{"CODEX_HOME", "CRW_CONFIG", parentEnv} {
		value, set := os.LookupEnv(name)
		if _, export := exported[name]; !export || (name == "CODEX_HOME" && set && value != "") {
			continue
		}
		if set {
			previous[name] = &value
		} else {
			previous[name] = nil
		}
		if err := os.Setenv(name, exported[name]); err != nil {
			restore()
			return nil, err
		}
	}
	return func() error {
		restore()
		for name, value := range previous {
			if value == nil {
				os.Unsetenv(name)
			} else {
				os.Setenv(name, *value)
			}
		}
		return os.RemoveAll(fakes)
	}, nil
}

// Setup is Install in the shape of a testsupport.Setup: the isolation root is the sandbox base, and the fakes
// lie in it, so the root's removal covers them.
func Setup(root string) (func() error, error) { return install(root, false, root) }

// SetupTemp is Install in the shape of a testsupport.Setup for a package whose tests make their
// directories with t.TempDir(): the sandbox base is the temporary directory (tmploc.Dir), and the fakes lie in
// the isolation root.
func SetupTemp(root string) (func() error, error) {
	dir, err := tmploc.Dir()
	if err != nil {
		return nil, err
	}
	return install(root, false, dir)
}
