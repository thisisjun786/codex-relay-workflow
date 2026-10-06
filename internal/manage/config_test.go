package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coreReportOf runs crw manage config and returns the decoded report.
func coreReportOf(t *testing.T) map[string]any {
	t.Helper()
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"config"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("crw manage config: exit %d %q", code, errOut.String())
	}
	report := map[string]any{}
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the config output is not JSON: %v\n%s", err, out.String())
	}
	return report
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

// With nothing but HOME the defaults are the install layout: the state directory below
// ~/.local/state, the App Server socket below ~/.codex, no bridge binary, and each
// value's source named as the default.
func TestConfigDefaultsBelowTheHome(t *testing.T) {
	home, codexHome, _ := coreTempHome(t)
	report := coreReportOf(t)
	if got, want := report["state_dir"], filepath.Join(home, ".local", "state", "crw", "manage"); got != want {
		t.Errorf("state_dir = %v, want %q", got, want)
	}
	relay := coreBlock(t, report, "relay")
	if relay["socket"] != filepath.Join(codexHome, "app-server-control", "app-server-control.sock") || relay["state"] != "" {
		t.Errorf("relay = %v", report["relay"])
	}
	if bridge := coreBlock(t, report, "bridge"); bridge["binary"] != "" || bridge["execution_policy"] != "" {
		t.Errorf("bridge = %v, want empty", report["bridge"])
	}
	if report["management_thread"] != "" || report["repository"] != "" {
		t.Errorf("a value with no default is not empty: %v %v", report["management_thread"], report["repository"])
	}
	sources := coreBlock(t, report, "sources")
	for _, key := range []string{"relay.socket", "state_dir", "bridge.binary"} {
		if sources[key] != coreSourceDefault {
			t.Errorf("sources[%q] = %v, want %q", key, sources[key], coreSourceDefault)
		}
	}
}

// XDG_STATE_HOME and CODEX_HOME move the two default paths.
func TestConfigDefaultsFollowTheStateAndCodexHomes(t *testing.T) {
	home, codexHome, _ := coreTempHome(t)
	state := filepath.Join(home, "state")
	t.Setenv("XDG_STATE_HOME", state)
	report := coreReportOf(t)
	if got, want := report["state_dir"], filepath.Join(state, "crw", "manage"); got != want {
		t.Errorf("state_dir = %v, want %q", got, want)
	}
	if got := coreBlock(t, report, "relay")["socket"]; got != filepath.Join(codexHome, "app-server-control", "app-server-control.sock") {
		t.Errorf("relay.socket = %v", got)
	}
}

// This issue reads no configuration file: a manage.json beside CRW_HOME changes
// nothing, and an argument is a usage error.
func TestConfigReadsNoFileAndTakesNoArgument(t *testing.T) {
	home, _, crwHome := coreTempHome(t)
	if err := os.MkdirAll(crwHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crwHome, "manage.json"), []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := coreReportOf(t)["state_dir"]; got != filepath.Join(home, ".local", "state", "crw", "manage") {
		t.Errorf("a file beside CRW_HOME changed the defaults: %v", got)
	}
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"config", "--config", "x"}, strings.NewReader(""), &out, &errOut); code != usageExit {
		t.Errorf("an argument: exit %d, want %d", code, usageExit)
	}
}

// Section decodes a raw key of the document into a struct; a key the document does not
// carry leaves the value untouched and reports no error.
func TestSectionDecodesARawKeyAndLeavesAMissingOne(t *testing.T) {
	cfg := &Config{raw: map[string]json.RawMessage{"custom": json.RawMessage(`{"answer": 42, "name": "x"}`)}}
	var section struct {
		Answer int    `json:"answer"`
		Name   string `json:"name"`
	}
	if err := cfg.Section("custom", &section); err != nil {
		t.Fatal(err)
	}
	if section.Answer != 42 || section.Name != "x" {
		t.Errorf("Section read %+v", section)
	}
	untouched := struct {
		Answer int `json:"answer"`
	}{Answer: 7}
	if err := cfg.Section("absent", &untouched); err != nil {
		t.Fatal(err)
	}
	if untouched.Answer != 7 {
		t.Errorf("Section changed a value for an absent key: %d", untouched.Answer)
	}
}
