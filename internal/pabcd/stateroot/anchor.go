package stateroot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// AnchorSubdir is the directory under the CRW home (host.CRWHome: CRW_HOME, else ~/.crw) that
// records, per relay-managed thread, the native root a CRW resume path resolved for it. The
// SessionStart bootstrap reads it there, so a hook needs neither a running relay nor its store: a
// thread no CRW resume path has resolved (a standalone terminal session) has no anchor and
// bootstraps in its payload cwd exactly as before.
const AnchorSubdir = "state-roots"

// anchorVersion is the shape of an anchor file; a file of another version is not read.
const anchorVersion = 1

type anchor struct {
	Version   int    `json:"version"`
	SessionID string `json:"sessionId"`
	NativeCwd string `json:"nativeCwd"`
}

// AnchorPath is the anchor file of sessionID, or "" when the CRW home cannot be resolved or the
// id is not canonical.
func AnchorPath(env host.LookupEnv, sessionID string) string {
	if !state.IsCanonicalSessionID(sessionID) {
		return ""
	}
	home, err := host.CRWHome(env)
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, AnchorSubdir, sessionID+".json")
}

// readAnchor is the native root recorded for sessionID, "" when there is none or it cannot be
// trusted.
func readAnchor(env host.LookupEnv, sessionID string) string {
	path := AnchorPath(env, sessionID)
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var a anchor
	if json.Unmarshal(raw, &a) != nil || a.Version != anchorVersion || a.SessionID != sessionID || !filepath.IsAbs(a.NativeCwd) {
		return ""
	}
	return a.NativeCwd
}

// AnchorCode is the refusal code of a resume whose thread's native root could not be recorded as
// its anchor.
const AnchorCode = "state_root_anchor_unrecorded"

// AnchorError is a resume refused because the native root of its thread could not be recorded:
// without the anchor a later SessionStart of the thread could not tell where its state lives, so
// it could open an empty IDLE state beside work in flight.
type AnchorError struct {
	SessionID string
	NativeCwd string // the root the anchor would have named
	Path      string // the anchor file, "" when the CRW home cannot be resolved
	Err       error
}

func (e *AnchorError) Error() string {
	where := "under the CRW home (CRW_HOME, else ~/.crw), which cannot be resolved"
	if e.Path != "" {
		where = "at " + e.Path
	}
	return fmt.Sprintf("%s: the native cwd %s of thread %s could not be recorded %s (%v), so the thread's SessionStart "+
		"could not be kept from opening an empty IDLE state beside its PABCD work. Nothing was sent; make the CRW home "+
		"writable and retry", AnchorCode, e.NativeCwd, e.SessionID, where, e.Err)
}

func (e *AnchorError) Unwrap() error { return e.Err }

// CodeOf is the refusal code of an error Guard returned: AnchorCode for an anchor that could not be
// recorded, Code otherwise.
func CodeOf(err error) string {
	if anchorErr := (*AnchorError)(nil); errors.As(err, &anchorErr) {
		return AnchorCode
	}
	return Code
}

// writeAnchor records root as sessionID's native root, replacing the file whole (written beside it
// and renamed over it), and reports why it could not: a failure leaves the previous anchor, or none.
func writeAnchor(env host.LookupEnv, sessionID, root string) error {
	path := AnchorPath(env, sessionID)
	fail := func(err error) error {
		return &AnchorError{SessionID: sessionID, NativeCwd: root, Path: path, Err: err}
	}
	if path == "" {
		return fail(errors.New("no absolute CRW home"))
	}
	if !filepath.IsAbs(root) {
		return fail(fmt.Errorf("the root %q is not absolute", root))
	}
	raw, err := json.Marshal(anchor{Version: anchorVersion, SessionID: sessionID, NativeCwd: root})
	if err != nil {
		return fail(err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(dir, "."+sessionID+".*.tmp")
	if err != nil {
		return fail(err)
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fail(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fail(err)
	}
	return nil
}

// recordAnchor records root as sessionID's anchor unless the anchor already names it. An absent
// anchor is always written: it is not the empty path, which Same would resolve to the process's own
// cwd.
func recordAnchor(env host.LookupEnv, sessionID, root string) error {
	if anchored := readAnchor(env, sessionID); anchored != "" && Same(root, anchored) {
		return nil
	}
	return writeAnchor(env, sessionID, clean(root))
}

// Resolve is Check for a CRW resume path of sessionID, nothing written: hostCwd is the cwd the host
// reports for the thread now, targetCwd the cwd the resume would send ("" when it sends none, so
// the thread runs wherever the host has it). The native root is the thread's anchor while that
// root holds work in flight, whatever the host reports now (an external resume may have moved the
// host's cwd away from it), and the host's cwd otherwise. The target is judged against that root
// with Check; a resume that sends no cwd is judged at the host's cwd.
func Resolve(env host.LookupEnv, hostCwd, targetCwd, sessionID string) *Conflict {
	if !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	root := nativeRoot(env, hostCwd, sessionID)
	if targetCwd == "" {
		targetCwd = hostCwd
	}
	return Check(root, targetCwd, sessionID)
}

// nativeRoot is the root that holds the thread's work: the anchored root while it holds work in
// flight, else the host's cwd.
func nativeRoot(env host.LookupEnv, hostCwd, sessionID string) string {
	if anchored := readAnchor(env, sessionID); anchored != "" {
		if _, flight, _, _ := inFlight(anchored, sessionID); flight {
			return anchored
		}
	}
	return hostCwd
}

// Guard is Resolve preceded by the record of the native root, run before anything is sent. The
// root is recorded as the thread's anchor whether or not a state file exists yet (a thread a CRW
// resume path has resolved is a relay-managed thread, and its first SessionStart is still to
// come), so a SessionStart bootstrap of that thread applies the same resolution. The root is the
// one the thread is on before the resume: it is not moved to a target the resume has not reached
// (Moved does that once the host confirms it), so a resume that fails leaves the protection where
// the work is. A thread with no anchor that no CRW path resolves (a standalone terminal session)
// has none.
//
// The error is the *Conflict of Resolve, or an *AnchorError when the root had to be recorded and
// could not be (an anchor that already names the root needs no write): a resume whose
// SessionStart could not be guarded is not sent. A host that reports no cwd gives no root to record
// or compare. CodeOf names the refusal code.
func Guard(env host.LookupEnv, hostCwd, targetCwd, sessionID string) error {
	if !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	var unrecorded error
	if root := nativeRoot(env, hostCwd, sessionID); root != "" {
		unrecorded = recordAnchor(env, sessionID, root)
	}
	if conflict := Resolve(env, hostCwd, targetCwd, sessionID); conflict != nil {
		return conflict
	}
	return unrecorded
}

// Moved records that a resume of sessionID to targetCwd reached the host, run after the host
// confirmed it: the thread now runs at targetCwd, which is its native root when nothing was in
// flight at the root it left (a resume that would have detached work was refused before it was
// sent). It is a no-op for a resume that sent no cwd or kept the root. The resume has already
// happened, so a failure to record is not a refusal: Guard has just recorded the root the thread
// left, which holds nothing in flight, and the thread's next SessionStart re-anchors it
// (Bootstrapped).
func Moved(env host.LookupEnv, hostCwd, targetCwd, sessionID string) {
	if targetCwd == "" || !state.IsCanonicalSessionID(sessionID) || Resolve(env, hostCwd, targetCwd, sessionID) != nil {
		return
	}
	_ = recordAnchor(env, sessionID, targetCwd)
}

// Bootstrap is Check for the SessionStart bootstrap of sessionID at cwd: the native root is the
// thread's anchor. It is nil, and the bootstrap goes ahead as before, when the thread has no
// anchor (a standalone session) and when the anchored root holds nothing in flight. A state file
// that already exists at cwd does not exempt it: a legacy IDLE state beside work in flight at the
// anchored root is refused as the resume to cwd is, so the agent is sent to the root that holds its
// work.
func Bootstrap(env host.LookupEnv, cwd, sessionID string) *Conflict {
	root := readAnchor(env, sessionID)
	if root == "" {
		return nil
	}
	return Check(root, cwd, sessionID)
}

// Bootstrapped records, after a SessionStart bootstrap that went ahead at cwd, that an anchored
// thread now runs there: its anchor was a root that held nothing in flight, so the thread's work
// starts at cwd. A thread without an anchor records nothing.
func Bootstrapped(env host.LookupEnv, cwd, sessionID string) {
	root := readAnchor(env, sessionID)
	if root == "" || cwd == "" || Same(root, cwd) {
		return
	}
	if _, flight, _, _ := inFlight(root, sessionID); !flight {
		// Best effort: the bootstrap already went ahead, and a failure leaves the idle root anchored.
		_ = writeAnchor(env, sessionID, clean(cwd))
	}
}
