package bridge

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

func TestCreateThreadRefusesAnUnworkableChildSandbox(t *testing.T) {
	for _, policy := range []map[string]any{nil, {"type": "readOnly", "networkAccess": false}} {
		b, host := testBridge(t)
		var err error
		b.Policy, err = execution.FromBytes([]byte(`{"roles":{"child":{"model":"explicit-model","reasoningEffort":"high"}}}`), "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		in := CreateThread{RequestID: "readonly-child", CWD: t.TempDir(), Sandbox: "read-only", Model: "explicit-model", Effort: "high", Role: "child", Policy: policy}
		if policy != nil {
			in.Sandbox = "workspace-write"
		}
		_, err = b.CreateThread(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "recreate") || !strings.Contains(err.Error(), "writable sandbox") {
			t.Fatalf("expected actionable readOnly child refusal, got %v", err)
		}
		if host.Count("thread/start") != 0 || host.Count("turn/start") != 0 {
			t.Fatalf("refused child reached host: %v", host.Requests())
		}
	}
}

// CRW-1286: create_worktree_thread is the other bridge path that creates a relay child; it refuses a readOnly child the
// same way, before the checkout exists or the host is asked.
func TestCreateWorktreeThreadRefusesAReadOnlyChild(t *testing.T) {
	b, host := policyBridge(t, `{"roles":{"child":{"model":"explicit-model","reasoningEffort":"high"}}}`)
	input := worktreeInput(t)
	input.Role = "child"
	_, err := b.CreateWorktreeThread(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "recreate") || !strings.Contains(err.Error(), "writable sandbox") || len(hostMethods(host)) != 0 {
		t.Fatalf("expected actionable readOnly child refusal before the host, got %v, calls %v", err, host.Requests())
	}
	if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
		t.Fatalf("refused child's checkout created: %v", err)
	}
}
