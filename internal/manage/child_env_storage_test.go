package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storageEnvLines is the storage-related environment a child printed with `env`.
func storageEnvLines(text string) map[string]string {
	seen := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch name {
		case "CRW_CONFIG", "GOCACHE", "GOMODCACHE", "GOTMPDIR", deliverPolicyEnv:
			seen[name] = value
		}
	}
	return seen
}

// storageFlagConfig loads a configuration named only by the parent's --config (CRW_CONFIG unset, the
// default location empty) that names all three tool roots, and returns it with the roots.
func storageFlagConfig(t *testing.T) (cfg *Config, path string, want map[string]string) {
	t.Helper()
	home, _, _ := coreTempHome(t)
	t.Setenv("CRW_CONFIG", "")
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOTMPDIR"} {
		t.Setenv(name, "/parent/"+name)
	}
	want = map[string]string{
		"GOCACHE":    filepath.Join(home, "disk", "bc"),
		"GOMODCACHE": filepath.Join(home, "disk", "mc"),
		"GOTMPDIR":   filepath.Join(home, "disk", "work"),
	}
	path = coreConfigWrite(t, filepath.Join(home, "flag", "config.json"), storageDoc(t,
		map[string]string{"build_cache": want["GOCACHE"], "module_cache": want["GOMODCACHE"], "build_work": want["GOTMPDIR"]}, nil))
	want["CRW_CONFIG"] = path
	state := coreLoad(&Env{Getenv: os.Getenv}, path)
	if state.err != nil {
		t.Fatal(state.err)
	}
	return state.cfg, path, want
}

func storageCheckEnv(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s: %s = %q, want %q (saw %v)", what, name, got[name], value, got)
		}
	}
}

// CRW-958 round 2: every managed child the management tool starts, not only the relay, gets the
// configuration file the parent was started with (a --config file as CRW_CONFIG) and the configured
// tool roots, and a configured build_work is made before it starts: the audit and pre-merge grader,
// the bridge the deliveries dial, and the commands the upgrade runs.
func TestStorageEveryManagedChildCarriesTheConfigurationAndToolRoots(t *testing.T) {
	t.Run("grader", func(t *testing.T) {
		cfg, _, want := storageFlagConfig(t)
		bundle := t.TempDir()
		section := auditSection{Grader: []string{"sh", "-c", "env > {bundle}/child-env.txt"}, GraderTimeoutSeconds: 60}
		if status := auditRunGrader(context.Background(), cfg, section, bundle, "prompt", &auditLog{}); status != "" {
			t.Fatalf("grader status %q", status)
		}
		raw, err := os.ReadFile(filepath.Join(bundle, "child-env.txt"))
		if err != nil {
			t.Fatal(err)
		}
		storageCheckEnv(t, "grader", storageEnvLines(string(raw)), want)
		if info, err := os.Stat(want["GOTMPDIR"]); err != nil || !info.IsDir() {
			t.Errorf("build_work was not made before the grader started: %v", err)
		}
	})
	t.Run("upgrade command", func(t *testing.T) {
		cfg, _, want := storageFlagConfig(t)
		out, _, code, err := upgradeRunCommand(context.Background(), cfg, "env")
		if err != nil || code != 0 {
			t.Fatalf("env: %d %v", code, err)
		}
		storageCheckEnv(t, "upgrade", storageEnvLines(out), want)
	})
	t.Run("bridge", func(t *testing.T) {
		cfg, _, want := storageFlagConfig(t)
		cfg.Bridge.ExecutionPolicy = "/policy.json"
		cmd, err := deliverBridgeCommand(&Env{Executable: "/bin/crw"}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		got := storageEnvLines(strings.Join(cmd.Env, "\n"))
		want[deliverPolicyEnv] = "/policy.json"
		storageCheckEnv(t, "bridge", got, want)
		if cmd.Path != "/bin/crw" || cmd.Args[len(cmd.Args)-1] != "bridge" {
			t.Errorf("bridge command %v", cmd.Args)
		}
	})
	t.Run("nothing configured", func(t *testing.T) {
		coreTempHome(t)
		coreConfigAt(t, `{}`)
		t.Setenv("GOCACHE", "/parent/GOCACHE")
		state := coreLoad(&Env{Getenv: os.Getenv}, "")
		if state.err != nil {
			t.Fatal(state.err)
		}
		out, _, _, err := upgradeRunCommand(context.Background(), state.cfg, "env")
		if err != nil {
			t.Fatal(err)
		}
		got := storageEnvLines(out)
		if got["GOCACHE"] != "/parent/GOCACHE" || got["CRW_CONFIG"] != "" || got["GOTMPDIR"] != "" {
			t.Errorf("with nothing configured the child environment changed: %v", got)
		}
	})
}

// CRW-958 round 2: the management tool judges only the relay_state the relay's selection will use.
// With manage.relay.state (passed as --state) or CODEX_SESSION_RELAY_STATE choosing the store, an
// unusable paths.relay_state does not stop the tool, as it does not stop a relay started directly
// (cmd/crw TestRun_the_relay_refuses_a_split_key_alone_and_ignores_an_unused_relay_state); a split
// key still refuses the configuration whatever chooses the store.
func TestStorageAnUnusedRelayStateIsNotRefused(t *testing.T) {
	home, _, _ := coreTempHome(t)
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env-store"))
	coreConfigAt(t, storageDoc(t, map[string]string{"relay_state": plain}, nil))
	if _, code, errOut := coreConfigReportOf(t); code != 0 {
		t.Errorf("env chooses the store: exit %d %s", code, errOut)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	coreConfigAt(t, storageDoc(t, map[string]string{"relay_state": plain}, map[string]any{"relay": map[string]string{"state": filepath.Join(home, "explicit")}}))
	if _, code, errOut := coreConfigReportOf(t); code != 0 {
		t.Errorf("manage.relay.state chooses the store: exit %d %s", code, errOut)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "env-store"))
	coreConfigAt(t, storageDoc(t, map[string]string{"relay_wal": filepath.Join(home, "wal")}, nil))
	if _, code, errOut := coreConfigReportOf(t); code != usageExit || !strings.Contains(errOut, "state_split") {
		t.Errorf("a split key alone with the environment variable set: exit %d %s", code, errOut)
	}
}
