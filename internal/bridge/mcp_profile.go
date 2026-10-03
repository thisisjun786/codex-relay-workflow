package bridge

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// HostCall is one read of the host: the bridge's own call, or the relay adapter's host seam.
type HostCall func(ctx context.Context, method string, params map[string]any) (map[string]any, error)

// ResolveMCP turns a selected MCP profile into what a thread is told, from what the host reports:
// the servers config.toml defines for cwd and, when the profile switches plugins off, the plugins
// that are installed and enabled. The host's answer decides, not a file this process reads, because
// it is the host that refuses an override for a server it does not define.
func ResolveMCP(ctx context.Context, call HostCall, selection execution.MCPSelection, cwd string) (*settings.MCPExpectation, map[string]any, error) {
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	read, err := call(ctx, "config/read", params)
	if err != nil {
		return nil, nil, err
	}
	var configured, installed []string
	for name := range pyjson.Map(pyjson.Map(read["config"])["mcp_servers"]) {
		configured = append(configured, name)
	}
	if len(selection.Profile.DisablePlugins) > 0 {
		answer, err := call(ctx, "plugin/installed", map[string]any{})
		if err != nil {
			return nil, nil, err
		}
		marketplaces, _ := answer["marketplaces"].([]any)
		for _, marketplace := range marketplaces {
			plugins, _ := pyjson.Map(marketplace)["plugins"].([]any)
			for _, item := range plugins {
				if plugin := pyjson.Map(item); plugin["installed"] == true && plugin["enabled"] == true {
					installed = append(installed, pyjson.Text(plugin["id"]))
				}
			}
		}
	}
	resolved, err := selection.Resolve(configured, installed)
	if err != nil {
		return nil, nil, err
	}
	info := map[string]any{"name": selection.Name, "implicit": selection.Implicit, "serversOn": anyStrings(resolved.On), "serversOff": anyStrings(resolved.Off),
		"pluginsOff": anyStrings(resolved.PluginsOff), "pluginsNotInstalled": anyStrings(resolved.PluginsAbsent)}
	return &settings.MCPExpectation{Disabled: resolved.Off, Enabled: resolved.On, PluginsOff: resolved.PluginsOff}, info, nil
}

// MCPStatus is the host's server status list for one thread.
func MCPStatus(ctx context.Context, call HostCall, thread string) (map[string]any, error) {
	return call(ctx, "mcpServerStatus/list", map[string]any{"threadId": thread, "detail": "toolsAndAuthOnly", "limit": 500})
}

// MCPRefusal is a profile refusal as the receipt carries it: failed, with the code in rpcError.
func MCPRefusal(method string, err error) error {
	if refusal, ok := err.(*execution.Refusal); ok {
		return &appserver.RPCError{Method: method, Message: refusal.Detail, Object: map[string]any{"code": refusal.Code, "message": refusal.Detail}}
	}
	return err
}

// selectMCP is the profile a request for role chose; a name for a role this host's policy does not
// declare is refused, since a profile belongs to a role.
func (b *Bridge) selectMCP(role, name string) (execution.MCPSelection, error) {
	declared, ok := b.Policy.Role(role)
	if !ok {
		if name == "" {
			return execution.MCPSelection{}, nil
		}
		return execution.MCPSelection{}, &execution.Refusal{Code: execution.MCPProfileUnknown, Field: "mcp_profile", Requested: name, Detail: "an MCP profile belongs to a role, and this request names none that this host's execution policy declares"}
	}
	return declared.SelectMCP(role, name)
}

// observeMCP reads the host's status list for the thread and returns the host's answer with the
// reduced observation added, so the settings comparison reads one object. The answer itself is not
// changed, and the raw list is kept on the receipt.
func (b *Bridge) observeMCP(ctx context.Context, expected *settings.MCPExpectation, thread string, response map[string]any, receipt ledger.Receipt) (map[string]any, error) {
	answer, err := MCPStatus(ctx, b.call, thread)
	if err != nil {
		return nil, err
	}
	receipt["mcpServerStatus"] = answer
	view := copyMap(response)
	if observed, ok := expected.Observe(answer); ok {
		view["mcpServers"] = observed
	}
	return view, nil
}
