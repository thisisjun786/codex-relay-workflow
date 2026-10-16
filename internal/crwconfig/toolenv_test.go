package crwconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
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

// CRW-958: the validator accepts a build_work that does not exist yet and stays read-only; the step
// that starts a tool makes it. Prepare makes it owner-only, makes nothing for a root nothing
// configured, and refuses one that cannot be made.
func TestToolRootsPrepareMakesAMissingBuildWork(t *testing.T) {
	home, _ := rootsHome(t)
	work := filepath.Join(home, "disk", "gotmp")
	tools := ToolRoots{BuildWork: work, BuildCache: filepath.Join(home, "disk", "bc")}
	if err := tools.Prepare(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(work)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("build_work after Prepare: %v %v", info, err)
	}
	if _, err := os.Stat(tools.BuildCache); err == nil {
		t.Error("Prepare made the build cache: go makes that itself")
	}
	if err := (ToolRoots{}).Prepare(); err != nil {
		t.Errorf("nothing configured: %v", err)
	}
	plain := filepath.Join(tmploctest.SocketDir(t, "rs"), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (ToolRoots{BuildWork: filepath.Join(plain, "work")}).Prepare(); err == nil {
		t.Error("a build_work below a file was made")
	}
}

// CRW-958: `crw config check` accepts the missing build_work without creating it, and a real go build
// started with the configured roots then succeeds only because the consumer prepared it. The module
// below has no dependency, so the build needs no network and no module cache.
func TestToolRootsAMissingBuildWorkIsUsableAfterPrepare(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command on PATH")
	}
	home, env := rootsHome(t)
	work := filepath.Join(home, "disk", "gotmp")
	file, err := Load(env, configWrite(t, `{"paths":{"build_work":"`+work+`","build_cache":"`+filepath.Join(home, "disk", "gocache")+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if report := Check(file, env, Probe{Only: toolRootNames, ConfiguredOnly: true}); report.Refusal() != nil {
		t.Fatalf("the check refused a creatable build_work: %v", report.Refusal())
	}
	if _, err := os.Stat(work); err == nil {
		t.Fatal("the check created build_work: the validator is read-only")
	}
	module := t.TempDir()
	for name, body := range map[string]string{"go.mod": "module example.test/m\n\ngo 1.21\n", "m.go": "package m\n\nfunc F() int { return 1 }\n"} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	build := func(prepare bool) ([]byte, error) {
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir = module
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOTELEMETRY=off")
		if prepare {
			if err := file.ToolRoots().ApplyTo(cmd); err != nil {
				return nil, err
			}
		} else {
			cmd.Env = file.ToolRoots().Apply(cmd.Env)
		}
		return cmd.CombinedOutput()
	}
	if out, err := build(false); err == nil {
		t.Fatalf("go built with a GOTMPDIR that does not exist (%s): the premise of the preparation step is gone", out)
	}
	if out, err := build(true); err != nil {
		t.Fatalf("go build with the prepared roots: %v\n%s", err, out)
	}
}

// ApplyTo leaves a command with nothing configured alone, so it keeps inheriting the environment, and
// puts the configured entries on top of a command's own environment when it has one.
func TestToolRootsApplyToACommand(t *testing.T) {
	cmd := exec.Command("true")
	if err := (ToolRoots{}).ApplyTo(cmd); err != nil || cmd.Env != nil {
		t.Errorf("nothing configured: Env = %q, err = %v", cmd.Env, err)
	}
	cmd = exec.Command("true")
	cmd.Env = []string{"A=1", "GOCACHE=/caller"}
	if err := (ToolRoots{BuildCache: "/cfg"}).ApplyTo(cmd); err != nil || !reflect.DeepEqual(cmd.Env, []string{"A=1", "GOCACHE=/cfg"}) {
		t.Errorf("own environment: Env = %q, err = %v", cmd.Env, err)
	}
	t.Setenv("CRW_TEST_MARK", "inherited")
	cmd = exec.Command("true")
	if err := (ToolRoots{ModuleCache: "/cfg/m"}).ApplyTo(cmd); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "CRW_TEST_MARK=inherited") || !slices.Contains(cmd.Env, "GOMODCACHE=/cfg/m") {
		t.Errorf("inherited environment lost or entry missing: %d entries", len(cmd.Env))
	}
}

// ConfigureCommand reads the configuration the way every product start does.
func TestConfigureCommandReadsTheConfiguration(t *testing.T) {
	home, env := rootsHome(t)
	work := filepath.Join(home, "w")
	configWriteAt(t, configDefaultPath(home), `{"paths":{"build_work":"`+work+`"}}`)
	cmd := exec.Command("true")
	if err := ConfigureCommand(cmd, env); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "GOTMPDIR="+work) {
		t.Errorf("Env lacks GOTMPDIR=%s", work)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Errorf("build_work not made: %v", err)
	}
	_, clean := rootsHome(t)
	cmd = exec.Command("true")
	if err := ConfigureCommand(cmd, clean); err != nil || cmd.Env != nil {
		t.Errorf("clean HOME: Env = %q, err = %v", cmd.Env, err)
	}
}

// A process with no home and no configuration location has no configuration (the default location
// cannot be named), while a location the caller did name that is unusable is still an error.
func TestLoadToolRootsWithoutAHomeIsNoConfiguration(t *testing.T) {
	none := rootsEnv("HOME", "")
	if tools, err := LoadToolRoots(none, ""); err != nil || tools != (ToolRoots{}) {
		t.Errorf("no home: %+v, %v", tools, err)
	}
	if root, err := ConfiguredRelayState(none); err != nil || root != "" {
		t.Errorf("no home: relay state %q, %v", root, err)
	}
	named := rootsEnv("HOME", "", "CRW_CONFIG", "relative.json")
	if _, err := LoadToolRoots(named, ""); err == nil {
		t.Error("a relative CRW_CONFIG was read as no configuration")
	}
}

// CRW-958: ConfiguredRelayState is the configured root only when the file names it; a split key or a
// root that is a file refuses.
func TestConfiguredRelayState(t *testing.T) {
	checkConfiguredRelayState(t)
}

// CRW-1568: the same judgements hold whatever TMPDIR is. The test runs below its own TMPDIR of at
// least 120 bytes, set before the home is made, so a relay root derived from TMPDIR would give a
// store socket over the bind limit (socket_path_too_long).
func TestConfiguredRelayState_isTMPDIRIndependent(t *testing.T) {
	base, err := tmploc.MkdirTemp("crw-long-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	long := filepath.Join(base, strings.Repeat("l", max(1, 120-len(base)-1)))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(long) < 120 {
		t.Fatalf("the long TMPDIR %q is %d bytes, want at least 120", long, len(long))
	}
	t.Setenv("TMPDIR", long)
	checkConfiguredRelayState(t)
}

// checkConfiguredRelayState is the body of the ConfiguredRelayState tests: a configured root, no
// file, a file that does not name relay_state, a split key and a root that is a file.
func checkConfiguredRelayState(t *testing.T) {
	t.Helper()
	home, env := rootsHome(t)
	// a store socket lies below the relay root, so the roots come from a socket directory (never below
	// TMPDIR); the configuration file stays under home
	root := filepath.Join(tmploctest.SocketDir(t, "rs"), "relay")
	path := configWriteAt(t, configDefaultPath(home), `{"paths":{"relay_state":"`+root+`"}}`)
	if got, err := ConfiguredRelayState(env); err != nil || got != root {
		t.Errorf("configured: %q, %v", got, err)
	}
	if got, err := ConfiguredRelayState(rootsEnvWith(env, "CRW_CONFIG", filepath.Join(home, "absent.json"))); err != nil || got != "" {
		t.Errorf("no file: %q, %v", got, err)
	}
	configWriteAt(t, path, `{"paths":{"tools_root":"`+filepath.Join(home, "t")+`"}}`)
	if got, err := ConfiguredRelayState(env); err != nil || got != "" {
		t.Errorf("relay_state not named: %q, %v", got, err)
	}
	configWriteAt(t, path, `{"paths":{"relay_state":"`+root+`","relay_db":"`+filepath.Join(home, "db")+`"}}`)
	if _, err := ConfiguredRelayState(env); err == nil || !strings.Contains(err.Error(), CheckStateSplit) {
		t.Errorf("split: %v", err)
	}
	plain := filepath.Join(tmploctest.SocketDir(t, "rs"), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	configWriteAt(t, path, `{"paths":{"relay_state":"`+plain+`"}}`)
	if _, err := ConfiguredRelayState(env); err == nil || !strings.Contains(err.Error(), CheckNotDirectory) {
		t.Errorf("file as root: %v", err)
	}
}

// rootsEnvWith is env with extra variables set.
func rootsEnvWith(base func(string) string, kv ...string) func(string) string {
	extra := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		extra[kv[i]] = kv[i+1]
	}
	return func(name string) string {
		if value, ok := extra[name]; ok {
			return value
		}
		return base(name)
	}
}
