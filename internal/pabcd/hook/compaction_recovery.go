package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The compaction recovery boundary (CRW-1090 evaluation d2, verification round 3). A compaction that no user prompt has
// followed is the window in which the model recovers the compacted context, and a Stop inside it releases instead of
// spending its budget. The window ends at the next user turn, never because tool output pushed the compaction out of
// reach, while every transcript read stays within the 64 KiB tail (CRW-1160). So the boundary is recorded when a hook
// sees it: PostCompact, which Codex runs for the compaction itself, and a Stop whose tail shows a compaction no prompt has
// followed (a PostCompact that did not run). The record is the file <session>.compacted beside the session state
// (.crw/sessions, which `crw pabcd reset --state` removes with it). The next UserPromptSubmit removes it, and so does a
// Stop whose tail shows a user prompt after the last compaction it shows.
//
// A compaction no hook saw, because PostCompact did not run and it left the tail before the first Stop, is not known: that
// Stop blocks, as one with no compaction does. The file is created and removed without the session lock: the hooks that
// touch it run one after another in a session, and a lost race costs one release or one block, never a state write.

// compactionRecoverySuffix names the record beside the session state file <session>.json.
const compactionRecoverySuffix = ".compacted"

// compactionRecoveryPath is the record of the session, or "" for a session id the state file name would rewrite.
func compactionRecoveryPath(cwd, sessionID string) string {
	if !state.IsCanonicalSessionID(sessionID) {
		return ""
	}
	return strings.TrimSuffix(state.StatePath(cwd, sessionID), ".json") + compactionRecoverySuffix
}

// compactionRecoveryBegin records the boundary for a session whose state file exists, in real directories: nothing is
// created for a workspace with no session state, and nothing through a symbolic link.
func compactionRecoveryBegin(cwd, sessionID string) {
	path := compactionRecoveryPath(cwd, sessionID)
	if path == "" {
		return
	}
	for _, dir := range []string{filepath.Join(cwd, crwdir.DirName), filepath.Dir(path)} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return
		}
	}
	if info, err := os.Lstat(state.StatePath(cwd, sessionID)); err != nil || !info.Mode().IsRegular() {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString("compacted\n")
	_ = f.Close()
}

// compactionRecoveryEnd removes the record, if it is one.
func compactionRecoveryEnd(cwd, sessionID string) {
	if compactionRecoveryHeld(cwd, sessionID) {
		_ = os.Remove(compactionRecoveryPath(cwd, sessionID))
	}
}

// compactionRecoveryHeld is whether the record exists as a regular file.
func compactionRecoveryHeld(cwd, sessionID string) bool {
	path := compactionRecoveryPath(cwd, sessionID)
	if path == "" {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// stopContextPressure is whether the Stop is inside a compaction's recovery window. The 64 KiB tail decides when it shows
// a boundary: a compaction no prompt has followed is pressure (and is recorded), a prompt after the last compaction it
// shows is not (and ends a recorded window). A tail that shows neither is pressure exactly when the boundary was recorded.
func stopContextPressure(p StopPayload) bool {
	g := host.ReadTranscriptGeneration(p.TranscriptPath, host.TailBytes)
	switch {
	case g.ContextPressure():
		compactionRecoveryBegin(p.Cwd, p.SessionID)
		return true
	case g.UserTurnRecorded():
		compactionRecoveryEnd(p.Cwd, p.SessionID)
		return false
	}
	return compactionRecoveryHeld(p.Cwd, p.SessionID)
}
