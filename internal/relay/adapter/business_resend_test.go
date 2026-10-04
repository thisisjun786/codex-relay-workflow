package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// Production engine, bridge, adapter and durable ledgers against a synthetic
// socket. The scripted external load loses the profile after the standby.
func TestBusinessResendProfileFailureOverSocket(t *testing.T) {
	root := t.TempDir()
	workspace, state := filepath.Join(root, "work"), filepath.Join(root, "state")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(root, "scopes"))
	policy, err := execution.FromBytes([]byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium","mcp":{"default":"minimal","profiles":{"minimal":{"servers":[]}}}}}}`), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	child := map[string]any{}
	for key, value := range settings {
		child[key] = value
	}
	child["mcpProfile"] = "minimal"
	request := map[string]any{"schema": managed.Schema, "requestId": "resend-socket", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Synthetic resend", "settings": child}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "one business turn", "required": true}}, "criteriaSource": "synthetic", "baselineRevision": "base", "scopeRef": "synthetic", "prompt": "synthetic business"}
	response := func() map[string]any {
		r := map[string]any{}
		for key, value := range settings {
			if key != "environments" {
				r[key] = value
			}
		}
		r["thread"] = map[string]any{"id": "resend-child", "environments": settings["environments"]}
		return r
	}
	host := fakehost.Start(t)
	var mu sync.Mutex
	load, applied, turnCount := "idle", true, 0
	foreign, failFinalRead := false, false
	resumes := []map[string]any{}
	host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"oracle": map[string]any{}, "node_repl": map[string]any{}}}}})
	host.Respond("plugin/installed", fakehost.Reply{Result: map[string]any{"marketplaces": []any{}}})
	host.Respond("thread/start", fakehost.Reply{Result: response()})
	host.Respond("thread/name/set", fakehost.Reply{})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		status := "connected"
		if applied {
			status = "disabled"
		}
		return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"name": "oracle", "pluginId": nil, "runtimeStatus": status}, map[string]any{"name": "node_repl", "pluginId": nil, "runtimeStatus": status}}, "nextCursor": nil}}
	})
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		if failFinalRead {
			return fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "synthetic read failure"}}
		}
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "resend-child", "status": map[string]any{"type": load}, "canAcceptDirectInput": true}}}
	})
	host.Handle("thread/resume", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		resumes = append(resumes, params)
		if load == "notLoaded" {
			applied = true
		}
		load = "idle"
		if foreign {
			failFinalRead = true
		}
		return fakehost.Reply{Result: response()}
	})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		turnCount++
		id := "standby"
		if turnCount == 1 {
			applied = false
		} else {
			id = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": id}}}
	})
	host.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		rows := []any{map[string]any{"id": "standby", "status": "completed"}}
		if turnCount > 1 {
			rows = append(rows, map[string]any{"id": "business", "status": "completed"})
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		rows := []any{}
		if params["archived"] != true {
			rows = append(rows, map[string]any{"id": "resend-child"})
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
	s, err := openStore(t.Context(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := Open(host.SocketPath, state, Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	engine := managed.Start{Store: s, Adapter: Managed{a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: filepath.Join(root, "markers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	run := func() map[string]any {
		t.Helper()
		out, err := engine.Run(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return plain(out).(map[string]any)
	}
	first := run()
	if first["reason"] != "business_failed" || host.Count("turn/start") != 1 {
		t.Fatalf("initial profile refusal: %v", first["reason"])
	}
	_, business := managed.OperationIDs("resend-socket")
	failure, err := a.ledger.Get(t.Context(), business)
	if err != nil {
		t.Fatal(err)
	}
	if failure["status"] != "failed" || failure["turnId"] != nil || failure["attemptedEffects"] != nil || failure["delivery"] != nil {
		t.Fatalf("legacy shape changed: %v", failure["status"])
	}
	before, _ := json.Marshal(failure)
	for range 2 {
		if got := run(); got["reason"] != "recipient_not_idle" {
			t.Fatalf("loaded retry: %v", got["reason"])
		}
	}
	if host.Count("thread/resume") != 1 || host.Count("turn/start") != 1 {
		t.Fatal("loaded retry consumed business operation")
	}
	// A live-context final host-read failure becomes a retry-safe guard refusal.
	mu.Lock()
	load, foreign = "notLoaded", true
	mu.Unlock()
	if got := run(); got["reason"] != "business_not_attempted" {
		t.Fatalf("final read failure was consumed: %v", got["reason"])
	}
	mu.Lock()
	load, foreign, failFinalRead = "notLoaded", false, false
	mu.Unlock()
	// Reconnect with the same durable ledgers, as after a runtime replacement.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(host.SocketPath, state, Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	engine.Adapter = Managed{a}
	admitted := run()
	if admitted["state"] != "admitted" || admitted["businessRequestId"] != business || admitted["businessTurnId"] != "business" {
		t.Fatalf("unloaded retry: %v", admitted)
	}
	for range 3 {
		if got := run(); !reflect.DeepEqual(got, admitted) {
			t.Fatal("admission replay changed")
		}
	}
	afterReceipt, err := a.ledger.Get(t.Context(), business)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(afterReceipt)
	if string(before) != string(after) || host.Count("turn/start") != 2 || host.Count("thread/start") != 1 {
		t.Fatal("original failure changed or duplicate turn started")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, resume := range resumes {
		config := resume["config"].(map[string]any)
		for _, server := range []string{"oracle", "node_repl"} {
			if config["mcp_servers"].(map[string]any)[server].(map[string]any)["enabled"] != false {
				t.Fatal("recorded profile was not transmitted")
			}
		}
	}
}
