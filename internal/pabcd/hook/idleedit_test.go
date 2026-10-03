package hook_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func idleEnv(t *testing.T) (host.LookupEnv, string) {
	t.Helper()
	dir := t.TempDir()
	m := map[string]string{"HOME": dir, "CODEX_HOME": dir, "CODEX_SQLITE_HOME": dir, "CRW_BIN": "crw"}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }, dir
}

func idlePayload(cwd, sid, tool string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": sid, "cwd": cwd, "tool_name": tool})
	return string(b)
}

func idleSeed(t *testing.T, cwd, sid string, armed bool, count float64) {
	t.Helper()
	s := state.DefaultState(sid, "")
	s.LoopArmSeen, s.IdleEditNudges = armed, count
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

func idleGoal(t *testing.T, dir, status string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, host.GoalsDBFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE thread_goals(thread_id TEXT, status TEXT); INSERT INTO thread_goals VALUES('s1', ?)", status); err != nil {
		t.Fatal(err)
	}
}

func TestIdleEditAdvisoryEnvelopeAndFrequency(t *testing.T) {
	// Ported from idle-edit.test.ts:23-70: allow context, suppression and 1/6/11 frequency.
	env, _ := idleEnv(t)
	cwd := t.TempDir()
	idleSeed(t, cwd, "s1", true, 0)
	var fired []bool
	for i := 0; i < 11; i++ {
		out := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "apply_patch"), env)
		fired = append(fired, out != "")
		if i == 0 {
			want := "[crw IDLE-EDIT] You are editing files while the PABCD FSM is un-armed but this session expects loop/goal work. If this edit belongs to the loop, arm first: `crw pabcd orchestrate status --session s1` -> enter P -> advance edges with --attest (one work-phase = one full PABCD cycle). C0 edits need no automatic devlog record. C1 edits record only in an existing owning unit. Do not create a unit just for a fast-path edit; explicit user/release record requirements remain controlling (UNIT-RESIDENCE-01, dev §0.1)."
			var v struct {
				Output struct{ HookEventName, PermissionDecision, AdditionalContext string } `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &v); err != nil || v.Output.HookEventName != "PreToolUse" || v.Output.PermissionDecision != "allow" || v.Output.AdditionalContext != want || !strings.HasSuffix(out, "\n") {
				t.Fatalf("%q / %v", out, err)
			}
		}
	}
	if !reflect.DeepEqual(fired, []bool{true, false, false, false, false, true, false, false, false, false, true}) || state.ReadState(cwd, "s1").IdleEditNudges != 11 {
		t.Fatal(fired, state.ReadState(cwd, "s1"))
	}
	if hook.NudgeEvery != 5 {
		t.Fatal(hook.NudgeEvery)
	}
}

func TestIdleEditAdvisorySuppressionAndPayloadQuirks(t *testing.T) {
	env, _ := idleEnv(t)
	cwd := t.TempDir()
	if got := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "apply_patch"), env); got != "" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
		t.Fatalf("silent advisory created state: %v", err)
	}
	for _, phase := range state.WorkPhases() {
		s := state.DefaultState("s1", "")
		s.Phase, s.LoopArmSeen = phase, true
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		if got := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "apply_patch"), env); got != "" {
			t.Fatalf("phase %s answered %q", phase, got)
		}
	}
	idleSeed(t, cwd, "s1", true, 0)
	for _, raw := range []string{"{not json", "null", "[]", "{}", "{} {}", "\ufeff" + idlePayload(cwd, "s1", "apply_patch"), idlePayload(cwd, "s1", "Bash"), idlePayload(cwd, "", "apply_patch"), idlePayload("", "s1", "Edit")} {
		if got := hook.HandleIdleEditAdvisory(raw, env); got != "" {
			t.Fatalf("%q answered %q", raw, got)
		}
	}
	if state.ReadState(cwd, "s1").IdleEditNudges != 0 {
		t.Fatal("ignored payload incremented counter")
	}
	for _, tool := range []string{"Write", "Edit", "apply_patch"} {
		idleSeed(t, cwd, "s1", true, 0)
		raw := strings.Replace(idlePayload(cwd, "s1", tool), "PreToolUse", "Stop", 1)
		if got := hook.HandleIdleEditAdvisory(raw, env); got == "" {
			t.Fatal("oracle ignores event on advisory path")
		}
	}
}

func TestIdleEditAdvisoryGoalStatusAndInvocation(t *testing.T) {
	for _, status := range []string{"active", "complete", "paused"} {
		t.Run(status, func(t *testing.T) {
			env, dir := idleEnv(t)
			idleGoal(t, dir, status)
			out := hook.HandleIdleEditAdvisory(idlePayload(t.TempDir(), "s1", "apply_patch"), env)
			if (out != "") != (status == "active") {
				t.Fatal(out)
			}
		})
	}
	env, dir := idleEnv(t)
	if err := os.WriteFile(filepath.Join(dir, host.GoalsDBFilename), []byte("bad database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := hook.HandleIdleEditAdvisory(idlePayload(t.TempDir(), "s1", "apply_patch"), env); out != "" {
		t.Fatal("unreadable goal must be inactive", out)
	}
	custom := func(k string) (string, bool) {
		if k == "CRW_BIN" {
			return "  custom-crw  ", true
		}
		return env(k)
	}
	if out := hook.IdleEditAdvisory("s1", custom); !strings.Contains(out, "`custom-crw pabcd orchestrate status --session s1`") {
		t.Fatal(out)
	}
}

func TestIdleEditCounterFailuresStayFailOpen(t *testing.T) {
	env, _ := idleEnv(t)
	cwd := t.TempDir()
	idleSeed(t, cwd, "s1", true, 0)
	path := state.StatePath(cwd, "s1")
	before, _ := os.ReadFile(path)
	if err := os.WriteFile(path+".lock", []byte("held by test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "Edit"), env); out == "" {
		t.Fatal("failed lock hid advisory")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("held lock did not preserve state")
	}
	// A valid filename whose temporary-write suffix exceeds NAME_MAX fails WriteState.
	sid := strings.Repeat("s", 240)
	path = state.StatePath(cwd, sid)
	before = []byte(`{"phase":"IDLE","loopArmSeen":true}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out := hook.HandleIdleEditAdvisory(idlePayload(cwd, sid, "Write"), env); out == "" {
			t.Fatal("failed counter write hid advisory")
		}
	}
	after, _ = os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("failed write changed state")
	}
}

func TestIdleEditPreservesLossyState(t *testing.T) {
	env, dir := idleEnv(t)
	idleGoal(t, dir, "active")
	for _, kind := range []string{"unreadable", "overflow", "malformed", "legacy recovery"} {
		t.Run(kind, func(t *testing.T) {
			cwd := t.TempDir()
			idleSeed(t, cwd, "s1", true, 0)
			s := state.DefaultState("s1", "")
			s.LoopArmSeen = true
			for i := 0; i < 66; i++ {
				s.UnverifiedSubagents = append(s.UnverifiedSubagents, state.UnverifiedSubagent{AgentID: "a", TurnID: "t", AgentType: "reviewer", Attempts: 1, ReceiptClaimed: "verdict", RecordedAt: "2026-01-01", Resolvable: true})
			}
			before, _ := json.Marshal(s)
			switch kind {
			case "unreadable":
				before = []byte(`{"phase":"IDLE","loopArmSeen":true,`)
			case "malformed":
				before = []byte(`{"phase":"IDLE","loopArmSeen":true,"unverifiedSubagents":[{"bad":true}]}`)
			case "legacy recovery":
				before = []byte(`{"phase":"IDLE","loopArmSeen":true,"checkEpoch":"e","dcloseRecovery":{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1"}}`)
			}
			path := state.StatePath(cwd, "s1")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			out := hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "apply_patch"), env)
			if kind == "unreadable" && out != "" {
				t.Fatal("unreadable state should be silent")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("%s state lost: %v", kind, err)
			}
		})
	}
}

func TestIdleEditRereadsAfterParticipatingWriter(t *testing.T) {
	env, dir := idleEnv(t)
	idleGoal(t, dir, "active")
	cwd := t.TempDir()
	idleSeed(t, cwd, "s1", false, 0)
	read, resume := make(chan struct{}), make(chan struct{})
	blockedEnv := func(k string) (string, bool) {
		if k == "CODEX_SQLITE_HOME" {
			close(read) // goal lookup happens after the first state read
			<-resume
		}
		return env(k)
	}
	answer := make(chan string, 1)
	go func() { answer <- hook.HandleIdleEditAdvisory(idlePayload(cwd, "s1", "Edit"), blockedEnv) }()
	<-read
	err := state.WithSessionLock(cwd, "s1", func() error {
		s := state.ReadState(cwd, "s1")
		s.MemoryWriteGrant = true
		s.UnverifiedSubagents = append(s.UnverifiedSubagents, state.UnverifiedSubagent{AgentID: "new-agent", TurnID: "t", AgentType: "reviewer", Attempts: 1, ReceiptClaimed: "verdict", RecordedAt: "2026-01-01", Resolvable: true})
		return state.WriteState(cwd, s)
	})
	close(resume)
	if out := <-answer; out == "" || err != nil {
		t.Fatalf("%q / %v", out, err)
	}
	s := state.ReadState(cwd, "s1")
	if !s.MemoryWriteGrant || len(s.UnverifiedSubagents) != 1 || s.IdleEditNudges != 1 {
		t.Fatalf("concurrent record was lost: %#v", s)
	}
}
