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
	t.Parallel()
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
	t.Parallel()
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

// A managed start this build cannot carry out is a host error in Go's words, after the input and
// selector checks and before any store is opened: a socket that cannot be resolved, and a build
// that registers no host adapter (this package's tests register none).
func Test27_MST_9_AStartThisBuildCannotMakeIsAHostError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "relay.sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := requestFixture(t)
	for _, c := range []struct{ socket, detail string }{
		{"~crw_user_that_does_not_exist/socket", "the relay socket cannot be resolved: "},
		{filepath.Join(dir, "socket"), "this build registers no host adapter, so it cannot start a managed task"},
	} {
		var out, stderr bytes.Buffer
		code := cli.ExecuteAs(context.Background(), "codex-session-relay", []string{"--state", state, "--socket", c.socket, "managed-start", "--request", string(raw), "--marker-root", filepath.Join(dir, "markers")}, &out, &stderr)
		var answer map[string]any
		if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
			t.Fatalf("%s: %v %s", c.socket, err, &out)
		}
		detail, _ := answer["detail"].(string)
		if code != 3 || answer["error"] != "host" || !strings.HasPrefix(detail, c.detail) {
			t.Fatalf("%s: exit %d %s %s", c.socket, code, &out, &stderr)
		}
		if info, err := os.Stat(filepath.Join(state, "relay.sqlite3")); err != nil || info.Size() != 0 {
			t.Fatalf("%s: the store was opened: %v", c.socket, err)
		}
	}
}
