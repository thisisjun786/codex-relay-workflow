package install

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// Budgets of the plugin-owned Stop registration (completion.py).
const (
	DefaultGuardTimeout   = 5
	RegisteredTimeout     = 10
	MaxPluginGuardSeconds = 7
	maxTimeoutSeconds     = 86400
)

// HookOptions are `crw install hook`'s inputs.
type HookOptions struct {
	Owner, Event, Relay, MarkerRoot, Database, Socket, JournalRoot, Mode, Isolation string
	GuardTimeout, Timeout                                                           int64
	DryRun                                                                          bool
}

// budgetComplaints is completion.budget_complaints: the adapter's own budget against the
// host's registered timeout, which only mean something together.
func budgetComplaints(guard, registered int64) []string {
	var found []string
	if guard <= 0 || guard > maxTimeoutSeconds {
		found = append(found, "the guard budget must be a positive number of seconds, at most "+strconv.Itoa(maxTimeoutSeconds))
	} else if guard >= registered {
		found = append(found, "the guard budget must be under the registered hook timeout of "+strconv.FormatInt(registered, 10)+"s, or the host can kill this adapter before it records why it did not answer")
	}
	if registered > RegisteredTimeout {
		found = append(found, "the registered timeout must not exceed "+strconv.Itoa(RegisteredTimeout)+"s: the host clamps an over-long timeout at discovery, and the clamped value is not measured here, so a larger number is not the deadline it appears to be")
	}
	return found
}

// Hook is `crw install hook --owner plugin`: write the settings the plugin's declared Stop hook
// (and every legacy launcher a cached turn still runs) reads, with the relay and the Go adapter
// named through the owned pointer and adapterInterpreter /usr/bin/env. It registers nothing:
// the plugin package declares the registration. It places no launcher file. A Python-era
// document is replaced only while the pointer already names a Go runtime (before that,
// `crw install install` replaces it immediately before it moves the pointer), and only by
// settings that record every host fact it records - mode, isolation, roots, database, socket,
// relay and budget - so that only the adapter and installedBy move; settings built from other
// flags answer config_differs with the fields and the repair, and nothing is written.
func Hook(ctx context.Context, o Options, h HookOptions) (Object, int) {
	hookFile := filepath.Join(o.CodexHome, "hooks.json")
	base := Object{field("command", "hook"), field("adapter", "completion"), field("owner", h.Owner), field("hookFile", hookFile)}
	usage := func(complaints []string) (Object, int) {
		return append(base, field("error", strings.Join(complaints, "; ")), field("settings", nil), field("note", "nothing was written: every precondition is checked first.")), Usage
	}
	if h.Owner != OwnerPlugin {
		return usage([]string{"only --owner plugin is supported: the user-owned registration (a hooks.json entry running completion_hook.py) is retired with runtime_install.py"})
	}
	event := h.Event
	if event == "" {
		event = "Stop"
	}
	var complaints []string
	if event != "Stop" {
		complaints = append(complaints, "this adapter implements the Stop contract and has no decision for "+event)
	}
	complaints = append(complaints, budgetComplaints(h.GuardTimeout, h.Timeout)...)
	if override := o.Env.Get(SettingsOverride); override != "" {
		complaints = append(complaints, SettingsOverride+" is set to "+override+", and a plugin-owned registration carries no settings argument: the hook rediscovers the path at every Stop from the Codex home. Unset it so the settings land where the hook looks")
	}
	mode := h.Mode
	if mode == "" {
		mode = hook.Observe
	}
	settingsInput := HookSettings{Destination: o.Dest, Relay: h.Relay, MarkerRoot: h.MarkerRoot, Database: h.Database, Mode: mode, JournalRoot: h.JournalRoot,
		Issue: o.Issue, Isolation: h.Isolation, Socket: h.Socket, Timeout: h.GuardTimeout, CodexHome: o.CodexHome}
	wanted, err := settingsInput.Document(func() (string, error) {
		selection, err := delivery.ResolveMarkerRoot("")
		return selection.Path, err
	})
	if err != nil {
		complaints = append(complaints, err.Error())
	} else {
		complaints = append(complaints, Complaints(wanted)...)
	}
	if len(complaints) > 0 {
		return usage(complaints)
	}
	path := filepath.Join(o.CodexHome, SettingsName)
	base = append(base, field("event", event))
	if found := reading.ReadJSON(path, "the completion hook configuration", nil, nil); found.OK() {
		if wrong := Complaints(asObject(found.Value)); len(wrong) > 0 {
			return append(base, field("error", "the settings at "+path+" could not be acted on ("+strings.Join(wrong, "; ")+"), so who owns this Stop registration was not established"), field("settings", nil), field("note", "nothing was written.")), Refused
		}
	}
	registrations, readable := hook.AdapterIdentities(hookFile, event)
	if !readable {
		return append(base, field("error", "the hook file could not be read, so whether this adapter is already registered for Stop was not established; nothing was written"), field("settings", nil)), Refused
	}
	if len(registrations) > 0 {
		return append(base, field("error", "this adapter is already registered for Stop in the hook file as "+strings.Join(registrations, ", ")+", which is the user-owned registration; a plugin declaration beside it would run two copies on every Stop. Remove that registration first, by hand"),
			field("registrations", strs(registrations)), field("settings", nil), field("note", "nothing was written. One owner registers this event; the other is reported with its evidence rather than joined.")), Refused
	}
	current := pointer.Path(o.Dest)
	replace := false
	if target, ok := pointerTarget(current); ok {
		replace = doctor.RuntimeKind(target) == doctor.KindGoRuntime
	}
	written := settingsWrite(ctx, path, wanted, !h.DryRun, replace)
	outcome, _ := record.Get(written, "outcome").(string)
	out := append(base, field("settings", written), field("registrations", []any{}),
		field("note", "Settings written; no registration was made and the hook file was not touched. The plugin owner registers this event through the plugin package's own manifest, so install that package to register it. Written, registered and observed to have fired stay three separate claims."))
	if configSettled[outcome] {
		return out, OK
	}
	return out, Refused
}

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

// pointerTarget is what the pointer at path resolves to, when it is a link.
func pointerTarget(path string) (string, bool) {
	read := pointer.Read(path)
	if read.State != pointer.Link {
		return "", false
	}
	target, err := pointer.TargetOf(path, read.Target)
	return target, err == nil
}
