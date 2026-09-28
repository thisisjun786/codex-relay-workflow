package skill

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func call(args []string, input string) (int, string, string) {
	var out, err bytes.Buffer
	code := Run(args, strings.NewReader(input), &out, &err)
	return code, out.String(), err.String()
}
func TestSkillFixtureReplays(t *testing.T) {
	for _, args := range [][]string{{"hook-probe", "replay"}, {"parent-title", "replay"}, {"start-policy", "selftest"}} {
		code, out, err := call(args, "")
		if code != 0 {
			t.Fatalf("%v: %d\n%s\n%s", args, code, out, err)
		}
	}
}

func TestSkillLivePythonReplayParity(t *testing.T) {
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	tests := []struct {
		name         string
		script       string
		args         []string
		fixtureDir   string
		fixtureCount int
		countText    string
	}{
		{name: "hook_probe_121_decisions", script: "hook_probe.py", args: []string{"hook-probe", "replay"}, fixtureDir: "decisions", fixtureCount: 121, countText: "121/121 fixtures matched"},
		{name: "parent_title_40_titles", script: "parent_title.py", args: []string{"parent-title", "replay"}, fixtureDir: "titles", fixtureCount: 40, countText: "Replayed 40 title fixtures"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths, err := filepath.Glob(filepath.Join(repositoryRoot(), "plugins", defaultFixture(test.fixtureDir), "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != test.fixtureCount {
				t.Fatalf("%s fixture count = %d, want %d", test.fixtureDir, len(paths), test.fixtureCount)
			}

			root := repositoryRoot()
			command := exec.Command(filepath.Join(root, ".venv", "bin", "python"), filepath.Join(root, "plugins", "crw", "skills", "crw-run", "scripts", test.script), "replay")
			var pythonOut, pythonErr bytes.Buffer
			command.Stdout, command.Stderr = &pythonOut, &pythonErr
			pythonExit := 0
			if err := command.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				pythonExit = exit.ExitCode()
			}

			goExit, goOut, goErr := call(test.args, "")
			if pythonExit != goExit || pythonOut.String() != goOut || pythonErr.String() != goErr {
				t.Fatalf("live Python mismatch\npython exit=%d stdout=%q stderr=%q\ngo exit=%d stdout=%q stderr=%q", pythonExit, pythonOut.String(), pythonErr.String(), goExit, goOut, goErr)
			}
			if pythonExit != 0 || !strings.Contains(pythonOut.String(), test.countText) {
				t.Fatalf("live Python replay did not prove %s: exit=%d stdout=%q stderr=%q", test.countText, pythonExit, pythonOut.String(), pythonErr.String())
			}
		})
	}
}

func TestParentTitleMutationFails(t *testing.T) {
	src := filepath.Join(repositoryRoot(), "plugins", defaultFixture("titles"), "family-crw-verbatim.json")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "[CRW] 설치형 플러그인 전환", "[WRONG] 설치형 플러그인 전환", 1)
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "wrong.json"), []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := call([]string{"parent-title", "replay", "--fixtures", dir, "--allow-unreached"}, "")
	if code != 1 || !strings.Contains(stderr, "wrong.json: title expected") {
		t.Fatalf("mutation survived: %d %q", code, stderr)
	}
}
func TestParentTitleCommands(t *testing.T) {
	request := `{"role":"parent","binding_verified":true,"project_labels":["CRW"],"family_candidates":["CRW"],"observed_title":"설치형 플러그인 전환","user_title":"none"}`
	code, out, err := call([]string{"parent-title", "decide"}, request)
	if code != 0 || err != "" || !strings.Contains(out, "[CRW] 설치형 플러그인 전환") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	code, out, _ = call([]string{"parent-title", "readback"}, `{"requested_title":"x","observed_title":null}`)
	if code != 1 || !strings.Contains(out, "unread") {
		t.Fatalf("%d %q", code, out)
	}
}
func TestStartPolicyCheck(t *testing.T) {
	code, out, err := call([]string{"start-policy", "check"}, "run_mode: loop\nobservation_path: blocked\n")
	if code != 0 || err != "" || !strings.Contains(out, "pairing: loop + blocked -> legal") {
		t.Fatalf("%d %q %q", code, out, err)
	}
}
