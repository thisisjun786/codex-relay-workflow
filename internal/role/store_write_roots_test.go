package role

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1119 (pre-merge evaluation of 2be6b6f2): the store's writers follow the same raw root the readers do, refuse a write the read
// path refuses, treat a link whose target cannot be read as an unusable store, and report a committed reset whose answer cannot be
// read whole.

// rootEnv is an environment with the given HOME and CRW_HOME (empty is unset).
func rootEnv(home, crwHome string) func(string) (string, bool) {
	vars := map[string]string{"HOME": home, "CRW_HOME": crwHome}
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

// A root that climbs through a link names the directory the kernel resolves it to: the lock, the temporary file and the publication
// all use that one directory, and the lexically cleaned one is never created.
func TestStoreWritersFollowARootThatClimbsThroughALink(t *testing.T) {
	for _, viaHome := range []bool{false, true} {
		base := t.TempDir()
		deep := filepath.Join(base, "real", "deep")
		check(t, os.MkdirAll(deep, 0o700))
		check(t, os.Symlink(deep, filepath.Join(base, "link")))
		var env func(string) (string, bool)
		var physical string
		if viaHome {
			env = rootEnv(filepath.Join(base, "link")+"/..", "")
			physical = filepath.Join(base, "real", ".crw")
		} else {
			env = rootEnv(t.TempDir(), filepath.Join(base, "link")+"/../roles")
			physical = filepath.Join(base, "real", "roles")
		}
		if _, err := SetRole(env, Explorer, RolePatch{Effort: Some(EffortHigh)}); err != nil {
			t.Fatalf("viaHome=%v: first write = %v", viaHome, err)
		}
		if _, err := os.Stat(filepath.Join(physical, StoreFile)); err != nil {
			t.Fatalf("viaHome=%v: the store is not in the directory the kernel resolves: %v", viaHome, err)
		}
		lexical := filepath.Join(base, ".crw")
		if !viaHome {
			lexical = filepath.Join(base, "roles")
		}
		if _, err := os.Lstat(lexical); err == nil {
			t.Fatalf("viaHome=%v: the lexically cleaned directory %s was created", viaHome, lexical)
		}
		// An update of the existing store publishes beside it and keeps the other role.
		if _, err := SetRole(env, Reviewer, RolePatch{Effort: Some(EffortLow)}); err != nil {
			t.Fatalf("viaHome=%v: update = %v", viaHome, err)
		}
		cfg, err := ResetRole(env, Explorer)
		check(t, err)
		if cfg.Roles[Reviewer].Effort == nil {
			t.Fatalf("viaHome=%v: the other role was lost", viaHome)
		}
		if entries, _ := filepath.Glob(filepath.Join(physical, "*.tmp")); len(entries) != 0 {
			t.Fatalf("temporary files left: %v", entries)
		}
		if _, err := os.Lstat(lexical); err == nil {
			t.Fatalf("viaHome=%v: an update created %s", viaHome, lexical)
		}
	}
}

// A write that finds no store at the new place while an earlier reading's place holds one is refused as the read is, so the roles the
// earlier store holds are not displaced by a store that holds only the one role set.
func TestWritesAreRefusedWhileAnEarlierStoreIsUnresolved(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "crw")
	const kept = `{"roles":{"explorer":{"mode":"model","model":"m"},"reviewer":{"mode":"model","model":"r"}}}`
	writeStore(t, old, kept)
	env := rootEnv(t.TempDir(), old+" ")
	newPath := old + " /" + StoreFile
	var unusable *UnusableSettingsError
	if _, err := SetRole(env, Architect, RolePatch{Mode: Some(ModeModel), Model: Some("a")}); !errors.As(err, &unusable) || !strings.Contains(err.Error(), "cannot update subagent config") {
		t.Fatalf("SetRole = %v", err)
	}
	if _, err := ResetRole(env, Explorer); !errors.As(err, &unusable) {
		t.Fatalf("ResetRole = %v", err)
	}
	if _, err := UpdateSettings(env, []byte(`{"role":"architect","effort":"low"}`)); !errors.As(err, &unusable) {
		t.Fatalf("UpdateSettings = %v", err)
	}
	if _, err := os.Lstat(newPath); err == nil {
		t.Fatal("a refused write created the new store")
	}
	if got := readText(t, filepath.Join(old, StoreFile)); got != kept {
		t.Fatalf("the earlier store changed: %s", got)
	}
}

// A store that is a link whose target cannot be read is an unusable store, not an absent one: a read is refused with the typed error
// and a write leaves the link where it is.
func TestADanglingStoreLinkIsAnUnusableStore(t *testing.T) {
	env, dir := home(t)
	check(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, StoreFile)
	target := filepath.Join(t.TempDir(), "gone", StoreFile)
	check(t, os.Symlink(target, path))
	var unusable *UnusableSettingsError
	if _, err := ReadSettings(env); !errors.As(err, &unusable) || !strings.Contains(err.Error(), path) {
		t.Fatalf("ReadSettings over a dangling link = %v", err)
	}
	if _, err := ResolveSpawnConfig(env, Explorer); !errors.As(err, &unusable) {
		t.Fatalf("ResolveSpawnConfig over a dangling link = %v", err)
	}
	if _, err := SetRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}); err == nil {
		t.Fatal("a set replaced a dangling link")
	}
	if got, err := os.Readlink(path); err != nil || got != target {
		t.Fatalf("the link changed: %q %v", got, err)
	}
	// A directory that is simply not there is still the empty store.
	missing, _ := home(t)
	if _, err := ReadSettings(missing); err != nil {
		t.Fatalf("a missing store = %v", err)
	}
}

// A reset publishes the repair before it reads the settings back. When another role stays unusable the reset is still a success that
// says so: it is not reported as a failed operation after the store has changed.
func TestAResetThatCommitsReportsTheRolesStillUnusable(t *testing.T) {
	env, dir := home(t)
	const store = `{"roles":{"explorer":{"mode":"model","model":"m"},"reviewer":null}}`
	path := writeStore(t, dir, store)
	report, err := ResetRoleReport(env, Explorer)
	if err != nil {
		t.Fatalf("a committed reset = %v", err)
	}
	var unusable *UnusableSettingsError
	if !errors.As(report.Unusable, &unusable) || unusable.Role != Reviewer {
		t.Fatalf("remaining = %v", report.Unusable)
	}
	if report.Config.Roles[Explorer].Mode != ModeDefault {
		t.Fatalf("the reset role = %+v", report.Config.Roles[Explorer])
	}
	if got := readText(t, path); strings.Contains(got, `"explorer"`) || !strings.Contains(got, `"reviewer"`) {
		t.Fatalf("store after the reset: %s", got)
	}

	// The settings API answers with the settings and names the role that stays unusable.
	writeStore(t, dir, store)
	s, err := UpdateSettings(env, []byte(`{"role":"explorer","inherit":true}`))
	if err != nil {
		t.Fatalf("UpdateSettings(inherit) = %v", err)
	}
	if s.Overrides[Explorer] || len(s.Unusable) != 1 || s.Unusable[Reviewer] == "" {
		t.Fatalf("settings after the reset = %+v", s)
	}
	encoded, err := json.Marshal(s)
	check(t, err)
	if !strings.Contains(string(encoded), `"unusable":{"reviewer":`) {
		t.Fatalf("settings JSON = %s", encoded)
	}
	// Settings with nothing unusable keep the oracle's shape.
	writeStore(t, dir, `{"roles":{"explorer":{"mode":"model","model":"m"}}}`)
	s, err = UpdateSettings(env, []byte(`{"role":"explorer","inherit":true}`))
	check(t, err)
	if encoded, _ = json.Marshal(s); strings.Contains(string(encoded), "unusable") {
		t.Fatalf("settings JSON = %s", encoded)
	}

	// The command line succeeds and says what remains.
	writeStore(t, dir, store)
	result := RunHelper(ParseHelperArgs([]string{"reset", "explorer"}), env)
	if result.Code != 0 || !strings.Contains(result.Output, "reviewer") || !strings.Contains(result.Output, "explorer") {
		t.Fatalf("crw role helper reset = %d %q", result.Code, result.Output)
	}
}
