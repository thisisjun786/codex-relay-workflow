package execution

import (
	"fmt"
	"slices"
)

// Refusal codes and the bound on a profile, server or plugin name.
const (
	MCPProfileUnknown = "execution_mcp_profile_unknown"
	MCPServerUnknown  = "execution_mcp_server_unknown"
	MCPNameMaximum    = 128
)

// MCPProfile is what one named profile does to a thread's MCP servers: Servers are the config.toml
// servers it keeps on (every other configured server is switched off for the thread) and
// DisablePlugins are plugins it switches off whole (a plugin's server has no switch of its own).
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

// MCPResolved is a selection resolved against the host: the configured servers it keeps on and
// switches off, the plugins it switches off, and those it names that are not installed.
type MCPResolved struct{ On, Off, PluginsOff, PluginsAbsent []string }

// SelectMCP is the profile a request for this role chose: the name it stated, else the role's
// default, else none. A name the role does not declare, or any name for a role that declares no
// profiles, is refused with the names it does declare.
func (r Role) SelectMCP(role, name string) (MCPSelection, error) {
	if r.MCP == nil {
		if name == "" {
			return MCPSelection{}, nil
		}
		return MCPSelection{}, &Refusal{Code: MCPProfileUnknown, Field: "mcp_profile", Requested: name, Detail: fmt.Sprintf("role %s declares no MCP profiles in this host's execution policy", repr(role))}
	}
	implicit := name == ""
	if implicit {
		name = r.MCP.Default
	}
	if name == "" {
		return MCPSelection{}, nil
	}
	profile, declared := r.MCP.Profiles[name]
	if !declared {
		names := sortedKeys(r.MCP.Profiles)
		return MCPSelection{}, &Refusal{Code: MCPProfileUnknown, Field: "mcp_profile", Requested: name, Allowed: anys(names), Detail: fmt.Sprintf("role %s declares the MCP profiles %s", repr(role), repr(names))}
	}
	return MCPSelection{Name: name, Profile: profile, Implicit: implicit}, nil
}

// Resolve turns the selection into what a thread is told, given the servers config.toml defines and
// the plugins that are installed. A server the profile keeps on that config.toml does not define is
// refused: switching nothing off for it would hide the mistake, and naming it in an override would
// fail the thread's start. A plugin that is not installed has nothing to switch off and is left out.
func (s MCPSelection) Resolve(configured, installed []string) (MCPResolved, error) {
	var out MCPResolved
	for _, name := range s.Profile.Servers {
		if !slices.Contains(configured, name) {
			return MCPResolved{}, &Refusal{Code: MCPServerUnknown, Field: "mcp_profile", Requested: name, Allowed: anys(sortedCopy(configured)),
				Detail: fmt.Sprintf("profile %s keeps the MCP server %s on, and this host's config.toml defines no such server (it defines %s); a plugin's server cannot be kept or switched here", repr(s.Name), repr(name), repr(sortedCopy(configured)))}
		}
		out.On = append(out.On, name)
	}
	for _, name := range sortedCopy(configured) {
		if !slices.Contains(s.Profile.Servers, name) {
			out.Off = append(out.Off, name)
		}
	}
	for _, id := range sortedCopy(s.Profile.DisablePlugins) {
		if slices.Contains(installed, id) {
			out.PluginsOff = append(out.PluginsOff, id)
		} else {
			out.PluginsAbsent = append(out.PluginsAbsent, id)
		}
	}
	slices.Sort(out.On)
	return out, nil
}

func sortedCopy(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

// parseMCP reads a role's mcp section: {"default": name, "profiles": {name: {servers, disablePlugins}}}.
func parseMCP(role string, value any) (*MCPProfiles, error) {
	where := "the mcp section of role " + repr(role)
	section, ok := value.(object)
	if !ok {
		return nil, &PolicyError{where + " must be an object"}
	}
	if err := only(section, []string{"default", "profiles"}, where); err != nil {
		return nil, err
	}
	declared, ok := section.Get("profiles").(object)
	if !ok || len(declared) == 0 {
		return nil, &PolicyError{where + ": profiles must be a non-empty object of named profiles"}
	}
	parsed := &MCPProfiles{Profiles: map[string]MCPProfile{}}
	for _, name := range keys(declared) {
		if _, err := identifier(name, "an MCP profile name of role "+repr(role), MCPNameMaximum); err != nil {
			return nil, err
		}
		label := "profile " + repr(name) + " of role " + repr(role)
		body, ok := declared.Get(name).(object)
		if !ok {
			return nil, &PolicyError{label + " must be an object"}
		}
		if err := only(body, []string{"disablePlugins", "servers"}, label); err != nil {
			return nil, err
		}
		servers, err := nameList(body, "servers", "servers of "+label, "a server of "+label)
		if err != nil {
			return nil, err
		}
		plugins, err := nameList(body, "disablePlugins", "disablePlugins of "+label, "a plugin of "+label)
		if err != nil {
			return nil, err
		}
		parsed.Profiles[name] = MCPProfile{Servers: servers, DisablePlugins: plugins}
	}
	if has(section, "default") {
		name, err := identifier(section.Get("default"), "the default MCP profile of role "+repr(role), MCPNameMaximum)
		if err != nil {
			return nil, err
		}
		if _, declared := parsed.Profiles[name]; !declared {
			return nil, &PolicyError{fmt.Sprintf("%s: default %s names no declared profile", where, repr(name))}
		}
		parsed.Default = name
	}
	return parsed, nil
}

// nameList reads an optional list of bounded names no name of which repeats.
func nameList(body object, key, what, each string) ([]string, error) {
	if !has(body, key) {
		return nil, nil
	}
	list, ok := body.Get(key).([]any)
	if !ok {
		return nil, &PolicyError{what + " must be a list of names"}
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		name, err := identifier(item, each, MCPNameMaximum)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, name) {
			return nil, &PolicyError{fmt.Sprintf("%s lists %s twice", what, repr(name))}
		}
		names = append(names, name)
	}
	return names, nil
}
