package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// Remove is `crw install remove <dir>`: delete one runtime directory under the destination,
// only when nothing may still be using it. It refuses a directory the record selects, one the
// owned pointer names (or might - an unread pointer is not a pointer aimed elsewhere), one
// with no claim this command (or runtime_install.py) wrote, an unreadable claim, a staging
// another run still holds, any directory a live process runs out of - its /proc/<pid>/exe,
// or the interpreter or script its argv starts, resolving inside it (which covers every
// daemon.json pid) - and any directory a registration the host reads names a path inside
// (doctor.RegisteredInside: the Stop settings, the bridge record, config.toml's mcp_servers,
// hooks.json, the cached plugin declarations and the launcher copy), or holds something that
// could not be read. It accepts a Python env-* directory and a Go bin-* one alike.
//
// The record goes first, as runtime_install.py's release_candidate drops a candidate's entries
// before its directory is removed: the directory's install entries are dropped under the host
// record's lock (where the selection is read again), and only once that write has landed is
// the directory removed. A drop that cannot be written refuses with nothing removed; a removal
// that does not finish after the drop is exit 3, the entries gone and the rest of the
// directory named for removal by hand.
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
	directory, err := filepath.Abs(named)
	if err != nil {
		return refuse(err.Error())
	}
	base = append(base, field("directory", directory))
	if !runtimeDirectory(filepath.Base(directory)) || !reading.SameDirectory(filepath.Dir(directory), o.Dest) {
		return refuse(directory + " is not an env-* or bin-* runtime directory directly under the destination " + o.Dest + ", so it is not one this command installs")
	}
	info, err := os.Lstat(directory)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return refuse("nothing exists at " + directory)
	case err != nil:
		return refuse("whether " + directory + " exists could not be established: " + store.PythonOSError(err))
	case !info.IsDir():
		return refuse(directory + " is not a directory, so it is not a runtime this command installed")
	}
	exclusive, err := record.Promote(o.RecordPath, 0)
	if err != nil {
		return refuse("another run holds the promotion lock: " + err.Error())
	}
	defer exclusive.Release()
	lock, err := record.Lock(directory, 0)
	if err != nil {
		return refuse("another run is deciding what to do with this directory: " + err.Error())
	}
	defer lock.Release()
	loaded := record.Load(o.RecordPath, definition.Version)
	if !loaded.Usable() {
		return refuse("the host record could not be read, so whether it selects this directory was not established: "+loaded.Detail, "reading", loaded.Refusal())
	}
	rec := loaded.Value.(Object)
	if u := selectedOrPointed(rec, o.Dest, directory); u != nil {
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
	if u := runningOrRegistered(ctx, o, directory); u != nil {
		return refuse(u.detail, u.extra()...)
	}
	dropped, why := dropInstalls(o.RecordPath, directory)
	if why != "" {
		return refuse(why)
	}
	if err := os.RemoveAll(directory); err != nil {
		_, statErr := os.Lstat(directory)
		return Object{
			field("command", "remove"), field("applied", true), field("directory", directory), field("removed", false),
			field("gone", errors.Is(statErr, os.ErrNotExist)), field("claim", claimValue), field("droppedInstallEntries", strs(dropped)),
			field("detail", "the directory could not be removed completely: "+store.PythonOSError(err)), field("residualPaths", []any{directory}),
			field("recoveryRequires", "remove what is left of "+directory+" by hand: nothing uses it (every check above passed), and its install entries are already dropped from the host record. Rerunning crw install remove finishes it only while its claim is still there"),
			field("note", "the host record no longer lists this directory's installs, and part of the directory may be gone. Its measured points stay in the host record as history."),
		}, Incomplete
	}
	return Object{
		field("command", "remove"), field("applied", true), field("directory", directory), field("removed", true),
		field("claim", claimValue), field("droppedInstallEntries", strs(dropped)),
		field("note", "the directory was removed after the record, the pointer, its claim, the process table and every registration the host reads all said nothing uses it, and after its install entries were dropped from the host record. Its measured points stay in the host record as history."),
	}, OK
}

// use is why a runtime directory may still be in use, and the reading that says so.
type use struct {
	detail string
	key    string
	value  any
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
// answer: the record selects something inside directory, or the pointer - the one the record
// names, or the default one - names it, or could not be read (an unread pointer is not a
// pointer aimed elsewhere). nil when neither does.
func selectedOrPointed(rec Object, dest, directory string) *use {
	if selectsUnder(rec, directory) {
		return &use{"the host record selects this runtime, so it is in service", "selected", record.Get(rec, "selected")}
	}
	for _, path := range uniqueStrings(recordedPointer(rec, dest), pointer.Path(dest)) {
		names := pointer.Names(path, directory)
		if names == nil {
			return &use{"whether the pointer at " + path + " names this runtime could not be established, and an unread pointer is not a pointer aimed elsewhere", "pointer", pointerObject(path)}
		}
		if *names {
			return &use{"the pointer at " + path + " names this runtime, so the commands a host reaches still resolve into it", "pointer", pointerObject(path)}
		}
	}
	return nil
}

// runningOrRegistered is the half of the in-use rule nothing in the record answers: a live
// process runs out of directory (its /proc/<pid>/exe, or the interpreter or script its argv
// starts, resolving inside it, which covers every daemon.json pid), or a registration the host
// reads names a path inside it (doctor.RegisteredInside) or could not be read or judged. nil
// when neither does.
func runningOrRegistered(ctx context.Context, o Options, directory string) *use {
	processes, err := liveProcesses(o.proc(), directory)
	if err != nil {
		return &use{"the process table could not be read, so whether a process still runs out of this directory was not established: " + err.Error(), "", nil}
	}
	if len(processes) > 0 {
		return &use{"live processes run out of this directory", "processes", processes}
	}
	registered, unreadable := doctor.RegisteredInside(ctx, doctor.RetentionOptions{Env: o.Env, CodexHome: o.CodexHome, Destination: o.Dest}, directory)
	if len(registered) > 0 {
		return &use{"a registration the host reads still names a path inside this directory, and each new session starts it from there", "registrations", registered}
	}
	if len(unreadable) > 0 {
		return &use{"a registration the host reads could not be read or judged, so whether it names a path inside this directory was not established", "unreadable", strs(unreadable)}
	}
	return nil
}

// dropInstalls drops every install entry whose environment resolves to directory, in one write
// under the host record's lock, after reading again that the record does not select it. It
// answers the environments dropped, or why nothing was written.
func dropInstalls(recordPath, directory string) ([]string, string) {
	lock, err := record.Lock(recordPath, 0)
	if err != nil {
		return nil, "the host record's lock could not be taken, so this directory's install entries could not be dropped before it is removed: " + err.Error()
	}
	defer lock.Release()
	current := record.Load(recordPath, definition.Version)
	if !current.Usable() {
		return nil, "the host record could not be read to drop this directory's install entries: " + current.Detail
	}
	rec := current.Value.(Object)
	if selectsUnder(rec, directory) {
		return nil, "the host record selects this runtime now, so it is in service"
	}
	root, err := record.Resolve(directory)
	if err != nil {
		return nil, "the directory could not be resolved: " + err.Error()
	}
	var dropped []string
	components, _ := record.Get(rec, "components").(Object)
	for i, c := range components {
		entry, _ := c.Value.(Object)
		installs, _ := record.Get(entry, "installs").([]any)
		kept := []any{}
		for _, raw := range installs {
			install, _ := raw.(Object)
			if environment, ok := record.Get(install, "environment").(string); ok && environment != "" {
				if real, err := record.Resolve(environment); err == nil && real == root {
					dropped = append(dropped, environment)
					continue
				}
			}
			kept = append(kept, raw)
		}
		if len(kept) != len(installs) {
			components[i].Value = record.Set(entry, "installs", kept)
		}
	}
	dropped = uniqueStrings(dropped...)
	if len(dropped) == 0 {
		return []string{}, ""
	}
	if err := record.Save(recordPath, rec); err != nil {
		return nil, "the host record could not be written to drop this directory's install entries: " + store.PythonOSError(err)
	}
	return dropped, ""
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

// liveProcesses is every process (but this one) whose executable, or the interpreter or script
// its argv starts, resolves inside directory. A Python runtime runs as <venv>/bin/python (whose
// /proc exe is the base interpreter outside the venv) or as a console script under <venv>/bin,
// so argv's first two words are read as spelled and as resolved; a Go runtime is its exe.
func liveProcesses(proc, directory string) ([]any, error) {
	if _, err := os.Stat(filepath.Join(proc, "self")); err != nil {
		return nil, errors.New("no process table (procfs) at " + proc)
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	root, err := record.Resolve(directory)
	if err != nil {
		return nil, err
	}
	inside := func(path string) bool {
		if !filepath.IsAbs(path) {
			return false
		}
		if record.Within(filepath.Clean(path), filepath.Clean(directory)) {
			return true
		}
		resolved, err := record.Resolve(path)
		return err == nil && record.Within(resolved, root)
	}
	self := os.Getpid()
	var found []any
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		base := filepath.Join(proc, entry.Name())
		var hits []string
		if exe, err := os.Readlink(filepath.Join(base, "exe")); err == nil {
			exe = strings.TrimSuffix(exe, " (deleted)")
			if inside(exe) {
				hits = append(hits, exe)
			}
		}
		raw, _ := os.ReadFile(filepath.Join(base, "cmdline"))
		words := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
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
	return found, nil
}
