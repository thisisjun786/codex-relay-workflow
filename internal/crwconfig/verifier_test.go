package crwconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGoRoot makes a Go toolchain root whose bin/go is an executable script, and answers the root.
func fakeGoRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "go")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// CRW-1319: the verifier section names the verifier's tools, the Go toolchain root and how crw-dev runs; a file without it
// names none, and a section the verifier could not use is refused by the section's reader, while the file's reading only records
// a warning for it (TestAMalformedVerifierSectionStopsNoOtherReader).
func TestVerifierToolsSection(t *testing.T) {
	_, env := rootsHome(t)
	path := configWrite(t, `{"paths":{}}`)
	file, err := Load(env, path)
	if err != nil {
		t.Fatal(err)
	}
	if tools, err := file.VerifierTools(); err != nil || tools.GoRoot != "" || tools.CrwDev != "" || tools.Config != path {
		t.Errorf("no section: %+v %v", tools, err)
	}
	path = configWrite(t, `{"verifier":{"go_root":"/opt/go","crw_dev":"tree"}}`)
	tools, err := LoadVerifierTools(env, path)
	if err != nil || tools.GoRoot != "/opt/go" || tools.CrwDev != CrwDevTree || tools.Config != path {
		t.Errorf("section: %+v %v", tools, err)
	}
	for document, want := range map[string]string{
		`{"verifier":{"go_root":"go"}}`:            "verifier.go_root",
		`{"verifier":{"crw_dev":"crw-dev"}}`:       "verifier.crw_dev",
		`{"verifier":"/opt/go"}`:                   "verifier is not an object",
		`{"verifier":{"go_root":["/opt/go"]}}`:     "verifier is not an object",
		`{"verifier":{"crw_dev":"./bin/crw-dev"}}`: "verifier.crw_dev",
	} {
		file, err := Load(env, configWrite(t, document))
		if err != nil {
			t.Errorf("%s: the file's reading refused it: %v", document, err)
			continue
		}
		if _, err := file.VerifierTools(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error naming %q", document, err, want)
		}
		if w := file.Warnings(); len(w) != 1 || w[0].Check != CheckVerifierSection || !strings.Contains(w[0].Message, want) {
			t.Errorf("%s: the warning %+v, want one naming %q", document, w, want)
		}
	}
}

// CRW-1319: the command a verifier starts. A configured Go root runs the verified tree's own crw-dev with that Go, and the Go
// is first on the PATH the command and its steps see; a configured crw-dev binary runs as it is; nothing configured is crw-dev
// on PATH (fail-open) with a note, and a hint for the reason a start that fails gives.
func TestVerifierCommand(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "cmd", "crw-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	has := func(env []string, want ...string) bool {
		for _, w := range want {
			if !slices.Contains(env, w) {
				return false
			}
		}
		return true
	}
	cmd, err := VerifierTools{GoRoot: "/opt/go", Config: "/c.json"}.Command(tree, "/usr/bin")
	if err != nil || !slices.Equal(cmd.Argv, []string{"/opt/go/bin/go", "run", "-tags", "dev", "./cmd/crw-dev"}) ||
		!has(cmd.Env, "PATH=/opt/go/bin:/usr/bin", "GOROOT=/opt/go", "GOTOOLCHAIN=local") || cmd.Note != "" {
		t.Errorf("go root: %+v %v", cmd, err)
	}
	cmd, err = VerifierTools{GoRoot: "/opt/go", CrwDev: CrwDevTree}.Command(tree, "")
	if err != nil || cmd.Argv[0] != "/opt/go/bin/go" || !has(cmd.Env, "PATH=/opt/go/bin") {
		t.Errorf("tree with go root, empty PATH: %+v %v", cmd, err)
	}
	cmd, err = VerifierTools{GoRoot: "/opt/go", CrwDev: "/x/crw-dev"}.Command(tree, "/usr/bin")
	if err != nil || !slices.Equal(cmd.Argv, []string{"/x/crw-dev"}) || !has(cmd.Env, "PATH=/opt/go/bin:/usr/bin", "GOROOT=/opt/go") {
		t.Errorf("binary with go root: %+v %v", cmd, err)
	}
	cmd, err = VerifierTools{CrwDev: "/x/crw-dev"}.Command(tree, "/usr/bin")
	if err != nil || !slices.Equal(cmd.Argv, []string{"/x/crw-dev"}) || cmd.Env != nil || !strings.Contains(cmd.Note, "verifier.go_root") {
		t.Errorf("binary without go root: %+v %v", cmd, err)
	}
	cmd, err = VerifierTools{CrwDev: CrwDevTree}.Command(tree, "/usr/bin")
	if err != nil || !slices.Equal(cmd.Argv, []string{"go", "run", "-tags", "dev", "./cmd/crw-dev"}) || cmd.Env != nil || !strings.Contains(cmd.Note, "PATH") {
		t.Errorf("tree without go root: %+v %v", cmd, err)
	}
	cmd, err = VerifierTools{Config: "/c.json"}.Command(tree, "/usr/bin")
	if err != nil || !slices.Equal(cmd.Argv, []string{"crw-dev"}) || cmd.Env != nil || !strings.Contains(cmd.Note, "fail-open") ||
		!strings.Contains(cmd.Hint, "verifier.go_root") || !strings.Contains(cmd.Hint, "/c.json") {
		t.Errorf("nothing configured: %+v %v", cmd, err)
	}
	if _, err := (VerifierTools{GoRoot: "/opt/go"}).Command(t.TempDir(), "/usr/bin"); err == nil || !strings.Contains(err.Error(), "cmd/crw-dev") || !strings.Contains(err.Error(), "verifier.crw_dev") {
		t.Errorf("a tree without cmd/crw-dev: %v", err)
	}
}

// CRW-1319: crw config verifier prints the verifier's tools, and with --go-root or --crw-dev records them in the configuration
// file, every other key kept; a Go root without an executable bin/go and a crw-dev that is neither "tree" nor an absolute path
// are refused and nothing is written.
func TestConfigVerifierRecordsTheSetting(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	path := configWrite(t, `{"schema":"crw-config/1","sandbox":{"project_roots":["/p"]}}`)
	code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot)
	if code != 0 {
		t.Fatalf("record: exit %d\n%s%s", code, out, errOut)
	}
	var doc map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	verifier, _ := doc["verifier"].(map[string]any)
	if verifier["go_root"] != goRoot || doc["schema"] != Schema || doc["sandbox"] == nil {
		t.Errorf("the recorded file: %s", data)
	}
	code, out, _ = configRun(t, env, "verifier", "--config", path)
	var report map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &report) != nil || report["go_root"] != goRoot || report["mode"] != "tree" || report["config"] != path {
		t.Errorf("show: exit %d %s", code, out)
	}
	if code, _, _ := configRun(t, env, "verifier", "--config", path, "--crw-dev", "/x/crw-dev"); code != 0 {
		t.Errorf("record crw_dev: exit %d", code)
	}
	if tools, err := LoadVerifierTools(env, path); err != nil || tools.GoRoot != goRoot || tools.CrwDev != "/x/crw-dev" {
		t.Errorf("after the second record: %+v %v", tools, err)
	}
	before, _ := os.ReadFile(path)
	for _, args := range [][]string{{"--go-root", t.TempDir()}, {"--go-root", "go"}, {"--crw-dev", "crw-dev"}} {
		if code, _, errOut := configRun(t, env, append([]string{"verifier", "--config", path}, args...)...); code != 2 || errOut == "" {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("a refused record changed the file:\n%s", after)
	}
	fresh := filepath.Join(t.TempDir(), "crw", "config.json")
	if code, out, errOut := configRun(t, env, "verifier", "--config", fresh, "--go-root", goRoot); code != 0 {
		t.Errorf("a missing file: exit %d %s%s", code, out, errOut)
	}
	if tools, err := LoadVerifierTools(env, fresh); err != nil || tools.GoRoot != goRoot {
		t.Errorf("the new file: %+v %v", tools, err)
	}
}
