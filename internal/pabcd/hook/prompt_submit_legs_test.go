package hook_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// promptSubmitLeg returns the wired UserPromptSubmit leg from the harness table.
func promptSubmitLeg(t *testing.T) harness.Leg {
	t.Helper()
	for _, leg := range harness.Legs() {
		if leg.ID == "user-prompt-submit-checking-pabcd-trigger" {
			if leg.Handle == nil {
				t.Fatal("the UserPromptSubmit leg has no handler")
			}
			return leg
		}
	}
	t.Fatal("no user-prompt-submit-checking-pabcd-trigger leg")
	return harness.Leg{}
}

// TestPromptSubmitLegAnswersThroughTheEnvelope drives the wired leg as the host does: the arming
// mandate comes back as one hookSpecificOutput line, the leg's quiet paths answer nothing, and the
// leg leaves the state the handler wrote. The legs read the process environment, so the test sets it.
func TestPromptSubmitLegAnswersThroughTheEnvelope(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_BIN", "{CRW}")
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(dir, "codex-home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(dir, "no-relay-state"))
	leg := promptSubmitLeg(t)
	if leg.Event != "user-prompt-submit" || leg.Slug != "user-prompt-submit" || leg.Stage != harness.Generic || leg.Recover || leg.SubagentExempt || leg.Gated {
		t.Errorf("the leg's registration changed: %+v", leg)
	}
	call := func(fields map[string]any, enabled bool) harness.Call {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return harness.Call{Raw: string(raw), PabcdEnabled: enabled}
	}
	// The recorded loop_arm_request, replayed with the CRW name (CRW-1084, port: fixed): a project coordination request gets
	// the scope pointer for crw-run, where the oracle gave the implementation recipe; the pointer arms nothing.
	answer := leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t1", "prompt": "Start crw-loop for the migration project."}, true))
	ctx := answerContext(t, answer, "UserPromptSubmit")
	if !strings.HasPrefix(ctx, "[crw: LOOP — scope choice (ORCH-MANDATE-01)]") || strings.Contains(ctx, "loop init") {
		t.Errorf("the project coordination pointer: %q", ctx)
	}
	if s := state.ReadState(cwd, "rec-s1"); s.LoopArmSeen || len(s.InjectedTurns) != 1 {
		t.Errorf("the pointer must not record loopArmSeen: %+v", s)
	}
	// A single-task loop request keeps the recipe and records loopArmSeen.
	answer = leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t1b", "prompt": "Run crw-loop for this task"}, true))
	ctx = answerContext(t, answer, "UserPromptSubmit")
	if !strings.HasPrefix(ctx, "[crw: LOOP — orchestrate arming mandate (ORCH-MANDATE-01)]") {
		t.Errorf("the arming mandate: %q", ctx)
	}
	if s := state.ReadState(cwd, "rec-s1"); !s.LoopArmSeen {
		t.Errorf("the leg did not record loopArmSeen: %+v", s)
	}
	// A foreign event is not this handler's: the parse refuses it and the leg answers nothing.
	if got := leg.Handle(call(map[string]any{"hook_event_name": "Stop", "session_id": "rec-s1", "cwd": cwd}, true)); got != "" {
		t.Errorf("a foreign event answered %q", got)
	}
	// A prompt that asks for nothing answers nothing.
	if got := leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t2", "prompt": "Summarise the README."}, true)); got != "" {
		t.Errorf("a quiet prompt answered %q", got)
	}
	// PABCD off with a remember request: silent, but the marker is still recorded.
	if got := leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t3", "prompt": "Remember this: the deploy key lives in the vault."}, false)); got != "" {
		t.Errorf("a pabcd-off prompt answered %q", got)
	}
	if s := state.ReadState(cwd, "rec-s1"); !s.MemoryWriteRequested {
		t.Errorf("the leg did not record the marker: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir, "rec-s1.json")); err != nil {
		t.Errorf("no state file: %v", err)
	}
}

// CRW-1084 / CRW-1116 (post-evaluation): the production leg reads the session's role from the relay registry, read only. A
// session the registry binds as a dispatched task keeps the loop recipe for a project-worded request, a bound parent gets the
// parent pointer for any loop request, and a session with no binding, a replaced binding, two roles, or no readable store is
// an unknown role that the prompt's scope words decide.
func TestPromptSubmitLegReadsTheRegistryRole(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "relay-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_BIN", "{CRW}")
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(dir, "codex-home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", stateDir)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(stateDir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	bind := func(id, role, kind, key, task, status string, session *string) {
		t.Helper()
		row := store.ScopeBindingsRow{BindingID: id, Role: role, ScopeKind: kind, ScopeKey: key, TaskID: task, HostID: "h", Status: status, Revision: 1,
			CreatedAt: "2026-10-10T00:00:00.000000+00:00", UpdatedAt: "2026-10-10T00:00:00.000000+00:00"}
		if session != nil {
			row.CXCSession.String, row.CXCSession.Valid = *session, true
		}
		if err := storeseed.InsertScopeBinding(ctx, st, row); err != nil {
			t.Fatal(err)
		}
	}
	viaSession := "thread-by-session"
	bind("b1", "child", "issue", "CRW-1", "thread-child", "active", nil)
	bind("b2", "parent", "project", "P-1", "thread-parent", "active", nil)
	bind("b3", "child", "issue", "CRW-2", "other-task", "active", &viaSession)
	bind("b4", "child", "issue", "CRW-3", "thread-replaced", "archived", nil)
	bind("b5", "child", "issue", "CRW-4", "thread-both", "active", nil)
	bind("b6", "parent", "project", "P-2", "thread-both", "active", nil)
	bind("b7", "parent", "project", "P-3", "thread-paused-parent", "paused", nil)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	leg := promptSubmitLeg(t)
	run := func(session, prompt string) string {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": session, "cwd": cwd, "turn_id": "t-" + session, "prompt": prompt})
		if err != nil {
			t.Fatal(err)
		}
		return answerContext(t, leg.Handle(harness.Call{Raw: string(raw), PabcdEnabled: true}), "UserPromptSubmit")
	}
	const pointer, recipe = "[crw: LOOP — scope choice (ORCH-MANDATE-01)]", "[crw: LOOP — orchestrate arming mandate (ORCH-MANDATE-01)]"
	cases := []struct {
		name, session, prompt, want string
		parentWording               bool
	}{
		{"a dispatched task that names the project", "thread-child", "Start crw-loop for the migration project.", recipe, false},
		{"a task bound through its cxc session", "thread-by-session", "Start crw-loop for the migration project.", recipe, false},
		{"a bound parent asking for a single-task loop", "thread-parent", "Run crw-loop for this task", pointer, true},
		{"a paused parent binding still counts", "thread-paused-parent", "Run crw-loop for this task", pointer, true},
		{"an unregistered session naming the project", "thread-nobody", "Start crw-loop for the migration project.", pointer, false},
		{"an unregistered session with a single-task loop", "thread-nobody2", "Run crw-loop for this task", recipe, false},
		{"a replaced binding is no role", "thread-replaced", "Start crw-loop for the migration project.", pointer, false},
		{"two roles are no role", "thread-both", "Run crw-loop for this task", recipe, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := run(c.session, c.prompt)
			if !strings.HasPrefix(got, c.want) {
				t.Fatalf("want %q:\n%s", c.want, got)
			}
			if c.want == pointer && strings.Contains(got, "registered as a project parent") != c.parentWording {
				t.Fatalf("parent wording = %v:\n%s", c.parentWording, got)
			}
		})
	}
	// No readable store: the role is unknown and the lexical scope decides.
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(dir, "absent"))
	if got := run("thread-child2", "Start crw-loop for the migration project."); !strings.HasPrefix(got, pointer) {
		t.Fatalf("no store:\n%s", got)
	}
}
