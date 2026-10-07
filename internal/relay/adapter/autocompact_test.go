package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// autoCompactManagedPolicy is the child pair a managed start creates under, carrying the limit this
// issue recommends. Managed start reaches the bridge's settings contract, so the value the operator
// puts on the pair is what the creation sends.
const autoCompactManagedPolicy = `{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium","autoCompactTokenLimit":550000}}}`

// autoCompactHostParams returns the parameters of the first request that used method.
func autoCompactHostParams(t *testing.T, host *fakehost.Server, method string) map[string]any {
	t.Helper()
	for _, request := range host.Requests() {
		if request.Method != method {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		return params
	}
	t.Fatalf("no %s request", method)
	return nil
}

// A managed start whose child pair declares the limit carries it in the creation's thread/start
// config, which is the path an IF DeepSeek child is created through.
func TestManagedStartSendsTheChildPairsAutoCompactLimit(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(root, "scopes"))
	workspace := filepath.Join(root, "work")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, []byte(autoCompactManagedPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	policy, err := execution.FromBytes([]byte(autoCompactManagedPolicy), policyPath)
	if err != nil {
		t.Fatal(err)
	}
	declared := func() map[string]any {
		return map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-auto-compact", "issueKey": "CRW-852", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": declared()}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": declared()}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:CRW-852", "baselineRevision": "baseline", "scopeRef": "issue:CRW-852", "prompt": "business-secret"}
	host := fakehost.Start(t)
	var mu sync.Mutex
	turns := 0
	response := func() map[string]any {
		answer := map[string]any{}
		for key, value := range declared() {
			if key != "environments" {
				answer[key] = value
			}
		}
		answer["thread"] = map[string]any{"id": "managed-child", "environments": declared()["environments"]}
		return answer
	}
	host.Respond("thread/start", fakehost.Reply{Result: response()})
	host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		turns++
		id := "standby"
		if turns > 1 {
			id = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": id}}}
	})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium", "cwd": workspace}}})
	host.Respond("thread/resume", fakehost.Reply{Result: response()})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "standby", "status": "completed"}}, "nextCursor": nil}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		data := []any{}
		if params["archived"] != true {
			data = append(data, map[string]any{"id": "managed-child"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	store, err := openStore(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(host.SocketPath, state, Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); store.Close() })
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	engine := managed.Start{Store: store, Adapter: Managed{a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: filepath.Join(root, "markers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := engine.Run(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.Get("state") != "admitted" {
		business, _ := a.GetOperation(context.Background(), pyjson.Text(result.Get("businessRequestId")))
		t.Fatalf("managed start did not admit: %s business=%v", dumps(result, false), business)
	}
	config, _ := autoCompactHostParams(t, host, "thread/start")["config"].(map[string]any)
	if config == nil || config[settings.AutoCompactTokenLimitKey] != float64(550000) {
		t.Fatalf("thread/start config = %v", config)
	}
}
