// Package sandboxroottest gives a test a sandbox-root rule that reads nothing of the real host
// (CRW-1265): a fake account home, a fake Codex home, a fake crw configuration and a sandbox base
// the test chooses. The real /home/jun, the real ~/.codex and the real crw configuration never
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
// base is <Root>/scratch/crw, its account home <Root>/home/jun and its worktree base
// <Root>/data/worktrees. Both bases and the home exist.
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
func Install(base string) (cleanup func() error, err error) {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	fakes, err := os.MkdirTemp("", "crw-sandboxroot-test-")
	if err != nil {
		return nil, err
	}
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
	return func() error {
		restore()
		return os.RemoveAll(fakes)
	}, nil
}

// Setup is Install in the shape of a testsupport.Setup: the isolation root is the sandbox base.
func Setup(root string) (func() error, error) { return Install(root) }
