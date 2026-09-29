package install

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

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
// record's outgoing selection, or the newest installs the record lists for exactly the named
// runtime directory - never for a directory that merely contains one.
func rollbackTarget(rec Object, named string) ([]contract.Field, string, string) {
	var selection []contract.Field
	environments := map[string]bool{}
	var environment string
	for _, c := range definition.Components {
		var install Object
		if named != "" {
			found := installsAt(rec, c.Name, named)
			if len(found) == 0 {
				return nil, "", "the host record lists no install of " + c.Name + " whose runtime directory is " + named + ", so it is not a runtime this host installed; name the runtime directory itself (an env-* or bin-* directory under the destination)"
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

// claimSays is what a readable claim says its run did, or "" when it could not be read.
func claimSays(environment string) (string, bool) {
	claim := staging.ReadClaim(environment)
	value, ok := claim.Value.(Object)
	if !claim.OK() || !ok {
		return "", false
	}
	says, _ := record.Get(value, "state").(string)
	return says, true
}

// selectedAs is whether the record, now or as its outgoing baseline, selects exactly selection:
// proof that a promotion committed that runtime, so that it was exercised and put in service
// even where its claim never settled (an exit 3, or a run killed after its pointer moved).
func selectedAs(rec Object, selection []contract.Field) bool {
	selected, _ := record.Get(rec, "selected").(Object)
	outgoing, _ := record.Get(rec, "outgoing").(Object)
	now, before := true, true
	for _, f := range selection {
		now = now && record.Get(selected, f.Key) == f.Value
		entry, _ := record.Get(outgoing, f.Key).(Object)
		before = before && record.Get(entry, "selected") == f.Value
	}
	return len(selection) > 0 && (now || before)
}

// Rollback is `crw install rollback [<dir>]`: point the owned pointer back at the selection
// the last promotion replaced (the record's outgoing), or at a runtime directory the record
// lists, under the target's directory lock and the promotion lock and with the same gate,
// ownership, second-owner and settings rules as a promotion, reading the pointer back. The target
// has to be launchable as it stands (nothing is run), and the one Stop settings document is never
// rewritten toward a venv: the venv has to serve it. The runtime the pointer leaves stays
// installed and is recorded as outgoing, so a second rollback returns to it. Where the pointer
// already names the target only the record moves, so the swap gate, which guards replacing a
// runtime, is not asked, and outgoing is left as it was.
//
// Lock order (docs/port/decisions.md 33): the target's <env>.crw-lock, then the host-wide
// .promotion-lock - runtime_install.py's order (its take/reclaim step holds the directory's lock,
// and its resume runs the promotion inside it) and take()'s. The target is found on a first
// reading, its directory lock taken, and the target found again under the promotion lock; a
// record that moved it in between refuses with nothing written.
func Rollback(ctx context.Context, o Options, named string) (Object, int) {
	base := Object{field("command", "rollback"), field("applied", false)}
	if !record.Stated(o.Issue) {
		// As Install: the placement a rollback records is rejected by record.PlacementRecorded
		// when recordedBy is blank, and every later run would refuse the pointer as unowned.
		return append(base, field("refused", "--issue is written into the host record as the evidence that this command placed the owned pointer, so it has to say something"), field("note", "nothing was read, no lock was taken and nothing was written.")), Refused
	}
	if named != "" {
		if strings.TrimSpace(named) == "" {
			return append(base, field("refused", "an empty directory names no runtime; name one, or give no directory to return to the outgoing selection")), Usage
		}
		absolute, err := filepath.Abs(named)
		if err != nil {
			return append(base, field("refused", err.Error())), Usage
		}
		named = absolute
	}
	pointerPath := pointer.Path(o.Dest)
	find := func() (Object, []contract.Field, string, Object, int) {
		loaded := record.Load(o.RecordPath, definition.Version)
		if !loaded.OK() {
			detail := loaded.Detail
			if loaded.State == reading.Absent {
				detail = "no host record exists, so nothing was ever promoted here and there is nothing to return to"
			}
			refused, code := append(append(Object{}, base...), field("refused", detail), field("hostRecordState", loaded.State), field("note", "nothing was written.")), Refused
			return nil, nil, "", refused, code
		}
		rec := loaded.Value.(Object)
		if why := foreignPointer(rec, o.Dest); why != "" {
			refused, code := append(append(Object{}, base...), field("refused", why), field("repair", foreignRepair(o)), field("note", "nothing was written.")), Refused
			return nil, nil, "", refused, code
		}
		if named == "" {
			if target, ok := pointerTarget(pointerPath); ok && targetRecorded(rec, target) && !selectsEvery(rec, target) {
				refused, code := append(append(Object{}, base...), field("refused", "the owned pointer names "+target+" and the host record does not select it: a promotion or a rollback was interrupted between its commit and its pointer move, so returning to the outgoing selection could undo a committed choice"),
					field("selected", record.Get(rec, "selected")), field("repair", "name the runtime to be on: crw install rollback <the directory the record selects> finishes the interrupted move, and crw install rollback "+target+" keeps the one the pointer names"),
					field("note", "nothing was written.")), Refused
				return nil, nil, "", refused, code
			}
		}
		selection, environment, why := rollbackTarget(rec, named)
		if why != "" {
			refused, code := append(append(Object{}, base...), field("refused", why), field("note", "nothing was written.")), Refused
			return nil, nil, "", refused, code
		}
		return rec, selection, environment, nil, 0
	}
	_, _, candidate, refused, code := find()
	if refused != nil {
		return refused, code
	}
	lock, err := record.Lock(candidate, 0)
	if err != nil {
		return append(base, field("environment", candidate), field("refused", "another run is deciding what to do with this runtime directory ("+err.Error()+"): runtime_install.py and crw install hold "+candidate+record.LockSuffix+" while they decide about it, and one left by a run that died is removed once it is "+record.StaleLock.String()+" old"), field("note", "nothing was written.")), Refused
	}
	defer lock.Release()
	exclusive, err := record.Promote(o.RecordPath, 0)
	if err != nil {
		return append(base, field("refused", "another run holds the promotion lock: "+err.Error()), field("note", "nothing was written.")), Refused
	}
	defer exclusive.Release()
	rec, selection, environment, refused, code := find()
	if refused != nil {
		return refused, code
	}
	if !reading.SameDirectory(environment, candidate) && !sameSpelling(environment, candidate) {
		return append(base, field("environment", environment), field("refused", "the host record changed while this run took its locks: the runtime to return to is now "+environment+", not "+candidate), field("note", "nothing was written; rerun to decide against the record as it now stands.")), Refused
	}
	base = append(base, field("environment", environment))
	nothing := func(detail string, extra ...contract.Field) (Object, int) {
		return append(append(base, field("refused", detail)), append(extra, field("note", "nothing was written; the pointer still names the runtime it named."))...), Refused
	}
	current, _ := record.Get(rec, "selected").(Object)
	current = copyObject(current)
	outgoingBefore, hadOutgoing := record.Lookup(rec, "outgoing")
	names := pointer.Names(pointerPath, environment)
	moving := names == nil || !*names
	if !moving && selectsEvery(rec, environment) {
		return nothing("the pointer already names " + environment + " and the record selects it, so there is nothing to roll back to it")
	}
	kind := doctor.RuntimeKind(environment)
	if kind != doctor.KindGoRuntime && kind != doctor.KindPythonVenv {
		return nothing(environment + " is neither a Go runtime (bin/crw) nor a Python virtual environment, so the pointer is not moved to it")
	}
	if liveness, detail := staging.OwnerLiveness(environment); liveness != staging.Dead {
		return nothing("whether another run is still building " + environment + " is not established as no: " + detail)
	}
	says, readable := claimSays(environment)
	settle := false
	switch {
	case says == staging.Complete:
	case readable && says == staging.Staging && selectedAs(rec, selection):
		// Committed by a promotion whose claim never settled; returning to it finishes that
		// bookkeeping, as resuming does.
		settle = true
	case readable:
		return nothing(environment + " carries a claim that says " + evidence.Repr(says) + ", and no selection of the host record proves a promotion put it in service, so it is not a runtime whose install finished")
	default:
		return nothing(environment + " carries no readable claim of this command's (" + staging.ReadClaim(environment).State + "), so it is not a runtime whose install finished")
	}
	if problems, unread := targetProblems(rec, environment, kind); len(problems)+len(unread) > 0 {
		return nothing("the pointer would name a runtime that cannot be launched as it stands: "+strings.Join(append(problems, unread...), "; "),
			field("launchable", Object{field("problems", strs(problems)), field("unread", strs(unread))}))
	}
	var gate Object
	if moving {
		var candidateSchema Object
		if kind == doctor.KindPythonVenv {
			// This command runs no interpreter. The Go store executes the identical DDL the Python
			// store runs (docs/port/decisions.md 14), and until the commit point no Go release
			// changes it (docs/port/cutover.md), so this build's declared schema stands for the
			// Python runtime's, and a store holding anything else still refuses.
			candidateSchema = record.Set(swapgate.DeclaredSchema(ctx), "command", "this crw build's declared schema, standing for the Python runtime's identical DDL (docs/port/decisions.md 14)")
		}
		gate = swapGate(ctx, o, rec, environment, candidateSchema)
		if record.Get(gate, "verdict") != swapgate.Allowed {
			return nothing("it is not established that the runtime can be replaced now: "+strings.Join(stringsOf(record.Get(gate, "blockedBy"), record.Get(gate, "unreadable")), "; "), field("swapGate", gate))
		}
	}
	ownedBefore, _ := record.Get(rec, "pointer").(Object)
	before := pointer.Read(pointerPath)
	if !pointer.Usable(before.State) || (before.State == pointer.Link && !record.PlacementRecorded(ownedBefore)) {
		return nothing("the owned pointer is not one this command may replace: "+before.Detail, field("pointer", pointerObject(pointerPath)))
	}
	target := providesFor(kind, environment)
	owners, conflict := secondOwnersFor(o.CodexHome, filepath.Join(pointerPath, "bin", definition.Bridge), pointerPath, target)
	if conflict != "" {
		return nothing(conflict, field("secondOwner", owners))
	}
	leaving := inService(rec, pointerPath)
	transition := transitionSettingsFor(o.CodexHome, pointerPath, kind, target)
	if transition.refused != "" {
		return append(base, field("refused", transition.refused), field("settings", append(transition.report, field("undone", transition.undo()))), field("note", "nothing was written; any Stop settings this run had set aside were put back (settings.undone).")), Refused
	}
	delta := record.Delta{Select: selection, Pointer: Object{field("path", pointerPath), field("recordedAt", o.stamp()), field("recordedBy", o.Issue)}}
	var outgoing any = outgoingBefore
	if !hadOutgoing {
		outgoing = nil
	}
	switch baseline, _ := outgoingBefore.(Object); {
	case moving:
		// The baseline a second rollback returns to is the runtime the host leaves - the one the
		// pointer names - which is the selection unless an interrupted move left them apart.
		left := outgoingOf(leftSelection(rec, pointerPath, current))
		delta.Outgoing, outgoing = &record.Outgoing{Value: left}, left
	case baselineIsOnly(baseline, environment):
		// The pointer stays where it is, so the host leaves nothing; outgoing is kept, unless it
		// names the target itself, when the one runtime the record selected instead (committed and
		// never reached, as an interrupted promotion leaves it) is what a second rollback returns to.
		if other := selectedRuntime(rec); other != "" && !sameSpelling(other, environment) {
			left := outgoingOf(current)
			delta.Outgoing, outgoing = &record.Outgoing{Value: left}, left
		}
	}
	committed, err := commitSelection(o.RecordPath, definition.Version, delta)
	if err != nil || !committed.Usable() {
		undone := transition.undo()
		return append(base, field("refused", "the selection could not be committed: "+commitDetail(committed, err)), field("settings", append(transition.report, field("undone", undone))),
			field("note", "the pointer was not moved, and the Stop settings were put back as 'settings.undone' says.")), Refused
	}
	var placeErr error
	if moving {
		placeErr = placePointer(pointerPath, environment)
	}
	landed := pointer.Names(pointerPath, environment)
	if placeErr != nil || landed == nil || !*landed {
		detail := "the pointer does not name " + environment + " after it was placed"
		if placeErr != nil {
			detail = "the pointer could not be placed: " + store.PythonOSError(placeErr)
		}
		var putBack any = "this run did not move the pointer"
		if moving {
			putBack = restorePointer(o, pointerPath, before, environment, ownedBefore)
		}
		outgoingObject, _ := outgoingBefore.(Object)
		restored := restoreSelection(o, current, selection, outgoingObject)
		undone := transition.undo()
		return append(base, field("refused", detail), field("pointerRestored", putBack), field("selectionRestored", restored), field("settings", append(transition.report, field("undone", undone))),
			field("note", "the selection, the pointer and the Stop settings were put back to what this run found.")), Refused
	}
	left := settleLeft(o, leaving, environment)
	exclusive.Release()
	var claim Object
	code = OK
	if settle {
		claim = settleClaim(o, environment, o.Issue)
		if record.Get(claim, "settled") != true {
			code = Incomplete
		}
	}
	var previousTarget any
	if before.Target != "" {
		previousTarget = before.Target
	}
	return Object{
		field("command", "rollback"), field("applied", true), field("environment", environment), field("kind", kind), field("moved", moving),
		field("pointer", Object{field("path", pointerPath), field("target", environment), field("previousTarget", previousTarget)}),
		field("selected", record.Get(committed.Value.(Object), "selected")), field("previousSelection", current), field("outgoing", outgoing),
		field("swapGate", orNull(gate)), field("secondOwner", owners), field("settings", transition.report), field("claim", orNull(claim)), field("leftClaim", orNull(left)),
		field("note", "the pointer names the runtime the record selects again, read back after the move. The runtime it left is still installed and is now the outgoing selection, so rolling back again returns to it. A process already started keeps the runtime it started in."),
	}, code
}

// sameSpelling is whether two runtime directories are one by their resolved spelling, for a
// directory that no longer exists (SameDirectory needs both to).
func sameSpelling(one, other string) bool {
	a, errA := record.Resolve(one)
	b, errB := record.Resolve(other)
	return errA == nil && errB == nil && a == b
}

// baselineIsOnly is whether an outgoing baseline selects nothing but environment.
func baselineIsOnly(baseline Object, environment string) bool {
	if baseline == nil {
		return false
	}
	for _, c := range definition.Components {
		entry, _ := record.Get(baseline, c.Name).(Object)
		location, _ := record.Get(entry, "selected").(string)
		if location == "" || !record.Under(location, environment) {
			return false
		}
	}
	return true
}

// selectedRuntime is the one runtime directory the record selects every component in, or "".
func selectedRuntime(rec Object) string {
	selected, _ := record.Get(rec, "selected").(Object)
	location, _ := record.Get(selected, definition.Relay).(string)
	if install := installAt(rec, definition.Relay, location); install != nil {
		if environment := environmentOf(install); selectsEvery(rec, environment) {
			return environment
		}
	}
	return ""
}

// leftSelection is the selection of the runtime the pointer names - the one a moving rollback
// leaves - when the record lists every component there; otherwise the record's selection.
func leftSelection(rec Object, pointerPath string, current Object) Object {
	target, ok := pointerTarget(pointerPath)
	if !ok || selectsEvery(rec, target) {
		return current
	}
	left := Object{}
	for _, c := range definition.Components {
		installs := installsAt(rec, c.Name, target)
		if len(installs) == 0 {
			return current
		}
		left = append(left, field(c.Name, record.Get(installs[len(installs)-1], "location")))
	}
	return left
}

// orNull is o, or JSON null for an object nobody wrote.
func orNull(o Object) any {
	if o == nil {
		return nil
	}
	return o
}

// inService is the runtime directory the record selects entirely and the pointer names: the
// one a rollback moves a host off.
func inService(rec Object, pointerPath string) string {
	target, ok := pointerTarget(pointerPath)
	if !ok || !selectsEvery(rec, target) {
		return ""
	}
	return target
}

// settleLeft writes the COMPLETE claim of the runtime a rollback moved the host off when its
// claim still says STAGING and nobody holds it: it was selected and named by the pointer, so it
// was promoted and in service, and a claim left STAGING would have the next install of its
// archive read an abandoned staging and remove a directory processes may still run out of.
func settleLeft(o Options, leaving, environment string) Object {
	if leaving == "" || leaving == environment {
		return nil
	}
	return settleInService(o, leaving, "the runtime this rollback left was promoted and in service with its claim never settled; it is kept, and now reads as a runtime whose install finished")
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
