package stateroot

import (
	"encoding/json"
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

// writeAnchor records root as sessionID's native root, replacing the file whole (written beside it
// and renamed over it). It is best effort: a failure leaves the previous anchor, or none, and the
// resume paths still refuse on Check alone.
func writeAnchor(env host.LookupEnv, sessionID, root string) {
	path := AnchorPath(env, sessionID)
	if path == "" || !filepath.IsAbs(root) {
		return
	}
	raw, err := json.Marshal(anchor{Version: anchorVersion, SessionID: sessionID, NativeCwd: root})
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "."+sessionID+".*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
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

// Guard is Resolve followed by the record of the native root, run before anything is sent. The
// root is recorded as the thread's anchor whether or not a state file exists yet (a thread a CRW
// resume path has resolved is a relay-managed thread, and its first SessionStart is still to
// come), so a SessionStart bootstrap of that thread applies the same resolution. The root is the
// one the thread is on before the resume: it is not moved to a target the resume has not reached
// (Moved does that once the host confirms it), so a resume that fails leaves the protection where
// the work is. A thread with no anchor that no CRW path resolves (a standalone terminal session)
// has none.
func Guard(env host.LookupEnv, hostCwd, targetCwd, sessionID string) *Conflict {
	if !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	if root := nativeRoot(env, hostCwd, sessionID); root != "" && !Same(root, readAnchor(env, sessionID)) {
		writeAnchor(env, sessionID, clean(root))
	}
	return Resolve(env, hostCwd, targetCwd, sessionID)
}

// Moved records that a resume of sessionID to targetCwd reached the host, run after the host
// confirmed it: the thread now runs at targetCwd, which is its native root when nothing was in
// flight at the root it left (a resume that would have detached work was refused before it was
// sent). It is a no-op for a resume that sent no cwd or kept the root.
func Moved(env host.LookupEnv, hostCwd, targetCwd, sessionID string) {
	if targetCwd == "" || !state.IsCanonicalSessionID(sessionID) || Resolve(env, hostCwd, targetCwd, sessionID) != nil {
		return
	}
	if !Same(targetCwd, readAnchor(env, sessionID)) {
		writeAnchor(env, sessionID, clean(targetCwd))
	}
}

// Bootstrap is Check for the SessionStart bootstrap of sessionID at cwd: the native root is the
// thread's anchor. It is nil, and the bootstrap goes ahead as before, when the thread has no
// anchor (a standalone session), when cwd already holds a state file for the session (the
// bootstrap creates nothing there), and when the anchored root holds nothing in flight.
func Bootstrap(env host.LookupEnv, cwd, sessionID string) *Conflict {
	root := readAnchor(env, sessionID)
	if root == "" {
		return nil
	}
	if _, err := os.Lstat(state.StatePath(cwd, sessionID)); err == nil {
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
		writeAnchor(env, sessionID, clean(cwd))
	}
}
