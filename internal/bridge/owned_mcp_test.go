package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-466: a thread created or resumed under an MCP profile starts only the servers the profile
// keeps. The bridge asks the host which servers config.toml defines, sends the switch-offs in the
// thread's config, and reads the host's status list afterwards.

const mcpChildPolicy = `{"roles":{"parent":{"model":"` + parentModel + `","reasoningEffort":"` + parentEffort + `"},"child":{"model":"` + pyModel + `","reasoningEffort":"` + pyEffort +
	`","mcp":{"default":"minimal","profiles":{"minimal":{},"ui-qa":{"servers":["node_repl"]},"second-opinion":{"servers":["oracle"],"disablePlugins":["cua@openai-bundled"]}}}}}}`

var mcpConfigured = []string{"gemini_notebook", "node_repl", "oracle"}

// mcpHost scripts what a creation or a resume under a profile asks the host. Its status list answers
// from the overrides of the last thread/start or thread/resume it saw, as a host that applied them
// would; applied false reports every server connected whatever was sent.
func mcpHost(host *fakehost.Server, cwd string, configured []string, applied bool) {
	servers := map[string]any{}
	for _, name := range configured {
		servers[name] = map[string]any{"enabled": true}
	}
	host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": servers}}})
	host.Respond("plugin/installed", fakehost.Reply{Result: map[string]any{"marketplaces": []any{map[string]any{"name": "openai-bundled", "plugins": []any{map[string]any{"id": "cua@openai-bundled", "installed": true, "enabled": true}}}}}})
	start := startReply(cwd)
	start.Result["model"], start.Result["reasoningEffort"] = pyModel, pyEffort
	host.Respond("thread/start", start)
	host.Respond("thread/resume", start)
	host.Respond("thread/name/set", fakehost.Reply{})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
		off, pluginsOff := sentOverrides(host)
		rows := []any{}
		for _, name := range configured {
			status := "connected"
			if applied && off[name] {
				status = "disabled"
			}
			rows = append(rows, map[string]any{"name": name, "pluginId": nil, "runtimeStatus": status})
		}
		if !(applied && pluginsOff["cua@openai-bundled"]) {
			rows = append(rows, map[string]any{"name": "cua_repl", "pluginId": "cua@openai-bundled", "runtimeStatus": "connected"})
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
}

// sentOverrides is what the last thread/start or thread/resume asked to switch off.
func sentOverrides(host *fakehost.Server) (servers, plugins map[string]bool) {
	servers, plugins = map[string]bool{}, map[string]bool{}
	var last map[string]any
	for _, req := range host.Requests() {
		if req.Method == "thread/start" || req.Method == "thread/resume" {
			last = nil
			_ = json.Unmarshal(req.Params, &last)
		}
	}
	config := pyjson.Map(last["config"])
	for name, entry := range pyjson.Map(config["mcp_servers"]) {
		servers[name] = pyjson.Map(entry)["enabled"] == false
	}
	for name, entry := range pyjson.Map(config["plugins"]) {
		plugins[name] = pyjson.Map(entry)["enabled"] == false
	}
	return servers, plugins
}

func mcpBridge(t *testing.T, applied bool) (*Bridge, *fakehost.Server, string) {
	t.Helper()
	b, host := policyBridge(t, mcpChildPolicy)
	cwd := t.TempDir()
	mcpHost(host, cwd, mcpConfigured, applied)
	return b, host, cwd
}

func mcpCreate(cwd, id, profile string) CreateThread {
	in := createInput(cwd, id)
	in.Role, in.Model, in.Effort, in.MCPProfile, in.Prompt = "child", pyModel, pyEffort, profile, "work"
	return in
}

func switchedOff(t *testing.T, host *fakehost.Server) []string {
	t.Helper()
	servers, _ := sentOverrides(host)
	names := []string{}
	for name, off := range servers {
		if off {
			names = append(names, name)
		}
	}
	return sortedNames(names)
}

func TestACreationUnderAProfileSwitchesOffTheServersItDoesNotKeep(t *testing.T) {
	for _, c := range []struct {
		profile string
		off     []string
		plugins bool
	}{{"ui-qa", []string{"gemini_notebook", "oracle"}, false}, {"second-opinion", []string{"gemini_notebook", "node_repl"}, true}} {
		t.Run(c.profile, func(t *testing.T) {
			b, host, cwd := mcpBridge(t, true)
			receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-"+c.profile, c.profile))
			if err != nil || receipt["status"] != "accepted" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if got := switchedOff(t, host); !equalNames(got, c.off) {
				t.Fatalf("switched off %v, want %v", got, c.off)
			}
			_, pluginsOff := sentOverrides(host)
			if pluginsOff["cua@openai-bundled"] != c.plugins || (host.Count("plugin/installed") == 1) != c.plugins {
				t.Fatalf("plugin switch %v, plugin/installed x%d", pluginsOff, host.Count("plugin/installed"))
			}
			methods := hostMethods(host)
			if methods[0] != "config/read" || indexOf(methods, "thread/start") < 1 || host.Count("mcpServerStatus/list") != 1 || host.Count("turn/start") != 1 {
				t.Fatalf("host calls %v", methods)
			}
			if profile := pyjson.Map(receipt["mcpProfile"]); profile["name"] != c.profile || profile["implicit"] != false {
				t.Fatalf("receipt.mcpProfile = %v", profile)
			}
			if params := hostParams(t, host, "mcpServerStatus/list"); params["threadId"] != "thread-1" {
				t.Fatalf("status asked for %v", params)
			}
		})
	}
}

func TestACreationNamingNoProfileGetsTheRolesDefault(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-default", ""))
	if err != nil || receipt["status"] != "accepted" || !equalNames(switchedOff(t, host), mcpConfigured) {
		t.Fatalf("receipt=%v err=%v off=%v", receipt, err, switchedOff(t, host))
	}
	if profile := pyjson.Map(receipt["mcpProfile"]); profile["name"] != "minimal" || profile["implicit"] != true {
		t.Fatalf("receipt.mcpProfile = %v", profile)
	}
}

func TestACreationNamingAnUnknownProfileNeverReachesTheHost(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	_, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-unknown", "nope"))
	var refusal *execution.Refusal
	if !errors.As(err, &refusal) || refusal.Code != execution.MCPProfileUnknown || len(hostMethods(host)) != 0 {
		t.Fatalf("err=%v host calls %v", err, hostMethods(host))
	}
}

func TestAKeptServerConfigTomlDoesNotDefineStopsTheCreationWithItsReason(t *testing.T) {
	b, host := policyBridge(t, mcpChildPolicy)
	cwd := t.TempDir()
	mcpHost(host, cwd, []string{"oracle"}, true)
	receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-missing", "ui-qa"))
	rpc := pyjson.Map(receipt["rpcError"])
	if err != nil || receipt["status"] != "failed" || rpc["code"] != execution.MCPServerUnknown || host.Count("thread/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if message, _ := rpc["message"].(string); !contains(message, "node_repl") || !contains(message, "config.toml") {
		t.Fatalf("the refusal does not say why: %q", message)
	}
}

// No override can turn on a server config.toml disables, so a profile that keeps one is refused with that
// reason before the thread starts, instead of the status check withholding the prompt afterwards.
func TestAKeptServerConfigTomlDisablesStopsTheCreationWithItsReason(t *testing.T) {
	b, host := policyBridge(t, mcpChildPolicy)
	cwd := t.TempDir()
	mcpHost(host, cwd, mcpConfigured, true)
	host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"gemini_notebook": map[string]any{"enabled": true}, "node_repl": map[string]any{"enabled": false}, "oracle": map[string]any{}}}}})
	receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-disabled", "ui-qa"))
	rpc := pyjson.Map(receipt["rpcError"])
	if message, _ := rpc["message"].(string); err != nil || receipt["status"] != "failed" || rpc["code"] != execution.MCPServerDisabled || host.Count("thread/start") != 0 || !contains(message, "node_repl") || !contains(message, "disables") {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func TestACreationWithoutProfilesAsksTheHostNothingExtra(t *testing.T) {
	for name, build := range map[string]func(t *testing.T) (*Bridge, *fakehost.Server){
		"a role that declares none": rolesBridge,
		"no role at all":            testBridge,
	} {
		t.Run(name, func(t *testing.T) {
			b, host := build(t)
			cwd := t.TempDir()
			start := startReply(cwd)
			role, model, effort := "", "explicit-model", "high"
			if name == "a role that declares none" {
				role, model, effort = "child", pyModel, pyEffort
				start.Result["model"], start.Result["reasoningEffort"] = model, effort
			}
			host.Respond("thread/start", start)
			in := createInput(cwd, "create-plain")
			in.Role, in.Model, in.Effort = role, model, effort
			receipt, err := b.CreateThread(context.Background(), in)
			config := pyjson.Map(hostParams(t, host, "thread/start")["config"])
			if err != nil || receipt["status"] != "accepted" || config["mcp_servers"] != nil || config["plugins"] != nil {
				t.Fatalf("receipt=%v err=%v config=%v", receipt, err, config)
			}
			for _, method := range []string{"config/read", "plugin/installed", "mcpServerStatus/list"} {
				if host.Count(method) != 0 {
					t.Fatalf("%s was asked", method)
				}
			}
		})
	}
}

func TestAThreadThatReportsAForbiddenServerWithholdsThePrompt(t *testing.T) {
	b, host, cwd := mcpBridge(t, false)
	receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-ignored", "ui-qa"))
	rpc := pyjson.Map(receipt["rpcError"])
	if err != nil || receipt["status"] != "failed" || rpc["code"] != settings.NotPreserved || host.Count("turn/start") != 0 || host.Count("thread/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if message, _ := rpc["message"].(string); !contains(message, "mcpServers") || !contains(message, "prompt withheld") {
		t.Fatalf("message %q", message)
	}
}

func TestAnUnreadableStatusAnswerIsUnobservableNotClean(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"data": "none"}}
	})
	receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-unreadable", "ui-qa"))
	if err != nil || receipt["status"] != "failed" || pyjson.Map(receipt["rpcError"])["code"] != settings.Unobservable || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

// A profile read the host drops is not an effect: nothing reached the host, so the same request may be sent again.
func TestALostProfileReadLeavesTheCreationRetrySafe(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	host.Respond("config/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1011, Reason: "gone"}})
	receipt, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-lost", "ui-qa"))
	if err != nil || receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || host.Count("thread/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func TestAReplayedCreationAsksTheHostNothingAndAnotherProfileIsAConflict(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	if _, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-replay", "ui-qa")); err != nil {
		t.Fatal(err)
	}
	calls := len(hostMethods(host))
	again, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-replay", "ui-qa"))
	if err != nil || again["replayed"] != true || len(hostMethods(host)) != calls {
		t.Fatalf("replay=%v err=%v calls %d -> %d", again, err, calls, len(hostMethods(host)))
	}
	if _, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-replay", "second-opinion")); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("another profile under the same id: %v", err)
	}
}

func TestTwoConcurrentCreationsOfOneRequestStartOneThread(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.CreateThread(context.Background(), mcpCreate(cwd, "create-twice", "ui-qa")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if host.Count("thread/start") != 1 || host.Count("config/read") != 1 {
		t.Fatalf("thread/start x%d, config/read x%d", host.Count("thread/start"), host.Count("config/read"))
	}
}

func mcpSend(id, profile string) SendMessage {
	expected := map[string]any{"model": pyModel, "reasoning_effort": pyEffort}
	if profile != "" {
		expected["mcp_profile"] = profile
	}
	return SendMessage{RequestID: id, ThreadID: "thread-1", Message: "work", Role: "child", Expected: expected}
}

func TestASendNamingAProfileResumesUnderItsOverrides(t *testing.T) {
	b, host, _ := mcpBridge(t, true)
	receipt, err := b.SendMessageToThread(context.Background(), mcpSend("send-ui-qa", "ui-qa"))
	if err != nil || receipt["status"] != "accepted" || !equalNames(switchedOff(t, host), []string{"gemini_notebook", "oracle"}) || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v off=%v", receipt, err, switchedOff(t, host))
	}
	if methods := hostMethods(host); indexOf(methods, "config/read") > indexOf(methods, "thread/resume") || host.Count("mcpServerStatus/list") != 1 {
		t.Fatalf("host calls %v", methods)
	}
	if _, err := b.SendMessageToThread(context.Background(), mcpSend("send-nope", "nope")); err == nil {
		t.Fatal("a send naming an unknown profile was accepted")
	}
}

func TestASendResumedOntoAThreadThatIgnoredTheOverridesIsWithheld(t *testing.T) {
	b, host, _ := mcpBridge(t, false)
	receipt, err := b.SendMessageToThread(context.Background(), mcpSend("send-ignored", "ui-qa"))
	if err != nil || receipt["status"] != "failed" || pyjson.Map(receipt["rpcError"])["code"] != settings.NotPreserved || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

// Overrides are not kept with a thread, and no default is applied on a send: a send that names no
// profile resumes as it always did.
func TestASendNamingNoProfileAsksTheHostNothingExtra(t *testing.T) {
	b, host, _ := mcpBridge(t, true)
	receipt, err := b.SendMessageToThread(context.Background(), mcpSend("send-plain", ""))
	config := pyjson.Map(hostParams(t, host, "thread/resume")["config"])
	if err != nil || receipt["status"] != "accepted" || config["mcp_servers"] != nil || host.Count("config/read") != 0 || host.Count("mcpServerStatus/list") != 0 {
		t.Fatalf("receipt=%v err=%v config=%v", receipt, err, config)
	}
}
