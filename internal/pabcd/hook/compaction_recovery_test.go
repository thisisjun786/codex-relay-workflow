package hook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// TestCompactionRecoveryRecordLivesBesideTheState is the record of compaction_recovery.go (CRW-1090 verification round 3):
// PostCompact writes it beside an existing session state, whatever the phase, never for a workspace with no state, never
// through a symbolic link and never for a session id the state file name would rewrite; the next UserPromptSubmit removes
// it, with or without PABCD enabled.
func TestCompactionRecoveryRecordLivesBesideTheState(t *testing.T) {
	record := func(cwd, sid string) string {
		return filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir, sid+compactionRecoverySuffix)
	}

	empty := t.TempDir()
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: empty, SessionID: "s1"})
	if _, err := os.Lstat(filepath.Join(empty, crwdir.DirName)); !os.IsNotExist(err) {
		t.Errorf("PostCompact in a workspace with no state created %s: %v", crwdir.DirName, err)
	}

	for _, enabled := range []bool{true, false} {
		cwd := t.TempDir()
		sessionHookStateFile(t, cwd, "s1", func(*state.State) {})
		before, err := os.ReadFile(state.StatePath(cwd, "s1"))
		if err != nil {
			t.Fatal(err)
		}
		SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
		SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
		if got, err := os.ReadFile(record(cwd, "s1")); err != nil || string(got) != "compacted\n" {
			t.Fatalf("the record of an IDLE session: %q, %v", got, err)
		}
		if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); string(after) != string(before) {
			t.Error("recording the boundary rewrote an IDLE state")
		}
		PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "next", TurnID: "t2", PabcdEnabled: enabled}, "", promptSubmitHost(cwd))
		if _, err := os.Lstat(record(cwd, "s1")); !os.IsNotExist(err) {
			t.Errorf("the next user turn (PABCD enabled %v) left the record: %v", enabled, err)
		}
	}

	// A sessions directory that is a symbolic link is not written through.
	cwd, elsewhere := t.TempDir(), t.TempDir()
	sessionHookStateFile(t, elsewhere, "s1", func(*state.State) {})
	if err := os.MkdirAll(filepath.Join(cwd, crwdir.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, crwdir.DirName, state.SessionsSubdir), filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir)); err != nil {
		t.Fatal(err)
	}
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
	if _, err := os.Lstat(record(elsewhere, "s1")); !os.IsNotExist(err) {
		t.Errorf("the record was written through a linked sessions directory: %v", err)
	}

	// A session id the file name would rewrite has no record.
	if compactionRecoveryPath(t.TempDir(), "s/1") != "" {
		t.Error("a non-canonical session id has a record path")
	}
}
