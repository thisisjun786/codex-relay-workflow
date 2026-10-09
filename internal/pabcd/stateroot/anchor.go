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

// Guard is Check for a CRW resume path, run before anything is sent: nativeCwd is the cwd the host
// reports for the thread now, targetCwd the cwd the resume would send ("" when it sends none, which
// keeps the native root). When the native root holds the thread's state, the root the thread will
// run in is recorded as its anchor so a SessionStart bootstrap applies the same resolution: the
// native root when the target is refused or is that root, the target when nothing is in flight and
// the thread moves on. A thread without state at its native root records nothing.
func Guard(env host.LookupEnv, nativeCwd, targetCwd, sessionID string) *Conflict {
	if nativeCwd == "" || !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	conflict := Check(nativeCwd, targetCwd, sessionID)
	if present, _, _, _ := inFlight(nativeCwd, sessionID); present {
		root := nativeCwd
		if conflict == nil && targetCwd != "" && !Same(nativeCwd, targetCwd) {
			root = targetCwd
		}
		writeAnchor(env, sessionID, clean(root))
	}
	return conflict
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
