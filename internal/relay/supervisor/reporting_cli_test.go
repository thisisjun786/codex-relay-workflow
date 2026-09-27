package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The fixture runner also executes these inputs; this test pins the whole public JSON
// against the Python binary under separate, isolated state roots.
func reportingCLIParity(t *testing.T, argv func(string) []string) (int, map[string]any, string, bool) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "crw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/crw")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	relayBinary := filepath.Join(binDir, "codex-session-relay")
	if err := os.Symlink(binary, relayBinary); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code    int
		value   map[string]any
		stderr  string
		created bool
	}
	run := func(python bool) answer {
		home := t.TempDir()
		state := filepath.Join(home, "absent-state")
		args := argv(home)
		for i, v := range args {
			args[i] = strings.ReplaceAll(v, "$STATE", state)
		}
		var command *exec.Cmd
		if python {
			command = exec.Command("uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, args...)...)
			command.Dir = filepath.Join(repo, "packages/codex-session-relay")
		} else {
			command = exec.Command(relayBinary, args...)
		}
		command.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir())
		out, err := command.Output()
		code := 0
		stderr := ""
		if err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = exit.ExitCode()
			stderr = string(exit.Stderr)
		}
		var value map[string]any
		if len(out) > 0 && json.Unmarshal(out, &value) != nil {
			t.Fatalf("bad JSON: %s", out)
		}
		_, stat := os.Stat(state)
		return answer{code, value, stderr, stat == nil}
	}
	py, goResult := run(true), run(false)
	if py.code != goResult.code || !reflect.DeepEqual(py.value, goResult.value) || py.created != goResult.created || py.stderr != goResult.stderr {
		t.Errorf("Go exit=%d JSON=%s stderr=%q state=%t; Python exit=%d JSON=%s stderr=%q state=%t", goResult.code, jsonText(goResult.value), goResult.stderr, goResult.created, py.code, jsonText(py.value), py.stderr, py.created)
	}
	return goResult.code, goResult.value, goResult.stderr, goResult.created
}
func Test24_RCL_2_SelectorRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv func(string) []string
		want string
	}{
		{"missing-state", func(home string) []string {
			return []string{"reporting-show", "--marker-root", home + "/markers", "--workspace", home + "/workspace", "--assignment", strings.Repeat("a", 64), "--session", "session-1", "--turn", "turn-1"}
		}, "--state"},
		{"socket", func(home string) []string {
			return []string{"--state", "$STATE", "--socket", home + "/app.sock", "reporting-show", "--marker-root", home + "/markers", "--workspace", home + "/workspace", "--assignment", strings.Repeat("a", 64), "--session", "session-1", "--turn", "turn-1"}
		}, "--socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, value, _, created := reportingCLIParity(t, tc.argv)
			if code != 4 || created || !strings.Contains(value["detail"].(string), tc.want) {
				t.Fatalf("exit=%d value=%s created=%t", code, jsonText(value), created)
			}
		})
	}
}
