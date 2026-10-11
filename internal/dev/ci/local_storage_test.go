//go:build dev

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// localEnvValue is the last value of name in an environment slice, as exec resolves a duplicate.
func localEnvValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

// CRW-958: with no tool root configured the sealed environment is what it was: the caller's
// GOCACHE is inherited and nothing names a build work directory.
func TestLocalStorage_unset_roots_pass_nothing(t *testing.T) {
	t.Setenv("GOCACHE", "/caller/gocache")
	temp := t.TempDir()
	env, err := localStepEnv(filepath.Join(temp, "home"), temp, localOptions{output: filepath.Join(temp, "output")})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := localEnvValue(env, "GOCACHE"); got != "/caller/gocache" {
		t.Errorf("GOCACHE = %q, want the inherited value", got)
	}
	for _, name := range []string{"GOTMPDIR", "GOMODCACHE"} {
		if got, found := localEnvValue(env, name); found {
			t.Errorf("%s = %q with nothing configured", name, got)
		}
	}
}

// CRW-958: a configured build_cache replaces the inherited GOCACHE and a configured build_work
// is the step's GOTMPDIR.
func TestLocalStorage_configured_build_roots_reach_a_step(t *testing.T) {
	t.Setenv("GOCACHE", "/caller/gocache")
	temp := t.TempDir()
	work := filepath.Join(t.TempDir(), "cfg-work") // made by the step environment, so it has to be creatable
	opts := localOptions{output: filepath.Join(temp, "output"), Tools: crwconfig.ToolRoots{BuildCache: "/cfg/build", BuildWork: work}}
	env, err := localStepEnv(filepath.Join(temp, "home"), temp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := localEnvValue(env, "GOCACHE"); got != "/cfg/build" {
		t.Errorf("GOCACHE = %q, want the configured build_cache", got)
	}
	if got, _ := localEnvValue(env, "GOTMPDIR"); got != work {
		t.Errorf("GOTMPDIR = %q, want the configured build_work", got)
	}
}

// CRW-958: the sealed run keeps its module cache apart. A module tree is trusted from a cache
// without the go.sum check, so a configured module_cache is not handed to a step: the run keeps
// the module cache under its own empty home, as before.
func TestLocalStorage_a_configured_module_cache_never_reaches_a_sealed_step(t *testing.T) {
	temp := t.TempDir()
	opts := localOptions{output: filepath.Join(temp, "output"), Tools: crwconfig.ToolRoots{ModuleCache: "/cfg/mod"}}
	env, err := localStepEnv(filepath.Join(temp, "home"), temp, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, found := localEnvValue(env, "GOMODCACHE"); found {
		t.Errorf("GOMODCACHE = %q reached a sealed step", got)
	}
}

// storageConfig writes a crw configuration file whose paths section is the given pairs and returns
// the CRW_CONFIG entry that selects it.
func storageConfig(t *testing.T, pairs ...string) string {
	t.Helper()
	var paths []string
	for i := 0; i+1 < len(pairs); i += 2 {
		paths = append(paths, `"`+pairs[i]+`":"`+pairs[i+1]+`"`)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"paths":{`+strings.Join(paths, ",")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return "CRW_CONFIG=" + path
}

// CRW-958: the module cache the configuration names is the one go mod tidy -diff reads, in
// place of the caller's GOMODCACHE. The caller's cache holds the module; the configured one is
// empty, so the check says its cache lacks the module.
func TestValidateStorage_the_configured_module_cache_is_the_one_go_reads(t *testing.T) {
	cache := cachedModule(t)
	r := validateRepo(t)
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n\nrequire example.com/dep v1.0.0\n")
	r.write("x.go", "package x\n\nimport _ \"example.com/dep\"\n")
	env := []string{"GITHUB_EVENT_NAME=", "BLOB_RANGE_BASE=", "GOMODCACHE=" + cache,
		storageConfig(t, "module_cache", t.TempDir())}
	got := goCheck(t, r.root, env, "validate")
	if got.code != 0 || !strings.Contains(got.stdout, "go mod tidy check skipped: the module cache lacks a module") {
		t.Errorf("configured module cache: %+v", got)
	}
}

// CRW-958: a configured tool root that fails a structural check is refused, with the check's
// words, before go is started.
func TestValidateStorage_a_module_cache_that_is_a_file_is_refused(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := validateRepo(t)
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n")
	got := goCheck(t, r.root, []string{"GITHUB_EVENT_NAME=", "BLOB_RANGE_BASE=", storageConfig(t, "module_cache", plain)}, "validate")
	if got.code != 1 || !strings.Contains(got.stderr, "module_cache") || !strings.Contains(got.stderr, crwconfig.CheckNotDirectory) {
		t.Errorf("module cache that is a file: %+v", got)
	}
}

// CRW-958: a configured build_work that does not exist yet passes the storage check, and the step that
// is about to start go makes it: go makes its work directory directly below GOTMPDIR and fails when
// that directory is missing. A real go build in the sealed environment succeeds with it.
func TestLocalStorage_a_missing_build_work_is_made_and_go_builds_with_it(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command on PATH")
	}
	temp := t.TempDir()
	work := filepath.Join(t.TempDir(), "disk", "gotmp")
	cache := filepath.Join(t.TempDir(), "disk", "gocache")
	tools, err := crwconfig.LoadToolRoots(func(name string) string {
		if name == "CRW_CONFIG" {
			return strings.TrimPrefix(storageConfig(t, "build_work", work, "build_cache", cache), "CRW_CONFIG=")
		}
		return os.Getenv(name)
	}, "")
	if err != nil {
		t.Fatalf("the check refused a creatable build_work: %v", err)
	}
	if _, err := os.Stat(work); err == nil {
		t.Fatal("loading the roots created build_work: the validator is read-only")
	}
	env, err := localStepEnv(filepath.Join(temp, "home"), temp, localOptions{output: filepath.Join(temp, "output"), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Fatalf("build_work after the step environment was made: %v", err)
	}
	module := t.TempDir()
	for name, body := range map[string]string{"go.mod": "module example.test/m\n\ngo 1.21\n", "m.go": "package m\n\nfunc F() int { return 1 }\n"} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = module
	cmd.Env = append(env, "GOPROXY=off", "GOTELEMETRY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build in the sealed environment: %v\n%s", err, out)
	}
}
