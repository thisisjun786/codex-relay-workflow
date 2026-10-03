package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-466: the relay resumes a child under the MCP profile its record states, because a host does
// not keep a thread's overrides, and checks what the host reports afterwards.

const mcpRelayPolicy = `{"roles":{"child":{"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh","mcp":{"default":"minimal","profiles":{"minimal":{},"ui-qa":{"servers":["node_repl"]},"second-opinion":{"servers":["oracle"],"disablePlugins":["cua@openai-bundled"]}}}}}}`

// mcpRPC is a host that answers a send's calls; applied false makes its status list report every
// server connected whatever the resume sent, as a host that ignored the overrides would.
type mcpRPC struct {
	mu         sync.Mutex
	calls      []string
	params     map[string]map[string]any
	configured []string
	applied    bool
	model      string
}

func (r *mcpRPC) Call(_ context.Context, method string, params map[string]any) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method)
	if r.params == nil {
		r.params = map[string]map[string]any{}
	}
	r.params[method] = params
	var answer any
	switch method {
	case "thread/read":
		answer = map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}
	case "thread/resume":
		resumed := resume()
		if r.model != "" {
			resumed["model"] = r.model
		}
		answer = resumed
	case "turn/start":
		answer = map[string]any{"turn": map[string]any{"id": "turn-1"}}
	case "config/read":
		servers := map[string]any{}
		for _, name := range r.configured {
			servers[name] = map[string]any{"enabled": true}
		}
		answer = map[string]any{"config": map[string]any{"mcp_servers": servers}}
	case "plugin/installed":
		answer = map[string]any{"marketplaces": []any{map[string]any{"plugins": []any{map[string]any{"id": "cua@openai-bundled", "installed": true, "enabled": true}}}}}
	case "mcpServerStatus/list":
		off, pluginsOff := map[string]bool{}, map[string]bool{}
		config, _ := r.params["thread/resume"]["config"].(map[string]any)
		for name, entry := range config["mcp_servers"].(map[string]any) {
			off[name] = entry.(map[string]any)["enabled"] == false
		}
		if plugins, ok := config["plugins"].(map[string]any); ok {
			for id := range plugins {
				pluginsOff[id] = true
			}
		}
		rows := []any{}
		for _, name := range r.configured {
			status := "connected"
			if r.applied && off[name] {
				status = "disabled"
			}
			rows = append(rows, map[string]any{"name": name, "pluginId": nil, "runtimeStatus": status})
		}
		if !(r.applied && pluginsOff["cua@openai-bundled"]) {
			rows = append(rows, map[string]any{"name": "cua_repl", "pluginId": "cua@openai-bundled", "runtimeStatus": "connected"})
		}
		answer = map[string]any{"data": rows, "nextCursor": nil}
	default:
		return nil, fmt.Errorf("unscripted %s", method)
	}
	return json.Marshal(answer)
}

func (r *mcpRPC) count(method string) int {
	n := 0
	for _, c := range r.calls {
		if c == method {
			n++
		}
	}
	return n
}

func mcpAdapter(t *testing.T, rpc *mcpRPC, policy bridge.ExecutionPolicy) *Adapter {
	t.Helper()
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{RPC: rpc, Ledger: l}
	if policy != nil {
		options.Policy = policy
	}
	a := New(options)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func relayPolicy(t *testing.T) bridge.ExecutionPolicy {
	t.Helper()
	p, err := execution.FromBytes([]byte(mcpRelayPolicy), "test")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func childRecord(free bool, profile string) *delivery.TaskSettings {
	data := ordered(authorized()).(delivery.Obj)
	data = delivery.Obj(contract.OrderedObject(data).Set("citedRole", "child"))
	if profile != "" {
		data = delivery.Obj(contract.OrderedObject(data).Set("mcpProfile", profile))
	}
	return &delivery.TaskSettings{Data: data, SettingsFreeResume: free}
}

func sendRecord(t *testing.T, a *Adapter, id string, record *delivery.TaskSettings) map[string]any {
	t.Helper()
	receipt, err := a.Send(context.Background(), id, "thread-1", "hello", record, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return plain(receipt).(map[string]any)
}

func resumeConfig(rpc *mcpRPC) map[string]any {
	config, _ := rpc.params["thread/resume"]["config"].(map[string]any)
	return config
}

func offServers(config map[string]any) []string {
	names := []string{}
	servers, _ := config["mcp_servers"].(map[string]any)
	for name, entry := range servers {
		if entry.(map[string]any)["enabled"] == false {
			names = append(names, name)
		}
	}
	return sortedStrings(names)
}

func TestARecordedProfileIsSentOnTheResumeAndCheckedAfterwards(t *testing.T) {
	rpc := &mcpRPC{configured: []string{"gemini_notebook", "node_repl", "oracle"}, applied: true}
	a := mcpAdapter(t, rpc, relayPolicy(t))
	record := childRecord(false, "ui-qa")
	before := len(record.Data)
	receipt := sendRecord(t, a, "send-ui-qa", record)
	if receipt["status"] != "accepted" || strings.Join(offServers(resumeConfig(rpc)), ",") != "gemini_notebook,oracle" || rpc.count("mcpServerStatus/list") != 1 || rpc.count("turn/start") != 1 {
		t.Fatalf("receipt=%v config=%v calls=%v", receipt, resumeConfig(rpc), rpc.calls)
	}
	if len(record.Data) != before {
		t.Fatalf("the caller's record grew from %d to %d keys", before, len(record.Data))
	}
	if resumeConfig(rpc)["model_reasoning_effort"] == nil {
		t.Fatalf("the recorded effort is no longer sent: %v", resumeConfig(rpc))
	}
}

// A role that lists several pairs is resumed without its pair; the profile is no pair and is still sent.
func TestASettingsFreeResumeStillSendsTheProfileAndOnlyTheProfile(t *testing.T) {
	rpc := &mcpRPC{configured: []string{"node_repl", "oracle"}, applied: true}
	a := mcpAdapter(t, rpc, relayPolicy(t))
	receipt := sendRecord(t, a, "send-free", childRecord(true, "second-opinion"))
	params := rpc.params["thread/resume"]
	config := resumeConfig(rpc)
	if receipt["status"] != "accepted" || receipt["settingsFreeResume"] != true || receipt["mcpOverridesTransmitted"] != true ||
		params["model"] != nil || params["sandbox"] != nil || config["model_reasoning_effort"] != nil || strings.Join(offServers(config), ",") != "node_repl" || config["plugins"] == nil {
		t.Fatalf("receipt=%v params=%v", receipt, params)
	}
}

func TestAHostThatIgnoredTheOverridesRefusesTheSend(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(fmt.Sprintf("settings-free=%v", free), func(t *testing.T) {
			rpc := &mcpRPC{configured: []string{"node_repl", "oracle"}}
			a := mcpAdapter(t, rpc, relayPolicy(t))
			receipt := sendRecord(t, a, "send-ignored", childRecord(free, "ui-qa"))
			rpcError, _ := receipt["rpcError"].(map[string]any)
			if receipt["status"] != "failed" || rpcError["code"] != registry.SettingsNotPreserved || rpc.count("turn/start") != 0 {
				t.Fatalf("receipt=%v", receipt)
			}
			if message, _ := rpcError["message"].(string); !strings.Contains(message, "mcpServers") || strings.Contains(message, "nothing was transmitted") {
				t.Fatalf("message %q", message)
			}
		})
	}
}

// The model is compared first, and the code of a settings-free refusal keeps its meaning for a field the
// resume could not have changed; the words stop saying nothing was transmitted once overrides went out.
func TestAModelDifferenceOnASettingsFreeResumeIsStillADifferenceAfterLoad(t *testing.T) {
	rpc := &mcpRPC{configured: []string{"node_repl", "oracle"}, applied: true, model: "other/model"}
	a := mcpAdapter(t, rpc, relayPolicy(t))
	receipt := sendRecord(t, a, "send-model", childRecord(true, "ui-qa"))
	rpcError, _ := receipt["rpcError"].(map[string]any)
	message, _ := rpcError["message"].(string)
	if receipt["status"] != "failed" || rpcError["code"] != registry.SettingsDifferAfterLoad || strings.Contains(message, "nothing was transmitted") || !strings.Contains(message, "MCP profile's overrides") {
		t.Fatalf("receipt=%v", receipt)
	}
}

func TestAServerConfigTomlLacksStopsTheSendBeforeAnythingIsSent(t *testing.T) {
	rpc := &mcpRPC{configured: []string{"oracle"}, applied: true}
	a := mcpAdapter(t, rpc, relayPolicy(t))
	receipt := sendRecord(t, a, "send-missing", childRecord(false, "ui-qa"))
	rpcError, _ := receipt["rpcError"].(map[string]any)
	if receipt["status"] != "not_attempted" || rpcError["code"] != execution.MCPServerUnknown || rpc.count("thread/resume") != 0 {
		t.Fatalf("receipt=%v calls=%v", receipt, rpc.calls)
	}
}

func TestARecordNamingNoProfileResumesAsItAlwaysDid(t *testing.T) {
	rpc := &mcpRPC{configured: []string{"node_repl"}, applied: true}
	a := mcpAdapter(t, rpc, relayPolicy(t))
	receipt := sendRecord(t, a, "send-plain", childRecord(false, ""))
	if receipt["status"] != "accepted" || resumeConfig(rpc)["mcp_servers"] != nil || rpc.count("config/read") != 0 || rpc.count("mcpServerStatus/list") != 0 {
		t.Fatalf("receipt=%v calls=%v", receipt, rpc.calls)
	}
}

// deliver and supervisor-send build their adapter without a policy of their own, as does any later
// caller: it reads the process's, as the daemon does with the same snapshot.
func TestAnAdapterWithoutAPolicyReadsTheEnvironmentsForBothConstructors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(mcpRelayPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, path)
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	declared := func(a *Adapter) bool {
		role, ok := a.bridge.Policy.Role("child")
		return ok && role.MCP != nil
	}
	if a := mcpAdapter(t, &mcpRPC{}, nil); !declared(a) {
		t.Fatal("New without a policy ignores the environment's")
	}
	host := fakehost.Start(t)
	opened, err := Open(host.SocketPath, t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if !declared(opened) {
		t.Fatal("Open without a policy ignores the environment's")
	}
	if a := mcpAdapter(t, &mcpRPC{}, execution.Policy{}); declared(a) {
		t.Fatal("an explicit policy lost to the environment's")
	}
	explicit, err := Open(host.SocketPath, t.TempDir(), Options{Policy: execution.Policy{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = explicit.Close() })
	if declared(explicit) {
		t.Fatal("an explicit policy lost to the environment's in Open")
	}
}

// --- managed-start over the built relay CLI and the scripted app-server

const mcpManagedPolicy = `{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium","mcp":{"default":"minimal","profiles":{"minimal":{},"ui-qa":{"servers":["node_repl"]}}}}}}`

// mcpScript makes the CLI's app-server answer the profile's reads, and report each thread's servers
// as the overrides of the last thread/start or thread/resume asked.
func mcpScript(c *managedCLI) {
	c.host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"gemini_notebook": map[string]any{}, "node_repl": map[string]any{}, "oracle": map[string]any{}}}}})
	c.host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
		var config map[string]any
		for _, req := range c.host.Requests() {
			if req.Method == "thread/start" || req.Method == "thread/resume" {
				var params map[string]any
				_ = json.Unmarshal(req.Params, &params)
				config, _ = params["config"].(map[string]any)
			}
		}
		off, _ := config["mcp_servers"].(map[string]any)
		rows := []any{}
		for _, name := range []string{"gemini_notebook", "node_repl", "oracle"} {
			status := "connected"
			if _, switched := off[name]; switched {
				status = "disabled"
			}
			rows = append(rows, map[string]any{"name": name, "pluginId": nil, "runtimeStatus": status})
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
}

func (c *managedCLI) childProfile(profile any) func(map[string]any) {
	return func(request map[string]any) {
		settings := map[string]any{}
		for k, v := range c.settings {
			settings[k] = v
		}
		if profile != nil {
			settings["mcpProfile"] = profile
		}
		request["child"].(map[string]any)["settings"] = settings
	}
}

func TestManagedStartRefusesAChildThatStatesNoDeclaredProfileBeforeCreatingIt(t *testing.T) {
	for name, scenario := range map[string]struct {
		profile any
		reason  string
	}{"none stated": {nil, "mcp_profile_required"}, "an undeclared one": {"nope", "mcp_profile_unknown"}} {
		t.Run(name, func(t *testing.T) {
			c := newManagedCLIOn(t, mcpManagedPolicy, "gpt-5.4", "medium")
			mcpScript(c)
			stdout, stderr, code := c.start(c.request(c.childProfile(scenario.profile)))
			var refused map[string]any
			if err := json.Unmarshal([]byte(stdout), &refused); err != nil {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
			if code != contract.ExitRefused || refused["stage"] != "preflight" || refused["reason"] != scenario.reason || c.host.Count("thread/start") != 0 {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestManagedStartCreatesAndResumesTheChildUnderTheStatedProfile(t *testing.T) {
	c := newManagedCLIOn(t, mcpManagedPolicy, "gpt-5.4", "medium")
	mcpScript(c)
	stdout, stderr, code := c.start(c.request(c.childProfile("ui-qa")))
	var admitted map[string]any
	if err := json.Unmarshal([]byte(stdout), &admitted); err != nil || code != contract.ExitOk || admitted["state"] != "admitted" {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var created, resumed map[string]any
	for _, req := range c.host.Requests() {
		var params map[string]any
		_ = json.Unmarshal(req.Params, &params)
		switch req.Method {
		case "thread/start":
			created = params
		case "thread/resume":
			resumed = params
		}
	}
	want := map[string]any{"gemini_notebook": map[string]any{"enabled": false}, "oracle": map[string]any{"enabled": false}}
	for name, params := range map[string]map[string]any{"thread/start": created, "thread/resume": resumed} {
		config, _ := params["config"].(map[string]any)
		if fmt.Sprint(config["mcp_servers"]) != fmt.Sprint(want) {
			t.Fatalf("%s config = %v, want mcp_servers %v", name, config, want)
		}
	}
	c.withStore(func(s *store.Store) {
		var recorded string
		if err := s.DB.QueryRow("SELECT settings FROM authorized_settings WHERE task_id = 'managed-child'").Scan(&recorded); err != nil || !strings.Contains(recorded, `"mcpProfile": "ui-qa"`) {
			t.Fatalf("the child's record %q (%v) does not state its profile", recorded, err)
		}
	})
}

func sortedStrings(names []string) []string { sort.Strings(names); return names }
