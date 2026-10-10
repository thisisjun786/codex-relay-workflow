package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storageConfig points the process at a configuration file naming the given paths section, under a
// fresh home with no XDG state variable, and returns the home.
func storageConfig(t *testing.T, paths string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	file := filepath.Join(home, "crw.json")
	if err := os.WriteFile(file, []byte(`{"paths":{`+paths+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", file)
	return home
}

// shortRoot is a relay_state root short enough that the store socket below it stays under the bind
// limit, which the structural check enforces.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "relay")
}

// CRW-958: a relay_state the configuration names is the root of the per-scope store directories for
// every consumer of the common state selection, and the selection says it came from the configuration.
func TestStateSelection_a_configured_relay_state_is_the_root_of_the_scope_directories(t *testing.T) {
	home := storageConfig(t, "")
	root := shortRoot(t)
	t.Setenv("CRW_CONFIG", writeConfig(t, home, `"relay_state":"`+root+`"`))
	scope, err := SocketScope("/run/one.sock")
	if err != nil {
		t.Fatal(err)
	}
	for name, resolve := range map[string]func() (StateSelection, error){
		"DiscoverStateDir": func() (StateSelection, error) { return DiscoverStateDir("/run/one.sock") },
		"ResolveStateDir":  func() (StateSelection, error) { return ResolveStateDir("", "/run/one.sock") },
	} {
		got, err := resolve()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Path != filepath.Join(root, scope) || got.Source != "config" || got.SocketScope != scope || !strings.Contains(got.Detail, "paths.relay_state="+root) {
			t.Errorf("%s: selection %+v, want %s from the configuration", name, got, filepath.Join(root, scope))
		}
	}
	// the XDG base is not consulted once the configuration names the root
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg"))
	if got, err := DiscoverStateDir("/run/one.sock"); err != nil || got.Path != filepath.Join(root, scope) {
		t.Errorf("with XDG_STATE_HOME set: %+v %v", got, err)
	}
}

func writeConfig(t *testing.T, home, paths string) string {
	t.Helper()
	file := filepath.Join(home, "crw-"+t.Name()[strings.LastIndex(t.Name(), "/")+1:]+".json")
	if err := os.WriteFile(file, []byte(`{"paths":{`+paths+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// CRW-958: --state and CODEX_SESSION_RELAY_STATE come before the configured relay_state, the
// configured relay_state before the XDG and home defaults, and nothing configured changes nothing.
func TestStateSelection_precedence_with_a_configured_relay_state(t *testing.T) {
	home := storageConfig(t, "")
	root := shortRoot(t)
	t.Setenv("CRW_CONFIG", writeConfig(t, home, `"relay_state":"`+root+`"`))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg"))
	scope, _ := SocketScope("/run/x.sock")

	got, err := ResolveStateDir("", "/run/x.sock")
	if err != nil || got.Source != "config" || got.Path != filepath.Join(root, scope) {
		t.Fatalf("config: %+v %v", got, err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env-store"))
	got, err = ResolveStateDir("", "/run/x.sock")
	if err != nil || got.Source != "env" || got.Path != filepath.Join(home, "env-store") {
		t.Fatalf("environment variable: %+v %v", got, err)
	}
	got, err = ResolveStateDir(filepath.Join(home, "flag-store"), "/run/x.sock")
	if err != nil || got.Source != "flag" || got.Path != filepath.Join(home, "flag-store") {
		t.Fatalf("flag: %+v %v", got, err)
	}
	// no configuration file: the XDG base decides, exactly as before
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("CRW_CONFIG", filepath.Join(home, "absent.json"))
	got, err = ResolveStateDir("", "/run/x.sock")
	if err != nil || got.Source != "xdg" || got.Path != filepath.Join(home, "xdg", "codex-session-relay", scope) {
		t.Fatalf("no configuration: %+v %v", got, err)
	}
}

// CRW-958: a store already recorded for the socket under another scope directory of the configured
// root is adopted exactly as discovery adopts it below the XDG base: the configured root changes where
// discovery looks, not what it does.
func TestStateSelection_a_configured_root_keeps_the_legacy_scope_rule(t *testing.T) {
	home := storageConfig(t, "")
	root := shortRoot(t)
	t.Setenv("CRW_CONFIG", writeConfig(t, home, `"relay_state":"`+root+`"`))
	// the legacy spelling of the socket keeps the directory this socket was already using
	socket := filepath.Join(home, "d", "..", "x.sock")
	canonical, err := SocketScope(socket)
	if err != nil {
		t.Fatal(err)
	}
	legacy := socketHash(pathlibSpelling(socket))
	if legacy == canonical {
		t.Skip("the socket spelling is canonical already")
	}
	dir := filepath.Join(root, legacy)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "relay.sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverStateDir(socket)
	if err != nil || got.Path != dir || got.Source != "config" {
		t.Errorf("selection %+v %v, want the legacy directory %s kept", got, err, dir)
	}
}

// CRW-958: a paths key that points a part of the state apart refuses the selection, so no store is
// opened against a split; a malformed configuration is not silently read as none.
func TestStateSelection_a_split_or_broken_configuration_is_refused(t *testing.T) {
	home := storageConfig(t, "")
	root := shortRoot(t)
	t.Setenv("CRW_CONFIG", writeConfig(t, home, `"relay_state":"`+root+`","relay_wal":"`+filepath.Join(home, "wal")+`"`))
	if _, err := DiscoverStateDir("/run/x.sock"); err == nil || !strings.Contains(err.Error(), "state_split") {
		t.Errorf("a split was accepted: %v", err)
	}
	broken := filepath.Join(home, "broken.json")
	if err := os.WriteFile(broken, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", broken)
	if _, err := DiscoverStateDir("/run/x.sock"); err == nil {
		t.Error("a malformed configuration was read as none")
	}
}

// CRW-958: a process with no home and no configuration location has no configuration, and the
// selection reports the home error it always did rather than a configuration error.
func TestStateSelection_an_unnameable_configuration_location_is_no_configuration(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CRW_CONFIG", "")
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	got, err := DiscoverStateDir("/run/x.sock")
	if err != nil || got.Source != "xdg" {
		t.Errorf("selection %+v %v, want the XDG base to decide", got, err)
	}
}

// CRW-958 round 2: a split key refuses the selection on its own, with no relay_state beside it, and
// it refuses every source of the selection: the configuration is wrong whichever store is chosen,
// so --state and CODEX_SESSION_RELAY_STATE do not make the split acceptable. A relay_state that the
// selection does not use (--state or the environment variable chose the store) is not judged.
func TestStateSelection_a_split_key_alone_is_refused_for_every_source(t *testing.T) {
	home := storageConfig(t, "")
	for _, key := range []string{"relay_store", "relay_db", "relay_wal", "relay_sidecar", "relay_sidecars"} {
		t.Setenv("CRW_CONFIG", writeConfig(t, home, `"`+key+`":"`+filepath.Join(home, "other")+`"`))
		t.Setenv("CODEX_SESSION_RELAY_STATE", "")
		for name, resolve := range map[string]func() (StateSelection, error){
			"discover": func() (StateSelection, error) { return DiscoverStateDir("/run/x.sock") },
			"resolve":  func() (StateSelection, error) { return ResolveStateDir("", "/run/x.sock") },
			"flag":     func() (StateSelection, error) { return ResolveStateDir(filepath.Join(home, "flag"), "/run/x.sock") },
			"env": func() (StateSelection, error) {
				t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env"))
				defer t.Setenv("CODEX_SESSION_RELAY_STATE", "")
				return ResolveStateDir("", "/run/x.sock")
			},
		} {
			if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "state_split") {
				t.Errorf("%s alone, %s: %v, want state_split", key, name, err)
			}
		}
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", writeConfig(t, home, `"relay_state":"`+plain+`"`))
	if _, err := ResolveStateDir("", "/run/x.sock"); err == nil {
		t.Error("an unusable relay_state the selection uses was accepted")
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env"))
	if got, err := ResolveStateDir("", "/run/x.sock"); err != nil || got.Source != "env" {
		t.Errorf("env over an unused relay_state: %+v %v", got, err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	if got, err := ResolveStateDir(filepath.Join(home, "flag"), "/run/x.sock"); err != nil || got.Source != "flag" {
		t.Errorf("--state over an unused relay_state: %+v %v", got, err)
	}
}
