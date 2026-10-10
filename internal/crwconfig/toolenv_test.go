package crwconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// toolFile loads a configuration document written under a fresh home.
func toolFile(t *testing.T, document string) (*File, func(string) string, string) {
	t.Helper()
	home, env := rootsHome(t)
	file, err := Load(env, configWrite(t, document))
	if err != nil {
		t.Fatal(err)
	}
	return file, env, home
}

// With nothing configured the helper returns no entry at all: Go keeps its own caches.
func TestToolEnvPassesNothingWhenNothingIsConfigured(t *testing.T) {
	file, _, _ := toolFile(t, `{}`)
	tools := file.ToolRoots()
	if tools != (ToolRoots{}) {
		t.Errorf("ToolRoots() = %+v, want the zero value", tools)
	}
	if env := tools.Env(); len(env) != 0 {
		t.Errorf("Env() = %q, want no entry", env)
	}
	in := []string{"GOCACHE=/caller/cache", "PATH=/bin"}
	if got := tools.Apply(in); !reflect.DeepEqual(got, in) {
		t.Errorf("Apply with nothing configured changed the environment: %q", got)
	}
}

// Only a configured root becomes an entry, under the name Go reads it by.
func TestToolEnvNamesOnlyTheConfiguredRoots(t *testing.T) {
	for _, test := range []struct {
		document string
		want     []string
	}{
		{`{"paths":{"build_cache":"/b/cache"}}`, []string{"GOCACHE=/b/cache"}},
		{`{"paths":{"module_cache":"/b/mod"}}`, []string{"GOMODCACHE=/b/mod"}},
		{`{"paths":{"build_work":"/b/work"}}`, []string{"GOTMPDIR=/b/work"}},
		{`{"paths":{"build_work":"/b/work","build_cache":"/b/cache","module_cache":"/b/mod"}}`,
			[]string{"GOCACHE=/b/cache", "GOMODCACHE=/b/mod", "GOTMPDIR=/b/work"}},
		// roots that are not tool roots never reach a tool's environment
		{`{"paths":{"temp_root":"/b/tmp","cache_root":"/b/c","data_root":"/b/d"}}`, nil},
	} {
		file, _, _ := toolFile(t, test.document)
		if got := file.ToolRoots().Env(); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: Env() = %q, want %q", test.document, got, test.want)
		}
	}
}

// Apply puts the configured entry in place of the caller's own and leaves the rest.
func TestToolEnvApplyReplacesTheCallersValue(t *testing.T) {
	file, _, _ := toolFile(t, `{"paths":{"build_cache":"/b/cache","build_work":"/b/work"}}`)
	got := file.ToolRoots().Apply([]string{"GOCACHE=/caller/cache", "PATH=/bin", "GOMODCACHE=/caller/mod", "GOCACHE=/caller/again"})
	want := []string{"PATH=/bin", "GOMODCACHE=/caller/mod", "GOCACHE=/b/cache", "GOTMPDIR=/b/work"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Apply = %q, want %q", got, want)
	}
}

// LoadToolRoots reads the file the way every command does and returns the three roots.
func TestLoadToolRootsReadsTheConfiguration(t *testing.T) {
	home, env := rootsHome(t)
	configWriteAt(t, configDefaultPath(home), `{"paths":{"build_cache":"`+filepath.Join(home, "bc")+`"}}`)
	tools, err := LoadToolRoots(env, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (ToolRoots{BuildCache: filepath.Join(home, "bc")}); tools != want {
		t.Errorf("LoadToolRoots = %+v, want %+v", tools, want)
	}
	// a clean HOME without any file is the zero value, not an error
	_, clean := rootsHome(t)
	tools, err = LoadToolRoots(clean, "")
	if err != nil || tools != (ToolRoots{}) {
		t.Errorf("clean HOME: %+v, %v", tools, err)
	}
}

// A tool root that fails a structural check is refused, so a product command does not start
// a tool against it; a warning alone never refuses.
func TestLoadToolRootsRefusesAStructuralFault(t *testing.T) {
	home, env := rootsHome(t)
	file := filepath.Join(home, "plain-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadToolRoots(env, configWrite(t, `{"paths":{"module_cache":"`+file+`"}}`))
	if err == nil || !strings.Contains(err.Error(), "module_cache") || !strings.Contains(err.Error(), CheckNotDirectory) {
		t.Errorf("a module cache that is a file: err = %v", err)
	}
	// a fault in a root that is not a tool root is not this command's refusal
	if _, err := LoadToolRoots(env, configWrite(t, `{"paths":{"data_root":"`+file+`"}}`)); err != nil {
		t.Errorf("a data_root fault refused a tool command: %v", err)
	}
}

// A relative tool root never resolves: the existing absolute-path refusal covers the new keys.
func TestANewRootRefusesARelativeOrEmptyPath(t *testing.T) {
	_, env := rootsHome(t)
	for _, name := range []string{RootBuildCache, RootModuleCache, RootBuildWork, RootLog, RootRelayState} {
		for _, value := range []string{"relative/dir", ""} {
			if _, err := Resolve(env, map[string]string{name: value}); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v, want a refusal naming the root", name, value, err)
			}
		}
	}
}
