package crwconfig

import (
	"path/filepath"
	"strings"
	"testing"
)

// rootsEnv is a getenv over the pairs given, so no test reads the host's own
// environment.
func rootsEnv(pairs ...string) func(string) string {
	values := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		values[pairs[i]] = pairs[i+1]
	}
	return func(key string) string { return values[key] }
}

// rootsHome is a fresh temporary home and a getenv that answers HOME with it.
func rootsHome(t *testing.T) (string, func(string) string) {
	t.Helper()
	home := t.TempDir()
	return home, rootsEnv("HOME", home)
}

// rootsDefaults is the table every root resolves to under one home with nothing
// configured and no XDG_* variable set.
func rootsDefaults(home string) map[string]string {
	share := filepath.Join(home, ".local", "share")
	cache := filepath.Join(home, ".cache")
	state := filepath.Join(home, ".local", "state")
	return map[string]string{
		RootTools:    filepath.Join(share, "crw", "tools"),
		RootCache:    filepath.Join(cache, "crw"),
		RootScratch:  filepath.Join(cache, "crw", "scratch"),
		RootWorktree: filepath.Join(share, "crw", "worktrees"),
		RootEvidence: filepath.Join(state, "crw", "evidence"),
		RootTemp:     filepath.Join("/tmp", "crw"),
		RootData:     filepath.Join(share, "crw", "data"),
		RootManage:   filepath.Join(state, "crw", "manage"),
	}
}

// rootsCheck compares the resolved roots against a table, expecting one source for
// all of them.
func rootsCheck(t *testing.T, roots map[string]Root, want map[string]string, source Source) {
	t.Helper()
	if len(roots) != len(want) {
		t.Fatalf("the roots number %d, want %d", len(roots), len(want))
	}
	for name, path := range want {
		if got := roots[name]; got.Path != path || got.Source != source {
			t.Errorf("%s = %+v, want %q from %q", name, got, path, source)
		}
	}
}

// C1: an empty temporary HOME with no XDG_* variable resolves every root to its
// documented default and names the source default.
func TestRootsDefaultsUnderATemporaryHome(t *testing.T) {
	home, env := rootsHome(t)
	roots, err := Resolve(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootsCheck(t, roots, rootsDefaults(home), SourceDefault)
}

// C2: each XDG_* variable and TMPDIR moves its own root, and the source becomes env.
func TestRootsFollowTheXdgVariables(t *testing.T) {
	home := t.TempDir()
	data, cache, state, temp := filepath.Join(home, "d"), filepath.Join(home, "c"), filepath.Join(home, "s"), filepath.Join(home, "t")
	env := rootsEnv("HOME", home, "XDG_DATA_HOME", data, "XDG_CACHE_HOME", cache, "XDG_STATE_HOME", state, "TMPDIR", temp)
	roots, err := Resolve(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootsCheck(t, roots, map[string]string{
		RootTools:    filepath.Join(data, "crw", "tools"),
		RootCache:    filepath.Join(cache, "crw"),
		RootScratch:  filepath.Join(cache, "crw", "scratch"),
		RootWorktree: filepath.Join(data, "crw", "worktrees"),
		RootEvidence: filepath.Join(state, "crw", "evidence"),
		RootTemp:     filepath.Join(temp, "crw"),
		RootData:     filepath.Join(data, "crw", "data"),
		RootManage:   filepath.Join(state, "crw", "manage"),
	}, SourceEnv)
}

// C2: an override input beats the environment for its own root and is named config,
// while the roots it does not name keep the environment's value.
func TestAnOverrideBeatsTheEnvironment(t *testing.T) {
	home := t.TempDir()
	share, override := filepath.Join(home, "share"), filepath.Join(home, "tools")
	env := rootsEnv("HOME", home, "XDG_DATA_HOME", share)
	roots, err := Resolve(env, map[string]string{RootTools: override})
	if err != nil {
		t.Fatal(err)
	}
	if got := roots[RootTools]; got.Path != override || got.Source != SourceConfig {
		t.Errorf("tools_root = %+v, want %q from config", got, override)
	}
	if got := roots[RootData]; got.Path != filepath.Join(share, "crw", "data") || got.Source != SourceEnv {
		t.Errorf("data_root = %+v, want the environment's value", got)
	}
}

// C3: overriding only cache_root moves scratch_root under the new cache_root, and
// names it config.
func TestACacheOverrideMovesTheScratchRoot(t *testing.T) {
	home, env := rootsHome(t)
	cache := filepath.Join(home, "elsewhere")
	roots, err := Resolve(env, map[string]string{RootCache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if got := roots[RootCache]; got.Path != cache || got.Source != SourceConfig {
		t.Errorf("cache_root = %+v, want %q from config", got, cache)
	}
	if got := roots[RootScratch]; got.Path != filepath.Join(cache, "scratch") || got.Source != SourceConfig {
		t.Errorf("scratch_root = %+v, want %q from config", got, filepath.Join(cache, "scratch"))
	}
	if got := roots[RootTools]; got.Source != SourceDefault {
		t.Errorf("tools_root = %+v, want an untouched default", got)
	}
}

// A named scratch_root of its own is not moved by a cache_root override.
func TestANamedScratchRootStandsApartFromTheCacheRoot(t *testing.T) {
	home, env := rootsHome(t)
	cache, scratch := filepath.Join(home, "elsewhere"), filepath.Join(home, "scratch")
	roots, err := Resolve(env, map[string]string{RootCache: cache, RootScratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if got := roots[RootScratch]; got.Path != scratch || got.Source != SourceConfig {
		t.Errorf("scratch_root = %+v, want %q from config", got, scratch)
	}
}

// A key this build does not know is left out rather than refused, so a configuration
// written for a later crw still resolves.
func TestAnUnknownRootNameIsIgnored(t *testing.T) {
	home, env := rootsHome(t)
	roots, err := Resolve(env, map[string]string{"no_such_root": filepath.Join(home, "x")})
	if err != nil {
		t.Fatal(err)
	}
	rootsCheck(t, roots, rootsDefaults(home), SourceDefault)
}

// C2: a relative path is refused with an error that names the input it came from,
// wherever it came from.
func TestARelativePathIsRefused(t *testing.T) {
	home, _ := rootsHome(t)
	for _, test := range []struct {
		name      string
		env       func(string) string
		overrides map[string]string
		names     string
	}{
		{"XDG_CACHE_HOME", rootsEnv("HOME", home, "XDG_CACHE_HOME", "relative/cache"), nil, "cache_root"},
		{"XDG_DATA_HOME", rootsEnv("HOME", home, "XDG_DATA_HOME", "relative/data"), nil, "tools_root"},
		{"XDG_STATE_HOME", rootsEnv("HOME", home, "XDG_STATE_HOME", "relative/state"), nil, "evidence_root"},
		{"TMPDIR", rootsEnv("HOME", home, "TMPDIR", "relative/tmp"), nil, "temp_root"},
		{"HOME", rootsEnv("HOME", "relative/home"), nil, "tools_root"},
		{"an override", rootsEnv("HOME", home), map[string]string{RootTools: "relative/tools"}, "tools_root"},
		{"an override of the cache", rootsEnv("HOME", home), map[string]string{RootCache: "relative/cache"}, "cache_root"},
	} {
		roots, err := Resolve(test.env, test.overrides)
		if err == nil {
			t.Errorf("%s: a relative path was accepted as %+v", test.name, roots)
			continue
		}
		if !strings.Contains(err.Error(), "absolute") || !strings.Contains(err.Error(), test.names) {
			t.Errorf("%s: error %q does not name %q and say the path is not absolute", test.name, err, test.names)
		}
	}
}
