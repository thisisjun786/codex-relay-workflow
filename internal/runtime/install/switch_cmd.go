package install

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/role"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/switchstate"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

const switchUsage = "Usage:\n" +
	"  crw install switch crw       turn the CRW hooks on and the CXC plugin off\n" +
	"  crw install switch cxc       return to CXC: restore what the switch changed\n" +
	"  crw install switch status    which side is on, plugin keys, hook trust, role files\n\n" +
	"  --json                 print one JSON document\n" +
	"  --codex-home <dir>     the Codex home (default $CODEX_HOME or ~/.codex)\n" +
	"  --plugin-root <dir>    the installed CRW plugin package whose hook trust status reports\n" +
	"  --help / -h / help in any argument position prints this text and writes nothing.\n\n" +
	"switch writes $CODEX_HOME/crw/switch.json, sets [plugins.\"codexclaw@codexclaw\"] enabled = false\n" +
	"(crw@crw is never touched), and replaces CXC-owned agents/executor.toml and agents/architect.toml with the\n" +
	"CRW role files. Every value it changes is recorded first in the `switch` section of\n" +
	"$CODEX_HOME/.crw-install.json, config.toml and each role file are copied to a timestamped .crw-<ts>.bak\n" +
	"before they change, and a step that fails undoes the steps before it. `switch cxc` puts the recorded\n" +
	"values back byte for byte. `features disable` does not touch the switch.\n"

// The four states status reports.
const (
	switchStateCRW      = "crw"
	switchStateCXC      = "cxc"
	switchStateConflict = "conflict"
	switchStateOff      = "off"
)

type switchPluginStatus struct {
	Key     string `json:"key"`
	Present bool   `json:"present"`
	Enabled bool   `json:"enabled"`
}

type switchFileStatus struct {
	Path      string `json:"path"`
	Active    string `json:"active,omitempty"`
	ChangedAt string `json:"changedAt,omitempty"`
	By        string `json:"by,omitempty"`
	Error     string `json:"error,omitempty"`
}

type switchTrustStatus struct {
	Available  bool   `json:"available"`
	PluginRoot string `json:"pluginRoot,omitempty"`
	Key        string `json:"key,omitempty"`
	Trusted    int    `json:"trusted"`
	Drifted    int    `json:"drifted"`
	Untrusted  int    `json:"untrusted"`
	Reason     string `json:"reason,omitempty"`
}

type switchRoleStatus struct {
	Role  string `json:"role"`
	Path  string `json:"path"`
	Owner string `json:"owner"`
}

type switchRecordStatus struct {
	Present bool   `json:"present"`
	Active  string `json:"active,omitempty"`
	Pending bool   `json:"pending,omitempty"`
}

// SwitchStatus is the document `crw install switch status` prints.
type SwitchStatus struct {
	Command   string                        `json:"command"`
	Action    string                        `json:"action"`
	State     string                        `json:"state"`
	CodexHome string                        `json:"codexHome"`
	Switch    switchFileStatus              `json:"switchFile"`
	Plugins   map[string]switchPluginStatus `json:"plugins"`
	HookTrust switchTrustStatus             `json:"hookTrust"`
	Roles     []switchRoleStatus            `json:"roles"`
	Record    switchRecordStatus            `json:"record"`
	Notes     []string                      `json:"notes,omitempty"`
}

type switchKeyOut struct {
	Table  string `json:"table"`
	Key    string `json:"key"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type switchRoleOut struct {
	Role       string  `json:"role"`
	Path       string  `json:"path"`
	Owner      string  `json:"owner"`
	Action     string  `json:"action"`
	BackupPath *string `json:"backupPath"`
}

type switchResult struct {
	Command       string          `json:"command"`
	Action        string          `json:"action"`
	OK            bool            `json:"ok"`
	Active        string          `json:"active"`
	ChangedAt     string          `json:"changedAt"`
	ConfigChanged bool            `json:"configChanged"`
	ConfigBackup  *string         `json:"configBackup"`
	Keys          []switchKeyOut  `json:"keys"`
	Roles         []switchRoleOut `json:"roles"`
	Notes         []string        `json:"notes"`
	Manifest      string          `json:"manifest"`
	Status        *SwitchStatus   `json:"status,omitempty"`
}

// runSwitch is `crw install switch`: routed before the generic install options, as features and
// config are, because it prints its own text or JSON report.
func runSwitch(args []string, env scope.Env, stdout, stderr io.Writer) int {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "help" {
			fmt.Fprint(stdout, switchUsage)
			return OK
		}
	}
	flags := flag.NewFlagSet("crw install switch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOut := flags.Bool("json", false, "print one JSON document")
	codexHome := flags.String("codex-home", "", "the Codex home (default $CODEX_HOME or ~/.codex)")
	pluginRoot := flags.String("plugin-root", "", "the installed CRW plugin package whose hook trust status reports")
	positional, err := parse(flags, args)
	if err != nil {
		return Usage
	}
	if len(positional) != 1 || (positional[0] != "crw" && positional[0] != "cxc" && positional[0] != "status") {
		fmt.Fprint(stderr, "usage: crw install switch <crw|cxc|status> [--json] [--codex-home <dir>]\n")
		return Usage
	}
	action := positional[0]
	home := *codexHome
	if home == "" {
		if home, err = resolveFeatureHome(env); err != nil {
			fmt.Fprintln(stderr, "crw: "+err.Error())
			return Refused
		}
	}
	root := *pluginRoot
	if root == "" {
		root = env.Get("PLUGIN_ROOT")
	}
	if action == "status" {
		status := ReadSwitchStatus(home, root)
		return renderSwitch(stdout, *jsonOut, status, nil)
	}
	report, err := configguard.RunSwitch(configguard.SwitchDeps{CodexHome: home}, action)
	if err != nil {
		fmt.Fprintln(stderr, "crw: switch "+action+": "+err.Error())
		return Refused
	}
	status := ReadSwitchStatus(home, root)
	result := switchResultOf(action, report, status)
	return renderSwitch(stdout, *jsonOut, status, &result)
}

func switchResultOf(action string, r *configguard.SwitchReport, status *SwitchStatus) switchResult {
	out := switchResult{Command: "switch", Action: action, OK: true, Active: r.Active, ChangedAt: r.ChangedAt, ConfigChanged: r.ConfigChanged,
		ConfigBackup: r.ConfigBackup, Keys: []switchKeyOut{}, Roles: []switchRoleOut{}, Notes: append([]string{}, r.Notes...), Manifest: r.ManifestPath, Status: status}
	for _, k := range r.Keys {
		out.Keys = append(out.Keys, switchKeyOut{k.Table, k.Key, k.Before, k.After})
	}
	for _, x := range r.Roles {
		out.Roles = append(out.Roles, switchRoleOut{x.Role, x.Path, x.Owner, x.Action, x.BackupPath})
	}
	return out
}

func renderSwitch(stdout io.Writer, asJSON bool, status *SwitchStatus, result *switchResult) int {
	if asJSON {
		var doc any = status
		if result != nil {
			doc = result
		}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return Refused
		}
		fmt.Fprintln(stdout, string(b))
		return OK
	}
	if result != nil {
		fmt.Fprintf(stdout, "crw: switched to %s at %s\n", result.Active, result.ChangedAt)
		for _, k := range result.Keys {
			fmt.Fprintf(stdout, "  [%s] %s: %s -> %s\n", k.Table, k.Key, k.Before, k.After)
		}
		for _, x := range result.Roles {
			line := fmt.Sprintf("  role %s: %s", x.Role, x.Action)
			if x.BackupPath != nil {
				line += " (backup " + *x.BackupPath + ")"
			}
			fmt.Fprintln(stdout, line)
		}
		if result.ConfigBackup != nil {
			fmt.Fprintf(stdout, "  config backup: %s\n", *result.ConfigBackup)
		}
		for _, n := range result.Notes {
			fmt.Fprintf(stdout, "  note: %s\n", n)
		}
	}
	fmt.Fprintf(stdout, "state: %s\n", status.State)
	if status.Switch.Active != "" {
		fmt.Fprintf(stdout, "switch.json: active=%s changedAt=%s by=%s\n", status.Switch.Active, status.Switch.ChangedAt, status.Switch.By)
	} else if status.Switch.Error != "" {
		fmt.Fprintf(stdout, "switch.json: %s\n", status.Switch.Error)
	} else {
		fmt.Fprintln(stdout, "switch.json: absent")
	}
	names := make([]string, 0, len(status.Plugins))
	for name := range status.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := status.Plugins[name]
		state := "not configured"
		switch {
		case p.Present && p.Enabled:
			state = "enabled"
		case p.Present:
			state = "disabled"
		}
		fmt.Fprintf(stdout, "plugin %s: %s\n", p.Key, state)
	}
	if status.HookTrust.Available {
		fmt.Fprintf(stdout, "hook trust (%s): trusted %d, drifted %d, untrusted %d\n", status.HookTrust.Key, status.HookTrust.Trusted, status.HookTrust.Drifted, status.HookTrust.Untrusted)
	} else {
		fmt.Fprintf(stdout, "hook trust: unavailable (%s)\n", status.HookTrust.Reason)
	}
	for _, r := range status.Roles {
		fmt.Fprintf(stdout, "role %s: %s\n", r.Role, r.Owner)
	}
	if status.Record.Present {
		fmt.Fprintf(stdout, "record: %s%s\n", status.Record.Active, map[bool]string{true: " (pending: run the switch again)", false: ""}[status.Record.Pending])
	}
	for _, n := range status.Notes {
		fmt.Fprintf(stdout, "note: %s\n", n)
	}
	return OK
}

// ReadSwitchStatus reads, and writes nothing. pluginRoot may be empty: the installed package is
// then looked for in the Codex home's plugin cache.
func ReadSwitchStatus(home, pluginRoot string) *SwitchStatus {
	s := &SwitchStatus{Command: "switch", Action: "status", CodexHome: home, Plugins: map[string]switchPluginStatus{}, Roles: []switchRoleStatus{}, Notes: []string{}}
	s.Switch.Path = switchstate.Path(home)
	selectedCRW := false
	if st, err := switchstate.Read(home); err != nil {
		s.Switch.Error = err.Error()
	} else if st != nil {
		s.Switch.Active, s.Switch.ChangedAt, s.Switch.By = string(st.Active), st.ChangedAt, st.By
		selectedCRW = st.Active == switchstate.CRW
	}
	config, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	for _, p := range []struct{ name, plugin string }{{"crw", "crw"}, {"codexclaw", "codexclaw"}} {
		keys, err := doctor.ReadInstalledPluginKeys(home, p.plugin)
		if err != nil {
			s.Notes = append(s.Notes, "plugin "+p.plugin+": "+err.Error())
		}
		key := p.plugin + "@" + p.plugin
		present := configguard.FindTableHeader(strings.Split(string(config), "\n"), `plugins."`+key+`"`) >= 0
		enabled := false
		for _, k := range keys {
			enabled = enabled || k == key
		}
		s.Plugins[p.name] = switchPluginStatus{Key: key, Present: present, Enabled: enabled}
	}
	// The CRW hooks act only while the CRW plugin is on: switch.json says which side was picked, the
	// plugin's own key says whether it is running.
	surfaceCRW := selectedCRW && s.Plugins["crw"].Enabled
	surfaceCXC := s.Plugins["codexclaw"].Enabled
	if selectedCRW && !s.Plugins["crw"].Enabled {
		s.Notes = append(s.Notes, "switch.json selects crw, but the CRW plugin ("+s.Plugins["crw"].Key+") is not enabled in config.toml: the CRW hooks are off; `crw install switch` does not turn crw@crw on")
	}
	switch {
	case surfaceCRW && surfaceCXC:
		s.State = switchStateConflict
		s.Notes = append(s.Notes, "both the CRW hooks and the CXC plugin are on: run `crw install switch crw` (CXC off) or `crw install switch cxc`")
	case surfaceCRW:
		s.State = switchStateCRW
	case surfaceCXC:
		s.State = switchStateCXC
	default:
		s.State = switchStateOff
	}
	s.HookTrust = switchTrust(home, pluginRoot)
	for _, name := range role.NativeRoles() {
		raw, err := role.ReadRoleFile(home, name)
		owner := string(role.OwnerOf(raw))
		if err != nil {
			owner = "unreadable"
			s.Notes = append(s.Notes, "role "+string(name)+": "+err.Error())
		}
		s.Roles = append(s.Roles, switchRoleStatus{Role: string(name), Path: role.RoleFilePath(home, name), Owner: owner})
	}
	if m, err := configguard.ReadInstallManifest(home); err != nil {
		s.Notes = append(s.Notes, err.Error())
	} else if m != nil && m.Switch != nil {
		s.Record = switchRecordStatus{Present: true, Active: m.Switch.Active, Pending: m.Switch.Pending}
		if m.Switch.Pending {
			s.Notes = append(s.Notes, "a switch did not finish: run `crw install switch "+m.Switch.Active+"` again")
		}
	}
	return s
}

func switchTrust(home, pluginRoot string) switchTrustStatus {
	unavailable := func(reason string) switchTrustStatus { return switchTrustStatus{Reason: reason} }
	if pluginRoot == "" {
		matches, _ := filepath.Glob(filepath.Join(home, "plugins", "cache", "*", "crw", "*", ".codex-plugin", "plugin.json"))
		switch len(matches) {
		case 0:
			return unavailable("no installed crw plugin package under " + filepath.Join(home, "plugins", "cache"))
		case 1:
			pluginRoot = filepath.Dir(filepath.Dir(matches[0]))
		default:
			return unavailable(fmt.Sprintf("%d crw plugin packages in the cache; pass --plugin-root", len(matches)))
		}
	}
	keys, err := doctor.ReadInstalledPluginKeys(home, "crw")
	if err != nil {
		return unavailable(err.Error())
	}
	if len(keys) != 1 {
		return unavailable(fmt.Sprintf("%d enabled crw install keys", len(keys)))
	}
	results, err := doctor.DiagnoseHookTrust(home, pluginRoot, keys[0])
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return unavailable(pathErr.Error())
		}
		return unavailable(err.Error())
	}
	t := switchTrustStatus{Available: true, PluginRoot: pluginRoot, Key: keys[0]}
	for _, r := range results {
		switch r.Status {
		case "trusted":
			t.Trusted++
		case "drifted":
			t.Drifted++
		default:
			t.Untrusted++
		}
	}
	return t
}
