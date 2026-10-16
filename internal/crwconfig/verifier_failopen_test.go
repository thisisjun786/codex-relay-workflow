package crwconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1319 (final verification d4ed9281): a verifier section the verifier cannot use is read by the verifier only, so it stops no
// other reader of the file. The shared reading ignores it with a recorded warning (Warnings, the crw config check finding
// verifier_section, a warning line of crw config show and paths); the relay state selection and the tool roots read the file as
// before; the verifier falls back to PATH, and its note and the probe say the section was ignored.
func TestAMalformedVerifierSectionStopsNoOtherReader(t *testing.T) {
	saved := runProbe
	runProbe = roomy()
	t.Cleanup(func() { runProbe = saved })
	home, _ := rootsHome(t)
	config := filepath.Join(home, "crw.json")
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"~/.local/go/go"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"HOME": home, "CRW_CONFIG": config, "PATH": filepath.Join(home, "empty-path")}
	getenv := func(k string) string { return vars[k] }

	file, err := Load(getenv, "")
	if err != nil {
		t.Fatalf("the shared reading refused the file: %v", err)
	}
	warnings := file.Warnings()
	if len(warnings) != 1 || warnings[0].Level != LevelWarning || warnings[0].Check != CheckVerifierSection ||
		!strings.Contains(warnings[0].Message, "verifier.go_root") || !strings.Contains(warnings[0].Message, "ignored") {
		t.Errorf("the recorded warning: %+v", warnings)
	}
	if _, err := file.VerifierTools(); err == nil || !strings.Contains(err.Error(), "verifier.go_root") {
		t.Errorf("the section itself still reads as unusable: %v", err)
	}
	if err := RelayStateSplit(getenv); err != nil {
		t.Errorf("the relay state selection: %v", err)
	}
	if _, err := LoadToolRoots(getenv, ""); err != nil {
		t.Errorf("the tool roots: %v", err)
	}

	tools, err := LoadVerifierTools(getenv, "")
	if err != nil || tools.GoRoot != "" || tools.CrwDev != "" || tools.Mode() != "path" || tools.Config != config ||
		!strings.Contains(tools.Ignored, "verifier.go_root") {
		t.Fatalf("the verifier's reading: %+v %v", tools, err)
	}
	tree := t.TempDir()
	cmd, err := tools.Command(tree, "/usr/bin")
	if err != nil || cmd.Argv[0] != "crw-dev" || !strings.Contains(cmd.Note, "ignored") || !strings.Contains(cmd.Note, "fail-open") ||
		!strings.Contains(cmd.Note, "verifier.go_root") || !strings.Contains(cmd.Hint, "crw config verifier --go-root") {
		t.Errorf("the verifier's command: %+v %v", cmd, err)
	}
	env := []string{"HOME=" + home, "CRW_CONFIG=" + config, "PATH=" + vars["PATH"]}
	p := ProbeVerifierTools(context.Background(), env, "")
	if problems := strings.Join(p.Problems, "; "); p.Mode != "path" || !strings.Contains(problems, "ignored") || !strings.Contains(problems, "verifier.go_root") {
		t.Errorf("the probe: %+v", p)
	}

	for _, verb := range []string{"show", "paths"} {
		if code, out, errOut := configRun(t, getenv, verb); code != 0 || out == "" || !strings.Contains(errOut, "warning") || !strings.Contains(errOut, "verifier.go_root") {
			t.Errorf("crw config %s: exit %d stdout %q stderr %q", verb, code, out, errOut)
		}
	}
	code, out, errOut := configRun(t, getenv, "check")
	var report Report
	if code != 0 || json.Unmarshal([]byte(out), &report) != nil {
		t.Fatalf("crw config check: exit %d\n%s%s", code, out, errOut)
	}
	var found bool
	for _, f := range report.Findings {
		found = found || (f.Check == CheckVerifierSection && f.Level == LevelWarning && f.Root == "verifier")
	}
	if !found {
		t.Errorf("crw config check has no verifier_section warning: %+v", report.Findings)
	}
	code, out, errOut = configRun(t, getenv, "verifier")
	if code != 0 || !strings.Contains(out, `"mode": "path"`) || !strings.Contains(errOut, "verifier.go_root") {
		t.Errorf("crw config verifier: exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// CRW-1319: crw config verifier repairs a section the verifier cannot use: the key it records replaces the unusable one, and a
// key it does not set is left as the file has it.
func TestConfigVerifierRepairsAMalformedSection(t *testing.T) {
	_, env := rootsHome(t)
	goRoot := fakeGoRoot(t)
	path := configWrite(t, `{"verifier":{"go_root":"~/.local/go/go","crw_dev":"tree"}}`)
	if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
		t.Fatalf("repair: exit %d\n%s%s", code, out, errOut)
	}
	file, err := Load(env, path)
	if err != nil || len(file.Warnings()) != 0 {
		t.Fatalf("after the repair: %v %+v", err, file)
	}
	if tools, err := file.VerifierTools(); err != nil || tools.GoRoot != goRoot || tools.CrwDev != CrwDevTree {
		t.Errorf("the repaired section: %+v %v", tools, err)
	}

	path = configWrite(t, `{"verifier":"/opt/go"}`)
	if code, out, errOut := configRun(t, env, "verifier", "--config", path, "--go-root", goRoot); code != 0 {
		t.Fatalf("repair of a section that is no object: exit %d\n%s%s", code, out, errOut)
	}
	if tools, err := LoadVerifierTools(env, path); err != nil || tools.GoRoot != goRoot || tools.Ignored != "" {
		t.Errorf("the section that was no object: %+v %v", tools, err)
	}

	// a key the command does not set stays as the file has it, so the section stays ignored, with the warning
	path = configWrite(t, `{"verifier":{"go_root":"~/.local/go/go"}}`)
	code, _, errOut := configRun(t, env, "verifier", "--config", path, "--crw-dev", "tree")
	if code != 0 || !strings.Contains(errOut, "verifier.go_root") {
		t.Errorf("a record that leaves the unusable key: exit %d stderr %q", code, errOut)
	}
	// (the file also carries the manage.child_check default the installation writes, CRW-1398, so only the verifier section is read)
	var doc struct {
		Verifier map[string]string `json:"verifier"`
	}
	data, _ := os.ReadFile(path)
	if json.Unmarshal(data, &doc) != nil || doc.Verifier["go_root"] != "~/.local/go/go" || doc.Verifier["crw_dev"] != "tree" {
		t.Errorf("the file: %s", data)
	}
}
