package role

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

func TestResolveSpawnConfig(t *testing.T) { // store.test.ts AC3 and effort tests, phase2.test.mjs S8 and S10
	env, _ := home(t)
	must(SetRole(env, Executor, RolePatch{Mode: Some(ModeModel), Model: Some("claude-opus")}))
	must(SetRole(env, Reviewer, RolePatch{Effort: Some(EffortLow)}))
	for role, want := range map[RoleName]SpawnResolution{
		Executor: {Role: Executor, Model: str("claude-opus")},
		Explorer: {Role: Explorer, UsesMainModel: true},
		Reviewer: {Role: Reviewer, UsesMainModel: true, Effort: eff(EffortLow)}, // effort is independent of mode
	} {
		if got := must(ResolveSpawnConfig(env, role)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v, want %+v", role, got, want)
		}
	}
	for _, role := range Roles() { // S8: every role keeps and honours its own model
		must(SetRole(env, role, RolePatch{Mode: Some(ModeModel), Model: Some("model-" + string(role))}))
		if got := must(ResolveSpawnConfig(env, role)); got.UsesMainModel || got.Model == nil || *got.Model != "model-"+string(role) {
			t.Errorf("%s = %+v", role, got)
		}
	}
	must(apply(env, `{"scope":"global","role":"reviewer","promptOverride":"Adversarial only."}`)) // S10
	if got := must(ResolveSpawnConfig(env, Reviewer)); got.PromptOverride == nil || *got.PromptOverride != "Adversarial only." {
		t.Errorf("prompt override = %v", got.PromptOverride)
	}
	if _, err := ResolveSpawnConfig(env, "nobody"); err == nil || err.Error() != `unknown role "nobody"` {
		t.Errorf("unknown role: %v", err)
	}
}

// plainGit makes the environment one in which only the repository a test builds can answer: the helpers inherit it, as the oracle's
// do, and a CI checkout or a hook may set the routing variables.
func plainGit(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, value, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			check(t, os.Unsetenv(name))
			t.Cleanup(func() { _ = os.Setenv(name, value) })
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repo is a temporary repository whose <root>/.crw/subagents.json holds config; tracked adds it with git add -f.
func repo(t *testing.T, config string, tracked bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	check(t, os.MkdirAll(filepath.Join(root, crwdir.DirName), 0o755))
	check(t, os.WriteFile(filepath.Join(root, crwdir.DirName, StoreFile), []byte(config), 0o644))
	runGit(t, root, "init", "-q")
	if tracked {
		runGit(t, root, "add", "-f", crwdir.DirName+"/"+StoreFile)
	}
	return root
}

// stubGit puts a git on PATH that runs script.
func stubGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	check(t, os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func digest(t *testing.T, root, config string) string {
	t.Helper()
	real := func(p string) string { return must(filepath.EvalSymlinks(p)) }
	data := must(os.ReadFile(config))
	sum := sha256.Sum256([]byte(real(root) + "\x00" + real(config) + "\x00" + string(data)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestTrackedProjectConfigDoesNotInfluenceResolution(t *testing.T) { // decision 7: the project layer is gone
	plainGit(t)
	env, _ := home(t)
	root := repo(t, `{"roles":{"executor":{"mode":"model","model":"repo-model","promptOverride":"repo instructions"}}}`, true)
	if !IsTrackedProjectConfig(root) {
		t.Fatal("the fixture's config is not tracked")
	}
	if got := must(ResolveSpawnConfig(env, Executor)); !got.UsesMainModel || got.PromptOverride != nil {
		t.Fatalf("a Git-tracked project config reached the resolution: %+v", got)
	}
}

func TestIsTrackedProjectConfig(t *testing.T) {
	plainGit(t)
	root := repo(t, "{}", false)
	if IsTrackedProjectConfig(root) {
		t.Error("an untracked config reads as tracked")
	}
	runGit(t, root, "add", "-f", crwdir.DirName+"/"+StoreFile)
	if !IsTrackedProjectConfig(root) {
		t.Error("a tracked config reads as untracked")
	}
	if IsTrackedProjectConfig(filepath.Join(root, "missing")) || IsTrackedProjectConfig(t.TempDir()) {
		t.Error("a directory that is missing, or outside a repository, reads as tracked")
	}
	t.Setenv("PATH", t.TempDir()) // no git at all
	if IsTrackedProjectConfig(root) {
		t.Error("a missing git reads as tracked")
	}
}

func TestProjectConfigTrustToken(t *testing.T) { // store.test.ts "Git-tracked project config is ignored until the operator explicitly trusts it"
	plainGit(t)
	config := `{"roles":{"executor":{"mode":"model","model":"repo-model"}}}`
	root := repo(t, config, true)
	file := filepath.Join(root, crwdir.DirName, StoreFile)
	token, ok := ProjectConfigTrustToken(root)
	if !ok || token != digest(t, root, file) {
		t.Fatalf("token %q, %v, want %q", token, ok, digest(t, root, file))
	}
	alias := filepath.Join(t.TempDir(), "alias")
	check(t, os.Symlink(root, alias))
	if got, _ := ProjectConfigTrustToken(alias); got != token {
		t.Errorf("a symbolic link to the repository gives %q", got)
	}
	sub := filepath.Join(root, "sub")
	check(t, os.MkdirAll(filepath.Join(sub, crwdir.DirName), 0o755))
	check(t, os.WriteFile(filepath.Join(sub, crwdir.DirName, StoreFile), []byte(config), 0o644))
	if got, ok := ProjectConfigTrustToken(sub); !ok || got == token || got != digest(t, root, filepath.Join(sub, crwdir.DirName, StoreFile)) {
		t.Errorf("a subdirectory (same root, other config path) gives %q", got)
	}
	if other, _ := ProjectConfigTrustToken(repo(t, config, true)); other == token {
		t.Error("the same bytes in another repository need separate review")
	}
	check(t, os.WriteFile(file, []byte(`{"roles":{}}`), 0o644))
	if edited, _ := ProjectConfigTrustToken(root); edited == token {
		t.Error("editing the reviewed config keeps the token")
	}
	if got, ok := ProjectConfigTrustToken(filepath.Join(root, "missing")); ok || got != "" {
		t.Errorf("a missing config gives %q", got)
	}
}

func TestProjectConfigTrustTokenOutsideARepositoryBindsTheDirectory(t *testing.T) {
	plainGit(t)
	dir := filepath.Join(t.TempDir(), "plain")
	check(t, os.MkdirAll(filepath.Join(dir, crwdir.DirName), 0o755))
	file := filepath.Join(dir, crwdir.DirName, StoreFile)
	check(t, os.WriteFile(file, []byte("{}"), 0o644))
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir)) // a stray repository above the temporary directory cannot answer
	if got, ok := ProjectConfigTrustToken(dir); !ok || got != digest(t, dir, file) {
		t.Fatalf("token %q, %v, want %q", got, ok, digest(t, dir, file))
	}
}

func TestGitThatHangsIsGivenUpOn(t *testing.T) {
	plainGit(t)
	root := repo(t, "{}", true)
	file := filepath.Join(root, crwdir.DirName, StoreFile)
	for name, c := range map[string]struct {
		script  string
		tracked bool // what a git that exits 0 is taken for
		token   bool
	}{
		"never answers":                      {script: "exec sleep 30"},
		"exits but a child keeps its stdout": {script: "sleep 5 &", tracked: true},
	} {
		t.Run(name, func(t *testing.T) {
			stubGit(t, c.script)
			start := time.Now()
			tracked := IsTrackedProjectConfig(root)
			got, ok := ProjectConfigTrustToken(root) // the root falls back to the directory, which is the root here
			if tracked != c.tracked || !ok || got != digest(t, root, file) {
				t.Errorf("tracked %v, token %q %v", tracked, got, ok)
			}
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Errorf("took %v: the git child was waited for", elapsed)
			}
		})
	}
}
