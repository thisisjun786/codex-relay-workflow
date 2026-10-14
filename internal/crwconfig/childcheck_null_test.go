package crwconfig

import (
	"encoding/json"
	"os"
	"testing"
)

// disabledServersRaw reads manage.child_check.disabled_servers out of a written configuration file as the raw JSON the file holds
// ("" when the key is not there), so null, [] and a non-list value stay told apart.
func disabledServersRaw(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Manage struct {
			ChildCheck map[string]json.RawMessage `json:"child_check"`
		} `json:"manage"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	raw, ok := doc.Manage.ChildCheck["disabled_servers"]
	if !ok {
		return ""
	}
	return compact(raw)
}

// CRW-1400: crw config verifier treats "disabled_servers": null as the reader does (not set) and writes the default, exactly as it
// fills a missing key; [] and a named list are the operator's and stay; a value that is no list stays for the reader to refuse.
func TestConfigVerifierReplacesANullChildCheckListLikeAMissingKey(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	for _, tc := range []struct {
		name     string
		document string
		want     string
	}{
		{"null", `{"manage":{"child_check":{"disabled_servers":null}}}`, `[]`},
		{"null with spaces", `{"manage":{"child_check":{"disabled_servers": null }}}`, `[]`},
		{"missing key", `{"manage":{"child_check":{"future":1}}}`, `[]`},
		{"empty list", `{"manage":{"child_check":{"disabled_servers":[]}}}`, `[]`},
		{"named list", `{"manage":{"child_check":{"disabled_servers":["alpha","beta"]}}}`, `["alpha","beta"]`},
		{"string", `{"manage":{"child_check":{"disabled_servers":"alpha"}}}`, `"alpha"`},
		{"number", `{"manage":{"child_check":{"disabled_servers":5}}}`, `5`},
		{"object", `{"manage":{"child_check":{"disabled_servers":{"a":1}}}}`, `{"a":1}`},
		{"list of non-names", `{"manage":{"child_check":{"disabled_servers":[1,2]}}}`, `[1,2]`},
	} {
		path := configWrite(t, tc.document)
		if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
			t.Fatalf("%s: exit %d %s%s", tc.name, code, out, errOut)
		}
		if got := disabledServersRaw(t, path); got != tc.want {
			t.Errorf("%s: disabled_servers is %s after the verifier, want %s", tc.name, got, tc.want)
		}
	}
}

// CRW-1400: replacing the null keeps every other key of the manage object and the child_check section, and the other sections.
func TestConfigVerifierReplacingANullChildCheckListKeepsTheOtherKeys(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	path := configWrite(t, `{"schema":"crw-config/1","sandbox":{"project_roots":["/p"]},"manage":{"state_dir":"/srv/manage","child_check":{"future":1,"disabled_servers":null}}}`)
	if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
		t.Fatalf("exit %d %s%s", code, out, errOut)
	}
	data, _ := os.ReadFile(path)
	var doc struct {
		Sandbox map[string]json.RawMessage `json:"sandbox"`
		Manage  struct {
			StateDir   string                     `json:"state_dir"`
			ChildCheck map[string]json.RawMessage `json:"child_check"`
		} `json:"manage"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Manage.StateDir != "/srv/manage" || compact(doc.Manage.ChildCheck["future"]) != "1" || compact(doc.Manage.ChildCheck["disabled_servers"]) != "[]" || doc.Sandbox["project_roots"] == nil {
		t.Errorf("a key of the file was lost or the null stayed:\n%s", data)
	}
}
