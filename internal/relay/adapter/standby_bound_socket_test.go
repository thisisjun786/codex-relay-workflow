package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The standby bound over the real socket (CRW-1211): the host answers the standby turn's read later than the bound, so the
// App Server client ends the read at the bound's deadline with its own PhaseTimeout. That is the bound cutting the read,
// not a host failure: the call answers standby_incomplete with its receipt instead of an error, and sends no business turn.
func TestManagedStandbyBoundCutsSlowReadOverSocket(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(root, "scopes"))
	workspace := filepath.Join(root, "work")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policyRaw := []byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`)
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, policyRaw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	policy, err := execution.FromBytes(policyRaw, policyPath)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"sandbox": relayChildSandbox(), "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-standby-bound", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settings}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	host := fakehost.Start(t)
	response := map[string]any{"thread": map[string]any{"id": "managed-child", "environments": settings["environments"]}}
	for k, v := range settings {
		if k != "environments" {
			response[k] = v
		}
	}
	var mu sync.Mutex
	turns := 0
	host.Respond("thread/start", fakehost.Reply{Result: response})
	host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		turns++
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "standby"}}}
	})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium", "cwd": workspace}}})
	host.Respond("thread/resume", fakehost.Reply{Result: response})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	// The standby turn's read is answered long after the bound.
	host.Respond("thread/turns/list", fakehost.Reply{Delay: 5 * time.Second, Result: map[string]any{"data": []any{map[string]any{"id": "standby", "status": "completed"}}, "nextCursor": nil}})
	host.Respond("thread/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "managed-child"}}, "nextCursor": nil}})
	s, err := openStore(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := Open(host.SocketPath, state, Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	engine := managed.Start{Store: s, Adapter: Managed{a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: filepath.Join(root, "markers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }, StandbyWait: 150 * time.Millisecond}
	started := time.Now()
	result, err := engine.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("the bound cut the standby read; that is standby_incomplete, not an error: %T %v", err, err)
	}
	if result.Get("state") != "incomplete" || result.Get("reason") != "standby_incomplete" || time.Since(started) > 4*time.Second {
		t.Fatalf("standby_incomplete at the bound: %s after %v", dumps(result, false), time.Since(started))
	}
	if host.Count("thread/turns/list") < 1 {
		t.Fatal("the standby turn was read over the socket")
	}
	mu.Lock()
	defer mu.Unlock()
	if turns != 1 {
		t.Fatalf("only the standby turn was started: %d", turns)
	}
}
