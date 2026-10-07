package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// autoCompactManagedPolicy is the child pair a managed start creates under, carrying the limit this
// issue recommends. Managed start reaches the bridge settings contract, so the value the operator
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

// autoCompactChildPolicy is the child role a relay delivery is judged against: the pair the record
// states carries the limit and a second pair does not, so a record on that pair must not inherit its
// sibling's value. The pair's model is taken from the record itself, so this fixture cannot drift
// away from the settings the send is verifying.
func autoCompactChildPolicy(t *testing.T, record *delivery.TaskSettings) bridge.ExecutionPolicy {
	t.Helper()
	model, _ := record.Data.Lookup("model")
	body := fmt.Sprintf(`{"roles":{"child":{"pairs":[{"model":%q,"reasoningEffort":"xhigh","autoCompactTokenLimit":550000},{"model":"gpt-6.1-sol","reasoningEffort":"xhigh"}]}}}`, model)
	p, err := execution.FromBytes([]byte(body), "test")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A relay delivery resumes a child from its recorded settings, and that record cannot hold this value
// because the host never reports it. The limit is resolved from the policy by the pair the record
// states, so an unloaded child comes back under its pair's threshold rather than the host's window --
// on both routes, including the settings-free resume that transmits no pair.
func TestARelayDeliveryResumeCarriesTheRecordedPairsAutoCompactLimit(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(map[bool]string{false: "with-pair", true: "settings-free"}[free], func(t *testing.T) {
			record := childRecord(free, "")
			rpc := &mcpRPC{}
			a := mcpAdapter(t, rpc, autoCompactChildPolicy(t, record))
			if receipt := sendRecord(t, a, "send-auto-compact", record); receipt["status"] != "accepted" {
				t.Fatalf("receipt=%v", receipt)
			}
			if got := resumeConfig(rpc)[settings.AutoCompactTokenLimitKey]; got != int64(550000) {
				t.Fatalf("thread/resume config = %v", resumeConfig(rpc))
			}
		})
	}
}

// A record on the role's other pair sends no limit: the value belongs to the pair, and a pair that
// declares none must not inherit its sibling's.
func TestARelayDeliveryOnAPairWithoutALimitSendsNone(t *testing.T) {
	record := childRecord(false, "")
	record.Data = delivery.Obj(contract.OrderedObject(record.Data).Set("model", "gpt-6.1-sol"))
	rpc := &mcpRPC{model: "gpt-6.1-sol"}
	a := mcpAdapter(t, rpc, autoCompactChildPolicy(t, childRecord(false, "")))
	if receipt := sendRecord(t, a, "send-other-pair", record); receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v", receipt)
	}
	if _, present := resumeConfig(rpc)[settings.AutoCompactTokenLimitKey]; present {
		t.Fatalf("thread/resume config = %v", resumeConfig(rpc))
	}
}

// A record citing an exception names no pair, so no limit is derived for it.
func TestARelayDeliveryCitingAnExceptionSendsNoLimit(t *testing.T) {
	record := childRecord(false, "")
	record.Data = delivery.Obj(contract.OrderedObject(record.Data).Set("citedException", "one-task"))
	rpc := &mcpRPC{}
	a := mcpAdapter(t, rpc, autoCompactChildPolicy(t, record))
	if receipt := sendRecord(t, a, "send-excepted", record); receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v", receipt)
	}
	if _, present := resumeConfig(rpc)[settings.AutoCompactTokenLimitKey]; present {
		t.Fatalf("an exception carried a pair-derived limit: %v", resumeConfig(rpc))
	}
}

// A settings-free resume transmits no pair, and its refusal says so. When the limit did go out, the
// sentence has to name it: a reader who was told nothing was transmitted would go looking for a
// different cause, and the limit is exactly what this record could not have carried.
// autoCompactProfilePolicy is the child role the wording test needs: the record's pair carries the
// limit and the role declares the MCP profiles its record names.
func autoCompactProfilePolicy(t *testing.T, record *delivery.TaskSettings) bridge.ExecutionPolicy {
	t.Helper()
	model, _ := record.Data.Lookup("model")
	body := fmt.Sprintf(`{"roles":{"child":{"model":%q,"reasoningEffort":"xhigh","autoCompactTokenLimit":550000,"mcp":{"default":"minimal","profiles":{"minimal":{},"ui-qa":{"servers":["node_repl"]}}}}}}`, model)
	p, err := execution.FromBytes([]byte(body), "test")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestASettingsFreeRefusalNamesTheLimitItSent(t *testing.T) {
	for _, profile := range []string{"", "ui-qa"} {
		t.Run("profile="+profile, func(t *testing.T) {
			record := childRecord(true, profile)
			rpc := &mcpRPC{configured: []string{"node_repl", "oracle"}, applied: true, model: "other/model"}
			a := mcpAdapter(t, rpc, autoCompactProfilePolicy(t, record))
			receipt := sendRecord(t, a, "send-limit-words", record)
			rpcError, _ := receipt["rpcError"].(map[string]any)
			message, _ := rpcError["message"].(string)
			if receipt["status"] != "failed" || !strings.Contains(message, "auto-compaction limit") || strings.Contains(message, "nothing was transmitted") {
				t.Fatalf("receipt=%v", receipt)
			}
		})
	}
}

// autoCompactSettings is the receipt's settings observation of a resume, or nil when it carries
// none. It is the same notation the bridge uses for a setting the host cannot confirm: the value
// that went out under requested, and the field under unobservable, never under verified.
func autoCompactSettings(t *testing.T, receipt map[string]any) map[string]any {
	t.Helper()
	observed, _ := receipt["settings"].(map[string]any)
	return observed
}

// autoCompactSentValue reads the value the receipt recorded as sent, whatever JSON shape it took.
func autoCompactSentValue(t *testing.T, receipt map[string]any) (int64, bool) {
	t.Helper()
	observed := autoCompactSettings(t, receipt)
	if observed == nil {
		return 0, false
	}
	requested, _ := observed["requested"].(map[string]any)
	value, present := requested[settings.AutoCompactTokenLimitKey]
	return autoCompactInt(value), present
}

// autoCompactInt reads a number the way the ledger round-trips it: the in-process receipt holds an
// int64, and the stored one holds whatever the decoder made of the same JSON.
func autoCompactInt(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return -1
}

// autoCompactMarkedUnobservable reports whether the receipt marks the limit as one the host does
// not report back, so a reader never takes its silence for confirmation.
func autoCompactMarkedUnobservable(t *testing.T, receipt map[string]any) bool {
	t.Helper()
	observed := autoCompactSettings(t, receipt)
	if observed == nil {
		return false
	}
	for _, field := range autoCompactStrings(observed["unobservable"]) {
		if field == settings.AutoCompactTokenLimitKey {
			return true
		}
	}
	return false
}

// autoCompactStrings reads a list of names from either shape the receipt takes.
func autoCompactStrings(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text, _ := item.(string)
			out = append(out, text)
		}
		return out
	}
	return nil
}

// The host never reports the limit back, so a resume that sends it has to say so itself: the value
// that went out is recorded as requested and marked unobservable, exactly as the bridge records a
// setting the host cannot confirm. The record has to survive the ledger, because a later reader sees
// the stored or replayed receipt, not this call's.
func TestARelayResumeReceiptRecordsTheLimitItSentAsUnobservable(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(map[bool]string{false: "with-pair", true: "settings-free"}[free], func(t *testing.T) {
			record := childRecord(free, "")
			rpc := &mcpRPC{}
			a := mcpAdapter(t, rpc, autoCompactChildPolicy(t, record))
			requestID := "send-limit-receipt"
			receipt := sendRecord(t, a, requestID, record)
			if receipt["status"] != "accepted" {
				t.Fatalf("receipt=%v", receipt)
			}
			value, present := autoCompactSentValue(t, receipt)
			if !present || value != 550000 || !autoCompactMarkedUnobservable(t, receipt) {
				t.Fatalf("the receipt does not record the limit it sent as unobservable: %v", receipt)
			}
			if verified := autoCompactStrings(autoCompactSettings(t, receipt)["verified"]); len(verified) != 0 {
				t.Fatalf("the limit was recorded as verified: %v", verified)
			}
			stored, err := a.GetOperation(context.Background(), requestID)
			if err != nil {
				t.Fatal(err)
			}
			value, present = autoCompactSentValue(t, plain(stored).(map[string]any))
			if !present || value != 550000 || !autoCompactMarkedUnobservable(t, plain(stored).(map[string]any)) {
				t.Fatalf("the stored receipt lost the limit: %v", stored)
			}
			replayed := sendRecord(t, a, requestID, record)
			if replayed["replayed"] != true {
				t.Fatalf("the send was not replayed: %v", replayed)
			}
			value, present = autoCompactSentValue(t, replayed)
			if !present || value != 550000 || !autoCompactMarkedUnobservable(t, replayed) {
				t.Fatalf("the replayed receipt lost the limit: %v", replayed)
			}
		})
	}
}

// A send whose pair declares no limit records nothing new: a reader of that receipt must not see a
// settings observation it never made.
func TestARelayResumeWithoutALimitRecordsNoSettingsObservation(t *testing.T) {
	record := childRecord(false, "")
	rpc := &mcpRPC{}
	a := mcpAdapter(t, rpc, relayPolicy(t))
	receipt := sendRecord(t, a, "send-no-limit-receipt", record)
	if receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v", receipt)
	}
	if observed := autoCompactSettings(t, receipt); observed != nil {
		t.Fatalf("a send without a limit recorded a settings observation: %v", observed)
	}
}
