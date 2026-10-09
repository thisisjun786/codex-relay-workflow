package install

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CXC config-guard/src/cli.ts:23-37, with the approved absorbed command names.
const configUsage = "Usage:\n" +
	"  crw install config list                          managed keys, their live values and side effects\n" +
	"  crw install config get <table.key>\n" +
	"  crw install config set <table.key> <true|false>\n" +
	"  crw install config unset <table.key>             restore the value from before crw set it\n" +
	"  crw pabcd config interview [off|new-unit|always]\n\n" +
	"Only whitelisted keys can be set; 'config list' shows them. Installation writes just the\n" +
	"ones marked auto-enable in managed-keys.ts, records the pre-install value, and 'crw install features\n" +
	"disable' restores it; the rest stay an explicit choice. (Separate vocabulary: the\n" +
	"[features] flags crw needs to run ARE turned on by install and by SessionStart\n" +
	"self-heal; see crw doctor's `features` check.)\n"

func configValue(value *string, absent string) string {
	if value == nil {
		return absent
	}
	return *value
}

// configStateValue shows a managed key's live value; a key written in a form crw does not edit is shown as such, never as
// unset (CRW-1141).
func configStateValue(state configguard.ManagedState) string {
	if state.Unsupported {
		return "(set in a form crw does not edit: " + state.Reason + ")"
	}
	return configValue(state.Value, "(unset)")
}

// runConfig ports cli.ts:48-113; help is only the first argument, not a global flag.
func runConfig(args []string, env scope.Env, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, configUsage)
		return 2
	}
	action := args[0]
	if action == "--help" || action == "-h" || action == "help" {
		fmt.Fprint(stdout, configUsage)
		return 0
	}
	home, err := resolveFeatureHome(env)
	if err == nil {
		home, err = configguard.ResolveCodexHome(home) // one physical home (CRW-1144)
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw: "+err.Error())
		return 1
	}
	path := filepath.Join(home, "config.toml")
	if action == "list" {
		states, err := configguard.ReadManagedState(path)
		if err != nil {
			fmt.Fprintln(stderr, "crw: "+err.Error())
			return 1
		}
		for _, state := range states {
			fmt.Fprintf(stdout, "%s = %s\n  %s\n", configguard.ManagedKeyID(state.Entry), configStateValue(state), state.Entry.Caution)
		}
		if len(states) == 0 {
			fmt.Fprintln(stdout, "(no managed keys)")
		}
		return 0
	}
	if len(args) < 2 || args[1] == "" {
		fmt.Fprintf(stderr, "config %s: a <table.key> argument is required\n%s", action, configUsage)
		return 2
	}
	id := args[1]
	if action == "get" {
		if entry, reason := configguard.ResolveManagedKey(id); entry == nil {
			fmt.Fprintln(stderr, reason)
			return 2
		}
		states, err := configguard.ReadManagedState(path)
		if err != nil {
			fmt.Fprintln(stderr, "crw: "+err.Error())
			return 1
		}
		value := "(unset)"
		for _, state := range states {
			if configguard.ManagedKeyID(state.Entry) == id {
				value = configStateValue(state)
				break
			}
		}
		fmt.Fprintf(stdout, "%s = %s\n", id, value)
		return 0
	}
	if action != "set" && action != "unset" {
		fmt.Fprintf(stderr, "config: unknown action '%s'\n%s", action, configUsage)
		return 2
	}
	var value *bool
	if action == "set" {
		raw := ""
		if len(args) > 2 {
			raw = args[2]
		}
		if raw != "true" && raw != "false" {
			fmt.Fprintf(stderr, "config set: the value must be true or false, got '%s'\n", raw)
			return 2
		}
		parsed := raw == "true"
		value = &parsed
		entry, reason := configguard.ResolveManagedKey(id)
		if entry == nil {
			fmt.Fprintln(stderr, reason)
			return 2
		}
		fmt.Fprintf(stdout, "주의: %s\n", entry.Caution)
	}
	if action == "unset" && len(args) > 2 && args[2] == "--release" {
		// The explicit release of a record crw no longer owns (CRW-1149): config.toml is not touched.
		r, err := configguard.ReleaseManagedKey(configguard.ConfigSetDeps{CodexHome: home}, id)
		if err != nil && !r.OK {
			fmt.Fprintln(stderr, "crw: "+err.Error())
			return 1
		}
		renderRecovered(stdout, r.Recovered)
		if !r.OK {
			fmt.Fprintf(stderr, "config unset: %s\n", r.Reason)
			return 1
		}
		fmt.Fprintf(stdout, "%s: crw's record released; config.toml left as it is\n", id)
		if err != nil {
			fmt.Fprintln(stderr, "crw: "+err.Error())
			return 1
		}
		return 0
	}
	r, err := configguard.ApplyManagedKey(configguard.ConfigSetDeps{CodexHome: home}, id, value)
	if err != nil && !r.OK {
		fmt.Fprintln(stderr, "crw: "+err.Error())
		return 1
	}
	renderRecovered(stdout, r.Recovered)
	if !r.OK {
		fmt.Fprintf(stderr, "config %s: %s\n", action, r.Reason)
		return 1
	}
	suffix := ""
	if !r.Changed && action == "set" {
		suffix = " (already set; recorded)"
	} else if !r.Changed {
		suffix = " (already at that value; record cleared)"
	}
	fmt.Fprintf(stdout, "%s: %s -> %s%s\n", id, configValue(r.PriorValue, "(unset)"), r.AppliedValue, suffix)
	if r.BackupPath != nil {
		fmt.Fprintf(stdout, "backup: %s\n", *r.BackupPath)
	}
	if err != nil {
		// In place and recorded, but not known to be durable (CRW-1153).
		fmt.Fprintln(stderr, "crw: "+err.Error())
		return 1
	}
	return 0
}
