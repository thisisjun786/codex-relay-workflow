package doctor_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
)

// registrationCase is one host state: the files it writes into the Codex home (an empty text
// removes the file) and the classes and reason text the doctor must answer with.
type registrationCase struct {
	name               string
	stop, bridge       string
	config, hooks      string
	relay, bridgeClass string
	want               []string
}

func (h *host) runRegistrationCases(t *testing.T, cases []registrationCase) {
	t.Helper()
	files := func(c registrationCase) map[string]string {
		return map[string]string{"crw-completion-hook.json": c.stop, "crw-bridge-mcp.json": c.bridge, "config.toml": c.config, "hooks.json": c.hooks}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for name, text := range files(c) {
				path := filepath.Join(h.codex, name)
				_ = os.Remove(path)
				if text != "" {
					write(t, path, text, 0o600)
				}
			}
			report := h.diagnose(t)
			relay, bridge := golden.Canon(at(report, "components", "codex-session-relay")), golden.Canon(at(report, "components", "codex-thread-bridge"))
			if at(report, "components", "codex-session-relay", "class") != c.relay || at(report, "components", "codex-thread-bridge", "class") != c.bridgeClass {
				t.Fatalf("want relay %s, bridge %s\nrelay %s\nbridge %s", c.relay, c.bridgeClass, relay, bridge)
			}
			// A reason quoting a value spells it as JSON, whose quotes the canonical report escapes.
			reasons := strings.ReplaceAll(relay+bridge, `\"`, `"`)
			for _, want := range c.want {
				if !strings.Contains(reasons, want) {
					t.Errorf("no reason names %q:\nrelay %s\nbridge %s", want, relay, bridge)
				}
			}
			wantInstalled := "not_verified"
			if c.relay == "own" && c.bridgeClass == "own" {
				wantInstalled = "verified"
			}
			if got := at(report, "checks", "results", "installed", "value"); got != wantInstalled {
				t.Errorf("installed = %v", got)
			}
		})
	}
}

// Finding 20. None of the programs that read a settings record looks a command up on PATH: the
// hook refuses a relative relayExecutable, and the bridge launcher execs
// bridgeExecutable as written. So a bare name is a conflict even when this command's own PATH
// holds the selected runtime. Codex does look a bare config.toml command up, on the PATH the
// server gets, which this command can read only when the table sets env.PATH; a bare Stop
// command in hooks.json is looked up on the session's PATH, which it cannot.
func TestDoctorNeverLooksARegisteredCommandUpOnItsOwnPATH(t *testing.T) {
	h, _, _ := goHost(t, true)
	bin := filepath.Join(h.current(), "bin")
	h.env = h.env.With("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	goodStop, goodBridge := encodeJSON(t, h.stopSettings(t, nil)), encodeJSON(t, h.bridgeRecord(nil))
	userStop := encodeJSON(t, h.stopSettings(t, map[string]any{"owner": "user", "adapterInterpreter": nil, "adapterEntryPoint": nil}))
	h.runRegistrationCases(t, []registrationCase{
		{name: "a bare relayExecutable", stop: encodeJSON(t, h.stopSettings(t, map[string]any{"relayExecutable": "codex-session-relay"})), bridge: goodBridge, relay: "conflict", bridgeClass: "own",
			want: []string{"relayExecutable must be an absolute path", "so a Stop runs no relay"}},
		{name: "a bare adapterEntryPoint, which nothing reads", stop: encodeJSON(t, h.stopSettings(t, map[string]any{"adapterEntryPoint": "crw-completion-hook"})), bridge: goodBridge, relay: "own", bridgeClass: "own"},
		{name: "a bare bridgeExecutable", stop: goodStop, bridge: encodeJSON(t, h.bridgeRecord(map[string]any{"bridgeExecutable": "codex-thread-bridge"})), relay: "own", bridgeClass: "conflict",
			want: []string{"must name bridgeExecutable as an absolute path", "so it starts no bridge"}},
		{name: "a bare config.toml command", stop: goodStop, bridge: goodBridge, config: "[mcp_servers.codex-thread-bridge]\ncommand = \"codex-thread-bridge\"\n", relay: "own", bridgeClass: "unreadable",
			want: []string{"a bare command Codex looks up on its own PATH, which this doctor does not read"}},
		{name: "a bare config.toml command on the table's env.PATH", stop: goodStop, bridge: goodBridge, config: "[mcp_servers.codex-thread-bridge]\ncommand = \"codex-thread-bridge\"\nenv = { PATH = \"" + bin + "\" }\n", relay: "own", bridgeClass: "own"},
		{name: "a bare config.toml command on an env.PATH with a relative directory first", stop: goodStop, bridge: goodBridge, config: "[mcp_servers.codex-thread-bridge]\ncommand = \"codex-thread-bridge\"\nenv = { PATH = \"bin:" + bin + "\" }\n", relay: "own", bridgeClass: "unreadable",
			want: []string{"whose relative or empty directory comes first, so what it names depends on the directory Codex starts the server in"}},
		{name: "a bare config.toml command no env.PATH directory holds", stop: goodStop, bridge: goodBridge, config: "[mcp_servers.codex-thread-bridge]\ncommand = \"codex-thread-bridge\"\nenv = { PATH = \"/nonexistent\" }\n", relay: "own", bridgeClass: "unreadable",
			want: []string{"a bare command no directory on the table's env.PATH (/nonexistent) holds"}},
		{name: "a bare Stop command in hooks.json", stop: userStop, bridge: goodBridge, hooks: `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "crw-completion-hook"}]}]}}`, relay: "unreadable", bridgeClass: "own",
			want: []string{"looks up on the session's PATH"}},
	})
}

// Findings 21 and 35. The Stop settings are judged as one document, by the check the Go hook
// runs before it asks the relay anything (hook.ReadSettings), so a document it refuses runs no
// relay, whatever its relayExecutable names: a conflict naming the complaint. configVersion true
// is 1 to the hook and to the launcher alike.
func TestDoctorJudgesTheStopSettingsAsTheHookAcceptsThem(t *testing.T) {
	h, _, _ := goHost(t, true)
	goodBridge := encodeJSON(t, h.bridgeRecord(nil))
	stop := func(changes map[string]any) string { return encodeJSON(t, h.stopSettings(t, changes)) }
	h.runRegistrationCases(t, []registrationCase{
		{name: "configVersion 2", stop: stop(map[string]any{"configVersion": 2}), bridge: goodBridge, relay: "conflict", bridgeClass: "own", want: []string{"configVersion must be 1, found 2"}},
		{name: "no configVersion", stop: stop(map[string]any{"configVersion": nil}), bridge: goodBridge, relay: "conflict", bridgeClass: "own", want: []string{"configVersion must be 1, found None"}},
		{name: "a plugin budget the launcher cannot outlast", stop: stop(map[string]any{"timeoutSeconds": 30}), bridge: goodBridge, relay: "conflict", bridgeClass: "own", want: []string{"timeoutSeconds must not exceed 7 when owner is plugin"}},
		{name: "an unknown owner", stop: stop(map[string]any{"owner": "robot"}), bridge: goodBridge, relay: "conflict", bridgeClass: "own", want: []string{"owner must be one of user, plugin"}},
		{name: "no markerRoot or mode", stop: stop(map[string]any{"markerRoot": nil, "mode": nil}), bridge: goodBridge, relay: "conflict", bridgeClass: "own",
			want: []string{"markerRoot must be a non-empty string", "mode must be one of observe, hold"}},
		{name: "a plugin owner without the retired adapter keys", stop: stop(map[string]any{"adapterInterpreter": nil, "adapterEntryPoint": nil}), bridge: goodBridge, relay: "own", bridgeClass: "own"},
		{name: "a plugin owner that still names them", stop: stop(map[string]any{"adapterInterpreter": "/usr/bin/env", "adapterEntryPoint": filepath.Join(h.current(), "bin", "crw-completion-hook")}), bridge: goodBridge, relay: "own", bridgeClass: "own"},
		{name: "configVersion true", stop: stop(map[string]any{"configVersion": true}), bridge: goodBridge, relay: "own", bridgeClass: "own"},
	})
}

// Findings 22 and 36. The plugin-owned bridge record is judged by the packaged launcher's
// contract (crw_bridge_mcp.py), which exits before exec - starting no bridge - on a version other
// than 1 or 2, a server name other than codex-thread-bridge, args that are not a list of strings,
// a version-1 record naming a policy, and a version-2 policy that is malformed, missing, not a
// regular file or changed since it was registered. A policy this command cannot open is
// unreadable. recordVersion true and 1.0 are 1 to the launcher.
func TestDoctorJudgesTheBridgeRecordAsTheLauncherAcceptsIt(t *testing.T) {
	h, _, _ := goHost(t, true)
	goodStop := encodeJSON(t, h.stopSettings(t, nil))
	policy := filepath.Join(h.home, "policy.toml")
	write(t, policy, "roles = []\n", 0o600)
	sum := sha256.Sum256([]byte("roles = []\n"))
	digest := hex.EncodeToString(sum[:])
	bridge := func(changes map[string]any) string { return encodeJSON(t, h.bridgeRecord(changes)) }
	v2 := func(reference any) string {
		return bridge(map[string]any{"recordVersion": 2, "executionPolicy": reference})
	}
	write(t, filepath.Join(h.home, "pol\x80icy.toml"), "roles = []\n", 0o600)
	mkdir(t, filepath.Join(h.home, "policy-directory"))
	refused := func(name, record, want string) registrationCase {
		return registrationCase{name: name, stop: goodStop, bridge: record, relay: "own", bridgeClass: "conflict", want: []string{want, "so it starts no bridge"}}
	}
	cases := []registrationCase{
		refused("version 3", bridge(map[string]any{"recordVersion": 3}), "it is version 3, and the launcher reads versions 1 and 2"),
		refused("no version", bridge(map[string]any{"recordVersion": nil}), "it is version null"),
		refused("args that are a string", bridge(map[string]any{"args": "--x"}), "it must list args as strings"),
		refused("args holding a number", bridge(map[string]any{"args": []any{"bridge", 5}}), "it must list args as strings"),
		refused("args that are an object", bridge(map[string]any{"args": map[string]any{"a": 1}}), "it must list args as strings"),
		refused("another server name", bridge(map[string]any{"serverName": "other"}), `it names the server "other"`),
		refused("a version-1 record naming a policy", bridge(map[string]any{"executionPolicy": map[string]any{"path": policy, "digest": digest}}), "it is version 1 and names an execution policy"),
		refused("a version-2 record naming no policy", bridge(map[string]any{"recordVersion": 2}), "must name executionPolicy as an object with exactly digest and path"),
		refused("a policy with another key", v2(map[string]any{"path": policy, "digest": digest, "extra": 1}), "exactly digest and path"),
		refused("a relative policy", v2(map[string]any{"path": "policy.toml", "digest": digest}), "must name the execution policy as an absolute path"),
		refused("a malformed digest", v2(map[string]any{"path": policy, "digest": strings.ToUpper(digest)}), "64 lowercase hexadecimal characters"),
		refused("a missing policy", v2(map[string]any{"path": filepath.Join(h.home, "gone.toml"), "digest": digest}), "gone.toml does not exist"),
		refused("a policy that is a directory", v2(map[string]any{"path": filepath.Join(h.home, "policy-directory"), "digest": digest}), "is not a regular file"),
		refused("a policy that changed", v2(map[string]any{"path": policy, "digest": strings.Repeat("0", 64)}), "now hashes to "+digest),
		{name: "a version-2 record whose policy agrees", stop: goodStop, bridge: v2(map[string]any{"path": policy, "digest": digest}), relay: "own", bridgeClass: "own"},
		// The launcher opens os.fsencode(path): a name holding a byte that is not UTF-8 is recorded
		// as its surrogate escape and opened as that byte; a surrogate fsencode refuses names no file.
		{name: "a policy whose name is not UTF-8", stop: goodStop, bridge: strings.Replace(v2(map[string]any{"path": filepath.Join(h.home, "POLICY-NAME"), "digest": digest}), "POLICY-NAME", `pol\udc80icy.toml`, 1), relay: "own", bridgeClass: "own"},
		refused("a policy path fsencode refuses", strings.Replace(v2(map[string]any{"path": filepath.Join(h.home, "POLICY-NAME"), "digest": digest}), "POLICY-NAME", `pol\ud800icy.toml`, 1), "cannot encode"),
		{name: "recordVersion true", stop: goodStop, bridge: bridge(map[string]any{"recordVersion": true}), relay: "own", bridgeClass: "own"},
		{name: "recordVersion 1.0", stop: goodStop, bridge: strings.Replace(bridge(nil), `"recordVersion":1`, `"recordVersion":1.0`, 1), relay: "own", bridgeClass: "own"},
		{name: "crw with its bridge mode", stop: goodStop, bridge: bridge(map[string]any{"bridgeExecutable": filepath.Join(h.current(), "bin", "crw"), "args": []any{"bridge"}}), relay: "own", bridgeClass: "own"},
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(h.home, "locked-policy.toml")
		write(t, locked, "roles = []\n", 0)
		cases = append(cases, registrationCase{name: "a policy this command may not read", stop: goodStop, bridge: v2(map[string]any{"path": locked, "digest": digest}), relay: "own", bridgeClass: "unreadable", want: []string{"the execution policy " + locked}})
	}
	h.runRegistrationCases(t, cases)
}

// Finding 23. config.toml's mcp_servers is read whole, as codexconfig.registration_view reads
// it: a server entry that is not a table, a command that is not a string or args that are not a
// list of strings, in any table, make the configuration unreadable.
func TestDoctorReadsTheCodexConfigurationWhole(t *testing.T) {
	h, dir, _ := goHost(t, true)
	goodStop, goodBridge := encodeJSON(t, h.stopSettings(t, nil)), encodeJSON(t, h.bridgeRecord(nil))
	good := "[mcp_servers.codex-thread-bridge]\ncommand = \"" + filepath.Join(dir, "bin", "crw") + "\"\nargs = [\"bridge\"]\n"
	unreadable := func(name, config, want string) registrationCase {
		return registrationCase{name: name, stop: goodStop, bridge: goodBridge, config: config, relay: "own", bridgeClass: "unreadable", want: []string{want}}
	}
	h.runRegistrationCases(t, []registrationCase{
		{name: "a good table", stop: goodStop, bridge: goodBridge, config: good, relay: "own", bridgeClass: "own"},
		unreadable("args that are a string", "[mcp_servers.codex-thread-bridge]\ncommand = \""+filepath.Join(h.current(), "bin", "codex-thread-bridge")+"\"\nargs = \"--x\"\n",
			`"codex-thread-bridge" has args that are not a list of strings, they are a string`),
		unreadable("mcp_servers that is not a table", "mcp_servers = 1\n", "mcp_servers is a table of servers, found an integer"),
		unreadable("another server that is not a table", "[mcp_servers]\nother = \"x\"\n\n"+good, `the registration for "other" is a table, found a string`),
		unreadable("another server's args", good+"\n[mcp_servers.other]\ncommand = \"x\"\nargs = 5\n", `"other" has args that are not a list of strings, they are an integer`),
		unreadable("another server's command", good+"\n[mcp_servers.other]\ncommand = 5\n", `"other" has a command that is not a string, it is an integer`),
	})
}

// Finding 24. Codex starts every mcp_servers table, and a host may name the bridge's table
// anything. So every table that starts the bridge is judged (its command or an argument is the
// bridge's console script, crw bridge): one starting another bridge is a conflict. A user-owned
// record's serverName no longer selects a table (decision 67): a table that starts no bridge
// is not judged whatever a record calls it.
func TestDoctorJudgesTheBridgeUnderEveryTableName(t *testing.T) {
	h, dir, _ := goHost(t, true)
	goodStop := encodeJSON(t, h.stopSettings(t, nil))
	venv := h.scripted()
	old, _ := h.goRuntime(t, "bin-0.2.0-oooooooooooo")
	userRecord := func(name string) string {
		return encodeJSON(t, map[string]any{"recordVersion": 1, "owner": "user", "serverName": name, "bridgeExecutable": filepath.Join(venv, "bin", "codex-thread-bridge")})
	}
	table := func(name, command string, args ...string) string {
		text := "[mcp_servers." + name + "]\ncommand = \"" + command + "\"\n"
		if len(args) > 0 {
			text += "args = [\"" + strings.Join(args, "\", \"") + "\"]\n"
		}
		return text
	}
	h.runRegistrationCases(t, []registrationCase{
		{name: "the table a user-owned record names starts the Python bridge", stop: goodStop, bridge: userRecord("crw-bridge"), config: table("crw-bridge", filepath.Join(venv, "bin", "codex-thread-bridge")),
			relay: "own", bridgeClass: "conflict", want: []string{"mcp_servers.crw-bridge.command names " + filepath.Join(venv, "bin", "codex-thread-bridge")}},
		{name: "the table a user-owned record names starts something else", stop: goodStop, bridge: userRecord("named"), config: table("named", "/bin/sh"),
			relay: "own", bridgeClass: "own"},
		{name: "an old runtime's bridge under another name", stop: goodStop, bridge: encodeJSON(t, h.bridgeRecord(nil)), config: table("anything", filepath.Join(old, "bin", "codex-thread-bridge")),
			relay: "own", bridgeClass: "conflict", want: []string{"mcp_servers.anything.command names " + filepath.Join(old, "bin", "codex-thread-bridge")}},
		{name: "the selected crw bridge under another name", stop: goodStop, bridge: userRecord("crw"), config: table("crw", filepath.Join(dir, "bin", "crw"), "bridge"),
			relay: "own", bridgeClass: "own"},
		{name: "a table that starts no bridge", stop: goodStop, bridge: encodeJSON(t, h.bridgeRecord(nil)), config: table("other", "/bin/sh", "-c", "true"),
			relay: "own", bridgeClass: "own"},
	})
	report := h.diagnose(t)
	if got := golden.Canon(at(report, "components", "codex-thread-bridge", "registrations")); strings.Contains(got, "mcp_servers.other") {
		t.Errorf("a table that starts no bridge was judged: %s", got)
	}
}

// Finding 25. For a user owner (or none, which reads as user) the registration the host runs is
// the Stop command in <CODEX_HOME>/hooks.json, so each one that runs the Stop hook is judged as
// the relay's registration: the selected crw hook, with no argument or --plugin-launch, agrees;
// an old runtime's hook, the retired crw-completion-hook link, crw hook given a settings path
// (which it releases in silence since decision 66) and the hook handed to an interpreter do
// not; a hooks.json that cannot be read, a bare hook and an expansion this command does not make
// are unreadable. The checkout's Python adapter is no longer recognised (decision 67).
func TestDoctorJudgesTheStopCommandsInHooksJSON(t *testing.T) {
	h, dir, _ := goHost(t, true)
	goodBridge := encodeJSON(t, h.bridgeRecord(nil))
	settings := filepath.Join(h.codex, "crw-completion-hook.json")
	userStop := encodeJSON(t, h.stopSettings(t, map[string]any{"owner": "user", "adapterInterpreter": nil, "adapterEntryPoint": nil}))
	noOwner := encodeJSON(t, h.stopSettings(t, map[string]any{"owner": nil, "adapterInterpreter": nil, "adapterEntryPoint": nil}))
	old, _ := h.goRuntime(t, "bin-0.2.0-oooooooooooo")
	venv := h.scripted()
	hooks := func(command string) string {
		return encodeJSON(t, map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}}}})
	}
	current := filepath.Join(h.current(), "bin")
	script := filepath.Join(h.home, "bin", "on-stop")
	write(t, script, "#!/bin/sh\necho stopped\n", 0o755)
	cases := []registrationCase{
		{name: "the selected hook", stop: userStop, bridge: goodBridge, hooks: hooks(filepath.Join(current, "crw") + " hook"), relay: "own", bridgeClass: "own"},
		{name: "crw hook --plugin-launch", stop: userStop, bridge: goodBridge, hooks: hooks(filepath.Join(dir, "bin", "crw") + " hook --plugin-launch"), relay: "own", bridgeClass: "own"},
		{name: "the retired link", stop: userStop, bridge: goodBridge, hooks: hooks(filepath.Join(current, "crw-completion-hook") + " " + settings), relay: "conflict", bridgeClass: "own",
			want: []string{"starts the selected runtime's binary as crw-completion-hook rather than as crw hook"}},
		{name: "crw hook given a settings path", stop: userStop, bridge: goodBridge, hooks: hooks(filepath.Join(dir, "bin", "crw") + " hook " + settings), relay: "conflict", bridgeClass: "own",
			want: []string{"gives crw hook the argument " + settings + ", and crw hook given any argument but --plugin-launch releases every Stop in silence"}},
		{name: "the selected hook spelled with $HOME", stop: noOwner, bridge: goodBridge,
			hooks: hooks(`"$HOME/.local/share/crw-runtime/current/bin/crw" hook`), relay: "own", bridgeClass: "own"},
		{name: "a Stop command that runs no adapter", stop: userStop, bridge: goodBridge, hooks: hooks("echo done"), relay: "own", bridgeClass: "own"},
		{name: "an old runtime's hook", stop: userStop, bridge: goodBridge, hooks: hooks(filepath.Join(old, "bin", "crw-completion-hook") + " " + settings), relay: "conflict", bridgeClass: "own",
			want: []string{"names " + filepath.Join(old, "bin", "crw-completion-hook") + ", which resolves to " + filepath.Join(old, "bin", "crw")}},
		{name: "the checkout's Python adapter", stop: noOwner, bridge: goodBridge, hooks: hooks(filepath.Join(venv, "bin", "python3") + " /checkout/scripts/completion_hook.py " + settings), relay: "own", bridgeClass: "own"},
		{name: "the hook handed to an interpreter", stop: userStop, bridge: goodBridge, hooks: hooks("/usr/bin/python3 " + filepath.Join(current, "crw") + " hook"), relay: "conflict", bridgeClass: "own",
			want: []string{"to /usr/bin/python3 as an argument"}},
		{name: "an expansion the grammar does not make", stop: userStop, bridge: goodBridge, hooks: hooks("$RUNTIME/bin/crw hook"), relay: "unreadable", bridgeClass: "own",
			want: []string{`it holds "$RUNTIME/bin/crw", which this scan cannot judge, so whether it starts the relay's hook cannot be told`}},
		{name: "a construct outside the grammar", stop: userStop, bridge: goodBridge, hooks: hooks("if true; then " + filepath.Join(current, "crw") + " hook; fi"), relay: "unreadable", bridgeClass: "own",
			want: []string{"which this scan cannot judge"}},
		{name: "the selected hook through exec", stop: userStop, bridge: goodBridge, hooks: hooks("exec " + filepath.Join(current, "crw") + " hook"), relay: "own", bridgeClass: "own"},
		{name: "an old runtime's hook inside sh -c", stop: userStop, bridge: goodBridge, hooks: hooks("sh -c '" + filepath.Join(old, "bin", "crw-completion-hook") + " " + settings + "'"), relay: "conflict", bridgeClass: "own",
			want: []string{"names " + filepath.Join(old, "bin", "crw-completion-hook")}},
		{name: "a script that may run the hook itself", stop: userStop, bridge: goodBridge, hooks: hooks(script), relay: "unreadable", bridgeClass: "own",
			want: []string{"a script that may run the adapter with settings of its own"}},
		{name: "a hooks.json that is not JSON", stop: userStop, bridge: goodBridge, hooks: `{"hooks": `, relay: "unreadable", bridgeClass: "own", want: []string{"hooks.json " + filepath.Join(h.codex, "hooks.json")}},
		{name: "a Stop list that is not a list", stop: userStop, bridge: goodBridge, hooks: `{"hooks": {"Stop": {"hooks": []}}}`, relay: "unreadable", bridgeClass: "own", want: []string{"hooks.Stop is an object, not a list"}},
		{name: "an old runtime's hook beside a plugin owner", stop: encodeJSON(t, h.stopSettings(t, nil)), bridge: goodBridge, hooks: hooks(filepath.Join(old, "bin", "crw-completion-hook")), relay: "conflict", bridgeClass: "own",
			want: []string{filepath.Join(old, "bin", "crw")}},
	}
	h.runRegistrationCases(t, cases)
}
