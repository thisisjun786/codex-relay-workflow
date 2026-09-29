package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
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
// another run still holds, and any directory a live process runs out of - its /proc/<pid>/exe,
// or the interpreter or script its argv starts, resolving inside it (which covers every
// daemon.json pid). It accepts a Python env-* directory and a Go bin-* one alike, and drops the
// directory's install entries from the host record once it is gone.
func Remove(_ context.Context, o Options, named string) (Object, int) {
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
	if selectsUnder(rec, directory) {
		return refuse("the host record selects this runtime, so it is in service", "selected", record.Get(rec, "selected"))
	}
	pointerPath := recordedPointer(rec, o.Dest)
	for _, path := range uniqueStrings(pointerPath, pointer.Path(o.Dest)) {
		names := pointer.Names(path, directory)
		if names == nil {
			return refuse("whether the pointer at "+path+" names this runtime could not be established, and an unread pointer is not a pointer aimed elsewhere", "pointer", pointerObject(path))
		}
		if *names {
			return refuse("the pointer at "+path+" names this runtime, so the commands a host reaches still resolve into it", "pointer", pointerObject(path))
		}
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
	processes, err := liveProcesses(o.proc(), directory)
	if err != nil {
		return refuse("the process table could not be read, so whether a process still runs out of this directory was not established: " + err.Error())
	}
	if len(processes) > 0 {
		return refuse("live processes run out of this directory", "processes", processes)
	}
	var environments []string
	resolved, _ := record.Resolve(directory)
	for _, c := range definition.Components {
		for _, install := range installsIn(rec, c.Name, directory) {
			if environment, ok := record.Get(install, "environment").(string); ok && environment != "" {
				if real, err := record.Resolve(environment); err == nil && real == resolved {
					environments = append(environments, environment)
				}
			}
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		_, statErr := os.Lstat(directory)
		return append(base, field("refused", "the directory could not be removed completely: "+store.PythonOSError(err)), field("residualPaths", []any{directory}),
			field("gone", errors.Is(statErr, os.ErrNotExist)), field("note", "whatever was removed is gone; the host record was not changed")), Refused
	}
	dropped := 0
	for _, environment := range uniqueStrings(environments...) {
		environment := environment
		if _, err := record.Update(o.RecordPath, definition.Version, record.Delta{DropEnvironment: &environment}); err == nil {
			dropped++
		}
	}
	return Object{
		field("command", "remove"), field("applied", true), field("directory", directory), field("removed", true),
		field("claim", claimValue), field("droppedInstallEntries", strs(uniqueStrings(environments...))),
		field("note", "the directory was removed after the record, the pointer, its claim and the process table all said nothing uses it. Its measured points stay in the host record as history."),
	}, OK
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
