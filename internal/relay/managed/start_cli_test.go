package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
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
	golden.CheckJSON(t, "managed-start", map[string]any{"exit": code, "stdout": out.String()}, golden.Substitute(dir, "<dir>"))
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
		args := append(append([]string{}, tc.selectors...), "managed-start", "--request", "{}", "--marker-root", filepath.Join(root, "markers"))
		// The expected answer is read in the parent test, so the cases share one golden.
		want := golden.Want(t, tc.name, func() []byte {
			var output, errors bytes.Buffer
			code := cli.ExecuteAs(context.Background(), "codex-session-relay", args, &output, &errors)
			answer, err := golden.Encode(map[string]any{"exit": code, "output": output.String()})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}, golden.Substitute(root, "<root>"))
		var expected struct {
			Exit   int    `json:"exit"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal(want, &expected); err != nil {
			t.Fatal(err)
		}
		t.Run(tc.name, func(t *testing.T) {
			var output, errors bytes.Buffer
			code := cli.ExecuteAs(context.Background(), "codex-session-relay", args, &output, &errors)
			if code != expected.Exit || output.String() != expected.Output || errors.Len() != 0 {
				t.Fatalf("Go exit=%d stdout=%q stderr=%q; golden exit=%d stdout=%q", code, output.Bytes(), errors.Bytes(), expected.Exit, expected.Output)
			}
		})
	}
}
