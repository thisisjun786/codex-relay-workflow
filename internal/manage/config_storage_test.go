package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
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

// storageEnvCalls reads the environment lines the fake recorded, one per call.
func storageEnvCalls(t *testing.T, path string) []map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		call := map[string]string{}
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("env record %q: %v", line, err)
		}
		calls = append(calls, call)
	}
	return calls
}

// CRW-958: a configured relay_state is not turned into a --state by the management tool. The relay's
// own state selection reads it (store.DiscoverStateDir, after --state and CODEX_SESSION_RELAY_STATE),
// so the tool asks the relay's doctor which store it selects and runs the relay on that answer, and a
// relay started directly with the same configuration and socket chooses the same bundle
// (cmd/crw TestRun_a_configured_relay_state_is_chosen_by_every_relay_start). manage.relay.state, which
// is passed as --state, stays the more specific setting, and CODEX_SESSION_RELAY_STATE is never
// overridden by a derived value.
func TestStorageRelayStateIsLeftToTheRelaysSelection(t *testing.T) {
	_, codexHome, _ := coreTempHome(t)
	short, err := tmploc.MkdirTemp("rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	root := filepath.Join(short, "relay")
	socket := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	for _, test := range []struct {
		name     string
		manage   map[string]any
		env      string
		wantCall []string
		doctor   bool
	}{
		{name: "relay_state only", wantCall: []string{"relay", "--state", "/doctor/chose", "--socket", socket, "status"}, doctor: true},
		{name: "environment variable set", env: "/env/store", wantCall: []string{"relay", "--state", "/doctor/chose", "--socket", socket, "status"}, doctor: true},
		{name: "manage.relay.state", manage: map[string]any{"relay": map[string]string{"state": "/srv/explicit"}},
			wantCall: []string{"relay", "--state", "/srv/explicit", "--socket", socket, "status"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CODEX_SESSION_RELAY_STATE", test.env)
			coreConfigAt(t, storageDoc(t, map[string]string{"relay_state": root}, test.manage))
			report, code, errOut := coreConfigReportOf(t)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			wantState := ""
			if test.manage != nil {
				wantState = "/srv/explicit"
			}
			if relay := coreBlock(t, report, "relay"); relay["state"] != wantState {
				t.Errorf("relay.state = %v, want %q: a configured relay_state is not a derived --state", relay["state"], wantState)
			}
			exe, record := coreFakeCRW(t, "/doctor/chose", 0)
			t.Setenv(coreFakeEnvRecordEnv, filepath.Join(t.TempDir(), "env.jsonl"))
			e := &Env{Getenv: os.Getenv, Executable: exe}
			state := coreLoad(e, "")
			if state.err != nil {
				t.Fatal(state.err)
			}
			if _, _, err := e.Relay(context.Background(), state.cfg, "status"); err != nil {
				t.Fatal(err)
			}
			want := [][]string{test.wantCall}
			if test.doctor {
				want = [][]string{{"relay", "--socket", socket, "doctor"}, test.wantCall}
			}
			coreCheckCalls(t, coreFakeCalls(t, record), want)
		})
	}
}

// CRW-958: a configuration file named with --config reaches the relay child as CRW_CONFIG, so the
// child's own state selection and tool roots read the file this tool read, and the configured tool
// roots are in its environment; a configured build_work that does not exist is made first. With
// nothing configured the child gets neither.
func TestStorageRelayChildCarriesTheConfigurationAndToolRoots(t *testing.T) {
	home, _, _ := coreTempHome(t)
	work := filepath.Join(home, "disk", "work")
	path := coreConfigWrite(t, filepath.Join(home, "flag", "config.json"), storageDoc(t,
		map[string]string{"build_cache": filepath.Join(home, "disk", "bc"), "module_cache": filepath.Join(home, "disk", "mc"), "build_work": work}, nil))
	exe, record := coreFakeCRW(t, "/doctor/chose", 0)
	envRecord := filepath.Join(t.TempDir(), "env.jsonl")
	t.Setenv(coreFakeEnvRecordEnv, envRecord)
	e := &Env{Getenv: os.Getenv, Executable: exe}
	state := coreLoad(e, path)
	if state.err != nil {
		t.Fatal(state.err)
	}
	if _, _, err := e.Relay(context.Background(), state.cfg, "status"); err != nil {
		t.Fatal(err)
	}
	_ = record
	calls := storageEnvCalls(t, envRecord)
	if len(calls) == 0 {
		t.Fatal("the fake saw no call")
	}
	for i, call := range calls {
		want := map[string]string{"CRW_CONFIG": path, "GOCACHE": filepath.Join(home, "disk", "bc"), "GOMODCACHE": filepath.Join(home, "disk", "mc"), "GOTMPDIR": work}
		if !reflect.DeepEqual(call, want) {
			t.Errorf("call %d environment = %v, want %v", i, call, want)
		}
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Errorf("build_work was not made before the child started: %v", err)
	}
	// nothing configured: the child inherits the environment and nothing is added
	coreConfigAt(t, `{}`)
	t.Setenv("GOCACHE", "")
	os.Unsetenv("GOCACHE")
	envRecord = filepath.Join(t.TempDir(), "env.jsonl")
	t.Setenv(coreFakeEnvRecordEnv, envRecord)
	e = &Env{Getenv: os.Getenv, Executable: exe}
	state = coreLoad(e, "")
	if _, _, err := e.Relay(context.Background(), state.cfg, "status"); err != nil {
		t.Fatal(err)
	}
	for i, call := range storageEnvCalls(t, envRecord) {
		if len(call) != 0 && !(len(call) == 1 && call["CRW_CONFIG"] == "") {
			t.Errorf("call %d environment = %v with nothing configured", i, call)
		}
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
	cmd, err := auditPkgGoListCommand(context.Background(), "/repo", "./...", crwconfig.ToolRoots{})
	if err != nil || cmd.Env != nil {
		t.Errorf("Env = %v with nothing configured, want the inherited environment", cmd.Env)
	}
	cmd, err = auditPkgGoListCommand(context.Background(), "/repo", "./...", crwconfig.ToolRoots{BuildCache: "/cfg/b", ModuleCache: "/cfg/m"})
	if err != nil {
		t.Fatal(err)
	}
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

// CRW-958: a configured build_work that does not exist yet is made before go list starts, because go
// makes its work directory directly below it and fails when it is missing.
func TestStorageGoListCommandMakesAConfiguredBuildWork(t *testing.T) {
	work := filepath.Join(t.TempDir(), "disk", "work")
	if _, err := auditPkgGoListCommand(context.Background(), "/repo", "./...", crwconfig.ToolRoots{BuildWork: work}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("build_work after the call: %v %v", info, err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auditPkgGoListCommand(context.Background(), "/repo", "./...", crwconfig.ToolRoots{BuildWork: plain}); err == nil {
		t.Error("a build_work that is a file was accepted")
	}
}
