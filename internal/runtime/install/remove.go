package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// runtimeDirectory is whether name is one of the installers' runtime directories: a Python
// env-<definition>-<digest> or a Go bin-<version>-<digest>.
func runtimeDirectory(name string) bool {
	return strings.HasPrefix(name, "env-") || strings.HasPrefix(name, "bin-")
}

// processScope is what a process-table reading can see, stated in every answer that rests on
// one: this host's procfs in this command's PID namespace.
const processScope = "this host's process table, as this command's PID namespace shows it: a process in another PID namespace (a container sharing this directory) or on another host (a network home) is not seen, and crw install remove is run where the runtime's processes run"

// Remove is `crw install remove <dir>`: delete one runtime directory under the destination,
// only when nothing may still be using it. The directory is taken by its name under the
// destination and judged by file identity (runtimeDir), so an alias of the destination - a
// symlink, a bind mount, a case-folded spelling - cannot pass a check its canonical spelling
// fails. It refuses a directory the record selects, one the owned pointer names (or might - an
// unread pointer is not a pointer aimed elsewhere), one with no claim this command (or
// runtime_install.py) wrote, an unreadable claim, a staging another run still holds, any
// directory a live process runs out of (liveProcesses, and every daemon a daemon.json records
// alive), and any directory a registration the host reads names a path inside
// (doctor.RegisteredMatching: the Stop settings, the bridge record, config.toml's mcp_servers,
// hooks.json, the cached plugin declarations and the launcher copy), or holds something that
// could not be read. It accepts a Python env-* directory and a Go bin-* one alike.
//
// Locks are taken in the one order every crw install path keeps (docs/port/decisions.md 33): the
// directory's .crw-lock, then the promotion lock, then the host record's .crw-lock; each wait
// ends when ctx does, and an interrupted run removes and writes nothing.
//
// The directory is renamed to its tombstone (<destination>/.crw-removing-<name>) in one step,
// its install entries (and an outgoing selection naming it) are dropped under the host record's
// lock, and only then is the tombstone deleted, so a kill leaves either the whole directory, or
// a tombstone this command finishes: `crw install remove` of the tombstone, or of the name when
// only its tombstone is left, deletes it and drops what the record still lists under the name.
// A drop that cannot be written renames the directory back and refuses; a deletion that does not
// finish is exit 3, naming the tombstone.
func Remove(ctx context.Context, o Options, named string) (Object, int) {
	base := Object{field("command", "remove"), field("applied", false)}
	refuse := func(detail string, extra ...any) (Object, int) {
		out := append(base, field("refused", detail))
		for i := 0; i+1 < len(extra); i += 2 {
			out = append(out, field(extra[i].(string), extra[i+1]))
		}
		return append(out, field("note", "nothing was removed and nothing was written.")), Refused
	}
	if named == "" {
		return append(base, field("refused", "name the runtime directory to remove")), Usage
	}
	spelled, err := filepath.Abs(named)
	if err != nil {
		return refuse(err.Error())
	}
	name := filepath.Base(spelled)
	original, tombstone := tombstoneOf(name)
	if !tombstone {
		original = name
	}
	if !runtimeDirectory(original) || !reading.SameDirectory(filepath.Dir(spelled), o.Dest) {
		return refuse(spelled + " is not an env-* or bin-* runtime directory (or the tombstone of one) directly under the destination " + o.Dest + ", so it is not one this command installs")
	}
	directory := filepath.Join(o.Dest, original)
	base = append(base, field("directory", directory))
	if spelled != directory {
		base = append(base, field("named", spelled))
	}
	lock, err := record.LockContext(ctx, directory, 0)
	if err != nil {
		if ctx.Err() != nil {
			return refuse(interrupted(err))
		}
		return refuse("another run is deciding what to do with this directory: " + err.Error())
	}
	defer lock.Release()
	exclusive, err := record.PromoteContext(ctx, o.RecordPath, 0)
	if err != nil {
		if ctx.Err() != nil {
			return refuse(interrupted(err))
		}
		return refuse("another run holds the promotion lock: " + err.Error())
	}
	defer exclusive.Release()
	grave := filepath.Join(o.Dest, tombstonePrefix+original)
	info, err := os.Lstat(directory)
	switch {
	case tombstone || errors.Is(err, os.ErrNotExist):
		if _, graveErr := os.Lstat(grave); graveErr != nil {
			if tombstone {
				return refuse("nothing exists at " + grave)
			}
			return refuse("nothing exists at " + directory)
		}
		return finishRemoval(ctx, o, base, directory, grave)
	case err != nil:
		return refuse("whether " + directory + " exists could not be established: " + store.PythonOSError(err))
	case !info.IsDir():
		return refuse(directory + " is not a directory, so it is not a runtime this command installed")
	}
	d, err := identify(o.Dest, original)
	if err != nil {
		return refuse("the destination " + o.Dest + " could not be read: " + store.PythonOSError(err))
	}
	loaded := record.Load(o.RecordPath, definition.Version)
	if !loaded.Usable() {
		return refuse("the host record could not be read, so whether it selects this directory was not established: "+loaded.Detail, "reading", loaded.Refusal())
	}
	rec := loaded.Value.(Object)
	if u := selectedOrPointed(rec, o.Dest, d); u != nil {
		return refuse(u.detail, u.extra()...)
	}
	claim := staging.ReadClaim(directory)
	switch {
	case claim.State == reading.Absent:
		return refuse("this directory carries no claim written by crw install or runtime_install.py, so it is somebody else's and is left alone")
	case !claim.OK():
		return refuse("the claim in this directory could not be read, so who owns it was not established: "+claim.Detail, "claim", claim.Refusal())
	}
	claimValue := claim.Value.(Object)
	if liveness, detail := staging.OwnerLiveness(directory); liveness != staging.Dead {
		return refuse("another run still holds this directory, or whether one does could not be established: "+detail, "claim", claimValue)
	}
	if u := runningOrRegistered(ctx, o, d); u != nil {
		return refuse(u.detail, u.extra()...)
	}
	if _, err := os.Lstat(grave); err == nil {
		// A tombstone of this name that an earlier run did not finish: the directory under the
		// name now is a later install, so the old copy goes without touching the record.
		if u := tombstoneInUse(ctx, o, grave); u != nil {
			return refuse("an earlier removal of this name left "+grave+", and it cannot be finished: "+u.detail, u.extra()...)
		}
		if err := deleteTombstone(grave); err != nil {
			return refuse("an earlier removal of this name left " + grave + ", which could not be deleted: " + store.PythonOSError(err))
		}
	}
	if err := ctx.Err(); err != nil {
		return refuse(interrupted(err))
	}
	if err := os.Rename(directory, grave); err != nil {
		return refuse("the directory could not be set aside as " + grave + ": " + store.PythonOSError(err))
	}
	dropped, clearedOutgoing, why := dropInstalls(o.RecordPath, d)
	if why != "" {
		if err := os.Rename(grave, directory); err != nil {
			return Object{
				field("command", "remove"), field("applied", true), field("directory", directory), field("removed", false), field("tombstone", grave),
				field("detail", why+"; and the directory could not be renamed back from "+grave+": "+store.PythonOSError(err)), field("residualPaths", []any{grave}),
				field("recoveryRequires", "rename "+grave+" back to "+directory+" by hand (nothing uses it, and the host record still lists it), or run crw install remove "+grave+" to finish removing it"),
				field("note", "the host record was not changed."),
			}, Incomplete
		}
		return refuse(why)
	}
	if err := deleteTombstone(grave); err != nil {
		return Object{
			field("command", "remove"), field("applied", true), field("directory", directory), field("removed", false), field("tombstone", grave),
			field("claim", claimValue), field("droppedInstallEntries", strs(dropped)), field("clearedOutgoing", clearedOutgoing),
			field("detail", "the directory was set aside as "+grave+" and could not be deleted completely: "+store.PythonOSError(err)), field("residualPaths", []any{grave}),
			field("recoveryRequires", "run crw install remove "+grave+" once whatever stopped the deletion is cleared: it finishes the removal. Nothing uses the directory (every check passed), and its install entries are already dropped from the host record"),
			field("processTable", processScope),
			field("note", "the host record no longer lists this directory's installs, and nothing is left under its name. Its measured points stay in the host record as history."),
		}, Incomplete
	}
	return Object{
		field("command", "remove"), field("applied", true), field("directory", directory), field("removed", true),
		field("claim", claimValue), field("droppedInstallEntries", strs(dropped)), field("clearedOutgoing", clearedOutgoing),
		field("processTable", processScope),
		field("note", "the directory was removed after the record, the pointer, its claim, the process table and every registration the host reads all said nothing uses it, and after its install entries were dropped from the host record. Its measured points stay in the host record as history."),
	}, OK
}

// mustIdentify is identify for a directory known to be there; a failure leaves only its name
// to match, which still reaches every path spelled under it.
func mustIdentify(dest, name string) *runtimeDir {
	if d, err := identify(dest, name); err == nil {
		return d
	}
	return &runtimeDir{path: filepath.Join(dest, name), name: name}
}

// tombstoneInUse is why the tombstone at grave may not be finished, or nil: it is not one this
// command began (unclaimedTombstone: no readable installer claim, or a run holds it), or a live
// process runs out of it or a registration names it, or that could not be ruled out.
func tombstoneInUse(ctx context.Context, o Options, grave string) *use {
	if why := unclaimedTombstone(grave); why != "" {
		return &use{why, "tombstone", grave, false}
	}
	return runningOrRegistered(ctx, o, mustIdentify(filepath.Dir(grave), filepath.Base(grave)))
}

// finishRemoval finishes a removal a killed or failed run left as a tombstone: the tombstone is
// deleted, its claim last, once it is established as this command's (it carries a readable claim
// of crw install's or runtime_install.py's whose staging lock nobody holds, or it is empty), no
// live process runs out of it and no registration names it, and when nothing is
// under the runtime's name any more, what the host record still lists under that name (its
// install entries, an outgoing selection naming it) is dropped first. A runtime installed again
// under the name since keeps its entries.
func finishRemoval(ctx context.Context, o Options, base Object, directory, grave string) (Object, int) {
	refuse := func(detail string, extra ...any) (Object, int) {
		out := append(append(Object{}, base...), field("tombstone", grave), field("refused", detail))
		for i := 0; i+1 < len(extra); i += 2 {
			out = append(out, field(extra[i].(string), extra[i+1]))
		}
		return append(out, field("note", "nothing was removed and nothing was written.")), Refused
	}
	if u := tombstoneInUse(ctx, o, grave); u != nil {
		return refuse(grave+" cannot be finished as an interrupted removal: "+u.detail, u.extra()...)
	}
	if err := ctx.Err(); err != nil {
		return refuse(interrupted(err))
	}
	dropped, clearedOutgoing := []string{}, false
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		d := mustIdentify(o.Dest, filepath.Base(directory))
		var why string
		if dropped, clearedOutgoing, why = dropInstalls(o.RecordPath, d); why != "" {
			return refuse(why)
		}
	}
	if err := deleteTombstone(grave); err != nil {
		return append(append(Object{}, base...), field("applied", true), field("removed", false), field("tombstone", grave),
			field("droppedInstallEntries", strs(dropped)), field("clearedOutgoing", clearedOutgoing),
			field("detail", "the tombstone could not be deleted completely: "+store.PythonOSError(err)), field("residualPaths", []any{grave}),
			field("recoveryRequires", "run crw install remove "+grave+" again once whatever stopped the deletion is cleared")), Incomplete
	}
	return Object{
		field("command", "remove"), field("applied", true), field("directory", directory), field("removed", true), field("finished", grave),
		field("droppedInstallEntries", strs(dropped)), field("clearedOutgoing", clearedOutgoing), field("processTable", processScope),
		field("note", "an interrupted removal of this runtime was finished: its tombstone is deleted and the host record lists nothing under a name that is gone."),
	}, OK
}

// use is why a runtime directory may still be in use, and the reading that says so. noTable is
// a platform without a process table, where the recovery by hand depends on the caller.
type use struct {
	detail  string
	key     string
	value   any
	noTable bool
}

// extra is the reading as a refusal carries it.
func (u *use) extra() []any {
	if u.key == "" {
		return nil
	}
	return []any{u.key, u.value}
}

// fields is the reading as result fields, or none.
func (u *use) fields() []contract.Field {
	if u.key == "" {
		return nil
	}
	return []contract.Field{field(u.key, u.value)}
}

// selectedOrPointed is the half of the in-use rule the host record and the owned pointer
// answer: the record selects something inside d, or the pointer - the one the record names, or
// the default one - reaches it, or could not be read (an unread pointer is not a pointer aimed
// elsewhere). nil when neither does.
func selectedOrPointed(rec Object, dest string, d *runtimeDir) *use {
	if selectionHeld(rec, d) {
		return &use{"the host record selects this runtime, so it is in service", "selected", record.Get(rec, "selected"), false}
	}
	for _, path := range uniqueStrings(recordedPointer(rec, dest), pointer.Path(dest)) {
		names := d.pointed(path)
		if names == nil {
			return &use{"whether the pointer at " + path + " names this runtime could not be established, and an unread pointer is not a pointer aimed elsewhere", "pointer", pointerObject(path), false}
		}
		if *names {
			return &use{"the pointer at " + path + " names this runtime, so the commands a host reaches still resolve into it", "pointer", pointerObject(path), false}
		}
	}
	return nil
}

// selectionHeld is whether the record selects any component inside d.
func selectionHeld(rec Object, d *runtimeDir) bool {
	selected, _ := record.Get(rec, "selected").(Object)
	for _, f := range selected {
		if location, ok := f.Value.(string); ok && location != "" && d.holds(location) {
			return true
		}
	}
	return false
}

// outgoingHeld is whether the record's outgoing baseline selects any component inside d.
func outgoingHeld(rec Object, d *runtimeDir) bool {
	outgoing, _ := record.Get(rec, "outgoing").(Object)
	for _, f := range outgoing {
		entry, _ := f.Value.(Object)
		if location, ok := record.Get(entry, "selected").(string); ok && location != "" && d.holds(location) {
			return true
		}
	}
	return false
}

// runningOrRegistered is the half of the in-use rule nothing in the record answers: a live
// process runs out of d (liveProcesses), a daemon a daemon.json records alive (its start time and
// boot id matching this process table) does, or a relay daemon record could not be read; or a
// registration the host reads names a path inside d (doctor.RegisteredMatching) or could not be
// read or judged. nil when none does. What it cannot see is processScope.
func runningOrRegistered(ctx context.Context, o Options, d *runtimeDir) *use {
	processes, unruled, err := liveProcesses(o.proc(), d)
	var missing *noProcessTable
	if errors.As(err, &missing) {
		return &use{"this platform (" + runtime.GOOS + ") has no process table this command can read (" + missing.proc + " is not a procfs), so whether a relay daemon or a bridge still runs out of this directory cannot be established, and a directory that may be in use is never removed", "recoveryRequires",
			"remove it by hand: stop the relay daemon started from it (" + filepath.Join(d.path, "bin", definition.Relay) + " service stop) and end every Codex session whose bridge it started, delete " + d.path + ", then run crw install status to see that the host record and the pointer still name the runtime you meant. Its install entries stay in the host record, where a rollback naming it is refused because the directory is gone", true}
	}
	if err != nil {
		return &use{"the process table could not be read, so whether a process still runs out of this directory was not established: " + err.Error(), "", nil, false}
	}
	if len(processes) > 0 {
		return &use{"live processes run out of this directory", "processes", processes, false}
	}
	if len(unruled) > 0 {
		return &use{"what a live process runs could not be read, so it cannot be ruled out that it runs out of this directory", "unreadableProcesses", unruled, false}
	}
	retention := doctor.RetentionOptions{Env: o.Env, CodexHome: o.CodexHome, Destination: o.Dest, Proc: o.proc(), ScopeRegistry: o.ScopeRegistry}
	if _, unreadable := doctor.RecordedDaemons(retention, o.State); len(unreadable) > 0 {
		return &use{"a relay daemon record could not be read, so whether a daemon it records still runs out of this directory was not established", "unreadable", strs(unreadable), false}
	}
	registered, unreadable := doctor.RegisteredMatching(ctx, retention, func(path, resolves string) string {
		switch {
		case d.holds(path):
			return path
		case resolves != "" && d.holds(resolves):
			return resolves
		}
		return ""
	})
	if len(registered) > 0 {
		return &use{"a registration the host reads still names a path inside this directory, and each new session starts it from there", "registrations", registered, false}
	}
	if len(unreadable) > 0 {
		return &use{"a registration the host reads could not be read or judged, so whether it names a path inside this directory was not established", "unreadable", strs(unreadable), false}
	}
	return nil
}

// dropInstalls drops every install entry whose environment is d, and the record's outgoing
// selection when it names anything inside d (a bare rollback would otherwise be sent to a
// runtime that no longer exists and has no install entry), in one write under the host record's
// lock, after reading again that the record does not select it. d is matched by identity and by
// its name in the destination, so this holds after the directory was renamed to its tombstone.
// It answers the environments dropped, whether outgoing was cleared, or why nothing was written.
func dropInstalls(recordPath string, d *runtimeDir) ([]string, bool, string) {
	lock, err := record.Lock(recordPath, 0)
	if err != nil {
		return nil, false, "the host record's lock could not be taken, so this directory's install entries could not be dropped before it is removed: " + err.Error()
	}
	defer lock.Release()
	current := record.Load(recordPath, definition.Version)
	if !current.Usable() {
		return nil, false, "the host record could not be read to drop this directory's install entries: " + current.Detail
	}
	rec := current.Value.(Object)
	if selectionHeld(rec, d) {
		return nil, false, "the host record selects this runtime now, so it is in service"
	}
	var dropped []string
	components, _ := record.Get(rec, "components").(Object)
	for i, c := range components {
		entry, _ := c.Value.(Object)
		installs, _ := record.Get(entry, "installs").([]any)
		kept := []any{}
		for _, raw := range installs {
			install, _ := raw.(Object)
			if environment, ok := record.Get(install, "environment").(string); ok && environment != "" && d.names(environment) {
				dropped = append(dropped, environment)
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) != len(installs) {
			components[i].Value = record.Set(entry, "installs", kept)
		}
	}
	dropped = uniqueStrings(dropped...)
	clearOutgoing := outgoingHeld(rec, d)
	if clearOutgoing {
		rec = record.Delete(rec, "outgoing")
	}
	if len(dropped) == 0 && !clearOutgoing {
		return []string{}, false, ""
	}
	if err := record.Save(recordPath, rec); err != nil {
		return nil, false, "the host record could not be written to drop this directory's install entries: " + store.PythonOSError(err)
	}
	if dropped == nil {
		dropped = []string{}
	}
	return dropped, clearOutgoing, ""
}

func (o Options) proc() string {
	if o.Proc != "" {
		return o.Proc
	}
	return "/proc"
}

func uniqueStrings(values ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// noProcessTable is liveProcesses' answer where there is no procfs to read, which is every
// platform but Linux (darwin has none; its process table is read through proc_pidpath or sysctl
// KERN_PROCARGS2, which this command does not do). Remove refuses on it, saying so.
type noProcessTable struct{ proc string }

func (n *noProcessTable) Error() string { return "no process table (procfs) at " + n.proc }

// processOwner is the uid a /proc/<pid> directory belongs to, which is the process's (a
// variable only so that a test's fake process table can hold another user's processes).
var processOwner = func(dir string) (int, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("the owner of " + dir + " could not be read")
	}
	return int(stat.Uid), nil
}

// vanished is whether a read of a /proc/<pid> entry failed because the process is gone (or has
// no executable to name: a zombie, a kernel thread), not because it could not be read.
func vanished(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// denied is whether reading a process's exe was refused for want of ptrace access, which the
// kernel refuses for another user's process and for this user's own when it holds capabilities
// this one does not (systemd --user holds CAP_WAKE_ALARM) or is not dumpable.
func denied(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

// readExe reads a /proc/<pid>/exe link (a variable only so that a test's fake process table can
// answer as the kernel does when ptrace access is refused).
var readExe = os.Readlink

// runtimeNames are the names a runtime's own executables run under.
func runtimeNames() map[string]bool {
	names := map[string]bool{Binary: true}
	for _, name := range definition.Links() {
		names[name] = true
	}
	return names
}

// liveProcesses is every process (but this one) whose executable, or the interpreter or script
// its argv starts, resolves inside directory, and every process that could not be ruled out. A
// Python runtime runs as <venv>/bin/python (whose /proc exe is the base interpreter outside the
// venv) or as a console script under <venv>/bin, so argv's first two words are read as spelled
// and as resolved; a Go runtime is its exe.
//
// A pid whose entries are gone (ENOENT, ESRCH) has exited and is skipped. Any other failure is
// not an absence. A process whose exe the kernel will not show (readlink needs ptrace access:
// another user's process, and this user's own when it holds capabilities or is not dumpable) is
// judged by its cmdline, which every user may read, and is not ruled out when that is hidden
// too (a procfs mounted hidepid) or when it starts one of this runtime's executables by a bare
// name, which could be this runtime's. A process of this user whose exe or cmdline cannot be
// read for any other reason is not ruled out.
func liveProcesses(proc string, d *runtimeDir) (found, unruled []any, err error) {
	if _, err := os.Stat(filepath.Join(proc, "self")); err != nil {
		return nil, nil, &noProcessTable{proc}
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, nil, err
	}
	inside := d.holds
	self, me := os.Getpid(), os.Getuid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		base := filepath.Join(proc, entry.Name())
		unknown := func(uid any, why string) {
			unruled = append(unruled, Object{field("pid", int64(pid)), field("uid", uid), field("why", why)})
		}
		uid, err := processOwner(base)
		switch {
		case vanished(err):
			continue
		case err != nil:
			unknown(nil, "whose process it is could not be read: "+store.PythonOSError(err))
			continue
		}
		var hits []string
		exe, exeErr := readExe(filepath.Join(base, "exe"))
		raw, cmdErr := os.ReadFile(filepath.Join(base, "cmdline"))
		words := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		switch {
		case vanished(exeErr) || vanished(cmdErr):
			continue
		case exeErr != nil && !denied(exeErr) && uid == me:
			unknown(int64(uid), "it runs as this user and its executable could not be read ("+store.PythonOSError(exeErr)+"), so what it runs is unknown")
			continue
		case cmdErr != nil:
			why := "its command line could not be read (" + store.PythonOSError(cmdErr) + "; a procfs mounted hidepid hides another user's)"
			if exeErr != nil {
				why += ", nor its executable (" + store.PythonOSError(exeErr) + ")"
			}
			unknown(int64(uid), why+", so what it runs is unknown")
			continue
		case exeErr == nil:
			exe = strings.TrimSuffix(exe, " (deleted)")
			if inside(exe) {
				hits = append(hits, exe)
			}
		case !strings.Contains(words[0], "/") && runtimeNames()[words[0]]:
			unknown(int64(uid), "its executable could not be read ("+store.PythonOSError(exeErr)+") and its command line starts "+words[0]+" by a bare name, which may be this runtime's, so what it runs is unknown")
			continue
		}
		for i, word := range words {
			if i > 1 {
				break
			}
			if inside(word) {
				hits = append(hits, word)
			}
		}
		if len(hits) > 0 {
			found = append(found, Object{field("pid", int64(pid)), field("runs", strs(hits))})
		}
	}
	return found, unruled, nil
}
