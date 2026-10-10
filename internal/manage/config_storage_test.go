package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// storageDoc is a configuration document with a paths section and, optionally, a manage object.
func storageDoc(t *testing.T, paths map[string]string, manage map[string]any) string {
	t.Helper()
	doc := map[string]any{"schema": "crw-config/1", "paths": paths}
	if manage != nil {
		doc["manage"] = manage
	}
	return coreConfigMarshal(t, doc)
}

// CRW-958: a configured relay_state moves the relay's store, WAL and sidecars as one bundle: the
// management tool runs the relay on the scope directory below it that the socket names. Nothing
// configured leaves the state to the relay's own discovery, as before.
func TestStorageRelayStateConfiguredSelectsTheScopeDirectory(t *testing.T) {
	home, codexHome, _ := coreTempHome(t)
	// a short root: the store socket below it must stay under the bind limit
	short, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	root := filepath.Join(short, "relay")
	_ = home
	coreConfigAt(t, storageDoc(t, map[string]string{"relay_state": root}, nil))
	socket := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	scope, err := store.SocketScope(socket)
	if err != nil {
		t.Fatal(err)
	}
	report, code, errOut := coreConfigReportOf(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	relay := coreBlock(t, report, "relay")
	if want := filepath.Join(root, scope); relay["state"] != want {
		t.Errorf("relay.state = %v, want %q", relay["state"], want)
	}
	if sources := coreBlock(t, report, "sources"); sources["relay.state"] != coreSourceConfig {
		t.Errorf("sources = %v, want relay.state from config", sources)
	}
	// manage.relay.state is the more specific setting and wins
	coreConfigAt(t, storageDoc(t, map[string]string{"relay_state": root}, map[string]any{"relay": map[string]string{"state": "/srv/explicit"}}))
	report, _, _ = coreConfigReportOf(t)
	if relay := coreBlock(t, report, "relay"); relay["state"] != "/srv/explicit" {
		t.Errorf("manage.relay.state did not win: %v", relay)
	}
	// nothing configured: the relay chooses
	coreConfigAt(t, `{}`)
	report, _, _ = coreConfigReportOf(t)
	if relay := coreBlock(t, report, "relay"); relay["state"] != "" {
		t.Errorf("relay.state = %v with nothing configured", relay["state"])
	}
	if sources := coreBlock(t, report, "sources"); sources["relay.state"] != nil {
		t.Errorf("sources names relay.state with nothing configured: %v", sources)
	}
}

// CRW-958: a relay_state pointed apart is refused by the management tool too.
func TestStorageRelayStateSplitIsRefused(t *testing.T) {
	home, _, _ := coreTempHome(t)
	coreConfigAt(t, storageDoc(t, map[string]string{"relay_state": filepath.Join(home, "rs"), "relay_wal": filepath.Join(home, "wal")}, nil))
	_, code, errOut := coreConfigReportOf(t)
	if code != usageExit || !strings.Contains(errOut, crwconfig.CheckStateSplit) {
		t.Errorf("exit %d stderr %q, want the split refused", code, errOut)
	}
}

// CRW-958: a tool root that is not a directory stops the command that would start go against it.
func TestStorageToolRootThatIsAFileIsRefused(t *testing.T) {
	home, _, _ := coreTempHome(t)
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	coreConfigAt(t, storageDoc(t, map[string]string{"build_cache": plain}, nil))
	_, code, errOut := coreConfigReportOf(t)
	if code != usageExit || !strings.Contains(errOut, "build_cache") || !strings.Contains(errOut, crwconfig.CheckNotDirectory) {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

// CRW-958: the pump log goes to a configured log_root and stays in the state directory otherwise.
func TestStoragePumpLogFollowsTheConfiguredLogRoot(t *testing.T) {
	home, _, _ := coreTempHome(t)
	logs := filepath.Join(home, "logs-disk", "crw")
	coreConfigAt(t, storageDoc(t, map[string]string{"log_root": logs}, nil))
	state := coreLoad(&Env{Getenv: os.Getenv}, "")
	if state.err != nil {
		t.Fatal(state.err)
	}
	pumpLog(state.cfg, "configured line")
	if raw, err := os.ReadFile(filepath.Join(logs, pumpLogFile)); err != nil || !strings.Contains(string(raw), "configured line") {
		t.Errorf("the pump log is not in log_root: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(state.cfg.StateDir, pumpLogFile)); err == nil {
		t.Error("the pump log was also written to the state directory")
	}
	coreConfigAt(t, `{}`)
	state = coreLoad(&Env{Getenv: os.Getenv}, "")
	pumpLog(&Config{StateDir: t.TempDir()}, "x") // best effort and unconfigured: unchanged path
	if state.cfg.logDir != "" {
		t.Errorf("logDir = %q with log_root unconfigured", state.cfg.logDir)
	}
}

// CRW-958: go list for an audit package expansion starts with the configured tool roots, and with
// no tool root configured it adds nothing to the caller's environment.
func TestStorageGoListCommandCarriesTheConfiguredRoots(t *testing.T) {
	t.Setenv("GOMODCACHE", "/caller/mod")
	cmd := auditPkgGoListCommand(context.Background(), "/repo", "./...", crwconfig.ToolRoots{})
	if cmd.Env != nil {
		t.Errorf("Env = %v with nothing configured, want the inherited environment", cmd.Env)
	}
	cmd = auditPkgGoListCommand(context.Background(), "/repo", "./...", crwconfig.ToolRoots{BuildCache: "/cfg/b", ModuleCache: "/cfg/m"})
	var got []string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GOCACHE=") || strings.HasPrefix(kv, "GOMODCACHE=") {
			got = append(got, kv)
		}
	}
	if len(got) != 2 || got[0] != "GOCACHE=/cfg/b" || got[1] != "GOMODCACHE=/cfg/m" {
		t.Errorf("tool entries = %v", got)
	}
	if cmd.Dir != "/repo" || cmd.Args[len(cmd.Args)-1] != "./..." {
		t.Errorf("command = %v in %q", cmd.Args, cmd.Dir)
	}
}
