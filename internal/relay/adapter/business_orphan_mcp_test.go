package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// Production engine, bridge, adapter and durable ledgers against a synthetic socket: another client loads the
// first child without the MCP profile after its standby turn (the host keeps the servers on, and ignores the
// overrides of a resume on a loaded thread). The child holds nothing but its standby turn, so it is an orphan like one
// held under another model: archived once, created again under the same request, and the second child takes
// the business turn. No unload row is written or needed.
func TestMCPOnlyMismatchRecreatedOverSocket(t *testing.T) {
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
	settings := map[string]any{"sandbox": relayChildSandbox(), "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	child := map[string]any{}
	for key, value := range settings {
		child[key] = value
	}
	child["mcpProfile"] = "minimal"
	request := map[string]any{"schema": managed.Schema, "requestId": "mcp-orphan", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Synthetic orphan", "settings": child}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "one business turn", "required": true}}, "criteriaSource": "synthetic", "baselineRevision": "base", "scopeRef": "synthetic", "prompt": "synthetic business"}
	response := func(id string) map[string]any {
		r := map[string]any{}
		for key, value := range settings {
			if key != "environments" {
				r[key] = value
			}
		}
		r["thread"] = map[string]any{"id": id, "environments": settings["environments"]}
		return r
	}
	threadOf := func(raw json.RawMessage) string {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		id, _ := params["threadId"].(string)
		return id
	}
	host := fakehost.Start(t)
	var mu sync.Mutex
	created := 0
	external := map[string]bool{} // another client loaded the thread without the profile
	archived := map[string]bool{}
	business := map[string]bool{}
	var archives []string
	host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"oracle": map[string]any{}, "node_repl": map[string]any{}}}}})
	host.Respond("plugin/installed", fakehost.Reply{Result: map[string]any{"marketplaces": []any{}}})
	host.Handle("thread/start", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		created++
		return fakehost.Reply{Result: response(fmt.Sprintf("orphan-child-%d", created))}
	})
	host.Respond("thread/name/set", fakehost.Reply{})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Handle("mcpServerStatus/list", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		status := "disabled"
		if external[threadOf(raw)] {
			status = "connected"
		}
		return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"name": "oracle", "pluginId": nil, "runtimeStatus": status}, map[string]any{"name": "node_repl", "pluginId": nil, "runtimeStatus": status}}, "nextCursor": nil}}
	})
	host.Handle("thread/read", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		id := threadOf(raw)
		load := "idle"
		if archived[id] {
			load = "notLoaded"
		}
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": id, "status": map[string]any{"type": load}, "canAcceptDirectInput": true, "model": "gpt-5.4", "reasoningEffort": "medium"}}}
	})
	host.Handle("thread/resume", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		return fakehost.Reply{Result: response(threadOf(raw))}
	})
	host.Handle("thread/archive", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		archives = append(archives, threadOf(raw))
		archived[threadOf(raw)] = true
		return fakehost.Reply{Result: map[string]any{}}
	})
	host.Handle("turn/start", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		id := threadOf(raw)
		turn := "standby-" + id
		if _, started := business[id]; started {
			turn = "business-" + id
		}
		business[id] = true
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": turn}}}
	})
	host.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		id := threadOf(raw)
		rows := []any{map[string]any{"id": "standby-" + id, "status": "completed"}}
		if external[id] {
			return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
		}
		// The first listing of a created thread is the engine's wait for its standby turn: the other client loads it right after.
		if id == "orphan-child-1" {
			external[id] = true
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		rows := []any{}
		for i := 1; i <= created; i++ {
			if id := fmt.Sprintf("orphan-child-%d", i); archived[id] == (params["archived"] == true) {
				rows = append(rows, map[string]any{"id": id})
			}
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
	if first["state"] != "admitted" || first["childTaskId"] != "orphan-child-2" {
		t.Fatalf("the second child takes the business turn: %v/%v child %v", first["state"], first["reason"], first["childTaskId"])
	}
	mu.Lock()
	gotArchives, gotCreated := append([]string(nil), archives...), created
	mu.Unlock()
	if !reflect.DeepEqual(gotArchives, []string{"orphan-child-1"}) || gotCreated != 2 {
		t.Fatalf("one orphan archived, two creations: archives %v creations %d", gotArchives, gotCreated)
	}
	for range 2 {
		if again := run(); again["state"] != "admitted" || again["childTaskId"] != first["childTaskId"] || again["businessTurnId"] != first["businessTurnId"] || again["relationshipId"] != first["relationshipId"] {
			t.Fatalf("a repeat admits the same child and business turn: %v/%v child %v", again["state"], again["reason"], again["childTaskId"])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(archives) != 1 || created != 2 {
		t.Fatalf("repeats add no archive or creation: %v %d", archives, created)
	}
	var rows int
	if err := s.DB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM journal WHERE kind='managed_resend_unloaded'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("no unload row is written: %d %v", rows, err)
	}
	if !strings.HasPrefix(fmt.Sprint(first["businessTurnId"]), "business-") {
		t.Fatalf("the business turn went to the second child: %v", first["businessTurnId"])
	}
}
