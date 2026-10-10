package skill

import (
	"context"
	"os"
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
	tmp, _ := mbsValue(env, "GOTMPDIR")
	if !strings.HasPrefix(tmp, spelled+"/") {
		t.Errorf("GOTMPDIR=%q is not below the raw build_work %q", tmp, spelled)
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
