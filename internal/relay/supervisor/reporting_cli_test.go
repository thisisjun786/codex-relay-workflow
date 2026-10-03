package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The fixture runner also executes these inputs; this test pins the whole public JSON the built
// binary answers under an isolated state root against the golden.
func reportingCLIParity(t *testing.T, argv func(string) []string) (int, map[string]any, string, bool) {
	t.Helper()
	binDir := t.TempDir()
	relayBinary := filepath.Join(binDir, "codex-session-relay")
	if err := os.Symlink(testsupport.CRW(t), relayBinary); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	state := filepath.Join(home, "absent-state")
	args := argv(home)
	for i, v := range args {
		args[i] = strings.ReplaceAll(v, "$STATE", state)
	}
	command := exec.Command(relayBinary, args...)
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
	created := stat == nil
	golden.CheckJSON(t, "reporting-cli", map[string]any{"code": code, "value": value, "stderr": stderr, "created": created}, golden.Substitute(home, "<home>"), golden.Substitute(repoRoot(t), "<repo>"))
	return code, value, stderr, created
}
func Test24_RCL_2_SelectorRefusals(t *testing.T) {
	t.Parallel()
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
