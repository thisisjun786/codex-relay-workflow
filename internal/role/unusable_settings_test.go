package role

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1119: a store that exists but cannot be read or understood, and a role whose routing fields are unusable, are refused with a
// typed error that names the repair, where the oracle read them as no overrides. The store's bytes are never touched by a read.

type unusableCase struct {
	name       string
	store      string
	dir        bool // a directory at the store path
	unreadable bool // a file the process may not open
	role       RoleName
	wholeStore bool // the whole store is unusable, so every role is
}

func unusableCases() []unusableCase {
	return []unusableCase{
		{name: "broken json", store: "{ not json ]", wholeStore: true},
		{name: "directory store", dir: true, wholeStore: true},
		{name: "unreadable store", store: `{"roles":{"explorer":{"mode":"model","model":"m"}}}`, unreadable: true, wholeStore: true},
		{name: "role not an object", store: `{"roles":{"explorer":"str"}}`, role: Explorer},
		{name: "role null", store: `{"roles":{"explorer":null}}`, role: Explorer},
		{name: "trim-empty primary model", store: `{"roles":{"explorer":{"mode":"model","model":"  "}}}`, role: Explorer},
		{name: "model mode without a model", store: `{"roles":{"explorer":{"mode":"model"}}}`, role: Explorer},
		{name: "unknown mode", store: `{"roles":{"explorer":{"mode":"turbo","model":"m"}}}`, role: Explorer},
		{name: "invalid effort", store: `{"roles":{"explorer":{"mode":"default","effort":"max"}}}`, role: Explorer},
		{name: "prompt of the wrong type", store: `{"roles":{"explorer":{"mode":"default","promptOverride":7}}}`, role: Explorer},
		{name: "fallback without a model", store: `{"roles":{"explorer":{"mode":"model","model":"m","fallback":{"model":" "}}}}`, role: Explorer},
		{name: "fallback of the wrong type", store: `{"roles":{"explorer":{"mode":"model","model":"m","fallback":"x"}}}`, role: Explorer},
	}
}

// unusableStore writes the case's store under a fresh home and returns the environment, the store path and a reader of the
// store's current state (bytes, or a marker for a directory).
func unusableStore(t *testing.T, c unusableCase) (func(string) (string, bool), string, func() string) {
	t.Helper()
	env, dir := home(t)
	path := filepath.Join(dir, StoreFile)
	check(t, os.MkdirAll(dir, 0o700))
	switch {
	case c.dir:
		check(t, os.Mkdir(path, 0o700))
	default:
		check(t, os.WriteFile(path, []byte(c.store), 0o600))
	}
	if c.unreadable {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode 0000 file")
		}
		check(t, os.Chmod(path, 0))
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	}
	state := func() string {
		info, err := os.Lstat(path)
		if err != nil {
			return "missing"
		}
		if info.IsDir() {
			return "dir"
		}
		if c.unreadable {
			return "mode " + info.Mode().Perm().String()
		}
		return string(must(os.ReadFile(path)))
	}
	return env, path, state
}

func TestUnusableSettingsAreRefusedAndTheStoreIsKept(t *testing.T) {
	for _, c := range unusableCases() {
		t.Run(c.name, func(t *testing.T) {
			env, path, state := unusableStore(t, c)
			before := state()
			var unusable *UnusableSettingsError
			if _, err := ReadSettings(env); !errors.As(err, &unusable) {
				t.Fatalf("ReadSettings = %v, want an UnusableSettingsError", err)
			} else if unusable.Path != path || !strings.Contains(err.Error(), path) {
				t.Fatalf("the error does not name the store: %v", err)
			}
			if _, err := ReadConfig(env); !errors.As(err, &unusable) {
				t.Fatalf("ReadConfig = %v", err)
			}
			if _, err := GetSettings(env, nil); !errors.As(err, &unusable) {
				t.Fatalf("GetSettings = %v", err)
			}
			if r := SettingsResponse(func() (Settings, error) { return GetSettings(env, nil) }); r.Status != 400 {
				t.Fatalf("settings response = %+v", r)
			}
			// The role whose routing is unusable, or every role of an unusable store, cannot be resolved.
			bad := c.role
			if c.wholeStore {
				bad = Reviewer
			}
			if _, err := ResolveSpawnConfig(env, bad); !errors.As(err, &unusable) {
				t.Fatalf("ResolveSpawnConfig(%s) = %v, want an UnusableSettingsError", bad, err)
			}
			if !strings.Contains(unusable.Error(), "reset") && !strings.Contains(unusable.Error(), "correct") {
				t.Fatalf("the error names no repair: %v", unusable)
			}
			// A managed start of that role is refused before it writes a dispatch.
			ws := t.TempDir()
			if _, err := RunDispatch(ws, map[string]any{"action": "start", "role": string(bad), "sessionId": "session-test", "dispatchId": "task-test"}, env); !errors.As(err, &unusable) {
				t.Fatalf("dispatch start = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")); err == nil {
				t.Fatal("a refused start wrote its dispatch")
			}
			// The CLI prints the refusal.
			if r := RunHelper(ParseHelperArgs([]string{"list"}), env); r.Code == 0 || !strings.Contains(r.Output, path) {
				t.Fatalf("helper list = %+v", r)
			}
			if after := state(); after != before {
				t.Fatalf("the store changed:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

// A role whose routing is unusable refuses only that role's routing: the other roles resolve from the same store.
func TestUnusableRoleLimitsTheRefusalToThatRole(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles":{"reviewer":{"mode":"model","model":"  "},"explorer":{"mode":"model","model":"vendor/explorer","effort":"high"}}}`)
	res, err := ResolveSpawnConfig(env, Explorer)
	check(t, err)
	if res.Model == nil || *res.Model != "vendor/explorer" || res.UsesMainModel {
		t.Fatalf("explorer = %+v", res)
	}
	if _, err := ResolveSpawnConfig(env, Reviewer); err == nil {
		t.Fatal("the unusable reviewer resolved")
	}
	r, err := RunDispatch(t.TempDir(), map[string]any{"action": "start", "role": "explorer", "sessionId": "session-test", "dispatchId": "task-test"}, env)
	check(t, err)
	if len(r.Attempts) != 1 || r.Attempts[0].Candidate.Model == nil || *r.Attempts[0].Candidate.Model != "vendor/explorer" {
		t.Fatalf("explorer start = %+v", r)
	}
}

// No store, and a valid store of default roles, still inherit; a model id outside any catalog is still a model.
func TestUsableSettingsStillInherit(t *testing.T) {
	env, dir := home(t)
	res, err := ResolveSpawnConfig(env, Executor)
	check(t, err)
	if !res.UsesMainModel || res.Model != nil {
		t.Fatalf("missing store = %+v", res)
	}
	writeStore(t, dir, `{"roles":{"executor":{"mode":"default","model":null,"effort":null,"promptOverride":null,"fallback":null}}}`)
	res, err = ResolveSpawnConfig(env, Executor)
	check(t, err)
	if !res.UsesMainModel || res.Model != nil {
		t.Fatalf("default mode = %+v", res)
	}
	writeStore(t, dir, `{"roles":{"executor":{"mode":"model","model":"someone/not-in-any-catalog-9"}}}`)
	res, err = ResolveSpawnConfig(env, Executor)
	check(t, err)
	if res.UsesMainModel || res.Model == nil || *res.Model != "someone/not-in-any-catalog-9" {
		t.Fatalf("arbitrary model = %+v", res)
	}
	if _, err := SetRole(env, Executor, RolePatch{Mode: Some(ModeModel), Model: Some("another/unlisted")}); err != nil {
		t.Fatalf("an arbitrary model id is refused: %v", err)
	}
}

// A primary model that is empty after a trim is refused on write, as a fallback model is, and nothing is written.
func TestTrimEmptyPrimaryModelIsRefusedOnWrite(t *testing.T) {
	env, dir := home(t)
	if _, err := SetRole(env, Explorer, RolePatch{Mode: Some(ModeModel), Model: Some(" \t ")}); err == nil || !strings.Contains(err.Error(), "non-empty model id") {
		t.Fatalf("SetRole = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, StoreFile)); err == nil {
		t.Fatal("a refused write created the store")
	}
	if _, err := UpdateSettings(env, []byte(`{"role":"explorer","mode":"model","model":"   "}`)); err == nil {
		t.Fatal("the settings API stored a blank model")
	}
}

// A write over an unusable role is refused, so a normalised copy never replaces what the operator wrote; a reset of the role is
// the repair and is allowed.
func TestUnusableRoleIsRepairedByAResetNotByASet(t *testing.T) {
	env, dir := home(t)
	const store = `{"roles":{"explorer":{"mode":"model","model":"  ","effort":"high","promptOverride":"keep"}}}`
	writeStore(t, dir, store)
	if _, err := SetRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}); err == nil {
		t.Fatal("a set over an unusable role was written")
	}
	if got := string(must(os.ReadFile(filepath.Join(dir, StoreFile)))); got != store {
		t.Fatalf("store changed: %s", got)
	}
	if _, err := ResetRole(env, Explorer); err != nil {
		t.Fatalf("reset = %v", err)
	}
	if _, err := ResolveSpawnConfig(env, Explorer); err != nil {
		t.Fatalf("after the reset = %v", err)
	}
}

// The store's home follows the root rule of internal/crwconfig: a set CRW_HOME is used as written and must be absolute, an empty
// one is unset, and the home is the host's (an empty HOME is the account home, not the working directory). A store left where an
// earlier reading looked (a trimmed CRW_HOME, or .crw below the working directory for an empty HOME) is reported, never read or
// moved.
func TestStorePathFollowsTheRootRule(t *testing.T) {
	base := t.TempDir()
	vars := map[string]string{"HOME": filepath.Join(base, "home")}
	env := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }

	vars["CRW_HOME"] = filepath.Join(base, "crw") + " "
	path, err := StorePath(env)
	check(t, err)
	if path != filepath.Join(base, "crw")+" /"+StoreFile {
		t.Fatalf("padded CRW_HOME = %q", path)
	}
	// The store the trimmed reading used is reported while the new place has none.
	old := filepath.Join(base, "crw", StoreFile)
	check(t, os.MkdirAll(filepath.Dir(old), 0o700))
	const kept = `{"roles":{"explorer":{"mode":"model","model":"m"}}}`
	check(t, os.WriteFile(old, []byte(kept), 0o600))
	var unusable *UnusableSettingsError
	if _, err := ResolveSpawnConfig(env, Explorer); !errors.As(err, &unusable) || !strings.Contains(err.Error(), old) {
		t.Fatalf("a store at the earlier place = %v", err)
	}
	if string(must(os.ReadFile(old))) != kept {
		t.Fatal("the earlier store changed")
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("the earlier store was moved")
	}

	vars["CRW_HOME"] = " " + filepath.Join(base, "crw")
	if _, err := StorePath(env); err == nil || !strings.Contains(err.Error(), "CRW_HOME") {
		t.Fatalf("a relative CRW_HOME = %v", err)
	}
	vars["CRW_HOME"] = "relative/crw"
	if _, err := StorePath(env); err == nil {
		t.Fatal("a relative CRW_HOME was used")
	}
	// A store path that cannot be resolved leaves the routing undecided: reads and resolutions are refused with the typed error.
	if _, err := ResolveSpawnConfig(env, Explorer); !errors.As(err, &unusable) || !strings.Contains(err.Error(), "CRW_HOME") {
		t.Fatalf("a relative CRW_HOME resolved: %v", err)
	}

	vars["CRW_HOME"] = ""
	path, err = StorePath(env)
	check(t, err)
	if path != filepath.Join(base, "home", ".crw", StoreFile) {
		t.Fatalf("empty CRW_HOME = %q", path)
	}
	// A HOME whose spelling climbs through a link keeps the directory the kernel resolves it to.
	check(t, os.MkdirAll(filepath.Join(base, "real", "deep"), 0o700))
	check(t, os.Symlink(filepath.Join(base, "real", "deep"), filepath.Join(base, "link")))
	vars["HOME"] = filepath.Join(base, "link") + "/.."
	path, err = StorePath(env)
	check(t, err)
	if path != filepath.Join(base, "link")+"/../.crw/"+StoreFile {
		t.Fatalf("linked HOME = %q", path)
	}

	// An empty HOME is the account home; a .crw below the working directory is reported, not read.
	vars["HOME"] = ""
	wd := t.TempDir()
	t.Chdir(wd)
	path, err = StorePath(env)
	check(t, err)
	if !filepath.IsAbs(path) || strings.HasPrefix(path, wd) {
		t.Fatalf("empty HOME = %q", path)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Skip("the account home holds a real store")
	}
	check(t, os.MkdirAll(filepath.Join(wd, ".crw"), 0o700))
	check(t, os.WriteFile(filepath.Join(wd, ".crw", StoreFile), []byte(kept), 0o600))
	if _, err := ReadSettings(env); !errors.As(err, &unusable) || !strings.Contains(err.Error(), filepath.Join(wd, ".crw", StoreFile)) {
		t.Fatalf("a store below the working directory = %v", err)
	}
}
