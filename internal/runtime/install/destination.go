package install

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// foreignPointer is why the host record's pointer is not the one crw install acts on, or "":
// the destination is fixed (<home>/.local/share/crw-runtime, the directory whose current/bin
// the plugin wiring runs), so a record whose pointer.path, read through Path() as
// runtime_install.py wrote it, names another link was installed at another destination
// (runtime_install.py --dest) and is not this command's to move.
func foreignPointer(rec Object, dest string) string {
	owned, _ := record.Get(rec, "pointer").(Object)
	recorded, ok := record.Get(owned, "path").(string)
	if !ok || !record.Stated(recorded) {
		return ""
	}
	want := pointer.Path(dest)
	if store.PathlibSpelling(recorded) == store.PathlibSpelling(want) {
		return ""
	}
	return "the host record names the owned pointer " + pyvalue.StrRepr(recorded) + ", and crw install acts only on " + want + ", the pointer the plugin wiring runs: this host was installed at another destination (runtime_install.py --dest)"
}

// foreignRepair is how a host installed at another destination comes back under crw install. The
// Python installer that could reinstall at the fixed destination left with todo 44, so the one
// repair is a clean record.
func foreignRepair(o Options) string {
	return "once the installation the record names is no longer used, move " + o.RecordPath + " aside so crw install starts from a clean record and installs at the fixed destination " + o.Dest + ", whose pointer " + pointer.Path(o.Dest) + " the plugin wiring runs"
}

// foreignDestination refuses a command on a host whose record names another pointer, before
// anything is read for acting or written. An unreadable or absent record is left to the command,
// which refuses or starts clean as it always has.
func foreignDestination(o Options, command string) Object {
	loaded := record.Load(o.RecordPath, definition.Version)
	rec, ok := loaded.Value.(Object)
	if !loaded.OK() || !ok {
		return nil
	}
	why := foreignPointer(rec, o.Dest)
	if why == "" {
		return nil
	}
	return Object{field("command", command), field("applied", false), field("refused", why), field("destination", o.Dest),
		field("repair", foreignRepair(o)), field("note", "nothing was written.")}
}

// unspellable is every string in a document about to be written that holds a byte that is not
// UTF-8 - a path from the environment (a marker root, say) that no flag check saw. The encoder
// would write it as U+FFFD, naming a file that does not exist; a surrogate escape (WTF-8, the
// execution policy path) is how such a byte is spelled on purpose and passes.
func unspellable(document any) []string {
	var found []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch value := v.(type) {
		case string:
			if !reading.WTF8(value) {
				found = append(found, key+" holds a byte that is not UTF-8 ("+pyvalue.StrRepr(pyvalue.FSDecode(value))+"), so it would be written as a replacement character naming nothing; crw install records only paths it can spell as UTF-8")
			}
		case Object:
			for _, f := range value {
				walk(f.Key, f.Value)
			}
		case []any:
			for _, item := range value {
				walk(key, item)
			}
		}
	}
	walk("the document", document)
	return found
}
