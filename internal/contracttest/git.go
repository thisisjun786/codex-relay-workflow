package contracttest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/worktrees"
)

func gitCommand(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func gitValue(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	value, err := gitCommand(ctx, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func asObject(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}
func stringValue(value any) string { result, _ := value.(string); return result }

func gitRepo(t *testing.T, scenario Scenario, root string) (string, string) {
	t.Helper()
	ctx := context.Background()
	repo := filepath.Join(root, "source")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, repo, "init", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	for name, value := range asObject(scenario.Given["files"]) {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(stringValue(value)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, repo, "commit", "--allow-empty", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base := gitValue(t, ctx, repo, "rev-parse", "HEAD")
	for name, value := range asObject(scenario.Given["staged"]) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(stringValue(value)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := gitCommand(ctx, repo, "add", name); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range asObject(scenario.Given["dirty"]) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(stringValue(value)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return repo, base
}

func runGit(t *testing.T, scenario Scenario) (map[string]any, error) {
	t.Helper()
	root, err := os.MkdirTemp("/dev/shm", "crw-git-contract-")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "Fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.invalid")
	repo, base := gitRepo(t, scenario, root)
	if scenario.Run["bridge"] == true {
		return runGitBridge(t, scenario, root, repo, base)
	}
	return runGitWorktree(t, scenario, root, repo, base)
}

func runGitWorktree(t *testing.T, scenario Scenario, root, repo, base string) (map[string]any, error) {
	ctx := context.Background()
	var inspection any
	checkout := filepath.Join(root, "isolated")
	for _, raw := range scenario.Run["steps"].([]any) {
		step := asObject(raw)
		switch step["kind"] {
		case "worktree":
			checkout = filepath.Join(root, stringValue(step["path"]))
			w, err := worktrees.Validate(ctx, repo, base, checkout)
			if err != nil {
				return nil, err
			}
			if err := w.Reserve(); err != nil {
				return nil, err
			}
			if err := w.Create(ctx); err != nil {
				return nil, err
			}
			if err := w.Checkout(ctx); err != nil {
				return nil, err
			}
			inspection, err = w.Inspect(ctx)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported git step %v", step["kind"])
		}
	}
	obs := map[string]any{"exit": float64(0), "base": base, "inspection": inspection, "revision": gitValue(t, ctx, repo, "rev-parse", "HEAD"), "status": gitValue(t, ctx, repo, "status", "--porcelain=v1"), "worktrees": gitValue(t, ctx, repo, "worktree", "list", "--porcelain"), "branches": strings.Split(gitValue(t, ctx, repo, "branch", "--format=%(refname:short)"), "\n")}
	for _, key := range []string{"source", "checkout"} {
		dir := repo
		if key == "checkout" {
			dir = checkout
		}
		files := map[string]any{}
		for _, name := range observedNames(scenario, key) {
			files[name] = fileText(filepath.Join(dir, name))
		}
		obs[key] = files
	}
	return jsonShape(obs), nil
}

func fileText(path string) any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return string(data)
}
func jsonShape(value map[string]any) map[string]any {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
func observedNames(scenario Scenario, key string) []string {
	names := []string{}
	for _, check := range scenario.Expect.Checks {
		if len(check.Path) == 2 && check.Path[0] == key {
			if name, ok := check.Path[1].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}
