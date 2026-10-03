package settings

import (
	"reflect"
	"testing"
)

// CRW-466: the MCP part of a contract. The overrides are the nested form of
// mcp_servers.<name>.enabled=false and plugins.<id>.enabled=false; the host's answer is reduced to
// the shape of the expectation and compared like every other setting.

func mcpExpectation() *MCPExpectation {
	return &MCPExpectation{Disabled: []string{"gemini_notebook", "oracle"}, Enabled: []string{"node_repl"}, PluginsOff: []string{"cua@openai-bundled"}}
}

func off() map[string]any { return map[string]any{"enabled": false} }

func TestConfigSwitchesOffOnlyTheListedNames(t *testing.T) {
	config := Contract{ReasoningEffort: "high", MCP: mcpExpectation()}.Config()
	want := map[string]any{
		"model_reasoning_effort": "high",
		"mcp_servers":            map[string]any{"gemini_notebook": off(), "oracle": off()},
		"plugins":                map[string]any{"cua@openai-bundled": off()},
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("config = %v, want %v", config, want)
	}
	for _, c := range []Contract{{ReasoningEffort: "high"}, {ReasoningEffort: "high", MCP: &MCPExpectation{Enabled: []string{"node_repl"}}}} {
		if got := c.Config(); got["mcp_servers"] != nil || got["plugins"] != nil {
			t.Fatalf("nothing is switched off, yet config = %v", got)
		}
	}
	if params := (Contract{MCP: mcpExpectation()}).StartParams(); params["config"].(map[string]any)["mcp_servers"] == nil {
		t.Fatalf("thread/start does not carry the overrides: %v", params)
	}
	if params := (Contract{CWD: "/w", MCP: mcpExpectation()}).ResumeParams("t"); params["config"].(map[string]any)["mcp_servers"] == nil {
		t.Fatalf("thread/resume does not carry the overrides: %v", params)
	}
}

func TestTheRequestedSettingsNameTheMCPExpectationOnlyWhenThereIsOne(t *testing.T) {
	want := map[string]any{"disabled": []any{"gemini_notebook", "oracle"}, "enabled": []any{"node_repl"}, "pluginsAbsent": []any{"cua@openai-bundled"}}
	if got := (Contract{MCP: mcpExpectation()}).Requested()["mcpServers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("requested mcpServers = %v, want %v", got, want)
	}
	if _, has := (Contract{Model: "m"}).Requested()["mcpServers"]; has {
		t.Fatal("a contract without a profile asks about MCP servers")
	}
}

func row(name string, pluginID any, status any) any {
	return map[string]any{"name": name, "pluginId": pluginID, "runtimeStatus": status}
}

func answer(rows ...any) map[string]any { return map[string]any{"data": rows, "nextCursor": nil} }

func TestObserveReducesTheHostsStatusListToTheExpectationsShape(t *testing.T) {
	expected := mcpExpectation()
	clean := answer(row("gemini_notebook", nil, "disabled"), row("oracle", nil, "disabled"), row("node_repl", nil, "starting"), row("other", "x@y", nil))
	for name, c := range map[string]struct {
		answer map[string]any
		ok     bool
		want   map[string]any
	}{
		"every row as expected": {clean, true, expected.Shape()},
		"a server that should be off reports connected": {answer(row("gemini_notebook", nil, "disabled"), row("oracle", nil, "connected"), row("node_repl", nil, "connected")), true,
			map[string]any{"disabled": []any{"gemini_notebook"}, "enabled": []any{"node_repl"}, "pluginsAbsent": []any{"cua@openai-bundled"}}},
		"a kept server that is disabled or missing": {answer(row("gemini_notebook", nil, "disabled"), row("oracle", nil, "disabled"), row("node_repl", nil, "disabled")), true,
			map[string]any{"disabled": []any{"gemini_notebook", "oracle"}, "enabled": []any{}, "pluginsAbsent": []any{"cua@openai-bundled"}}},
		"a missing entry for a server that should be off": {answer(row("oracle", nil, "disabled"), row("node_repl", nil, "connected")), true,
			map[string]any{"disabled": []any{"oracle"}, "enabled": []any{"node_repl"}, "pluginsAbsent": []any{"cua@openai-bundled"}}},
		"a plugin that is still listed": {answer(row("gemini_notebook", nil, "disabled"), row("oracle", nil, "disabled"), row("node_repl", nil, "ready"), row("cua_repl", "cua@openai-bundled", "connected")), true,
			map[string]any{"disabled": []any{"gemini_notebook", "oracle"}, "enabled": []any{"node_repl"}, "pluginsAbsent": []any{}}},
		"data that is no list":                     {map[string]any{"data": "none"}, false, nil},
		"no data":                                  {map[string]any{}, false, nil},
		"a row that is no object":                  {answer("oracle"), false, nil},
		"a row without a name":                     {answer(map[string]any{"pluginId": nil, "runtimeStatus": "disabled"}), false, nil},
		"a row without a pluginId":                 {answer(map[string]any{"name": "other", "runtimeStatus": "connected"}), false, nil},
		"a pluginId that is no text":               {answer(row("other", 7, "connected")), false, nil},
		"an expected server with no runtimeStatus": {answer(row("oracle", nil, nil)), false, nil},
		"an expected server with a numeric status": {answer(row("node_repl", nil, 1)), false, nil},
		"another page":                             {map[string]any{"data": []any{}, "nextCursor": "more"}, false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := expected.Observe(c.answer)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Observe = %v, %v; want %v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestMCPServersAreComparedLikeEveryOtherSetting(t *testing.T) {
	c := Contract{MCP: mcpExpectation()}
	response := func(observed any) map[string]any {
		r := map[string]any{"approvalPolicy": "never"}
		if observed != nil {
			r["mcpServers"] = observed
		}
		return r
	}
	if findings := c.Findings(response(c.MCP.Shape())); len(findings) != 0 {
		t.Fatalf("a matching observation is a finding: %v", findings)
	}
	differ := map[string]any{"disabled": []any{"gemini_notebook"}, "enabled": []any{"node_repl"}, "pluginsAbsent": []any{"cua@openai-bundled"}}
	if findings := c.Findings(response(differ)); len(findings) != 1 || findings[0].Code != NotPreserved || findings[0].Field != "mcpServers" {
		t.Fatalf("a differing observation: %v", findings)
	}
	if findings := c.Findings(response(nil)); len(findings) != 1 || findings[0].Code != Unobservable || findings[0].Field != "mcpServers" {
		t.Fatalf("a missing observation: %v", findings)
	}
	if receipt := c.Receipt(response(c.MCP.Shape()), "creation"); receipt["verification"] != "observed_at_creation" {
		t.Fatalf("receipt = %v", receipt)
	}
}
