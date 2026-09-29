package doctor

import (
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// Registrations are the places a host is told to start a Go runtime's components, judged
// against the selected runtime: the Stop settings (crw-completion-hook.json relayExecutable, and
// for a plugin-owned record the adapter the packaged launcher runs), the plugin-owned bridge
// record (crw-bridge-mcp.json bridgeExecutable) and the Codex configuration's
// mcp_servers.codex-thread-bridge table. Each is the OPS-2.1 registration signal for its
// component (runtime_install.classify_component compares the Codex registration the same way):
// one that starts something other than the selected runtime's entry point is a conflict, and one
// that cannot be read stops classification.
type Registrations map[string]*componentRegistrations

type componentRegistrations struct {
	report    []any
	conflicts []string
	unread    []string
}

// crwModes are the component names crw runs for a mode argument (crw relay, crw bridge, crw hook).
var crwModes = map[string]string{"relay": definition.Relay, "bridge": definition.Bridge, "hook": definition.HookScript}

// ReadRegistrations reads every registration of the relay and the bridge and judges it against
// the runtime at target (the pointer's resolved target). env supplies the PATH a bare command
// is looked up in.
func ReadRegistrations(codexHome, target string, env scope.Env) Registrations {
	out := Registrations{definition.Relay: {}, definition.Bridge: {}}
	crw, err := record.Resolve(filepath.Join(target, "bin", "crw"))
	if err != nil {
		for _, c := range out {
			c.unread = append(c.unread, "the selected runtime's binary "+filepath.Join(target, "bin", "crw"))
		}
		return out
	}
	j := judge{crw: crw, path: env.Get("PATH")}

	relay := out[definition.Relay]
	stop := filepath.Join(codexHome, "crw-completion-hook.json")
	if document, ok := readRecord(stop, "the Stop settings", relay); ok {
		if command, ok := stringField(document, "relayExecutable", stop, relay); ok {
			j.command(relay, stop, "relayExecutable", command, nil, definition.Relay)
		}
		if record.Get(document, "owner") == "plugin" {
			if entry, ok := stringField(document, "adapterEntryPoint", stop, relay); ok {
				j.command(relay, stop, "adapterEntryPoint", entry, nil, definition.HookScript)
			}
			if interpreter, ok := record.Get(document, "adapterInterpreter").(string); ok && interpreter != "" {
				j.interpreter(relay, stop, interpreter)
			}
		}
	}

	bridge := out[definition.Bridge]
	bridgeRecord := filepath.Join(codexHome, "crw-bridge-mcp.json")
	if document, ok := readRecord(bridgeRecord, "the bridge record", bridge); ok && record.Get(document, "owner") == "plugin" {
		if command, ok := stringField(document, "bridgeExecutable", bridgeRecord, bridge); ok {
			j.command(bridge, bridgeRecord, "bridgeExecutable", command, anyStrings(record.Get(document, "args")), definition.Bridge)
		}
	}
	j.codexConfig(bridge, filepath.Join(codexHome, "config.toml"))
	return out
}

// readRecord reads a settings record: absent is no registration, and anything but an object is
// a registration that cannot be established.
func readRecord(path, what string, into *componentRegistrations) (Object, bool) {
	read := reading.ReadJSON(path, what, nil, nil)
	if read.State == reading.Absent {
		return nil, false
	}
	if !read.OK() {
		into.unread = append(into.unread, what+" "+path+" ("+read.Detail+")")
		return nil, false
	}
	document, ok := read.Value.(Object)
	if !ok {
		into.unread = append(into.unread, what+" "+path+" (not a JSON object)")
	}
	return document, ok
}

// stringField is a required command field; one that is missing or not a non-empty string leaves
// the registration unestablished.
func stringField(document Object, key, source string, into *componentRegistrations) (string, bool) {
	value, _ := record.Get(document, key).(string)
	if value == "" {
		into.unread = append(into.unread, source+" "+key+" (missing, empty or not a string)")
		return "", false
	}
	return value, true
}

type judge struct {
	crw  string // the selected runtime's binary, resolved
	path string // PATH for a bare command
}

// command judges one registered command: it starts the selected component when it resolves to
// the selected runtime's binary and crw is invoked under the component's name (the command's
// own basename, which crw dispatches on, or crw with that component's mode argument).
func (j judge) command(into *componentRegistrations, source, field, command string, args []string, want string) {
	entry := Object{{Key: "source", Value: source}, {Key: "field", Value: field}, {Key: "command", Value: command}}
	report := func(resolves, agrees any, detail string) {
		into.report = append(into.report, append(entry, record.Object{{Key: "resolves", Value: resolves}, {Key: "agrees", Value: agrees}, {Key: "detail", Value: detail}}...))
	}
	path := command
	if !filepath.IsAbs(path) {
		if strings.Contains(path, "/") {
			into.unread = append(into.unread, source+" "+field+" "+command+" (a relative path, which resolves wherever the host starts it)")
			report(nil, nil, "a relative path, which resolves wherever the host starts it")
			return
		}
		found, err := lookPath(path, j.path)
		if err != nil {
			into.unread = append(into.unread, source+" "+field+" "+command+" (a bare command no directory on PATH holds)")
			report(nil, nil, "a bare command no directory on PATH holds")
			return
		}
		path = found
	}
	resolved, err := record.Resolve(path)
	if err != nil {
		into.unread = append(into.unread, source+" "+field+" "+command+" ("+err.Error()+")")
		report(nil, nil, "the command could not be resolved: "+err.Error())
		return
	}
	invoked := filepath.Base(command)
	if invoked == "crw" && len(args) > 0 {
		if mode, ok := crwModes[args[0]]; ok {
			invoked = mode
		} else {
			invoked = "crw " + args[0]
		}
	}
	switch {
	case resolved != j.crw:
		detail := source + " " + field + " names " + command + ", which resolves to " + resolved + ", not the selected runtime's " + j.crw + ", so the host starts something other than the runtime that was promoted"
		into.conflicts = append(into.conflicts, detail)
		report(resolved, false, detail)
	case invoked != want:
		detail := source + " " + field + " starts the selected runtime's binary as " + invoked + " rather than as " + want + ", so it does not run " + want
		into.conflicts = append(into.conflicts, detail)
		report(resolved, false, detail)
	default:
		report(resolved, true, "starts the selected runtime's "+want)
	}
}

// interpreter judges the Stop settings' adapterInterpreter: the packaged launcher runs
// [adapterInterpreter, adapterEntryPoint], so a Python interpreter there (the Python-era
// .../current/bin/python3, which a Go runtime does not hold) never reaches the Go hook.
func (j judge) interpreter(into *componentRegistrations, source, interpreter string) {
	e := Classify(interpreter, "", "")
	entry := Object{{Key: "source", Value: source}, {Key: "field", Value: "adapterInterpreter"}, {Key: "command", Value: interpreter}, {Key: "resolves", Value: nullable(e.Resolves)}}
	if e.Python {
		detail := source + " adapterInterpreter " + interpreter + " is a Python interpreter (" + e.Detail + "), so the packaged launcher does not run the selected runtime's hook"
		into.conflicts = append(into.conflicts, detail)
		into.report = append(into.report, append(entry, record.Object{{Key: "agrees", Value: false}, {Key: "detail", Value: detail}}...))
		return
	}
	into.report = append(into.report, append(entry, record.Object{{Key: "agrees", Value: nil}, {Key: "detail", Value: "not a Python interpreter; what it runs the entry point with is not compared"}}...))
}

// codexConfig judges config.toml's mcp_servers.codex-thread-bridge, the table
// runtime_install.registration_state reads.
func (j judge) codexConfig(into *componentRegistrations, path string) {
	read := reading.ReadText(path, "config.toml")
	switch {
	case read.State == reading.Absent:
		return
	case !read.OK():
		into.unread = append(into.unread, "the Codex configuration "+path+" ("+read.Detail+")")
		return
	}
	var document map[string]any
	if _, err := toml.Decode(read.Value.(string), &document); err != nil {
		into.unread = append(into.unread, "the Codex configuration "+path+" ("+err.Error()+")")
		return
	}
	servers, _ := document["mcp_servers"].(map[string]any)
	raw, present := servers[definition.Bridge]
	if !present {
		return
	}
	table, _ := raw.(map[string]any)
	command, _ := table["command"].(string)
	field := "mcp_servers." + definition.Bridge + ".command"
	if command == "" {
		into.unread = append(into.unread, path+" "+field+" (missing, empty or not a string)")
		return
	}
	var args []string
	if list, ok := table["args"].([]any); ok {
		for _, arg := range list {
			text, ok := arg.(string)
			if !ok {
				into.unread = append(into.unread, path+" mcp_servers."+definition.Bridge+".args (not a list of strings)")
				return
			}
			args = append(args, text)
		}
	}
	j.command(into, path, field, command, args, definition.Bridge)
}

// of is one component's registrations, or none.
func (r Registrations) of(name string) *componentRegistrations {
	if c, ok := r[name]; ok {
		return c
	}
	return &componentRegistrations{}
}
