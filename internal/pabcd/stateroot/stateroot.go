// Package stateroot decides which directory holds a relay-managed thread's PABCD state when the
// thread is resumed or bootstrapped somewhere else (CRW-1140). PABCD state is addressed by cwd
// (cwd/.crw/sessions/<id>.json, state.StatePath), so a thread that is resumed at another cwd would
// open a fresh IDLE state machine beside the one it left in flight, detaching its phase, epochs and
// bound goalplan. The canonical root of a thread is its native cwd: the cwd the host reports for
// the thread before anything is resumed (the execution's own working directory, never a bound
// source worktree). Every CRW resume path and the SessionStart bootstrap judge a target cwd against
// that root with Check, refuse a target that would leave in-flight work behind, and never move,
// copy or choose between state files: relocation is an explicit handover to a thread started at the
// new cwd.
package stateroot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Code is the refusal code every path reports for a target that would detach in-flight state.
const Code = "state_root_conflict"

// Conflict is a target cwd that is not the thread's native root while the native root holds the
// thread's PABCD state in flight (or a state file that cannot be read, which cannot be told apart
// from one in flight).
type Conflict struct {
	SessionID  string
	NativeCwd  string // the root the state lives in
	TargetCwd  string // the cwd the resume or bootstrap asked for
	StatePath  string // the preserved state file
	Phase      string // its phase; empty when Unreadable
	Unreadable bool
}

func (c *Conflict) Error() string {
	held := fmt.Sprintf("is in flight at %s (phase %s)", c.StatePath, c.Phase)
	if c.Unreadable {
		held = fmt.Sprintf("is at %s and cannot be read, so it may be in flight", c.StatePath)
	}
	return fmt.Sprintf("%s: the PABCD state of thread %s %s; running the thread at %s would start an empty IDLE "+
		"state machine beside it. Nothing was moved and the state was left as it is. Resume the thread at its native "+
		"cwd %s, or hand the work over explicitly to a thread started at the new cwd (crw relay linkage-handover); "+
		"a bound source worktree is not a native cwd", Code, c.SessionID, held, c.TargetCwd, c.NativeCwd)
}

// Same reports whether a and b name the same directory: the same file when both exist (so a
// symbolic-link alias of the native root is the root), the same cleaned absolute path otherwise.
func Same(a, b string) bool {
	if left, err := os.Stat(a); err == nil {
		if right, err := os.Stat(b); err == nil {
			return os.SameFile(left, right)
		}
	}
	return clean(a) == clean(b)
}

func clean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// inFlight reads the session's state at root: present reports a state file at all, and the
// conflict fields say whether it holds work that a move would detach. An absent file holds
// nothing; an IDLE state has nothing in flight; any other phase, or a file that cannot be read, is
// in flight.
func inFlight(root, sessionID string) (present, flight, unreadable bool, phase string) {
	if _, err := os.Lstat(state.StatePath(root, sessionID)); errors.Is(err, fs.ErrNotExist) {
		return false, false, false, ""
	} else if err != nil {
		return true, true, true, ""
	}
	s, bad := state.ReadStateStrict(root, sessionID)
	if bad {
		return true, true, true, ""
	}
	return true, s.Phase != state.PhaseIdle, false, string(s.Phase)
}

// Check judges a resume or bootstrap of sessionID at targetCwd against the thread's native root
// nativeCwd. It is nil when either is empty (no root to protect, or no cwd asked for), when the
// target is the native root, and when the native root holds no state in flight; otherwise the
// Conflict names the preserved state. Nothing is written.
func Check(nativeCwd, targetCwd, sessionID string) *Conflict {
	if nativeCwd == "" || targetCwd == "" || !state.IsCanonicalSessionID(sessionID) || Same(nativeCwd, targetCwd) {
		return nil
	}
	_, flight, unreadable, phase := inFlight(nativeCwd, sessionID)
	if !flight {
		return nil
	}
	return &Conflict{SessionID: sessionID, NativeCwd: nativeCwd, TargetCwd: targetCwd,
		StatePath: state.StatePath(nativeCwd, sessionID), Phase: phase, Unreadable: unreadable}
}
