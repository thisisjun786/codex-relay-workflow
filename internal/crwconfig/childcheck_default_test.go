package crwconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// manageChildCheck reads manage.child_check.disabled_servers out of a written configuration file: the list and whether the key
// is there at all.
func manageChildCheck(t *testing.T, path string) ([]string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Manage struct {
			ChildCheck struct {
				DisabledServers *[]string `json:"disabled_servers"`
			} `json:"child_check"`
		} `json:"manage"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if doc.Manage.ChildCheck.DisabledServers == nil {
		return nil, false
	}
	return *doc.Manage.ChildCheck.DisabledServers, true
}

// CRW-1398 item 1: the installation's writer of the configuration file (crw config verifier) writes the default of
// manage.child_check.disabled_servers when the file does not name it, so crw manage child-resume finds the key; a new file gets it too.
func TestConfigVerifierWritesTheChildCheckDefault(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	fresh := filepath.Join(t.TempDir(), "crw", "config.json")
	if code, out, errOut := configRun(t, env, "verifier", "--config", fresh, "--go-root", goRoot); code != 0 {
		t.Fatalf("a missing file: exit %d %s%s", code, out, errOut)
	}
	if list, ok := manageChildCheck(t, fresh); !ok || len(list) != 0 {
		t.Errorf("the new file names manage.child_check.disabled_servers %v (present %v), want the default empty list", list, ok)
	}
	// A file with other sections and a manage object without the key gets the key and keeps everything else.
	path := configWrite(t, `{"schema":"crw-config/1","sandbox":{"project_roots":["/p"]},"manage":{"state_dir":"/srv/manage","child_check":{"future":1}}}`)
	if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
		t.Fatalf("record: exit %d %s%s", code, out, errOut)
	}
	if list, ok := manageChildCheck(t, path); !ok || len(list) != 0 {
		t.Errorf("manage.child_check.disabled_servers = %v (present %v), want the default empty list", list, ok)
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
	if doc.Manage.StateDir != "/srv/manage" || compact(doc.Manage.ChildCheck["future"]) != "1" || doc.Sandbox["project_roots"] == nil {
		t.Errorf("a key of the file was lost:\n%s", data)
	}
}

// CRW-1398 item 1: a list the file already names is the operator's, whatever it holds; a manage value that is no object is left
// as it is (the writer does not repair what it does not own).
func TestConfigVerifierKeepsTheChildCheckListAFileNames(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	for name, document := range map[string]string{
		"named":  `{"manage":{"child_check":{"disabled_servers":["alpha","beta"]}}}`,
		"empty":  `{"manage":{"child_check":{"disabled_servers":[]}}}`,
		"blanks": `{"manage":{"child_check":{"disabled_servers":["", "x"]}}}`,
	} {
		path := configWrite(t, document)
		before, _ := manageChildCheck(t, path)
		if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
			t.Fatalf("%s: exit %d %s%s", name, code, out, errOut)
		}
		after, ok := manageChildCheck(t, path)
		if !ok || len(after) != len(before) {
			t.Errorf("%s: the list changed from %v to %v (present %v)", name, before, after, ok)
		}
		for i := range before {
			if after[i] != before[i] {
				t.Errorf("%s: the list changed from %v to %v", name, before, after)
			}
		}
	}
	path := configWrite(t, `{"manage":"by hand"}`)
	if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
		t.Fatalf("manage that is no object: exit %d %s%s", code, out, errOut)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil || doc["manage"] != "by hand" {
		t.Errorf("a manage value that is no object was replaced:\n%s", data)
	}
}

// CRW-1398 item 1: the default is the empty list, and a caller that changes the slice it got changes nothing for the next.
func TestDefaultChildCheckDisabledServersIsAnEmptyListEveryTime(t *testing.T) {
	first := DefaultChildCheckDisabledServers()
	if first == nil || len(first) != 0 {
		t.Fatalf("the default is %#v, want an empty non-nil list", first)
	}
	_ = append(first, "x")
	if again := DefaultChildCheckDisabledServers(); len(again) != 0 {
		t.Errorf("the default after a caller appended: %v", again)
	}
}
