package settings

import "slices"

// MCPExpectation is what an MCP profile resolved to against the host: the servers that must report
// "disabled", the servers kept on, and the plugins switched off whole.
type MCPExpectation struct{ Disabled, Enabled, PluginsOff []string }

func names(list []string) []any {
	out := make([]any, len(list))
	for i, name := range slices.Sorted(slices.Values(list)) {
		out[i] = name
	}
	return out
}

// Shape is the expectation as a comparison sees it. Observe reduces the host's answer to the same
// shape, so the two are compared like any other setting.
func (m *MCPExpectation) Shape() map[string]any {
	return map[string]any{"disabled": names(m.Disabled), "enabled": names(m.Enabled), "pluginsAbsent": names(m.PluginsOff)}
}

// Overrides is the part of a thread's config that switches the listed servers and plugins off. Nested
// objects, never dotted keys: a dotted key with a quoted plugin id is silently ignored by the host.
func (m *MCPExpectation) Overrides() map[string]any {
	out := map[string]any{}
	for key, list := range map[string][]string{"mcp_servers": m.Disabled, "plugins": m.PluginsOff} {
		if len(list) == 0 {
			continue
		}
		section := map[string]any{}
		for _, name := range list {
			section[name] = map[string]any{"enabled": false}
		}
		out[key] = section
	}
	return out
}

// Observe reduces a mcpServerStatus/list answer to the shape of the expectation: the expected-off
// servers that report "disabled", the kept servers that report anything else, and the plugins that
// are expected off and no longer listed. ok is false when the answer cannot decide that: data that is
// no list, a further page, a row that is no object or lacks a name or a pluginId, or an expected
// server without a status. An unreadable answer is never a clean one.
func (m *MCPExpectation) Observe(answer map[string]any) (map[string]any, bool) {
	rows, isList := answer["data"].([]any)
	if cursor, paged := answer["nextCursor"]; !isList || (paged && cursor != nil && cursor != "") {
		return nil, false
	}
	expected := append(slices.Clone(m.Disabled), m.Enabled...)
	status, plugins := map[string]string{}, map[string]bool{}
	for _, item := range rows {
		row, isObject := item.(map[string]any)
		name, named := row["name"].(string)
		pluginID, hasPlugin := row["pluginId"]
		if !isObject || !named || !hasPlugin {
			return nil, false
		}
		if id, isText := pluginID.(string); isText {
			plugins[id] = true
		} else if pluginID != nil {
			return nil, false
		}
		state, hasState := row["runtimeStatus"].(string)
		if slices.Contains(expected, name) && (!hasState || state == "") {
			return nil, false
		}
		status[name] = state
	}
	observed := map[string]any{"disabled": []any{}, "enabled": []any{}, "pluginsAbsent": []any{}}
	for key, list := range map[string][]string{"disabled": m.Disabled, "enabled": m.Enabled, "pluginsAbsent": m.PluginsOff} {
		found := []string{}
		for _, name := range list {
			state, listed := status[name]
			switch {
			case key == "disabled" && state == "disabled", key == "enabled" && listed && state != "disabled", key == "pluginsAbsent" && !plugins[name]:
				found = append(found, name)
			}
		}
		observed[key] = names(found)
	}
	return observed, true
}
