package bridge

import (
	"context"
	"fmt"
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

// CRW-1286: a relay child reports only through the relay daemon's control.sock, and Codex refuses that connect (EPERM) in
// a workspace-write sandbox without network access as in a read-only one (codex-cli 0.154.0). Such a child is refused
// at creation on both bridge paths, naming the sandboxes that reach the daemon; those two pass the refusal.
func TestCreateThreadRefusesARelayChildThatCannotReachTheDaemon(t *testing.T) {
	workspace := func(network bool) map[string]any {
		return map[string]any{"type": "workspaceWrite", "networkAccess": network, "writableRoots": []any{}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	}
	for _, c := range []struct {
		sandbox string
		policy  map[string]any
		refused bool
	}{
		{"workspace-write", nil, true},
		{"workspace-write", workspace(false), true},
		{"workspace-write", workspace(true), false},
		{"danger-full-access", nil, false},
		{"danger-full-access", map[string]any{"type": "dangerFullAccess"}, false},
	} {
		t.Run(fmt.Sprint(c.sandbox, c.policy), func(t *testing.T) {
			b, host := policyBridge(t, `{"roles":{"child":{"model":"explicit-model","reasoningEffort":"high"}}}`)
			cwd := t.TempDir()
			started := startReply(cwd)
			started.Result["sandbox"] = map[string]any{"type": "dangerFullAccess"}
			if c.policy != nil {
				started.Result["sandbox"] = c.policy
			}
			host.Respond("thread/start", started)
			_, err := b.CreateThread(context.Background(), CreateThread{RequestID: "child", CWD: cwd, Sandbox: c.sandbox, Model: "explicit-model", Effort: "high", Role: "child", Policy: c.policy})
			outcomes := []struct {
				err    error
				starts int
			}{{err, host.Count("thread/start")}}
			// create_worktree_thread requires the complete expected policy.
			if c.policy != nil {
				worktree, wHost := policyBridge(t, `{"roles":{"child":{"model":"explicit-model","reasoningEffort":"high"}}}`)
				input := worktreeInput(t)
				input.Role, input.Sandbox, input.Policy = "child", c.sandbox, c.policy
				worktreeHost(wHost, input.Destination, c.policy)
				_, wErr := worktree.CreateWorktreeThread(context.Background(), input)
				outcomes = append(outcomes, struct {
					err    error
					starts int
				}{wErr, wHost.Count("thread/start")})
			}
			for _, got := range outcomes {
				refusal := got.err != nil && strings.Contains(got.err.Error(), "daemon's socket")
				if c.refused != refusal || c.refused != (got.starts == 0) {
					t.Fatalf("refused=%v: got %v with %d thread/start", c.refused, got.err, got.starts)
				}
				if c.refused && (!strings.Contains(got.err.Error(), "recreate") || !strings.Contains(got.err.Error(), "writable sandbox") || !strings.Contains(got.err.Error(), "danger-full-access") || !strings.Contains(got.err.Error(), "networkAccess")) {
					t.Fatalf("refusal does not name the sandboxes that reach the daemon: %v", got.err)
				}
			}
		})
	}
}
