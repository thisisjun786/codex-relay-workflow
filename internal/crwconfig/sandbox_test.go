package crwconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// CRW-1265: sandbox_root is the base path a sandboxed Codex runs in. Its default is /scratch/crw, the
// configuration's paths section moves it, and the sandbox section lists the project roots.
func TestSandboxRootDefaultAndOverride(t *testing.T) {
	home, env := rootsHome(t)
	roots, err := Resolve(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := roots[RootSandbox]; got.Path != "/scratch/crw" || got.Source != SourceDefault {
		t.Fatalf("the default sandbox_root is %+v, want /scratch/crw from the default", got)
	}
	// No environment variable and no other root moves it.
	moved := rootsEnv("HOME", home, "XDG_CACHE_HOME", filepath.Join(home, "c"), "TMPDIR", filepath.Join(home, "t"), "XDG_DATA_HOME", filepath.Join(home, "d"))
	if roots, err = Resolve(moved, nil); err != nil || roots[RootSandbox].Path != "/scratch/crw" {
		t.Fatalf("an environment moved sandbox_root: %+v %v", roots[RootSandbox], err)
	}
	override := filepath.Join(home, "sandbox")
	if roots, err = Resolve(env, map[string]string{RootSandbox: override}); err != nil {
		t.Fatal(err)
	}
	if got := roots[RootSandbox]; got.Path != override || got.Source != SourceConfig {
		t.Fatalf("an override gives %+v, want %q from config", got, override)
	}
	if _, err := Resolve(env, map[string]string{RootSandbox: "relative/sandbox"}); err == nil || !strings.Contains(err.Error(), "sandbox_root") {
		t.Fatalf("a relative sandbox_root: %v", err)
	}
	// The configuration file's paths section reaches it, and crw config paths lists it last.
	path := configWrite(t, `{"paths": {"sandbox_root": "`+override+`"}}`)
	file, err := Load(env, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Roots()[RootSandbox]; got.Path != override || got.Source != SourceConfig {
		t.Fatalf("the file gives %+v", got)
	}
	names := RootNames()
	if names[len(names)-1] != RootSandbox {
		t.Fatalf("root names %v do not end with sandbox_root", names)
	}
	code, out, errOut := configRun(t, env, "paths", "--config", path)
	if code != 0 || !strings.Contains(out, `"name": "sandbox_root"`) || !strings.Contains(out, override) {
		t.Fatalf("crw config paths: %d %s %s", code, out, errOut)
	}
}

func TestSandboxProjectRootsLoad(t *testing.T) {
	home, env := rootsHome(t)
	none, err := Load(env, filepath.Join(home, "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := none.SandboxProjectRoots(); err != nil || len(got) != 0 {
		t.Fatalf("no file: %v %v", got, err)
	}
	path := configWrite(t, `{"sandbox": {"project_roots": ["/work/a", "/work/b/c"]}}`)
	file, err := Load(env, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := file.SandboxProjectRoots()
	if err != nil || !reflect.DeepEqual(got, []string{"/work/a", "/work/b/c"}) {
		t.Fatalf("project roots %v %v", got, err)
	}
	got[0] = "changed"
	if again, _ := file.SandboxProjectRoots(); again[0] != "/work/a" {
		t.Fatal("the caller changed the file's list")
	}
	if code, out, _ := configRun(t, env, "show", "--config", path); code != 0 || !strings.Contains(out, "project_roots") {
		t.Fatalf("crw config show: %d %s", code, out)
	}
	for name, document := range map[string]string{
		"not an object":         `{"sandbox": ["/a"]}`,
		"not a list":            `{"sandbox": {"project_roots": "/a"}}`,
		"a non-string":          `{"sandbox": {"project_roots": [1]}}`,
		"a relative path":       `{"sandbox": {"project_roots": ["a/b"]}}`,
		"another relative path": `{"sandbox": {"project_roots": ["relative"]}}`,
	} {
		if _, err := Load(env, configWrite(t, document)); err == nil || !strings.Contains(err.Error(), "sandbox") {
			t.Errorf("%s: want a sandbox error, got %v", name, err)
		}
	}
	// A section without project_roots, and a null one, are no roots.
	for _, document := range []string{`{"sandbox": {}}`, `{"sandbox": {"project_roots": null}}`, `{"sandbox": null}`} {
		file, err := Load(env, configWrite(t, document))
		if err != nil {
			t.Fatalf("%s: %v", document, err)
		}
		if got, err := file.SandboxProjectRoots(); err != nil || len(got) != 0 {
			t.Fatalf("%s: %v %v", document, got, err)
		}
	}
}
