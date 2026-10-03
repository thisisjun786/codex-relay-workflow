package adapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// profileStart is what one managed start under a custom permission profile left behind.
type profileStart struct {
	state, stage, reason string
	record               map[string]any // the child's stored settings
	creation             map[string]any // the creation response the bridge's ledger retained
	turns                int            // turn/start calls the host saw: the standby, then the business turn
}

// startUnderPermissionProfile runs managed-start over the production bridge, ledger and a fake App
// Server socket, with both roles' records naming requested as their expectedPermissionProfile. The host
// reports created as the new thread's activePermissionProfile and resumed as the one a resume reports.
// A nil one is left out of that response, as a host that does not report a profile leaves it out.
func startUnderPermissionProfile(t *testing.T, requested, created, resumed any) profileStart {
	t.Helper()
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
	settings := func() map[string]any {
		return map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "gpt-5.4", "reasoningEffort": "medium", "environments": []any{map[string]any{"environmentId": "local", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}}}, "expectedPermissionProfile": requested}
	}
	request := map[string]any{"schema": managed.Schema, "requestId": "managed-real-profile", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settings()}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settings()}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"parent"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	host := fakehost.Start(t)
	var mu sync.Mutex
	turns := 0
	response := func(profile any) map[string]any {
		r := map[string]any{}
		for k, v := range settings() {
			if k != "environments" && k != "expectedPermissionProfile" {
				r[k] = v
			}
		}
		r["thread"] = map[string]any{"id": "managed-child", "environments": settings()["environments"]}
		if profile != nil {
			r["activePermissionProfile"] = profile
		}
		return r
	}
	host.Respond("thread/start", fakehost.Reply{Result: response(created)})
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
	host.Respond("thread/resume", fakehost.Reply{Result: response(resumed)})
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
	t.Cleanup(func() { a.Close(); s.Close() })
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	engine := managed.Start{Store: s, Adapter: Managed{a}, Now: delivery.NewFakeClock().ISO, Socket: host.SocketPath, MarkerRoot: filepath.Join(root, "markers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := engine.Run(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	got := profileStart{state: result.Get("state").(string)}
	got.stage, _ = result.Get("stage").(string)
	got.reason, _ = result.Get("reason").(string)
	mu.Lock()
	got.turns = turns
	mu.Unlock()
	var stored string
	switch err := s.DB.QueryRowContext(context.Background(), "SELECT settings FROM authorized_settings WHERE task_id = 'managed-child'").Scan(&stored); {
	case err == nil:
		if err := json.Unmarshal([]byte(stored), &got.record); err != nil {
			t.Fatal(err)
		}
	case !errors.Is(err, sql.ErrNoRows):
		t.Fatal(err)
	}
	create, _ := managed.OperationIDs("managed-real-profile")
	receipt, err := (Managed{a}).GetOperation(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	if receipt != nil {
		got.creation, _ = receipt["creation"].(map[string]any)
	}
	return got
}

// A custom profile object goes through the production path: the request carries it, the bridge's ledger
// retains the creation response that reports it, the record holds it, and the resume the business
// dispatch checks reports it back. The text, boolean and null members are the shapes the production
// decoding of a creation receipt could spell differently from the request's own.
func TestManagedStartPermissionProfileObjectOnTheProductionPath(t *testing.T) {
	profile := map[string]any{"id": "trusted r\u00e9sum\u00e9 \"q\" <&>", "extends": nil, "audited": true, "label": "caf\u00e9", "unset": nil}
	same := func() any {
		raw, err := json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
		var copied any
		if err := json.Unmarshal(raw, &copied); err != nil {
			t.Fatal(err)
		}
		return copied
	}
	other := map[string]any{"id": "trusted", "extends": nil}

	t.Run("a host reporting the recorded object is admitted", func(t *testing.T) {
		got := startUnderPermissionProfile(t, profile, same(), same())
		if got.state != "admitted" || got.turns != 2 {
			t.Fatalf("custom profile start: %+v", got)
		}
		if !reflect.DeepEqual(got.record["expectedPermissionProfile"], same()) {
			t.Fatalf("the child's record holds %v", got.record["expectedPermissionProfile"])
		}
		if !reflect.DeepEqual(got.creation["activePermissionProfile"], same()) {
			t.Fatalf("the ledger's creation response holds %v", got.creation["activePermissionProfile"])
		}
	})
	t.Run("a creation reporting another object is refused with no record", func(t *testing.T) {
		got := startUnderPermissionProfile(t, profile, other, other)
		if got.state != "refused" || got.stage != "creation" || got.reason != "creation_settings_unverified" || got.record != nil || got.turns != 1 {
			t.Fatalf("another profile at creation: %+v", got)
		}
	})
	t.Run("a resume reporting another object holds the business turn", func(t *testing.T) {
		got := startUnderPermissionProfile(t, profile, same(), other)
		if got.state != "incomplete" || got.stage != "business" || got.reason != "business_failed" || got.turns != 1 {
			t.Fatalf("another profile at resume: %+v", got)
		}
	})
}
