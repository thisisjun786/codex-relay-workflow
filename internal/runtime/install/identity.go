package install

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// runtimeDir is one runtime directory known by identity rather than by spelling: the
// destination's file, the directory's own file (device and inode), and its name. A path names
// it when, as written or once its links are followed, it (or, for holds, an ancestor) is the
// directory's own file, or is an entry of its name in the destination's file. So a bind-mounted
// or symlinked alias of the destination, and a case-folded spelling of its name, reach the same
// directory, and so does its name after the directory itself was renamed away to a tombstone.
type runtimeDir struct {
	path string // <destination>/<name>, spelled as the destination is
	name string
	dest os.FileInfo
	self os.FileInfo // nil when nothing is at path
}

// identify reads the destination's identity and, when it exists, the directory's.
func identify(dest, name string) (*runtimeDir, error) {
	destInfo, err := os.Stat(dest)
	if err != nil {
		return nil, err
	}
	d := &runtimeDir{path: filepath.Join(dest, name), name: name, dest: destInfo}
	if info, err := os.Lstat(d.path); err == nil && info.IsDir() {
		d.self = info
	}
	return d, nil
}

// spellings is path cleaned, and as its links resolve when that differs.
func spellings(path string) []string {
	clean := filepath.Clean(path)
	out := []string{clean}
	if resolved, err := record.Resolve(clean); err == nil && resolved != clean {
		out = append(out, resolved)
	}
	return out
}

// is is whether candidate is the directory: its own file, or an entry of its name (compared
// case-insensitively when fold) in the destination's file.
func (d *runtimeDir) is(candidate string, fold bool) bool {
	if d.self != nil {
		if info, err := os.Stat(candidate); err == nil && os.SameFile(info, d.self) {
			return true
		}
	}
	if base := filepath.Base(candidate); base == d.name || (fold && strings.EqualFold(base, d.name)) {
		if info, err := os.Stat(filepath.Dir(candidate)); err == nil && os.SameFile(info, d.dest) {
			return true
		}
	}
	return false
}

// names is whether path is exactly this directory, as an install entry's environment names it.
func (d *runtimeDir) names(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, spelling := range spellings(path) {
		if d.is(spelling, false) {
			return true
		}
	}
	return false
}

// holds is whether path is this directory or lies inside it. A name that differs only in case
// counts, which errs toward keeping a directory.
func (d *runtimeDir) holds(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, spelling := range spellings(path) {
		for p := spelling; ; p = filepath.Dir(p) {
			if d.is(p, true) {
				return true
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return false
}

// pointed is whether the pointer at path reaches this directory: true, false, or nil when that
// could not be established (an unread pointer is not a pointer aimed elsewhere).
func (d *runtimeDir) pointed(path string) *bool {
	answer := pointer.Read(path)
	if answer.State == pointer.Unreachable {
		return nil
	}
	no := false
	if answer.State != pointer.Link {
		return &no
	}
	target := answer.Target
	if !filepath.IsAbs(target) {
		target = filepath.Dir(path) + "/" + target
	}
	yes := d.holds(target)
	return &yes
}

// tombstonePrefix names a runtime directory crw install is removing: remove, the reclaim of an
// abandoned staging and the release of a failed candidate first rename the directory to
// <destination>/.crw-removing-<name> in one atomic step, then delete that. A kill part-way
// through the deletion leaves the tombstone, never a directory under the runtime's own name
// with some of its files (its claim among them) gone. crw install remove finishes one; status
// lists every one.
const tombstonePrefix = ".crw-removing-"

// tombstoneOf is the runtime directory name a tombstone name was renamed from.
func tombstoneOf(name string) (string, bool) {
	original, ok := strings.CutPrefix(name, tombstonePrefix)
	return original, ok && runtimeDirectory(original)
}

// discard renames directory to its tombstone and deletes that. It answers whether the name is
// free (nothing is left under it) and, when the deletion did not finish, the tombstone left and
// why. A tombstone already there - an earlier discard of the same name that did not finish - is
// deleted first.
func discard(directory string) (free bool, residue string, err error) {
	tombstone := filepath.Join(filepath.Dir(directory), tombstonePrefix+filepath.Base(directory))
	if _, statErr := os.Lstat(tombstone); statErr == nil {
		if err := os.RemoveAll(tombstone); err != nil {
			return false, tombstone, err
		}
	}
	if err := os.Rename(directory, tombstone); err != nil {
		return false, "", err
	}
	if err := os.RemoveAll(tombstone); err != nil {
		return true, tombstone, err
	}
	return true, "", nil
}

// landedAt proves the placed pointer reaches environment as a host will: it resolves without an
// error (no loop, nothing dangling), to the directory itself by identity, and <pointer>/bin/crw is
// a regular file this user may execute. Comparing resolved spellings is not proof: a link that
// loops resolves, without error, to the same text as the environment spelled through it.
func landedAt(pointerPath, environment string) (bool, string) {
	reached, err := os.Stat(pointerPath)
	if err != nil {
		return false, "the pointer does not resolve: " + store.PythonOSError(err)
	}
	wanted, err := os.Stat(environment)
	if err != nil {
		return false, "the runtime the pointer was placed at could not be read: " + store.PythonOSError(err)
	}
	if !os.SameFile(reached, wanted) {
		return false, "the pointer resolves to a directory other than " + environment
	}
	crw := filepath.Join(pointerPath, "bin", Binary)
	info, err := os.Stat(crw)
	switch {
	case err != nil:
		return false, crw + " could not be read through the pointer: " + store.PythonOSError(err)
	case !info.Mode().IsRegular():
		return false, crw + " is not a regular file"
	}
	if err := unix.Access(crw, unix.X_OK); err != nil {
		return false, crw + " is not executable by this user: " + err.Error()
	}
	return true, ""
}

// completePayload is the COMPLETE claim that settles environment's STAGING one, in the writer's
// own shape: a claim runtime_install.py wrote (a venv's) is settled as runtime_install.py would
// write it, so that runtime_install.py still reads the directory as its own (its staging.shape
// accepts only its own marker); any other is crw install's.
func completePayload(environment string, issue any) record.Object {
	claim := staging.ReadClaim(environment)
	if value, ok := claim.Value.(record.Object); ok && claim.OK() && record.Get(value, "writtenBy") == staging.WrittenByPython {
		host, _ := os.Hostname()
		return staging.Payload(staging.Complete, staging.WrittenByPython, issue, strconv.Itoa(os.Getpid()), os.Getpid(), host, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	}
	return staging.NewPayload(staging.Complete, issue, strconv.Itoa(os.Getpid()))
}

// interrupted is the refusal detail for a context that ended before a destructive step.
func interrupted(err error) string {
	return "the command was interrupted (" + err.Error() + ") before anything was removed or written"
}
