package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The built CLI refuses a project with no bound parent exactly as before, in the same words and
// with the same exit code, but before the app-server is asked to start a thread; the same request
// then creates its one child once the parent is bound.
func Test28_ManagedStartRefusesAnUnboundProjectBeforeCreation(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	scope := filepath.Join(root, "scopes")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scope)
	workspace := filepath.Join(root, "work")
	marker := filepath.Join(root, "markers")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, policyPath)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	settings := map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-unbound", "issueKey": "REL-MANAGED", "projectKey": "P-SCOPE", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settings}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	host := fakehost.Start(t)
	response := func() map[string]any {
		r := map[string]any{}
		for k, v := range settings {
			if k != "environments" {
				r[k] = v
			}
		}
		r["thread"] = map[string]any{"id": "managed-child", "environments": settings["environments"]}
		return r
	}
	var mu sync.Mutex
	turns := 0
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
	s, err := openStore(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	publishWorker(t, state, host.SocketPath, scope, filepath.Dir(suiteBinary))
	start := func() (string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(suiteBinary, "relay", "--state", state, "--socket", host.SocketPath, "managed-start", "--request", string(raw), "--marker-root", marker)
		cmd.Env = append(os.Environ(), "CODEX_SESSION_RELAY_SCOPE_DIR="+scope)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("managed-start: %v %s", err, &stderr)
			}
			code = exit.ExitCode()
		}
		return stdout.String(), code
	}

	out, code := start()
	want := "{\n  \"error\": \"refused\",\n  \"reason\": \"unregistered_scope\",\n  \"detail\": \"project 'P-SCOPE' has no registered parent, so an issue cannot be attached to it yet\"\n}\n"
	if code != contract.ExitRefused || out != want {
		t.Fatalf("unbound project: exit %d stdout %q", code, out)
	}
	if n := host.Count("thread/start"); n != 0 {
		t.Fatalf("the refused request asked the app-server to start %d threads", n)
	}

	s, err = openStore(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := &registry.Registry{Store: s, Now: registry.SystemISO}
	if _, err := reg.BindScopeAs(context.Background(), "parent", "P-SCOPE", registry.Endpoint{TaskID: "parent", HostID: "host"}, registry.Active); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	out, code = start()
	var admitted map[string]any
	if err := json.Unmarshal([]byte(out), &admitted); err != nil {
		t.Fatalf("bound project: %v %q", err, out)
	}
	if code != contract.ExitOk || admitted["state"] != "admitted" || admitted["childTaskId"] != "managed-child" {
		t.Fatalf("bound project: exit %d stdout %q", code, out)
	}
	if n := host.Count("thread/start"); n != 1 {
		t.Fatalf("the retried request started %d threads, want 1", n)
	}
}
