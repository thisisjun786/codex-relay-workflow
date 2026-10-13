package crwconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// CRW-1319 (evaluation a48fe518 d2): crw config verifier writes through a configuration file that is a symbolic link (a
// dotfiles-managed file, or CRW_CONFIG naming a link): the link stays and its target holds the recorded section.
func TestConfigVerifierWritesThroughASymlinkedFile(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "crw.json")
	configWriteAt(t, target, `{"schema":"crw-config/1","paths":{}}`)
	link := filepath.Join(dir, "config", "crw", "config.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../dotfiles/crw.json", link); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := configRun(t, env, "verifier", "--config", link, "--go-root", goRoot); code != 0 {
		t.Fatalf("record through a link: exit %d %s%s", code, out, errOut)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	if tools, err := LoadVerifierTools(env, target); err != nil || tools.GoRoot != goRoot {
		t.Errorf("the link's target: %+v %v", tools, err)
	}
	if tools, err := LoadVerifierTools(env, link); err != nil || tools.GoRoot != goRoot {
		t.Errorf("read through the link: %+v %v", tools, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*", ".config-*"))
	more, _ := filepath.Glob(filepath.Join(dir, "config", "crw", ".config-*"))
	if len(leftovers)+len(more) > 0 {
		t.Errorf("temporary files left: %v %v", leftovers, more)
	}
}

// CRW-1319 (evaluation a48fe518 d5): a record keeps the keys of the verifier section it does not set.
func TestConfigVerifierKeepsTheSectionsOtherKeys(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	path := configWrite(t, `{"verifier":{"crw_dev":"tree","future_key":{"a":1}}}`)
	if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
		t.Fatalf("record: exit %d %s%s", code, out, errOut)
	}
	var doc struct {
		Verifier map[string]json.RawMessage `json:"verifier"`
	}
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if compact(doc.Verifier["future_key"]) != `{"a":1}` || string(doc.Verifier["crw_dev"]) != `"tree"` || string(doc.Verifier["go_root"]) != `"`+goRoot+`"` {
		t.Errorf("the section after the record: %s", data)
	}
	// setting a key to empty removes it from the section
	if code, _, errOut := configRun(t, env, "verifier", "--config", path, "--crw-dev", ""); code != 0 {
		t.Fatalf("clear crw_dev: exit %d %s", code, errOut)
	}
	data, _ = os.ReadFile(path)
	doc.Verifier = nil
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Verifier["crw_dev"]; ok || compact(doc.Verifier["future_key"]) != `{"a":1}` {
		t.Errorf("the section after clearing crw_dev: %s", data)
	}
}

// compact is a JSON value without its whitespace.
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}
