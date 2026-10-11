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
// directory of their own. The
// returned cleanup removes it. A test that needs particular roots uses Fake instead, which replaces
// this until the test ends.
//
// That directory lies where the process that started the test binary removes it however the binary ends
// (fakesParent): a helper process of a test binary (a crash child, a hook helper) installs the rule too and
// may end without running its cleanup.
func Install(base string, projects ...string) (cleanup func() error, err error) {
	return install(fakesParent(), base, projects)
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
// the fakes of the test binary that started this process, else TMPDIR ("").
func fakesParent() string {
	for _, name := range []string{isolationRootEnv, parentEnv} {
		dir := os.Getenv(name)
		if !filepath.IsAbs(dir) {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// install is Install with the directory its fakes are made in ("" is TMPDIR).
func install(parent, base string, projects []string) (cleanup func() error, err error) {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	fakes, err := os.MkdirTemp(parent, "crw-sandboxroot-test-")
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
	doc := map[string]any{"paths": map[string]string{"sandbox_root": realBase}}
	var roots []string
	for _, project := range projects {
		if real, err := filepath.EvalSymlinks(project); err == nil {
			roots = append(roots, real)
		}
	}
	if len(roots) > 0 {
		doc["sandbox"] = map[string]any{"project_roots": roots}
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(config, data, 0o644); err != nil {
		return nil, err
	}
	env := map[string]string{"HOME": home, "CODEX_HOME": codex, "CRW_CONFIG": config}
	restore := sandboxroot.Inject(func(name string) string { return env[name] }, home)
	// A process the tests start reads the rule from its own environment, so the test process exports the
	// fake crw configuration to it (the rule's account home there is the passwd home), and the fake Codex home
	// when the test binary has none of its own.
	// When the fakes lie in TMPDIR, it exports their directory too (parentEnv): a helper process of the test
	// binary nests its own there.
	exported := map[string]string{"CODEX_HOME": codex, "CRW_CONFIG": config}
	if parent == "" {
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
func Setup(root string) (func() error, error) { return install(root, root, FallbackTempParents) }

// FallbackTempParents are the system temporary locations testsupport falls back to when TMPDIR is
// inside a repository; they are project roots of the rule a test binary installs.
var FallbackTempParents = []string{"/var/tmp", "/dev/shm"}

// SetupTemp is Install in the shape of a testsupport.Setup for a package whose tests make their
// directories with t.TempDir(): the sandbox base is the temporary directory, and the fakes lie in the
// isolation root.
func SetupTemp(root string) (func() error, error) {
	return install(root, os.TempDir(), FallbackTempParents)
}
