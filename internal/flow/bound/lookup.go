package bound

import (
	"bytes"
	"io"
	"os"
	"syscall"
)

// State is what the hook concludes about the thread of a native spawn.
type State int

// The states of a thread.
const (
	// Standalone: not bound (no directory, a session id that is no thread id, no file for the thread, or a closed last line).
	Standalone State = iota
	// Engine: a thread of an engine call of a run; Line is its last line.
	Engine
	// Orchestrator: the thread that orchestrates an open run; Line is its last line.
	Orchestrator
	// Unavailable: the state cannot be trusted or read; the spawn is refused (fail closed).
	Unavailable
)

// Result is the outcome of Lookup.
type Result struct {
	State State
	// Line is the last line of the thread's file (Engine, Orchestrator).
	Line Line
	// Dir is the flow-bound directory the lookup used (empty for a Standalone answer that did not reach it).
	Dir string
	// Err explains an Unavailable answer (an *UnavailableError).
	Err error
}

// maxFile bounds a binding file that is read: one line per turn of a thread is far below it.
const maxFile = 8 << 20

// Lookup is the hook's read of the binding of the thread named by a hook payload's session_id, in four steps that no step
// widens: (a) a session id that is not a thread id, or no directory, is Standalone; (b) a directory that is a symlink, is not
// the current user's or is not mode 0700 makes every verdict under it untrustworthy, so it is Unavailable; (c) a file that is
// not there is a thread no run registered, Standalone; (d) a file that is there must be a regular file of the current user with
// mode 0600 that can be read whole and ends in a complete line that parses: otherwise Unavailable. A closed last line is
// Standalone. Lookup only reads.
func Lookup(sessionID string) Result {
	if !ValidThreadID(sessionID) {
		return Result{State: Standalone}
	}
	dir, err := Dir()
	if err != nil {
		return Result{State: Standalone} // no home, so no run could have written the directory
	}
	return LookupIn(dir, sessionID)
}

// LookupIn is Lookup over an explicit flow-bound directory.
func LookupIn(dir, sessionID string) Result {
	if !ValidThreadID(sessionID) {
		return Result{State: Standalone}
	}
	exists, err := CheckDir(dir)
	if err != nil {
		return Result{State: Unavailable, Dir: dir, Err: err}
	}
	if !exists {
		return Result{State: Standalone}
	}
	line, present, err := ReadLast(dir, sessionID)
	switch {
	case err != nil:
		return Result{State: Unavailable, Dir: dir, Err: err}
	case !present:
		return Result{State: Standalone, Dir: dir}
	}
	switch line.Kind {
	case KindClosed:
		return Result{State: Standalone, Dir: dir, Line: line}
	case KindOrchestrator:
		return Result{State: Orchestrator, Dir: dir, Line: line}
	}
	return Result{State: Engine, Dir: dir, Line: line}
}

// ReadLast reads the last line of the binding file of thread under the (already checked) directory dir. present is false when
// the file is not there.
func ReadLast(dir, thread string) (line Line, present bool, err error) {
	root, err := openDir(dir)
	if err != nil {
		return Line{}, false, err
	}
	defer root.Close()
	name := thread + FileSuffix
	if link, err := root.Lstat(name); err == nil && link.Mode()&os.ModeSymlink != 0 {
		return Line{}, true, unavailable(ReasonUnsafeFile, "%s is a symbolic link", name)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errorsIsAbsent(err) {
			return Line{}, false, nil
		}
		return Line{}, false, unavailable(ReasonUnreadable, "%s: %v", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Line{}, true, unavailable(ReasonUnreadable, "%s: %v", name, err)
	}
	if bad := trusted(info, name, false, 0o600); bad != nil {
		return Line{}, true, bad
	}
	if info.Size() > maxFile {
		return Line{}, true, unavailable(ReasonUnreadable, "%s is larger than %d bytes", name, maxFile)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return Line{}, true, unavailable(ReasonUnreadable, "%s: %v", name, err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return Line{}, true, unavailable(ReasonTruncated, "%s does not end in a complete line", name)
	}
	body := data[:len(data)-1]
	if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	}
	line, perr := decodeLine(body)
	if perr == nil {
		perr = line.Validate(thread)
	}
	if perr != nil {
		return Line{}, true, unavailable(ReasonInvalid, "%s: the last line is not a binding line: %v", name, perr)
	}
	return line, true, nil
}

// openDir opens dir as a root after checking it: the directory must still be the one CheckDir judged.
func openDir(dir string) (*os.Root, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, unavailable(ReasonUnsafeDir, "%v", err)
	}
	if bad := trusted(before, dir, true, 0o700); bad != nil {
		return nil, bad
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, unavailable(ReasonUnsafeDir, "%v", err)
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, unavailable(ReasonUnsafeDir, "%s changed while it was opened", dir)
	}
	return root, nil
}
