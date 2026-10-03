package execution

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// CRW-466: a role may carry MCP profiles. A profile keeps some of the host's config.toml servers on
// (every other configured server is switched off for the thread) and may switch whole plugins off.

func mcpSection() doc {
	return doc{"default": "minimal", "profiles": doc{
		"minimal":        doc{},
		"ui-qa":          doc{"servers": []any{"node_repl"}},
		"second-opinion": doc{"servers": []any{"oracle"}, "disablePlugins": []any{"unified-computer-use@openai-bundled"}},
	}}
}

func childWith(mcp any) doc {
	child := doc{"model": model, "reasoningEffort": effort}
	if mcp != nil {
		child["mcp"] = mcp
	}
	return doc{"parent": doc{"model": parentModel, "reasoningEffort": parentEffort}, "child": child}
}

func mcpChild(t *testing.T, mcp any) Role {
	t.Helper()
	role, ok := mustLoad(t, rolesPolicy(childWith(mcp), false)).Role("child")
	if !ok {
		t.Fatal("no child role")
	}
	return role
}

func TestAChildRoleCarriesItsMCPProfiles(t *testing.T) {
	role := mcpChild(t, mcpSection())
	if role.MCP == nil || role.MCP.Default != "minimal" || len(role.MCP.Profiles) != 3 {
		t.Fatalf("mcp section not read: %+v", role.MCP)
	}
	if got := role.MCP.Profiles["ui-qa"].Servers; !reflect.DeepEqual(got, []string{"node_repl"}) {
		t.Fatalf("ui-qa servers = %v", got)
	}
	if got := role.MCP.Profiles["second-opinion"].DisablePlugins; !reflect.DeepEqual(got, []string{"unified-computer-use@openai-bundled"}) {
		t.Fatalf("second-opinion plugins = %v", got)
	}
	if plain := mcpChild(t, nil); plain.MCP != nil {
		t.Fatalf("a role with no mcp section has profiles: %+v", plain.MCP)
	}
}

func TestMCPProfilesAreRefusedWhenReadWithTheirOwnReason(t *testing.T) {
	section := func(change func(doc)) doc {
		s := mcpSection()
		change(s)
		return s
	}
	profile := func(name string, body doc) func(doc) {
		return func(s doc) { s["profiles"].(doc)[name] = body }
	}
	for name, scenario := range map[string]struct {
		mcp  any
		want string
	}{
		"unknown key in the section":       {section(func(s doc) { s["extra"] = 1 }), "the mcp section of role \"child\" has unknown keys"},
		"unknown key in a profile":         {section(profile("ui-qa", doc{"servers": []any{}, "x": 1})), "profile \"ui-qa\" of role \"child\" has unknown keys"},
		"no profiles":                      {doc{"profiles": doc{}}, "must be a non-empty object of named profiles"},
		"profiles of the wrong type":       {doc{"profiles": []any{}}, "must be a non-empty object of named profiles"},
		"a default that is no profile":     {section(func(s doc) { s["default"] = "nope" }), "default \"nope\" names no declared profile"},
		"servers that are no list":         {section(profile("ui-qa", doc{"servers": "node_repl"})), "servers of profile \"ui-qa\" of role \"child\" must be a list of names"},
		"a blank server name":              {section(profile("ui-qa", doc{"servers": []any{" "}})), "must be a non-empty string"},
		"a server named twice":             {section(profile("ui-qa", doc{"servers": []any{"node_repl", "node_repl"}})), "lists \"node_repl\" twice"},
		"a plugin named twice":             {section(profile("ui-qa", doc{"disablePlugins": []any{"a@b", "a@b"}})), "lists \"a@b\" twice"},
		"a name over the bound":            {section(profile(strings.Repeat("p", MCPNameMaximum+1), doc{})), "must be a non-empty string"},
		"an mcp section of the wrong type": {[]any{}, "the mcp section of role \"child\" must be an object"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, rolesPolicy(childWith(scenario.mcp), false))
			var refused *PolicyError
			if !errors.As(err, &refused) || !strings.Contains(refused.Detail, scenario.want) {
				t.Fatalf("want a policy error containing %q, got %v", scenario.want, err)
			}
		})
	}
	roles := doc{"supervisor": doc{"expectation": "record", "mcp": mcpSection()}}
	if _, err := load(t, rolesPolicy(roles, false)); err == nil || !strings.Contains(err.Error(), "role 'supervisor' cannot declare \"mcp\"") {
		t.Fatalf("a supervisor with profiles must not load: %v", err)
	}
}

// The relay publishes the description of a policy's roles and compares it with the worker's, so
// declaring profiles must not change it.
func TestDeclaringMCPProfilesLeavesTheRolesDescriptionAsItWas(t *testing.T) {
	with, without := mustLoad(t, rolesPolicy(childWith(mcpSection()), false)), mustLoad(t, rolesPolicy(childWith(nil), false))
	if !reflect.DeepEqual(with.Summary()["roles"], without.Summary()["roles"]) {
		t.Fatalf("roles described differently:\n%v\n%v", with.Summary()["roles"], without.Summary()["roles"])
	}
}

func TestSelectMCP(t *testing.T) {
	role := mcpChild(t, mcpSection())
	for _, c := range []struct {
		name, want string
		implicit   bool
	}{{"", "minimal", true}, {"ui-qa", "ui-qa", false}, {"second-opinion", "second-opinion", false}} {
		got, err := role.SelectMCP("child", c.name)
		if err != nil || got.Name != c.want || got.Implicit != c.implicit {
			t.Fatalf("SelectMCP(%q) = %+v, %v", c.name, got, err)
		}
	}
	_, err := role.SelectMCP("child", "nope")
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != MCPProfileUnknown || refusal.Requested != "nope" ||
		!reflect.DeepEqual(refusal.Allowed, []any{"minimal", "second-opinion", "ui-qa"}) || !strings.Contains(refusal.Detail, "\"child\"") {
		t.Fatalf("an unknown profile: %#v", err)
	}
	plain := mcpChild(t, nil)
	if got, err := plain.SelectMCP("child", ""); err != nil || got.Name != "" {
		t.Fatalf("a role without profiles selects one: %+v, %v", got, err)
	}
	if _, err := plain.SelectMCP("child", "ui-qa"); !errors.As(err, &refusal) || refusal.Code != MCPProfileUnknown || !strings.Contains(refusal.Detail, "declares no MCP profiles") {
		t.Fatalf("a name for a role without profiles: %v", err)
	}
	noDefault := mcpChild(t, doc{"profiles": doc{"minimal": doc{}}})
	if got, err := noDefault.SelectMCP("child", ""); err != nil || got.Name != "" {
		t.Fatalf("a role with no default selects one: %+v, %v", got, err)
	}
}

func TestResolveSwitchesOffEveryConfiguredServerTheProfileDoesNotKeep(t *testing.T) {
	role := mcpChild(t, mcpSection())
	configured := []string{"oracle", "gemini_notebook", "node_repl"}
	for _, c := range []struct {
		profile   string
		installed []string
		want      MCPResolved
	}{
		{"minimal", nil, MCPResolved{Off: []string{"gemini_notebook", "node_repl", "oracle"}}},
		{"ui-qa", nil, MCPResolved{On: []string{"node_repl"}, Off: []string{"gemini_notebook", "oracle"}}},
		{"second-opinion", []string{"other@x", "unified-computer-use@openai-bundled"},
			MCPResolved{On: []string{"oracle"}, Off: []string{"gemini_notebook", "node_repl"}, PluginsOff: []string{"unified-computer-use@openai-bundled"}}},
		{"second-opinion", []string{"other@x"},
			MCPResolved{On: []string{"oracle"}, Off: []string{"gemini_notebook", "node_repl"}, PluginsAbsent: []string{"unified-computer-use@openai-bundled"}}},
	} {
		selected, err := role.SelectMCP("child", c.profile)
		if err != nil {
			t.Fatal(err)
		}
		got, err := selected.Resolve(configured, c.installed)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %+v, %v; want %+v", c.profile, got, err, c.want)
		}
	}
	if !reflect.DeepEqual(configured, []string{"oracle", "gemini_notebook", "node_repl"}) {
		t.Fatalf("Resolve reordered its input: %v", configured)
	}
}

// A kept-on server config.toml does not define is refused with its reason: sending an override for
// it would fail thread/start with "invalid transport".
func TestResolveRefusesAKeptServerConfigTomlDoesNotDefine(t *testing.T) {
	selected, err := mcpChild(t, mcpSection()).SelectMCP("child", "ui-qa")
	if err != nil {
		t.Fatal(err)
	}
	_, err = selected.Resolve([]string{"oracle"}, nil)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != MCPServerUnknown || refusal.Requested != "node_repl" ||
		!reflect.DeepEqual(refusal.Allowed, []any{"oracle"}) || !strings.Contains(refusal.Detail, "config.toml") || !strings.Contains(refusal.Detail, "\"ui-qa\"") {
		t.Fatalf("an unknown server: %#v", err)
	}
}
