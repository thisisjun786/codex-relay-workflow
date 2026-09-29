package install

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// installsIn is every install entry of component name the record lists inside environment,
// newest last.
func installsIn(rec Object, name, environment string) []Object {
	components, _ := record.Get(rec, "components").(Object)
	component, _ := record.Get(components, name).(Object)
	entries, _ := record.Get(component, "installs").([]any)
	var found []Object
	for _, raw := range entries {
		install, ok := raw.(Object)
		if !ok {
			continue
		}
		location, _ := record.Get(install, "location").(string)
		if location != "" && record.Under(location, environment) {
			found = append(found, install)
		}
	}
	return found
}

// environmentOf is the runtime directory an install entry lives in.
func environmentOf(install Object) string {
	if environment, ok := record.Get(install, "environment").(string); ok && environment != "" {
		return environment
	}
	location, _ := record.Get(install, "location").(string)
	return filepath.Dir(location)
}

// installAt is the install entry the record lists for component name at exactly location.
func installAt(rec Object, name, location string) Object {
	components, _ := record.Get(rec, "components").(Object)
	component, _ := record.Get(components, name).(Object)
	entries, _ := record.Get(component, "installs").([]any)
	var found Object
	for _, raw := range entries {
		if install, ok := raw.(Object); ok && record.Get(install, "location") == location {
			found = install
		}
	}
	return found
}

// rollbackTarget is the selection a rollback returns to and the one directory it lives in: the
// record's outgoing selection, or the installs the record lists inside a named directory.
func rollbackTarget(rec Object, named string) ([]contract.Field, string, string) {
	var selection []contract.Field
	environments := map[string]bool{}
	var environment string
	for _, c := range definition.Components {
		var install Object
		if named != "" {
			found := installsIn(rec, c.Name, named)
			if len(found) == 0 {
				return nil, "", "the host record lists no install of " + c.Name + " inside " + named + ", so it is not a runtime this host installed"
			}
			install = found[len(found)-1]
		} else {
			outgoing, _ := record.Get(rec, "outgoing").(Object)
			entry, _ := record.Get(outgoing, c.Name).(Object)
			location, _ := record.Get(entry, "selected").(string)
			if location == "" {
				return nil, "", "the host record carries no outgoing selection of " + c.Name + " to return to; name a runtime directory the record lists instead"
			}
			if install = installAt(rec, c.Name, location); install == nil {
				return nil, "", "the outgoing selection of " + c.Name + " (" + location + ") has no install entry in the host record"
			}
		}
		environment = environmentOf(install)
		resolved, err := record.Resolve(environment)
		if err != nil {
			resolved = environment
		}
		environments[resolved] = true
		selection = append(selection, field(c.Name, record.Get(install, "location")))
	}
	if len(environments) != 1 {
		return nil, "", "the selection to return to spans more than one runtime directory, so there is no single runtime to point at"
	}
	return selection, environment, ""
}

// Rollback is `crw install rollback [<dir>]`: point the owned pointer back at the selection
// the last promotion replaced (the record's outgoing), or at a runtime directory the record
// lists, under the promotion lock and with the same gate, ownership, second-owner and settings
// rules as a promotion, reading the pointer back. The runtime it leaves stays installed, and
// is recorded as outgoing, so a second rollback returns to it.
func Rollback(ctx context.Context, o Options, named string) (Object, int) {
	base := Object{field("command", "rollback"), field("applied", false)}
	if named != "" {
		absolute, err := filepath.Abs(named)
		if err != nil {
			return append(base, field("refused", err.Error())), Usage
		}
		named = absolute
	}
	exclusive, err := record.Promote(o.RecordPath, 0)
	if err != nil {
		return append(base, field("refused", "another run holds the promotion lock: "+err.Error()), field("note", "nothing was written.")), Refused
	}
	defer exclusive.Release()
	loaded := record.Load(o.RecordPath, definition.Version)
	if !loaded.OK() {
		detail := loaded.Detail
		if loaded.State == reading.Absent {
			detail = "no host record exists, so nothing was ever promoted here and there is nothing to return to"
		}
		return append(base, field("refused", detail), field("hostRecordState", loaded.State), field("note", "nothing was written.")), Refused
	}
	rec := loaded.Value.(Object)
	pointerPath := recordedPointer(rec, o.Dest)
	selection, environment, why := rollbackTarget(rec, named)
	if why != "" {
		return append(base, field("refused", why), field("note", "nothing was written.")), Refused
	}
	base = append(base, field("environment", environment))
	current, _ := record.Get(rec, "selected").(Object)
	current = copyObject(current)
	outgoingBefore, _ := record.Get(rec, "outgoing").(Object)
	if names := pointer.Names(pointerPath, environment); names != nil && *names && selectsEvery(rec, environment) {
		return append(base, field("refused", "the pointer already names "+environment+" and the record selects it, so there is nothing to roll back to it"), field("note", "nothing was written.")), Refused
	}
	kind := doctor.RuntimeKind(environment)
	if kind != doctor.KindGoRuntime && kind != doctor.KindPythonVenv {
		return append(base, field("refused", environment+" is neither a Go runtime (bin/crw) nor a Python virtual environment, so the pointer is not moved to it"), field("note", "nothing was written.")), Refused
	}
	claim := staging.ReadClaim(environment)
	if liveness, detail := staging.OwnerLiveness(environment); liveness != staging.Dead {
		return append(base, field("refused", "whether another run is still building "+environment+" is not established as no: "+detail), field("note", "nothing was written.")), Refused
	}
	if !staging.IsSettled(claim) {
		return append(base, field("refused", environment+" carries no settled claim of this command's ("+claim.State+"), so it is not a runtime whose install finished"), field("note", "nothing was written.")), Refused
	}
	var candidateSchema Object
	if kind == doctor.KindPythonVenv {
		// This command runs no interpreter. The Go store executes the identical DDL the Python
		// store runs (docs/port/decisions.md 14), and until the commit point no Go release
		// changes it (docs/port/cutover.md), so this build's declared schema stands for the
		// Python runtime's, and a store holding anything else still refuses.
		candidateSchema = record.Set(swapgate.DeclaredSchema(ctx), "command", "this crw build's declared schema, standing for the Python runtime's identical DDL (docs/port/decisions.md 14)")
	}
	gate := swapGate(ctx, o, rec, environment, candidateSchema)
	if record.Get(gate, "verdict") != swapgate.Allowed {
		return append(base, field("refused", "it is not established that the runtime can be replaced now: "+strings.Join(stringsOf(record.Get(gate, "blockedBy"), record.Get(gate, "unreadable")), "; ")), field("swapGate", gate), field("note", "nothing was written; the pointer still names the runtime it named.")), Refused
	}
	ownedBefore, _ := record.Get(rec, "pointer").(Object)
	before := pointer.Read(pointerPath)
	if !pointer.Usable(before.State) || (before.State == pointer.Link && !record.PlacementRecorded(ownedBefore)) {
		return append(base, field("refused", "the owned pointer is not one this command may replace: "+before.Detail), field("pointer", pointerObject(pointerPath)), field("note", "nothing was written.")), Refused
	}
	owners, conflict := secondOwners(o.CodexHome, filepath.Join(pointerPath, "bin", definition.Bridge))
	if conflict != "" {
		return append(base, field("refused", conflict), field("secondOwner", owners), field("note", "nothing was written.")), Refused
	}
	transition := transitionSettings(o.CodexHome, pointerPath, kind)
	if transition.refused != "" {
		return append(base, field("refused", transition.refused), field("settings", append(transition.report, field("undone", transition.undo()))), field("note", "nothing was written; any Stop settings this run had set aside were put back (settings.undone).")), Refused
	}
	outgoing := outgoingOf(current)
	committed, err := commitSelection(o.RecordPath, definition.Version, record.Delta{
		Select:   selection,
		Pointer:  Object{field("path", pointerPath), field("recordedAt", o.stamp()), field("recordedBy", o.Issue)},
		Outgoing: &record.Outgoing{Value: outgoing},
	})
	if err != nil || !committed.Usable() {
		undone := transition.undo()
		return append(base, field("refused", "the selection could not be committed: "+commitDetail(committed, err)), field("settings", append(transition.report, field("undone", undone))),
			field("note", "the pointer was not moved, and the Stop settings were put back as 'settings.undone' says.")), Refused
	}
	placeErr := placePointer(pointerPath, environment)
	landed := pointer.Names(pointerPath, environment)
	if placeErr != nil || landed == nil || !*landed {
		detail := "the pointer does not name " + environment + " after it was placed"
		if placeErr != nil {
			detail = "the pointer could not be placed: " + store.PythonOSError(placeErr)
		}
		putBack := restorePointer(o, pointerPath, before, environment, ownedBefore)
		restored := restoreSelection(o, current, selection, outgoingBefore)
		undone := transition.undo()
		return append(base, field("refused", detail), field("pointerRestored", putBack), field("selectionRestored", restored), field("settings", append(transition.report, field("undone", undone))),
			field("note", "the selection, the pointer and the Stop settings were put back to what this run found.")), Refused
	}
	var previousTarget any
	if before.Target != "" {
		previousTarget = before.Target
	}
	return Object{
		field("command", "rollback"), field("applied", true), field("environment", environment), field("kind", kind),
		field("pointer", Object{field("path", pointerPath), field("target", environment), field("previousTarget", previousTarget)}),
		field("selected", record.Get(committed.Value.(Object), "selected")), field("previousSelection", current), field("outgoing", outgoing),
		field("swapGate", gate), field("secondOwner", owners), field("settings", transition.report),
		field("note", "the pointer names the runtime the record selects again, read back after the move. The runtime it left is still installed and is now the outgoing selection, so rolling back again returns to it. A process already started keeps the runtime it started in."),
	}, OK
}

func stringsOf(values ...any) []string {
	var out []string
	for _, v := range values {
		for _, item := range asList(v) {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
