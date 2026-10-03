package execution

// Refusal codes and bounds of a role's MCP profiles.
const (
	MCPProfileUnknown = "execution_mcp_profile_unknown"
	MCPServerUnknown  = "execution_mcp_server_unknown"
	MCPNameMaximum    = 128
)

// MCPProfile is what one named profile does to a thread's MCP servers.
type MCPProfile struct{ Servers, DisablePlugins []string }

// MCPProfiles is a role's declaration: its profiles and the one a creation that names none gets.
type MCPProfiles struct {
	Default  string
	Profiles map[string]MCPProfile
}

// MCPSelection is the profile a request chose; the zero value is no profile.
type MCPSelection struct {
	Name     string
	Profile  MCPProfile
	Implicit bool
}

// MCPResolved is a selection resolved against the host.
type MCPResolved struct{ On, Off, PluginsOff, PluginsAbsent []string }

func (r Role) SelectMCP(role, name string) (MCPSelection, error) { return MCPSelection{}, nil }

func (s MCPSelection) Resolve(configured, installed []string) (MCPResolved, error) {
	return MCPResolved{}, nil
}
