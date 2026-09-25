package contracttest

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Test_mcp_isolated_launch_and_followup_are_durable is test_worktree.py's
// test_mcp_isolated_launch_and_followup_are_durable, deferred to this todo: through two
// separate `crw bridge` processes sharing one ledger, a worktree launch and its follow-up are
// accepted once, the second process replays the launch instead of repeating it, get_operation
// returns the same worktree, and the host holds one thread whose turns carry the exact prompt
// and the follow-up.
func Test_mcp_isolated_launch_and_followup_are_durable(t *testing.T) {
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	// Under /dev/shm like the other worktree tests: the destination must be outside every
	// repository, and a shared /tmp may itself sit inside one.
	root, err := os.MkdirTemp("/dev/shm", "crw-mcp-worktree-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	gitValue(t, ctx, source, "init")
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(source, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked", "base\n")
	write(".gitignore", "ignored\n")
	gitValue(t, ctx, source, "add", ".")
	gitValue(t, ctx, source, "commit", "-m", "base")
	revision := gitValue(t, ctx, source, "rev-parse", "HEAD")
	write("tracked", "staged\n")
	gitValue(t, ctx, source, "add", "tracked")
	write("tracked", "unstaged\n")
	write("untracked", "untracked\n")
	write("ignored", "ignored\n")

	host := startMCPHost(t, nil, nil, false)
	prompt := "  exact\ninitial  "
	arguments := map[string]any{
		"request_id": "isolated", "source_repository": source, "starting_revision": revision,
		"destination": filepath.Join(root, "isolated"), "worktree_mode": "bridge-managed-retained",
		"sandbox": "read-only", "expected_sandbox_policy": map[string]any{"type": "readOnly", "networkAccess": false},
		"prompt": prompt, "model": "explicit-model", "reasoning_effort": "high",
	}
	receipts := []map[string]any{}
	for range 2 {
		cmd := commandFor(binary, host.server.SocketPath, filepath.Join(root, "mcp-state"), root)
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		session, err := sdk.NewClient(&sdk.Implementation{Name: "worktree-test", Version: "0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		result, err := session.CallTool(callCtx, &sdk.CallToolParams{Name: "create_worktree_thread", Arguments: arguments})
		if err != nil || result.IsError {
			t.Fatalf("create_worktree_thread: %v %v", err, result)
		}
		receipt := asObject(mustJSON(t, result.StructuredContent))
		if receipt["status"] != "accepted" {
			t.Fatalf("receipt %v", receipt)
		}
		receipts = append(receipts, receipt)
		followup, err := session.CallTool(callCtx, &sdk.CallToolParams{Name: "send_message_to_thread", Arguments: map[string]any{
			"request_id": "followup", "thread_id": receipt["threadId"], "message": "FOLLOWUP",
			"expected_settings": map[string]any{"model": "explicit-model", "reasoning_effort": "high"},
		}})
		if err != nil || asObject(mustJSON(t, followup.StructuredContent))["status"] != "accepted" {
			t.Fatalf("followup: %v %v", err, followup)
		}
		state, err := session.CallTool(callCtx, &sdk.CallToolParams{Name: "get_operation", Arguments: map[string]any{"request_id": "isolated"}})
		if err != nil || !reflect.DeepEqual(asObject(mustJSON(t, state.StructuredContent))["worktree"], receipt["worktree"]) {
			t.Fatalf("get_operation: %v %v", err, state)
		}
		cancel()
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if receipts[1]["replayed"] != true {
		t.Fatalf("second launch was not a replay: %v", receipts[1])
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.threads()) != 1 {
		t.Fatalf("threads %v", host.threads())
	}
	texts := []any{}
	for _, turn := range host.thread(receipts[0]["threadId"])["turns"].([]any) {
		texts = append(texts, asObject(asObject(turn)["items"].([]any)[0])["text"])
	}
	if !reflect.DeepEqual(texts, []any{prompt, "FOLLOWUP"}) {
		t.Fatalf("turn texts %q", texts)
	}
	if asObject(receipts[0]["creation"])["reasoningEffort"] != "high" {
		t.Fatalf("creation %v", receipts[0]["creation"])
	}
}

func mustJSON(t *testing.T, value any) any {
	t.Helper()
	out, err := jsonValue(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
