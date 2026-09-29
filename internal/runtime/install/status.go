package install

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// Status is `crw install status`: what the host record selects, what the owned pointer names
// and whether they agree (the doctor's own reading), the outgoing selection a rollback returns
// to, and every runtime directory under the destination with its claim. It writes nothing.
func Status(_ context.Context, o Options) (Object, int) {
	loaded := record.Load(o.RecordPath, definition.Version)
	var rec Object
	var refusal any
	if loaded.Usable() {
		rec, _ = loaded.Value.(Object)
	} else {
		refusal = loaded.Refusal()
	}
	pointerPath, from := pointer.Path(o.Dest), "dest"
	if owned, ok := record.Get(rec, "pointer").(Object); ok {
		if path, ok := record.Get(owned, "path").(string); ok && filepath.IsAbs(path) {
			pointerPath, from = path, "record"
		}
	}
	runtime := doctor.Runtime(pointerPath, from, rec)
	var runtimes []any
	entries, err := os.ReadDir(o.Dest)
	var listing any
	if err != nil {
		listing = "the destination could not be listed: " + store.PythonOSError(err)
	}
	var names []string
	interrupted := []any{}
	for _, entry := range entries {
		if entry.IsDir() && runtimeDirectory(entry.Name()) {
			names = append(names, entry.Name())
		}
		if original, ok := tombstoneOf(entry.Name()); ok {
			path := filepath.Join(o.Dest, entry.Name())
			one := Object{field("path", path), field("runtime", filepath.Join(o.Dest, original))}
			if why := unclaimedTombstone(path); why != "" {
				one = append(one, field("ours", false), field("detail", why), field("recoveryRequires", nil))
			} else {
				one = append(one, field("ours", true), field("detail", "a run that set the runtime aside to delete it did not live to finish"),
					field("recoveryRequires", "crw install remove "+path+" finishes this removal, once no process runs out of it and no registration names it. Do not delete it by hand: finishing it also drops what the host record still lists under the runtime's name"))
			}
			interrupted = append(interrupted, one)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		directory := filepath.Join(o.Dest, name)
		claim := staging.ReadClaim(directory)
		var state, writtenBy any
		if value, ok := claim.Value.(Object); ok && claim.OK() {
			state, writtenBy = record.Get(value, "state"), record.Get(value, "writtenBy")
		}
		liveness, _ := staging.OwnerLiveness(directory)
		var selected any
		if rec != nil {
			selected = selectsUnder(rec, directory)
		}
		runtimes = append(runtimes, Object{
			field("path", directory), field("kind", doctor.RuntimeKind(directory)),
			field("claim", Object{field("state", claim.State), field("saying", state), field("writtenBy", writtenBy)}),
			field("liveness", liveness), field("selected", selected), field("pointerNames", boolOrNil(pointer.Names(pointerPath, directory))),
		})
	}
	if runtimes == nil {
		runtimes = []any{}
	}
	lockState, lockDetail := record.Probe(o.RecordPath + record.PromotionLockSuffix)
	settings := Object{}
	for _, f := range doctor.SettingsFiles {
		settings = append(settings, field(f.Name, doctor.ReadSettings(o.CodexHome, f.Name, f.Keys, pointerPath)))
	}
	return Object{
		field("command", "status"), field("hostRecord", o.RecordPath), field("hostRecordState", loaded.State), field("hostRecordReading", refusal),
		field("destination", o.Dest), field("destinationAgrees", destinationAgrees(rec, o)), field("selected", record.Get(runtime, "kind")), field("runtime", runtime),
		field("outgoing", record.Get(rec, "outgoing")), field("runtimes", runtimes), field("destinationListing", listing),
		field("promotionLock", Object{field("state", lockState), field("detail", lockDetail)}), field("settings", settings),
		field("interruptedRemovals", interrupted),
		field("note", "read-only. 'runtime.agrees' is whether the owned pointer contains what the host record selects; a failed install or update leaves both as they were, and its directory is gone unless it is reported here."),
	}, OK
}

// destinationAgrees is whether the host record's pointer is the fixed destination's, and when it
// is not, why every other crw install command refuses and how to bring the host back.
func destinationAgrees(rec Object, o Options) Object {
	why := foreignPointer(rec, o.Dest)
	if why == "" {
		return Object{field("agrees", true), field("pointer", pointer.Path(o.Dest))}
	}
	return Object{field("agrees", false), field("pointer", pointer.Path(o.Dest)), field("detail", why), field("repair", foreignRepair(o))}
}
