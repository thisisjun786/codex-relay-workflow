package crwconfig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shortDir is a directory short enough that a store socket below it stays under the bind limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// statusOf is the status the report gives one root.
func statusOf(r Report, name string) RootStatus {
	for _, s := range r.Roots {
		if s.Name == name {
			return s
		}
	}
	return RootStatus{}
}

// CRW-958 round 2: a split key refuses the relay's state selection on its own; it does not need a
// relay_state beside it. Each key alone is refused with state_split.
func TestConfiguredRelayStateRefusesASplitKeyAlone(t *testing.T) {
	home, env := rootsHome(t)
	path := configDefaultPath(home)
	for _, key := range splitKeys {
		configWriteAt(t, path, `{"paths":{"`+key+`":"`+filepath.Join(home, "other", key)+`"}}`)
		if _, err := ConfiguredRelayState(env); err == nil || !strings.Contains(err.Error(), CheckStateSplit) {
			t.Errorf("%s alone: ConfiguredRelayState error %v, want %s", key, err, CheckStateSplit)
		}
		if err := RelayStateSplit(env); err == nil || !strings.Contains(err.Error(), CheckStateSplit) {
			t.Errorf("%s alone: RelayStateSplit error %v, want %s", key, err, CheckStateSplit)
		}
	}
	// no split key: nothing to refuse, and a relay_state that is unusable is not RelayStateSplit's
	// business (it is judged only where the selection uses it)
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	configWriteAt(t, path, `{"paths":{"relay_state":"`+plain+`"}}`)
	if err := RelayStateSplit(env); err != nil {
		t.Errorf("no split key: %v", err)
	}
}

// CRW-958 round 2: the pump log already written to the management state directory is the log
// placement in use; a configured log_root elsewhere is drift naming both paths.
func TestCheckReportsDriftOfTheExistingPumpLog(t *testing.T) {
	home, env := rootsHome(t)
	manage := filepath.Join(home, ".local", "state", "crw", "manage")
	if err := os.MkdirAll(manage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manage, "pump.log"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(home, "logs-disk")
	file, err := Load(env, configWrite(t, `{"paths":{"log_root":"`+logs+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	report := Check(file, env, roomy())
	f := mustFind(t, report, CheckDrift, RootLog, LevelWarning)
	if !strings.Contains(f.Message, filepath.Join(manage, "pump.log")) || !strings.Contains(f.Message, logs) {
		t.Errorf("drift message %q does not name the pump log in use and the configured log_root", f.Message)
	}
	if status := statusOf(report, RootLog); !slices.Contains(status.InUse, filepath.Join(manage, "pump.log")) {
		t.Errorf("in use = %v", status.InUse)
	}
	// manage.state_dir moves the management state, and the pump log with it
	moved := filepath.Join(home, "mstate")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, "pump.log"), []byte("y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err = Load(env, configWrite(t, `{"paths":{"log_root":"`+logs+`"},"manage":{"state_dir":"`+moved+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := mustFind(t, Check(file, env, roomy()), CheckDrift, RootLog, LevelWarning); !strings.Contains(f.Message, filepath.Join(moved, "pump.log")) {
		t.Errorf("drift message %q does not name the pump log below manage.state_dir", f.Message)
	}
	// the configuration agreeing with the place the log is in is no drift
	file, err = Load(env, configWrite(t, `{"paths":{"log_root":"`+manage+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := find(Check(file, env, roomy()), CheckDrift, RootLog); f != nil {
		t.Errorf("agreeing log_root reported as drift: %+v", f)
	}
}

// CRW-958 round 2: a relay store a registered service uses outside the default location (a scope
// registry claim, the installer's unit) is the relay placement in use, even when the XDG default is
// empty; a configured relay_state elsewhere is drift naming both, and a relay daemon log kept in such
// a store is the log placement in use.
func TestCheckReportsDriftOfARegisteredRelayStore(t *testing.T) {
	home, env := rootsHome(t)
	other := filepath.Join(home, "srv", "relay-store")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "daemon.log"), []byte("started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(home, ".codex-session-relay", "scopes")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "abc.json"), []byte(`{"stateDir":"`+other+`","socketPath":"/run/x.sock"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(shortDir(t), "relay")
	logs := filepath.Join(home, "logs")
	file, err := Load(env, configWrite(t, `{"paths":{"relay_state":"`+root+`","log_root":"`+logs+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	report := Check(file, env, roomy())
	f := mustFind(t, report, CheckDrift, RootRelayState, LevelWarning)
	if !strings.Contains(f.Message, other) || !strings.Contains(f.Message, root) || !strings.Contains(f.Message, filepath.Join(registry, "abc.json")) {
		t.Errorf("drift message %q does not name the registered store, its record and the configured root", f.Message)
	}
	if status := statusOf(report, RootRelayState); !slices.Contains(status.InUse, other) {
		t.Errorf("relay_state in use = %v", status.InUse)
	}
	if f := mustFind(t, report, CheckDrift, RootLog, LevelWarning); !strings.Contains(f.Message, filepath.Join(other, "daemon.log")) {
		t.Errorf("log drift %q does not name the relay daemon log in use", f.Message)
	}

	// the installer's unit names its store with --state; the registry alone is not the only record
	if err := os.RemoveAll(registry); err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := "[Service]\nExecStart=/opt/crw/current/bin/codex-session-relay --state " + other + " --socket /run/x.sock service start\n"
	if err := os.WriteFile(filepath.Join(unitDir, "crw-relay.service"), []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	f = mustFind(t, Check(file, env, roomy()), CheckDrift, RootRelayState, LevelWarning)
	if !strings.Contains(f.Message, other) || !strings.Contains(f.Message, "crw-relay.service") {
		t.Errorf("drift message %q does not name the unit's store", f.Message)
	}

	// a registered store below the configured root is no drift
	inside := filepath.Join(root, "0123456789abcdef")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "crw-relay.service"), []byte(strings.ReplaceAll(unit, other, inside)), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := find(Check(file, env, roomy()), CheckDrift, RootRelayState); f != nil {
		t.Errorf("a store below the configured root is drift: %+v", f)
	}
}

// CRW-958 round 2: the relay's daemon log is a log, not part of the durable state bundle: a
// daemon.log kept in another directory is not a split, while a WAL kept apart still is.
func TestCheckTheRelayDaemonLogIsNotAStateSidecar(t *testing.T) {
	home, env := rootsHome(t)
	root := filepath.Join(shortDir(t), "relay")
	store := filepath.Join(root, "0123456789abcdef")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "relay.sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "daemon.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "daemon.log"), filepath.Join(store, "daemon.log")); err != nil {
		t.Fatal(err)
	}
	file, err := Load(env, configWrite(t, `{"paths":{"relay_state":"`+root+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := find(Check(file, env, roomy()), CheckStateSplit, RootRelayState); f != nil {
		t.Errorf("a daemon log kept apart is a state split: %+v", f)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "relay.sqlite3-wal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "relay.sqlite3-wal"), filepath.Join(store, "relay.sqlite3-wal")); err != nil {
		t.Fatal(err)
	}
	mustFind(t, Check(file, env, roomy()), CheckStateSplit, RootRelayState, LevelError)
}

// CRW-958 round 2: the relay daemon log of one store sits below a configured log_root in a directory
// of its own, named from the store's path, so two stores never share a log; with log_root not
// configured there is no file and the log stays in the store directory.
func TestRelayLogFile(t *testing.T) {
	home, env := rootsHome(t)
	if got, err := RelayLogFile(env, "/srv/a/0123456789abcdef"); err != nil || got != "" {
		t.Errorf("unconfigured: %q %v", got, err)
	}
	logs := filepath.Join(home, "logs")
	path := configWriteAt(t, configDefaultPath(home), `{"paths":{"log_root":"`+logs+`"}}`)
	a, err := RelayLogFile(env, "/srv/a/0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RelayLogFile(env, "/srv/b/0123456789abcdef")
	again, _ := RelayLogFile(env, "/srv/a/0123456789abcdef")
	if !strings.HasPrefix(a, logs+"/relay/") || filepath.Base(a) != "daemon.log" || a == b || a != again {
		t.Errorf("log files a=%q b=%q again=%q", a, b, again)
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	configWriteAt(t, path, `{"paths":{"log_root":"`+plain+`"}}`)
	if _, err := RelayLogFile(env, "/srv/a/x"); err == nil || !strings.Contains(err.Error(), CheckNotDirectory) {
		t.Errorf("a log_root that is a file: %v", err)
	}
}

// CRW-958 round 2: the preflight a command that changes the host runs reports every finding, refuses
// only a structural fault of a root the configuration names, and is clean on a clean home.
func TestPreflight(t *testing.T) {
	// the probe answers from a roomy ordinary file system, so the verdict does not depend on the
	// disk or on the length of the test's temporary home
	saved := runProbe
	runProbe = roomy()
	t.Cleanup(func() { runProbe = saved })
	home, env := rootsHome(t)
	report, err := Preflight(env)
	if err != nil || len(report.Faults()) != 0 {
		t.Errorf("clean home: %v %+v", err, report.Faults())
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := configWriteAt(t, configDefaultPath(home), `{"paths":{"data_root":"`+plain+`"}}`)
	report, err = Preflight(env)
	if err == nil || !strings.Contains(err.Error(), CheckNotDirectory) || find(report, CheckNotDirectory, RootData) == nil {
		t.Errorf("a configured root that is a file: %v %+v", err, report.Findings)
	}
	configWriteAt(t, path, `{"paths":{"relay_wal":"/x"}}`)
	if _, err := Preflight(env); err == nil || !strings.Contains(err.Error(), CheckStateSplit) {
		t.Errorf("split: %v", err)
	}
	configWriteAt(t, path, `{not json`)
	if _, err := Preflight(env); err == nil {
		t.Error("a malformed configuration passed the preflight")
	}
	// a warning alone never refuses
	configWriteAt(t, path, `{"paths":{"data_root":"`+filepath.Join(home, "d")+`","tools_root":"`+filepath.Join(home, "d")+`"}}`)
	report, err = Preflight(env)
	if err != nil || find(report, CheckSharedPath, RootData) == nil && find(report, CheckSharedPath, RootTools) == nil {
		t.Errorf("shared path: %v %+v", err, report.Findings)
	}
	if object := report.Ordered(); len(object) == 0 || object[0].Key != "config" {
		t.Errorf("ordered report = %v", object)
	}
}
