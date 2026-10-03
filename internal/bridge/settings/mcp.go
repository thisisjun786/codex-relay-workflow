package settings

// MCPExpectation is what a profile resolved to against the host.
type MCPExpectation struct{ Disabled, Enabled, PluginsOff []string }

func (m *MCPExpectation) Shape() map[string]any { return nil }

func (m *MCPExpectation) Overrides() map[string]any { return nil }

func (m *MCPExpectation) Observe(answer map[string]any) (map[string]any, bool) { return nil, false }
