// Package pointer is scripts/crw_runtime/pointer.py: the owned pointer, one stable path
// (<destination>/current) that names whichever runtime is selected. Registrations and settings
// name current/bin/<name> once, and an update moves the pointer by placing a temporary link
// beside it and renaming it over, so current/bin/<name> is valid at every instant.
//
// The pointer is a way to REACH a runtime, never an identity: a process started through it
// keeps running the runtime it started in after the pointer moves.
package pointer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// Name is the pointer's basename under a destination.
const Name = "current"

// The four answers about what is at the pointer path.
const (
	NoPointer   = "NO_POINTER"
	Link        = "LINK"
	NotALink    = "NOT_A_LINK"
	Unreachable = "UNREACHABLE"
)

// States is every answer, in pointer.py's order.
var States = []string{NoPointer, Link, NotALink, Unreachable}

// Usable reports whether a pointer in this state may be placed over: a link, or nothing.
// NOT_A_LINK is somebody's real directory and UNREACHABLE established nothing.
func Usable(state string) bool { return state == Link || state == NoPointer }

// Path is pointer.pointer_path.
func Path(destination string) string { return filepath.Join(destination, Name) }

// Answer is pointer.read's reading.
type Answer struct {
	State  string
	Target string // "" unless State is Link
	Detail string
}

// Read is what is at this path, decided by lstat without following the link.
func Read(path string) Answer {
	if strings.ContainsRune(path, 0) {
		return Answer{State: Unreachable, Detail: "this path cannot name a file: ValueError: embedded null byte"}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Answer{State: NoPointer, Detail: "nothing exists at " + path}
	}
	if err != nil {
		return Answer{State: Unreachable, Detail: "whether anything exists at " + path + " could not be established: " + store.PythonOSError(err)}
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return Answer{State: NotALink, Detail: "this path is a real file or directory, not a pointer this command placed, so it is left exactly as it is"}
	}
	target, err := os.Readlink(path)
	if err != nil {
		return Answer{State: Unreachable, Detail: "the pointer is a link whose target could not be read: " + store.PythonOSError(err)}
	}
	return Answer{State: Link, Target: target, Detail: "the pointer names " + target}
}

// RefusedError is Place declining a path whose state it may not place over: somebody's real
// file or directory (NOT_A_LINK), or a path whose state could not be read (UNREACHABLE).
type RefusedError struct {
	Path   string
	Answer Answer
}

func (e *RefusedError) Error() string {
	return "no pointer is placed at " + e.Path + ": it is " + e.Answer.State + ": " + e.Answer.Detail
}

// Place points this pointer at target atomically, replacing only a link: a temporary link
// beside it, then a rename. Anything else at the path is refused before anything is written,
// with a *RefusedError naming the state, because a rename replaces a regular file as readily as
// a link (only a directory makes it fail). Whether this command owns an existing link is the
// caller's question, asked before this. The lstat and the rename are two steps, so a caller
// holds the promotion lock (record.Exclusive) around its check and this placement, as
// runtime_install.py does and the installer of todo 38 does; that keeps every writer that
// places a pointer out of the gap between them.
//
// pointer.place has no such check: os.replace over a regular file succeeds, and only its
// callers' usable() check under the lock keeps it from destroying one.
func Place(path, target string) error {
	if answer := Read(path); !Usable(answer.State) {
		return &RefusedError{Path: path, Answer: answer}
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".crw-pointer-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	_ = file.Close()
	if err := os.Remove(temporary); err != nil {
		return err
	}
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// Remove takes away a pointer this run placed, only while it still names expected, and reads
// its absence back before claiming it. It returns whether the pointer is now absent and why.
func Remove(path, expected string) (bool, string) {
	answer := Read(path)
	if answer.State == NoPointer {
		return true, "there is no pointer here, which is the state this restores to"
	}
	if answer.State != Link {
		return false, "this path is not a pointer this run placed: " + answer.Detail
	}
	if names := Names(path, expected); names == nil || !*names {
		return false, "the pointer names " + answer.Target + " rather than " + expected + ", so it is not this run's to remove"
	}
	if err := os.Remove(path); err != nil {
		return false, "the pointer could not be removed: " + store.PythonOSError(err)
	}
	after := Read(path)
	if after.State == NoPointer {
		return true, "the pointer this run placed was removed and its absence was read back"
	}
	return false, "the pointer is still there after removing it: " + after.Detail
}

// Names is whether the pointer names this environment: true, false, or nil when that could not
// be established. NO_POINTER and NOT_A_LINK are established answers (no).
func Names(path, environment string) *bool {
	answer := Read(path)
	if answer.State == Unreachable {
		return nil
	}
	no := false
	if answer.State != Link {
		return &no
	}
	resolved, err := TargetOf(path, answer.Target)
	if err != nil {
		return nil
	}
	wanted, err := record.Resolve(environment)
	if err != nil {
		return nil
	}
	yes := resolved == wanted
	return &yes
}

// TargetOf resolves a link's target, relative to the link's directory when it is relative.
func TargetOf(path, target string) (string, error) {
	if !filepath.IsAbs(target) {
		// Joined without cleaning, as Path(parent) / target is: "x/.." has to follow x first.
		target = filepath.Dir(path) + "/" + target
	}
	return record.Resolve(target)
}
