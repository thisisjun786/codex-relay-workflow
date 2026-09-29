package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func Test28_ManagedSixMethodsRealSocket(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	// One host, one scope registry root: the store records the scope key of the root it is
	// created under, and the built CLI below serves it under that same root.
	scope := filepath.Join(root, "scopes")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", scope)
	workspace := filepath.Join(root, "work")
	marker := filepath.Join(root, "markers")
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
	settings := map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-real-host", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settings}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	host := fakehost.Start(t)
	var mu sync.Mutex
	turns := 0
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
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(host.SocketPath, state, Options{Policy: policy, Clock: delivery.NewFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	engine := managed.Start{Store: s, Adapter: Managed{a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: marker, StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := engine.Run(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if field(result, "state") != "admitted" {
		t.Fatalf("managed not admitted: %s", dumps(result, false))
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := contract.Emit(&got, result); err != nil {
		t.Fatal(err)
	}
	// Python replays the start from the store Go wrote, after a takeover; the built CLI then
	// replays it again after the store is taken back.
	testsupport.HandOver(t, filepath.Join(state, "relay.sqlite3"), "python")
	spec, err := json.Marshal(map[string]any{"state": state, "marker": marker, "socket": host.SocketPath, "request": request})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := filepath.Abs("../../..")
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/managed_capture.py"))
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(spec)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle %v %s", err, out)
	}
	if !bytes.Equal(got.Bytes(), out) {
		t.Fatalf("Go %s\nPython %s", &got, out)
	}
	testsupport.HandOver(t, filepath.Join(state, "relay.sqlite3"), "go")
	binary := suiteBinary
	publishWorker(t, state, host.SocketPath, scope, filepath.Dir(binary))
	cliCmd := exec.Command(binary, "relay", "--state", state, "--socket", host.SocketPath, "managed-start", "--request", string(raw), "--marker-root", marker)
	cliCmd.Env = append(os.Environ(), "CODEX_SESSION_RELAY_SCOPE_DIR="+scope)
	built, err := cliCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("managed CLI %v %s", err, built)
	}
	if !bytes.Equal(built, out) {
		t.Fatalf("built managed receipt %s Python %s", built, out)
	}
}
