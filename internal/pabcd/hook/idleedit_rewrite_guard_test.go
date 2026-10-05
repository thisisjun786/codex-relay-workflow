package hook_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-593: the idle-edit counter is written back from the strict reader's state. A stored record the reader changes (a receiptClaimed
// of 257 characters, a field of the wrong type) was rewritten changed; now the write is skipped, and the advisory still answers.

func rewriteGuardIdleRecords(n int, edit func(m map[string]any)) []any {
	out := make([]any, n)
	for i := range out {
		m := map[string]any{"agentId": fmt.Sprintf("a%d", i+1), "turnId": "t1", "agentType": "executor", "attempts": 3, "receiptClaimed": "none", "recordedAt": "2026-01-01T00:00:00.000Z", "resolvable": true}
		if edit != nil {
			edit(m)
		}
		out[i] = m
	}
	return out
}

// rewriteGuardIdleSeed stores list as it is in an armed, idle session whose counter is 0, and returns the file's bytes.
func rewriteGuardIdleSeed(t *testing.T, cwd string, list []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"phase": "IDLE", "sessionId": "s1", "loopArmSeen": true, "idleEditNudges": 0, "unverifiedSubagents": list})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(state.StatePath(cwd, "s1")), 0o777); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(state.StatePath(cwd, "s1"), body, 0o666); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestIdleEditSkipsTheCounterOverARecordTheReaderChanges(t *testing.T) {
	env, _ := idleEnv(t)
	for name, edit := range map[string]func(m map[string]any){
		"a receipt of 257 characters":         func(m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen+1) },
		"a receipt cut inside an astral char": func(m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", 255) + "\U0001F600b" },
		"attempts stored as text":             func(m map[string]any) { m["attempts"] = "3" },
	} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			before := rewriteGuardIdleSeed(t, cwd, rewriteGuardIdleRecords(1, edit))
			if out := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "apply_patch"), env); out == "" {
				t.Error("a skipped counter hid the advisory")
			}
			if after, err := os.ReadFile(state.StatePath(cwd, "s1")); err != nil || !bytes.Equal(after, before) {
				t.Errorf("the counter write changed the file (%d bytes, was %d; %v)", len(after), len(before), err)
			}
		})
	}
}

// A list the reader keeps whole is counted as before, and so is a session that has no file yet.
func TestIdleEditCountsAListTheReaderKeepsWhole(t *testing.T) {
	env, dir := idleEnv(t)
	idleGoal(t, dir, "active")
	cwd := t.TempDir()
	rewriteGuardIdleSeed(t, cwd, rewriteGuardIdleRecords(3, nil))
	if out := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "apply_patch"), env); out == "" {
		t.Error("no advisory")
	}
	if s := state.ReadState(cwd, "s1"); s.IdleEditNudges != 1 || len(s.UnverifiedSubagents) != 3 {
		t.Errorf("counter %v, records %d", s.IdleEditNudges, len(s.UnverifiedSubagents))
	}
	fresh := t.TempDir()
	if out := hook.HandleIdleEditAdvisory(idlePayload(fresh, "s1", "apply_patch"), env); out == "" {
		t.Error("no advisory without a state file")
	}
	if s := state.ReadState(fresh, "s1"); s.IdleEditNudges != 1 {
		t.Errorf("counter %v: the state was not created", s.IdleEditNudges)
	}
}

// The first read sees a clean state and the writer that follows stores a receipt of 257 characters under the lock: the counter write
// reads the state again inside the lock and leaves that file as the writer left it.
func TestIdleEditRefusesARecordStoredMeanwhile(t *testing.T) {
	env, dir := idleEnv(t)
	idleGoal(t, dir, "active")
	cwd := t.TempDir()
	idleSeed(t, cwd, "s1", false, 0)
	read, resume := make(chan struct{}), make(chan struct{})
	blockedEnv := func(k string) (string, bool) {
		if k == "CODEX_SQLITE_HOME" {
			close(read) // the goal lookup follows the first state read
			<-resume
		}
		return env(k)
	}
	answer := make(chan string, 1)
	go func() { answer <- hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "Edit"), blockedEnv) }()
	<-read
	var stored []byte
	err := state.WithSessionLock(cwd, "s1", func() error {
		s := state.ReadState(cwd, "s1")
		s.UnverifiedSubagents = append(s.UnverifiedSubagents, state.UnverifiedSubagent{AgentID: "new-agent", TurnID: "t", AgentType: "reviewer", Attempts: 1, ReceiptClaimed: strings.Repeat("r", state.MaxReceiptClaimLen+1), RecordedAt: "2026-01-01", Resolvable: true})
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		var readErr error
		stored, readErr = os.ReadFile(state.StatePath(cwd, "s1"))
		return readErr
	})
	close(resume)
	if out := <-answer; out == "" || err != nil {
		t.Fatalf("%q / %v", out, err)
	}
	if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); !bytes.Equal(after, stored) {
		t.Errorf("the counter write changed the writer's file (%d bytes, was %d)", len(after), len(stored))
	}
}
