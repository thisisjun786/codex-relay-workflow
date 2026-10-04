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
			fmt.Fprintf(stdout, "%s = %s\n  %s\n", configguard.ManagedKeyID(state.Entry), configValue(state.Value, "(unset)"), state.Entry.Caution)
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
		var value *string
		for _, state := range states {
			if configguard.ManagedKeyID(state.Entry) == id {
				value = state.Value
				break
			}
		}
		fmt.Fprintf(stdout, "%s = %s\n", id, configValue(value, "(unset)"))
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
	r, err := configguard.ApplyManagedKey(configguard.ConfigSetDeps{CodexHome: home}, id, value)
	if err != nil {
		fmt.Fprintln(stderr, "crw: "+err.Error())
		return 1
	}
	if !r.OK {
		fmt.Fprintf(stderr, "config %s: %s\n", action, r.Reason)
		return 1
	}
	suffix := ""
	if !r.Changed {
		suffix = " (already set; recorded)"
	}
	fmt.Fprintf(stdout, "%s: %s -> %s%s\n", id, configValue(r.PriorValue, "(unset)"), r.AppliedValue, suffix)
	if r.BackupPath != nil {
		fmt.Fprintf(stdout, "backup: %s\n", *r.BackupPath)
	}
	return 0
}
