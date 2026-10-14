package hook

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// TestChildBindingOfASubagentThreadPayload (CRW-1303): the hook input of a thread a relay child spawns carries the child's own
// session_id (the root session of the thread tree, which the rollout's session_meta names as session_id and as source.subagent.
// thread_spawn.parent_thread_id) beside the spawned thread's id as agent_id. Read live on 2026-10-14 from the host's hook decision
// records and rollouts: each of the seven sub-agent pairs there has sessionId == parent_thread_id == session_meta.session_id and
// agentId == the sub-agent's own thread id; no sub-agent thread id appears as a sessionId. So ChildBinding, asked for the payload's
// session_id, resolves the sub-agent thread to its parent child's binding (the same parent_cwd), while the sub-agent's own thread id
// and the sessions of a parent or an integrator, which the store holds no child_task_id for, resolve to no child.
func TestChildBindingOfASubagentThreadPayload(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	home, state := filepath.Join(dir, "codex"), filepath.Join(dir, "state")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	const (
		kid        = "01a1357c-25e3-71e1-9120-17a30d441d91" // the relay child's thread: relationships.child_task_id
		spawned    = "01a1357d-48f8-7391-9d70-efab8d0dcfff" // the sub-agent thread the child spawned: the payload's agent_id
		parent     = "01a13570-0000-7000-8000-000000000001"
		integrator = "01a13570-0000-7000-8000-000000000002"
	)
	writeTest(t, filepath.Join(home, ConfigName), []byte(`{"configVersion": 1, "event": "Stop", "relayExecutable": "/nonexistent/relay", "markerRoot": "`+dir+`/markers", "dbPath": "`+
		filepath.Join(state, "relay.sqlite3")+`", "mode": "observe", "timeoutSeconds": 5, "journalRoot": "`+dir+`/journal", "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": null}`))
	s, err := fixtureStore(ctx, filepath.Join(state, "relay.sqlite3"), filepath.Join(dir, "app.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','ISS-1','active',?,'host','/work/parent',?,'host',NULL,1,'[]','[]','2026-10-14T10:00:00Z','2026-10-14T10:00:00Z')", parent, kid); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	payload := func(session string, extra map[string]any) string {
		o := map[string]any{"hook_event_name": "PreToolUse", "session_id": session, "turn_id": "t", "cwd": "/work/kid", "tool_name": "Bash", "tool_input": map[string]any{"command": "git push origin HEAD:dev"}}
		for k, v := range extra {
			o[k] = v
		}
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	bind := func(raw string) (bool, string, error) {
		var p struct {
			Session string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return ChildBinding(ctx, p.Session)
	}
	sub := payload(kid, map[string]any{"agent_id": spawned, "agent_type": "explorer"})
	if !harness.IsSubagentHookPayload(sub) {
		t.Fatal("the fixture payload is not a sub-agent payload")
	}
	if child, cwd, err := bind(sub); err != nil || !child || cwd != "/work/parent" {
		t.Errorf("the sub-agent payload of the relay child: child %v, parent_cwd %q, %v", child, cwd, err)
	}
	if child, cwd, err := bind(payload(kid, nil)); err != nil || !child || cwd != "/work/parent" {
		t.Errorf("the relay child's own payload: child %v, parent_cwd %q, %v", child, cwd, err)
	}
	for what, raw := range map[string]string{
		"the parent":                     payload(parent, nil),
		"a sub-agent of the parent":      payload(parent, map[string]any{"agent_id": spawned, "agent_type": "worker"}),
		"an integrator":                  payload(integrator, nil),
		"a sub-agent of the integrator":  payload(integrator, map[string]any{"agent_id": spawned, "agent_type": "worker"}),
		"a thread known only by its id":  payload(spawned, nil),
		"a session the relay never knew": payload("01a13570-0000-7000-8000-00000000ffff", nil),
	} {
		if child, cwd, err := bind(raw); err != nil || child || cwd != "" {
			t.Errorf("%s: child %v, parent_cwd %q, %v", what, child, cwd, err)
		}
	}
}
