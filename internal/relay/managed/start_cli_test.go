package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

func Test27_MST_9_CLIRejectsUnknownFieldBeforeHostOrStore(t *testing.T) {
	dir := t.TempDir()
	raw := requestFixture(t)
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	request["overridePermissions"] = true
	invalid, _ := json.Marshal(request)
	var output, errors bytes.Buffer
	code := cli.ExecuteAs(context.Background(), "codex-session-relay", []string{"--state", filepath.Join(dir, "absent"), "--socket", filepath.Join(dir, "socket"), "managed-start", "--request", string(invalid), "--marker-root", filepath.Join(dir, "markers")}, &output, &errors)
	if code != 4 || !strings.Contains(output.String(), "unknown") {
		t.Fatalf("usage exit %d: %s %s", code, &output, &errors)
	}
	if _, err := os.Stat(filepath.Join(dir, "absent", "relay.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("invalid input created store: %v", err)
	}
}
func Test27_MST_9_MissingWorkerRefusesBeforeStoreCreation(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "absent")
	raw := requestFixture(t)
	var out, stderr bytes.Buffer
	code := cli.ExecuteAs(context.Background(), "codex-session-relay", []string{"--state", state, "--socket", filepath.Join(dir, "socket"), "managed-start", "--request", string(raw), "--marker-root", filepath.Join(dir, "markers")}, &out, &stderr)
	var answer map[string]any
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if code != 2 || answer["state"] != "refused" || answer["stage"] != "preflight" || answer["reason"] != "worker_policy_unreadable" {
		t.Fatalf("missing worker: %d %s %s", code, &out, &stderr)
	}
	home, err := os.MkdirTemp("/dev/shm", "crw-managed-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	argv := []string{"--state", state, "--socket", filepath.Join(dir, "socket"), "managed-start", "--request", string(raw), "--marker-root", filepath.Join(dir, "markers")}
	cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, argv...)...)
	cmd.Dir = filepath.Clean(filepath.Join("../../.."))
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm")
	py, pyErr := cmd.Output()
	if exit, ok := pyErr.(*exec.ExitError); !ok || exit.ExitCode() != code || !bytes.Equal(py, out.Bytes()) {
		t.Fatalf("Go exit=%d stdout=%q; Python err=%v stdout=%q", code, out.Bytes(), pyErr, py)
	}
	if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("missing worker created store: %v", err)
	}
}
func Test27_MST_9_MissingStateOrSocketUsage(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "CODEX_HOME"} {
		t.Setenv(key, root)
	}
	for _, tc := range []struct {
		name      string
		selectors []string
	}{
		{"missing-state", []string{"--socket", filepath.Join(root, "socket")}},
		{"missing-socket", []string{"--state", filepath.Join(root, "state")}},
		{"both-missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.selectors...), "managed-start", "--request", "{}", "--marker-root", filepath.Join(root, "markers"))
			var output, errors bytes.Buffer
			code := cli.ExecuteAs(context.Background(), "codex-session-relay", args, &output, &errors)
			home, err := os.MkdirTemp("/dev/shm", "crw-managed-cli-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(home)
			cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, args...)...)
			_, file, _, _ := runtime.Caller(0)
			cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm")
			py, pyErr := cmd.CombinedOutput()
			pyCode := 0
			if pyErr != nil {
				if exit, ok := pyErr.(*exec.ExitError); ok {
					pyCode = exit.ExitCode()
				} else {
					t.Fatal(pyErr)
				}
			}
			if code != pyCode || !bytes.Equal(output.Bytes(), py) || errors.Len() != 0 {
				t.Fatalf("Go exit=%d stdout=%q stderr=%q; Python exit=%d stdout=%q", code, output.Bytes(), errors.Bytes(), pyCode, py)
			}
		})
	}
}
