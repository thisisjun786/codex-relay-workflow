package doctor_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// hookTrustConfigRecorded is one case of testdata/hooktrust/config-oracle.json, recorded by
// testdata/hooktrust/record-config.mjs from CXC v0.2.40's dist (3c1459ac): one plugin whose
// hooks/sample.json holds Document, and a config.toml (Config text, ConfigBase64 bytes, or none),
// with the oracle's answers. Keys is what readInstalledPluginKeys returned; Diagnose holds one
// answer per diagnoseHookTrust result, in order. An engine error of either read is recorded by
// its class in KeysError or DiagnoseError.
type hookTrustConfigRecorded struct {
	Name               string                  `json:"name"`
	Document           string                  `json:"document"`
	Key                string                  `json:"key"`
	PluginName         string                  `json:"pluginName"`
	NoConfig           bool                    `json:"noConfig"`
	ConfigDir          bool                    `json:"configDir"`
	Config             *string                 `json:"config"`
	ConfigBase64       *string                 `json:"configBase64"`
	Keys               []string                `json:"keys"`
	KeysError          string                  `json:"keysError"`
	KeysErrorClass     string                  `json:"keysErrorClass"`
	Diagnose           []hookTrustConfigAnswer `json:"diagnose"`
	DiagnoseError      string                  `json:"diagnoseError"`
	DiagnoseErrorClass string                  `json:"diagnoseErrorClass"`
}

// hookTrustConfigAnswer is one diagnoseHookTrust result (hook-trust.ts:59-62): the entry key,
// the status, and the recorded trusted_hash value, nil when the config holds no single one.
type hookTrustConfigAnswer struct {
	Key    string  `json:"key"`
	Status string  `json:"status"`
	Actual *string `json:"actual"`
}

func hookTrustConfigCases(t *testing.T) []hookTrustConfigRecorded {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hooktrust", "config-oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Oracle string                    `json:"oracle"`
		Cases  []hookTrustConfigRecorded `json:"cases"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.Oracle, "3c1459ac") || len(recorded.Cases) < 27 {
		t.Fatalf("config-oracle.json holds %d cases for oracle %q", len(recorded.Cases), recorded.Oracle)
	}
	return recorded.Cases
}

// hookTrustConfigErrorClass maps a read error to the class the recorder records for the
// oracle's engine errors.
func hookTrustConfigErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, syscall.EISDIR):
		return "EISDIR"
	}
	return "other"
}

// hookTrustConfigDefaultDocument is the plugin document the recorder uses when a case names
// none (record-config.mjs stopDocument): one Stop group with one command hook.
const hookTrustConfigDefaultDocument = `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo ok"}]}]}}`

// hookTrustConfigTempHome points HOME, CODEX_HOME and CRW_HOME at fresh directories under one
// temporary root (the operator rule of 2026-10-04: a test that can reach config or hook code
// never reads or writes the real homes), and returns the temporary codex home. The listings of
// the temporary ~/.codex and ~/.crw are compared before and after the test, so a path that
// escaped the temporary root is reported.
func hookTrustConfigTempHome(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	codex := filepath.Join(base, "codex")
	crw := filepath.Join(base, "crw")
	for _, dir := range []string{home, codex, crw} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("CRW_HOME", crw)
	before := hookTrustConfigHomeListing(home)
	t.Cleanup(func() {
		if after := hookTrustConfigHomeListing(home); !reflect.DeepEqual(before, after) {
			t.Errorf("the temporary ~/.codex and ~/.crw changed: %v -> %v", before, after)
		}
	})
	return codex
}

// hookTrustConfigHomeListing lists the temporary home's .codex and .crw.
func hookTrustConfigHomeListing(home string) []string {
	listing := []string{}
	for _, name := range []string{".codex", ".crw"} {
		entries, err := os.ReadDir(filepath.Join(home, name))
		if err != nil {
			listing = append(listing, name+": absent")
			continue
		}
		for _, entry := range entries {
			listing = append(listing, name+"/"+entry.Name())
		}
	}
	return listing
}

// hookTrustConfigSection is trustSection of hook-trust.test.ts: one exact hooks.state section
// with its one trusted_hash line.
func hookTrustConfigSection(key, hash string) string {
	return fmt.Sprintf("[hooks.state.\"%s\"]\ntrusted_hash = \"%s\"\n", key, hash)
}

// hookTrustConfigDiagnose checks one recorded case's diagnosis against the port.
func hookTrustConfigDiagnose(t *testing.T, want hookTrustConfigRecorded, got []doctor.HookTrustResult, err error) {
	t.Helper()
	switch {
	case want.DiagnoseError != "":
		if err == nil || err.Error() != want.DiagnoseError {
			t.Fatalf("DiagnoseHookTrust error = %v, want %q", err, want.DiagnoseError)
		}
	case want.DiagnoseErrorClass != "":
		if class := hookTrustConfigErrorClass(err); class != want.DiagnoseErrorClass {
			t.Fatalf("DiagnoseHookTrust error = %v, want an engine error of class %s", err, want.DiagnoseErrorClass)
		}
	case err != nil:
		t.Fatalf("DiagnoseHookTrust: %v", err)
	case len(got) != len(want.Diagnose):
		t.Fatalf("DiagnoseHookTrust returned %d results, want %d", len(got), len(want.Diagnose))
	}
	for i, answer := range want.Diagnose {
		if i >= len(got) {
			return
		}
		switch {
		case got[i].Key != answer.Key || got[i].Status != answer.Status:
			t.Fatalf("result %d = %s/%s, want %s/%s", i, got[i].Key, got[i].Status, answer.Key, answer.Status)
		case (got[i].Actual == nil) != (answer.Actual == nil):
			t.Fatalf("result %d actual = %v, want %v", i, got[i].Actual, answer.Actual)
		case got[i].Actual != nil && *got[i].Actual != *answer.Actual:
			t.Fatalf("result %d actual = %q, want %q", i, *got[i].Actual, *answer.Actual)
		}
	}
}

// TestHookTrustConfig_recordedCases replays every recorded oracle answer: the keys, each
// diagnosis and the class of an engine error.
func TestHookTrustConfig_recordedCases(t *testing.T) {
	for _, want := range hookTrustConfigCases(t) {
		t.Run(want.Name, func(t *testing.T) {
			home := hookTrustConfigTempHome(t)
			document := want.Document
			if document == "" {
				document = hookTrustConfigDefaultDocument
			}
			root := hookTrustEntriesPlugin(t, json.RawMessage(document), "./hooks/sample.json")
			switch {
			case want.ConfigDir:
				if err := os.Mkdir(filepath.Join(home, "config.toml"), 0o755); err != nil {
					t.Fatal(err)
				}
			case want.NoConfig:
			case want.Config != nil:
				hookTrustEntriesWrite(t, filepath.Join(home, "config.toml"), []byte(*want.Config))
			case want.ConfigBase64 != nil:
				raw, err := base64.StdEncoding.DecodeString(*want.ConfigBase64)
				if err != nil {
					t.Fatal(err)
				}
				hookTrustEntriesWrite(t, filepath.Join(home, "config.toml"), raw)
			default:
				t.Fatal("the case holds no config")
			}
			key := want.Key
			if key == "" {
				key = "fixture@market"
			}
			pluginName := want.PluginName
			if pluginName == "" {
				pluginName = "fixture"
			}
			keys, err := doctor.ReadInstalledPluginKeys(home, pluginName)
			switch {
			case want.KeysError != "":
				if err == nil || err.Error() != want.KeysError {
					t.Fatalf("ReadInstalledPluginKeys error = %v, want %q", err, want.KeysError)
				}
			case want.KeysErrorClass != "":
				if got := hookTrustConfigErrorClass(err); got != want.KeysErrorClass {
					t.Fatalf("ReadInstalledPluginKeys error = %v, want an engine error of class %s", err, want.KeysErrorClass)
				}
			case err != nil:
				t.Fatalf("ReadInstalledPluginKeys: %v", err)
			case !reflect.DeepEqual(keys, want.Keys):
				t.Fatalf("ReadInstalledPluginKeys = %v, want %v", keys, want.Keys)
			}
			diagnosed, err := doctor.DiagnoseHookTrust(home, root, key)
			hookTrustConfigDiagnose(t, want, diagnosed, err)
		})
	}
}

// The four tests below are hook-trust.test.ts:187-266, ported. The oracle's test at :217 also
// asserts the retrust write, which belongs to the retrust port and is not repeated here.

func TestReadInstalledPluginKeys_enabledCandidatesAndDisabledSections(t *testing.T) {
	home := hookTrustConfigTempHome(t)
	hookTrustEntriesWrite(t, filepath.Join(home, "config.toml"), []byte(strings.Join([]string{
		`[plugins."fixture@one"]`,
		"enabled = true",
		`[plugins."fixture@off"]`,
		"enabled = false # intentionally disabled",
		`[plugins."other@market"]`,
		"enabled = true",
		`[plugins."fixture@two"]`,
		`source = "dev"`,
		"",
	}, "\n")))
	want := []string{"fixture@one", "fixture@two"}
	if got, err := doctor.ReadInstalledPluginKeys(home, "fixture"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadInstalledPluginKeys = %v, %v, want %v", got, err, want)
	}
	if got, err := doctor.ReadInstalledPluginKeys(home, "missing"); err != nil || len(got) != 0 {
		t.Fatalf("ReadInstalledPluginKeys for a missing plugin = %v, %v, want none", got, err)
	}
}

func TestHookTrust_sectionHeadersAllowTrailingComments(t *testing.T) {
	root := hookTrustEntriesPlugin(t, stopDocument("echo comment headers"), "./hooks/sample.json")
	entry := hookTrustConfigFirstEntry(t, root)
	home := hookTrustConfigTempHome(t)
	hookTrustEntriesWrite(t, filepath.Join(home, "config.toml"), []byte(strings.Join([]string{
		`[plugins."fixture@market"] # installed from local marketplace`,
		"enabled = true",
		fmt.Sprintf(`[hooks.state."%s"]  # retained by operator`, entry.Key),
		fmt.Sprintf(`trusted_hash = "%s"`, entry.Hash),
		"",
	}, "\n")))
	if got, err := doctor.ReadInstalledPluginKeys(home, "fixture"); err != nil || !reflect.DeepEqual(got, []string{"fixture@market"}) {
		t.Fatalf("ReadInstalledPluginKeys = %v, %v", got, err)
	}
	diagnosed, err := doctor.DiagnoseHookTrust(home, root, "fixture@market")
	if err != nil || len(diagnosed) != 1 || diagnosed[0].Status != "trusted" {
		t.Fatalf("DiagnoseHookTrust = %+v, %v, want one trusted hook", diagnosed, err)
	}
}

func TestHookTrust_multilineStringsCannotPoisonReads(t *testing.T) {
	for _, quote := range []string{`"""`, "'''"} {
		root := hookTrustEntriesPlugin(t, stopDocument("echo "+quote), "./hooks/sample.json")
		entry := hookTrustConfigFirstEntry(t, root)
		poison := strings.Join([]string{
			"inline = " + quote + "closed" + quote,
			"payload = " + quote,
			`[plugins."fixture@poison"]`,
			fmt.Sprintf(`[hooks.state."%s"]`, entry.Key),
			fmt.Sprintf(`trusted_hash = "%s"`, entry.Hash),
			quote,
			`[plugins."fixture@market"] # real section after same-line close`,
			"enabled = true",
			"plugin_note = " + quote,
			"enabled = false # string content must not disable the plugin",
			quote,
			"",
		}, "\n")
		home := hookTrustConfigTempHome(t)
		hookTrustEntriesWrite(t, filepath.Join(home, "config.toml"), []byte(poison))
		if got, err := doctor.ReadInstalledPluginKeys(home, "fixture"); err != nil || !reflect.DeepEqual(got, []string{"fixture@market"}) {
			t.Fatalf("%s: ReadInstalledPluginKeys = %v, %v", quote, got, err)
		}
		diagnosed, err := doctor.DiagnoseHookTrust(home, root, "fixture@market")
		if err != nil || len(diagnosed) != 1 || diagnosed[0].Status != "untrusted" {
			t.Fatalf("%s: DiagnoseHookTrust = %+v, %v, want one untrusted hook", quote, diagnosed, err)
		}
	}
}

func TestDiagnoseHookTrust_reportsTrustedDriftedUntrusted(t *testing.T) {
	root := hookTrustEntriesPlugin(t, map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{hookTrustEntriesCommand("echo trusted", nil)}},
		map[string]any{"hooks": []any{hookTrustEntriesCommand("echo drifted", nil)}},
		map[string]any{"hooks": []any{hookTrustEntriesCommand("echo untrusted", nil)}},
	}}}, "./hooks/sample.json")
	entries, err := doctor.ListHookTrustEntries(root, "fixture@market")
	if err != nil || len(entries) != 3 {
		t.Fatalf("ListHookTrustEntries = %+v, %v, want three entries", entries, err)
	}
	home := hookTrustConfigTempHome(t)
	hookTrustEntriesWrite(t, filepath.Join(home, "config.toml"), []byte(hookTrustConfigSection(entries[0].Key, entries[0].Hash)+"\n"+hookTrustConfigSection(entries[1].Key, "sha256:stale")))
	diagnosed, err := doctor.DiagnoseHookTrust(home, root, "fixture@market")
	if err != nil || len(diagnosed) != 3 {
		t.Fatalf("DiagnoseHookTrust = %+v, %v", diagnosed, err)
	}
	statuses := []string{diagnosed[0].Status, diagnosed[1].Status, diagnosed[2].Status}
	if !reflect.DeepEqual(statuses, []string{"trusted", "drifted", "untrusted"}) {
		t.Fatalf("statuses = %v, want trusted/drifted/untrusted", statuses)
	}
	if diagnosed[1].Actual == nil || *diagnosed[1].Actual != "sha256:stale" {
		t.Fatalf("drifted actual = %v, want sha256:stale", diagnosed[1].Actual)
	}
	if diagnosed[2].Actual != nil {
		t.Fatalf("untrusted actual = %q, want none", *diagnosed[2].Actual)
	}
}

// stopDocument is the one Stop group with one command hook of hook-trust.test.ts makePlugin.
func stopDocument(command string) map[string]any {
	return map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{hookTrustEntriesCommand(command, nil)}},
	}}}
}

func hookTrustConfigFirstEntry(t *testing.T, root string) doctor.HookTrustEntry {
	t.Helper()
	entries, err := doctor.ListHookTrustEntries(root, "fixture@market")
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListHookTrustEntries = %+v, %v, want one entry", entries, err)
	}
	return entries[0]
}
