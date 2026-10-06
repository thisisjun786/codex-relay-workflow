package manage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// coreRunManage runs one crw manage command line and returns its exit status and streams.
func coreRunManage(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// coreConfigDocument is a crw configuration document whose top-level manage object is the
// value given, with the schema the file declares.
func coreConfigDocument(t *testing.T, manage any) string {
	t.Helper()
	return coreConfigMarshal(t, map[string]any{"schema": "crw-config/1", "manage": manage})
}

// coreConfigMarshal encodes a configuration document.
func coreConfigMarshal(t *testing.T, doc map[string]any) string {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// coreConfigAt writes a crw configuration document at the default location below a fresh
// XDG_CONFIG_HOME and points the variable at it, so a test exercises the location chain
// crwconfig owns without naming the file itself.
func coreConfigAt(t *testing.T, document string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("CRW_CONFIG", "")
	return coreConfigWrite(t, filepath.Join(dir, "crw", "config.json"), document)
}

// coreConfigWrite writes a document to a named path and returns it.
func coreConfigWrite(t *testing.T, path, document string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// coreConfigReportOf runs crw manage config and returns the decoded report, the exit
// status and the error text.
func coreConfigReportOf(t *testing.T, args ...string) (map[string]any, int, string) {
	t.Helper()
	code, out, errOut := coreRunManage(t, append([]string{"config"}, args...)...)
	report := map[string]any{}
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("the config output is not JSON: %v\n%s", err, out)
		}
	}
	return report, code, errOut
}

// coreBlock reads one nested object of the report.
func coreBlock(t *testing.T, report map[string]any, name string) map[string]any {
	t.Helper()
	block, ok := report[name].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", name, report[name])
	}
	return block
}

// coreConfigSourceOf reads the report config member: the file the configuration came from
// and where that path came from.
func coreConfigSourceOf(t *testing.T, report map[string]any) map[string]any {
	t.Helper()
	source := coreBlock(t, report, "config")
	if source["path"] == nil || source["source"] == nil {
		t.Fatalf("the report names no configuration path or source: %v", source)
	}
	return source
}

// coreTestEnv is an Env over the process environment, for a test that drives a helper
// directly rather than through Run.
func coreTestEnv() *Env {
	return &Env{Stdin: strings.NewReader(""), Stdout: &strings.Builder{}, Stderr: &strings.Builder{}, Getenv: os.Getenv}
}

// coreFailWriter refuses every write, so a command that ignores a write failure reads as
// success while its output never left.
type coreFailWriter struct{}

func (coreFailWriter) Write([]byte) (int, error) { return 0, errors.New("the output is closed") }

// C1: the manage object of the crw configuration file is the manage configuration: its
// common fields land in Config, and Section reads a key inside manage.
func TestConfigReadsTheManageSectionOfTheCrwConfigFile(t *testing.T) {
	coreTempHome(t)
	path := coreConfigAt(t, coreConfigDocument(t, map[string]any{
		"management_thread": "01abc",
		"parents":           map[string]string{"lane": "01parent"},
		"repository":        "/srv/checkout",
		"relay":             map[string]string{"state": "/srv/relay-state", "socket": "/srv/app.sock"},
		"bridge":            map[string]string{"binary": "/srv/crw", "execution_policy": "/srv/policy.json"},
		"state_dir":         "/srv/manage",
		"settings": map[string]any{
			"management": map[string]string{"model": "m1", "reasoning_effort": "low"},
			"parent":     map[string]string{"model": "m2", "reasoning_effort": "high"},
		},
		"child_check": map[string]any{"disabled_servers": []string{"alpha", "beta"}},
	}))

	report, code, errOut := coreConfigReportOf(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if report["management_thread"] != "01abc" || report["repository"] != "/srv/checkout" || report["state_dir"] != "/srv/manage" {
		t.Errorf("the manage section did not reach the report: %v", report)
	}
	if parents := coreBlock(t, report, "parents"); parents["lane"] != "01parent" {
		t.Errorf("parents = %v", parents)
	}
	relay := coreBlock(t, report, "relay")
	if relay["state"] != "/srv/relay-state" || relay["socket"] != "/srv/app.sock" {
		t.Errorf("relay = %v", relay)
	}
	bridge := coreBlock(t, report, "bridge")
	if bridge["binary"] != "/srv/crw" || bridge["execution_policy"] != "/srv/policy.json" {
		t.Errorf("bridge = %v", bridge)
	}
	settings := coreBlock(t, report, "settings")
	if management := coreBlock(t, settings, "management"); management["model"] != "m1" || management["reasoning_effort"] != "low" {
		t.Errorf("settings.management = %v", management)
	}
	if parent := coreBlock(t, settings, "parent"); parent["model"] != "m2" {
		t.Errorf("settings.parent = %v", parent)
	}
	// Each value the report names says where it came from: the manage object carried
	// these three, so they are the file's, not the install layout's.
	sources := coreBlock(t, report, "sources")
	for _, key := range []string{"relay.socket", "state_dir", "bridge.binary"} {
		if sources[key] != coreSourceConfig {
			t.Errorf("sources[%q] = %v, want %q", key, sources[key], coreSourceConfig)
		}
	}
	if source := coreConfigSourceOf(t, report); source["path"] != path || source["source"] != "env" {
		t.Errorf("config = %v, want path %q from env", source, path)
	}

	// A key inside manage that this package does not name is read with Section.
	cfg, err := coreLoadFile(coreTestEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	section := map[string]any{}
	if err := cfg.Section("child_check", &section); err != nil {
		t.Fatal(err)
	}
	servers, _ := section["disabled_servers"].([]any)
	if len(servers) != 2 || servers[0] != "alpha" || servers[1] != "beta" {
		t.Errorf("Section read %v", section)
	}
}

// C2: with no configuration file the defaults stand, and the state directory is the
// manage_state root crwconfig resolves.
func TestConfigDefaultsWithoutAFileAndTheManageStateRoot(t *testing.T) {
	home, codexHome, _ := coreTempHome(t)
	report, code, errOut := coreConfigReportOf(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got, want := report["state_dir"], filepath.Join(home, ".local", "state", "crw", "manage"); got != want {
		t.Errorf("state_dir = %v, want %q", got, want)
	}
	relay := coreBlock(t, report, "relay")
	if relay["socket"] != filepath.Join(codexHome, "app-server-control", "app-server-control.sock") || relay["state"] != "" {
		t.Errorf("relay = %v", relay)
	}
	if bridge := coreBlock(t, report, "bridge"); bridge["binary"] != "" || bridge["execution_policy"] != "" {
		t.Errorf("bridge = %v, want empty", report["bridge"])
	}
	sources := coreBlock(t, report, "sources")
	for _, key := range []string{"relay.socket", "state_dir", "bridge.binary"} {
		if sources[key] != coreSourceDefault {
			t.Errorf("sources[%q] = %v, want %q", key, sources[key], coreSourceDefault)
		}
	}
	if report["management_thread"] != "" || report["repository"] != "" {
		t.Errorf("a value with no default is not empty: %v %v", report["management_thread"], report["repository"])
	}
	if source := coreConfigSourceOf(t, report); source["source"] != "default" {
		t.Errorf("config = %v, want the built-in location", source)
	}

	// The manage_state root follows XDG_STATE_HOME, and a paths override moves it.
	state := filepath.Join(home, "state")
	t.Setenv("XDG_STATE_HOME", state)
	if report, code, errOut := coreConfigReportOf(t); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	} else if got, want := report["state_dir"], filepath.Join(state, "crw", "manage"); got != want {
		t.Errorf("state_dir = %v, want %q", got, want)
	}
	coreConfigAt(t, coreConfigMarshal(t, map[string]any{
		"schema": "crw-config/1",
		"paths":  map[string]string{"manage_state": "/srv/elsewhere"},
		"manage": map[string]any{},
	}))
	if report, code, errOut := coreConfigReportOf(t); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	} else if report["state_dir"] != "/srv/elsewhere" {
		t.Errorf("state_dir = %v, want the paths override", report["state_dir"])
	}
}

// C3: --config names the file ahead of CRW_CONFIG, which names it ahead of the default
// location. Each is its own test.
func TestConfigFlagBeatsTheEnvironment(t *testing.T) {
	coreTempHome(t)
	coreConfigAt(t, coreConfigDocument(t, map[string]any{"state_dir": "/from/xdg"}))
	envFile := coreConfigWrite(t, filepath.Join(t.TempDir(), "env.json"), coreConfigDocument(t, map[string]any{"state_dir": "/from/env"}))
	t.Setenv("CRW_CONFIG", envFile)
	flagFile := coreConfigWrite(t, filepath.Join(t.TempDir(), "flag.json"), coreConfigDocument(t, map[string]any{"state_dir": "/from/flag"}))
	report, code, errOut := coreConfigReportOf(t, "--config", flagFile)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if report["state_dir"] != "/from/flag" {
		t.Errorf("state_dir = %v, want the flag file", report["state_dir"])
	}
	if source := coreConfigSourceOf(t, report); source["path"] != flagFile || source["source"] != "config" {
		t.Errorf("config = %v, want the flag path from config", source)
	}
}

func TestConfigEnvironmentBeatsTheDefaultLocation(t *testing.T) {
	coreTempHome(t)
	coreConfigAt(t, coreConfigDocument(t, map[string]any{"state_dir": "/from/xdg"}))
	envFile := coreConfigWrite(t, filepath.Join(t.TempDir(), "env.json"), coreConfigDocument(t, map[string]any{"state_dir": "/from/env"}))
	t.Setenv("CRW_CONFIG", envFile)
	report, code, errOut := coreConfigReportOf(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if report["state_dir"] != "/from/env" {
		t.Errorf("state_dir = %v, want the CRW_CONFIG file", report["state_dir"])
	}
	if source := coreConfigSourceOf(t, report); source["path"] != envFile || source["source"] != "env" {
		t.Errorf("config = %v, want the CRW_CONFIG path from env", source)
	}
}

// C4: a manage section that is not a JSON object, or a field inside it of the wrong
// shape, is exit 2 with an English error naming the file, and no report.
func TestConfigRefusesAManageSectionThatIsNotAnObject(t *testing.T) {
	coreTempHome(t)
	for _, manage := range []any{
		[]any{1},
		"x",
		nil,
		map[string]any{"relay": 5},
		map[string]any{"parents": []string{"a"}},
	} {
		path := coreConfigAt(t, coreConfigDocument(t, manage))
		report, code, errOut := coreConfigReportOf(t)
		if code != usageExit {
			t.Errorf("%v: exit %d, want %d: %s", manage, code, usageExit, errOut)
		}
		if len(report) != 0 {
			t.Errorf("%v: the failing run wrote a report: %v", manage, report)
		}
		if !strings.Contains(errOut, path) {
			t.Errorf("%v: stderr does not name the file: %q", manage, errOut)
		}
	}

	// --config names another file, so a broken one at the default location does not
	// stop the command that reports the configuration: the flag wins first, as it does
	// for every other reader of this file.
	good := coreConfigWrite(t, filepath.Join(t.TempDir(), "good.json"),
		coreConfigDocument(t, map[string]any{"state_dir": "/from/flag"}))
	report, code, errOut := coreConfigReportOf(t, "--config", good)
	if code != 0 {
		t.Fatalf("a good --config file beside a broken default: exit %d: %s", code, errOut)
	}
	if report["state_dir"] != "/from/flag" {
		t.Errorf("state_dir = %v, want the file the flag named", report["state_dir"])
	}
}

// A help request reads no configuration, so a file this product cannot use does not take
// the usage away from an operator who is trying to read it.
func TestHelpWorksWithAnUnusableConfigurationFile(t *testing.T) {
	coreTempHome(t)
	coreConfigAt(t, coreConfigDocument(t, "not an object"))
	for _, args := range [][]string{
		{"host-read", "--help"},
		{"child-check", "--help"},
		{"audit", "--help"},
	} {
		code, out, errOut := coreRunManage(t, args...)
		if code != 0 {
			t.Errorf("%q: exit %d, want 0: %s", args, code, errOut)
		}
		if !strings.Contains(out, "usage:") {
			t.Errorf("%q: the usage is missing from %q", args, out)
		}
	}
}

// C5: a write to stdout that fails ends crw manage config with exit 1 and names the
// failure, rather than reporting a truncated report as success.
func TestConfigReportsAWriteFailure(t *testing.T) {
	coreTempHome(t)
	var errOut strings.Builder
	code := Run(context.Background(), []string{"config"}, strings.NewReader(""), coreFailWriter{}, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "crw manage config: error: write output:") {
		t.Errorf("stderr = %q, want the write failure", errOut.String())
	}
}

// C10: a configuration file this product cannot use stops a subcommand with exit 2 before
// it runs, rather than letting it continue with the defaults. Two subcommands prove it.
func TestConfigRefusesABadFileBeforeEverySubcommand(t *testing.T) {
	coreTempHome(t)
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"model": "m", "reasoningEffort": "none", "status": map[string]any{"type": "idle"},
	}}})
	hostReadEnv(t, host.SocketPath)
	path := coreConfigAt(t, coreConfigDocument(t, "not an object"))

	for _, args := range [][]string{
		{"host-read", "--method", "thread/read"},
		{"child-check", "--thread", "t1", "--model", "m", "--effort", "none", "--disabled", "alpha"},
		{"config"},
	} {
		code, _, errOut := coreRunManage(t, args...)
		if code != usageExit {
			t.Errorf("%q: exit %d, want %d: %s", args, code, usageExit, errOut)
		}
		if !strings.Contains(errOut, path) {
			t.Errorf("%q: stderr does not name the configuration file: %q", args, errOut)
		}
	}
	if requests := host.Requests(); len(requests) != 0 {
		t.Errorf("a subcommand read the socket through a bad configuration: %+v", requests)
	}
}

// C11: the audit and checkout sections of the crw configuration file take audit round
// start and audit package past the configuration gates and leave one package audited,
// through a fake grader and a temporary checkout, with no relay and no network.
func TestConfigDrivesTheAuditRoundEndToEnd(t *testing.T) {
	home, _, _ := coreTempHome(t)
	state := filepath.Join(home, "state")
	t.Setenv("XDG_STATE_HOME", state)
	grader := auditFake(t, "json", auditJSONClean)
	auditPkgFakeGit(t, "head1", map[string][]string{"pkg/a": {"pkg/a/a.go"}}, map[string]string{"pkg/a/a.go": "package a\n"})
	auditPkgFakeCriteria(t, map[string]string{})
	coreConfigAt(t, coreConfigDocument(t, map[string]any{
		"audit":    map[string]any{"grader": grader},
		"checkout": map[string]string{"repository": "/checkout"},
	}))

	if code, out, errOut := coreRunManage(t, "audit", "round", "start", "--name", "r1", "--package", "pkg/a"); code != 0 {
		t.Fatalf("round start: exit %d, %s %s", code, out, errOut)
	} else if !strings.Contains(out, "\"pending\":1") {
		t.Fatalf("round start wrote %s", out)
	}
	code, out, errOut := coreRunManage(t, "audit", "package", "--round", "r1", "--next", "1", "--head", "head1")
	if code != 0 {
		t.Fatalf("audit package: exit %d, %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "\"audited\":1") || !strings.Contains(out, "\"clean\":true") {
		t.Fatalf("the round is not clean after the graded package: %s", out)
	}
	ledger := auditLedgerStatuses(t, filepath.Join(state, "crw", "manage"))
	if len(ledger) != 1 || ledger[0] != auditStatusOK {
		t.Errorf("the ledger says %v, want one ok result", ledger)
	}
}

// Section decodes a key of the manage object into a value; a key it does not carry leaves
// the value untouched and reports no error.
func TestSectionDecodesAKeyAndLeavesAMissingOne(t *testing.T) {
	cfg := &Config{raw: map[string]json.RawMessage{"custom": json.RawMessage("{\"answer\": 42, \"name\": \"x\"}")}}
	read := map[string]any{}
	if err := cfg.Section("custom", &read); err != nil {
		t.Fatal(err)
	}
	if read["answer"] != float64(42) || read["name"] != "x" {
		t.Errorf("Section read %v", read)
	}
	untouched := map[string]any{"answer": float64(7)}
	if err := cfg.Section("absent", &untouched); err != nil {
		t.Fatal(err)
	}
	if untouched["answer"] != float64(7) || len(untouched) != 1 {
		t.Errorf("Section changed a value for an absent key: %v", untouched)
	}
}
