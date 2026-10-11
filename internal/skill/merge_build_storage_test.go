package skill

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

func mbsValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

// CRW-958: with no tool root configured the go environment is exactly what it was.
func TestMergeBuildToolEnvPassesNothingWhenNothingIsConfigured(t *testing.T) {
	t.Setenv("GOENV", "off")
	scratch := t.TempDir()
	base := []string{"A=1", "HOME=/caller"}
	got, cleanup, err := mergeBuildToolEnv(context.Background(), base, scratch, crwconfig.ToolRoots{}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	want, err := mergeBuildEnv(context.Background(), base, filepath.Join(scratch, "go"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environment\n got %v\nwant %v", got, want)
	}
}

// CRW-958: configured caches are named last, so they win over the caller's and, on darwin, over
// the values read from the caller's go environment.
func TestMergeBuildToolEnvNamesTheConfiguredCachesLast(t *testing.T) {
	t.Setenv("GOENV", "off")
	for _, goos := range []string{"linux", "darwin"} {
		base := append(os.Environ(), "HOME="+t.TempDir(), "GOCACHE=/caller/build", "GOMODCACHE=/caller/mod")
		tools := crwconfig.ToolRoots{BuildCache: "/cfg/build", ModuleCache: "/cfg/mod"}
		env, cleanup, err := mergeBuildToolEnv(context.Background(), base, t.TempDir(), tools, goos)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		cleanup()
		if got, _ := mbsValue(env, "GOCACHE"); got != "/cfg/build" {
			t.Errorf("%s: GOCACHE=%q, want the configured build cache", goos, got)
		}
		if got, _ := mbsValue(env, "GOMODCACHE"); got != "/cfg/mod" {
			t.Errorf("%s: GOMODCACHE=%q, want the configured module cache", goos, got)
		}
	}
}

// CRW-958: a configured build_work holds the run's owned directory, so the go tool's temporary
// files land there; the run removes what it made and leaves the configured directory.
func TestMergeBuildToolEnvPlacesTheOwnedDirectoryUnderBuildWork(t *testing.T) {
	t.Setenv("GOENV", "off")
	work := filepath.Join(t.TempDir(), "not", "yet", "there")
	scratch := t.TempDir()
	env, cleanup, err := mergeBuildToolEnv(context.Background(), []string{"HOME=/caller"}, scratch, crwconfig.ToolRoots{BuildWork: work}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ := mbsValue(env, "GOTMPDIR")
	if !strings.HasPrefix(tmp, work+string(filepath.Separator)) || !strings.HasSuffix(tmp, string(filepath.Separator)+"tmp") {
		t.Errorf("GOTMPDIR=%q is not below the configured build_work %q", tmp, work)
	}
	if other, _ := mbsValue(env, "TMPDIR"); other != tmp {
		t.Errorf("TMPDIR=%q differs from GOTMPDIR=%q", other, tmp)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOTMPDIR=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GOTMPDIR is named %d times", n)
	}
	if info, err := os.Stat(tmp); err != nil || !info.IsDir() {
		t.Errorf("the owned temporary directory is missing: %v", err)
	}
	cleanup()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the owned temporary directory survived the run: %v", err)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Errorf("the configured build_work was removed: %v", err)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("the run left %d entries in build_work", len(entries))
	}
	if _, err := os.Stat(filepath.Join(scratch, "go")); err == nil {
		t.Error("the owned directory was also made under the scratch root")
	}
}

// CRW-958 fix round 3: a build_work spelled through a symbolic link and ".." means the directory the
// filesystem resolves it to. The owned directory, the go tool's temporary directory and the cleanup
// all stay on that disk, and nothing is made at the lexically cleaned place.
func TestMergeBuildToolEnvKeepsTheRawSpellingOfBuildWork(t *testing.T) {
	t.Setenv("GOENV", "off")
	base := t.TempDir()
	for _, dir := range []string{"a", "b/deep"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "b", "deep"), filepath.Join(base, "a", "link")); err != nil {
		t.Fatal(err)
	}
	spelled, real, cleaned := base+"/a/link/../root", filepath.Join(base, "b", "root"), filepath.Join(base, "a", "root")
	env, cleanup, err := mergeBuildToolEnv(context.Background(), []string{"HOME=/caller"}, t.TempDir(), crwconfig.ToolRoots{BuildWork: spelled}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	// The go tool cleans the paths it joins below XDG_CONFIG_HOME, TMPDIR and HOME, so the
	// environment names the owned directory by its physical path, never the raw spelling.
	tmp, _ := mbsValue(env, "GOTMPDIR")
	if !strings.HasPrefix(tmp, real+string(filepath.Separator)) {
		t.Errorf("GOTMPDIR=%q is not below the physical build_work %q", tmp, real)
	}
	if config, _ := mbsValue(env, "XDG_CONFIG_HOME"); !strings.HasPrefix(config, real+string(filepath.Separator)) {
		t.Errorf("XDG_CONFIG_HOME=%q is not below the physical build_work %q", config, real)
	}
	if entries, _ := os.ReadDir(real); len(entries) != 1 {
		t.Errorf("the owned directory is not on the real disk %s: %d entries", real, len(entries))
	}
	if _, err := os.Stat(cleaned); err == nil {
		t.Errorf("the lexically cleaned directory %s was made", cleaned)
	}
	cleanup()
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Errorf("the run left %d entries in %s", len(entries), real)
	}
}

// CRW-958 fix round 4: the real go tool, run in the environment for a build_work spelled through a
// symbolic link and "..", reads the off telemetry mode the run wrote and keeps its telemetry and
// temporary files inside the owned directory: nothing lands at the lexically cleaned place, and the
// cleanup leaves the physical build_work empty.
func TestMergeBuildToolEnvRawBuildWorkHoldsTheRealGoTool(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("this test runs the real go tool and needs it on PATH: %v", err)
	}
	t.Setenv("GOENV", "off")
	base := t.TempDir()
	for _, dir := range []string{"a", "b/deep"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "b", "deep"), filepath.Join(base, "a", "link")); err != nil {
		t.Fatal(err)
	}
	spelled, real, cleaned := base+"/a/link/../root", filepath.Join(base, "b", "root"), filepath.Join(base, "a", "root")
	caller := append(os.Environ(), "GOTELEMETRYDIR=")
	env, cleanup, err := mergeBuildToolEnv(context.Background(), caller, t.TempDir(), crwconfig.ToolRoots{BuildWork: spelled})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	probe := exec.Command(goTool, "env", "GOTELEMETRY", "GOTELEMETRYDIR")
	probe.Env = env
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("go env: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "off" {
		t.Errorf("the go tool does not read the off mode the run wrote: %q", out)
	}
	if len(lines) == 2 && !strings.HasPrefix(lines[1], real+string(filepath.Separator)) {
		t.Errorf("the go tool keeps telemetry at %q, not below the physical build_work %q", lines[1], real)
	}
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.test/raw\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "raw.go"), []byte("package raw\n\nfunc Value() int { return 958 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goTool, "build", "-o", filepath.Join(module, "raw.a"), ".")
	build.Dir = module
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		t.Errorf("go build in the environment: %v\n%s", err, out)
	}
	if _, err := os.Stat(cleaned); err == nil {
		entries, _ := os.ReadDir(cleaned)
		t.Errorf("the go tool wrote at the lexically cleaned directory %s: %v", cleaned, entries)
	}
	cleanup()
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Errorf("the cleanup left %d entries in the physical build_work %s", len(entries), real)
	}
}
