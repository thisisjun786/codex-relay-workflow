package bridge

import (
	"context"
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
