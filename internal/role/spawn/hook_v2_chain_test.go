package spawn

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1260: the whole V2 flow of a managed review, through the real hook and the real checked boundary on a temporary store and
// a temporary native database. The task of a native V2 spawn is host ciphertext, so the dispatch marker the claim handed out is
// invisible to the hook; the hook ties the call to the session's one claimed attempt, and the created report by the agent path
// the call returned ends in a verified receipt that lets the review complete.

const (
	v2ChainSession = "rec-s1"
	v2ChainChild   = "01a128ac-1eb7-7910-9bb9-180b3dd53765"
	v2ChainOld     = "01a128ac-0000-7910-9bb9-180b3dd53765"
	v2ChainPath    = "/root/independent_review"
)

func v2ChainLedger(t *testing.T, ws, dispatch, kind string) string {
	t.Helper()
	file := filepath.Join(ws, ".crw", "dispatches", v2ChainSession, dispatch+".json")
	spawnHookMust(t, os.MkdirAll(filepath.Dir(file), 0o755))
	spawnHookMust(t, os.WriteFile(file, []byte(`{"version":1,"sessionId":"`+v2ChainSession+`","id":"`+dispatch+`","role":"`+kind+`","candidates":[{"model":"gpt-6.1-sol","effort":"high"}],"attempts":[{"id":"att-1","candidate":{"model":"gpt-6.1-sol","effort":"high"},"claimed":true,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"claimed","reconciliation":null,"spawnIssued":false,"toolUseId":null}],"status":"active"}`), 0o644))
	return file
}

// v2ChainNative writes the host's thread database (the codex schema of the columns the boundary reads) with one partial-fork child
// per id, and the parent's rollout in the form the host writes a V2 spawn: the function call, its SubAgentActivity event and the
// output that holds the agent path.
func v2ChainNative(t *testing.T, codex string, calls map[string]string) {
	t.Helper()
	spawnHookMust(t, os.RemoveAll(codex))
	spawnHookMust(t, os.MkdirAll(codex, 0o755))
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(codex, "state_5.sqlite")}).String())
	spawnHookMust(t, err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT, source TEXT, archived INTEGER, first_user_message TEXT, model TEXT, reasoning_effort TEXT, agent_path TEXT)")
	spawnHookMust(t, err)
	_, err = db.Exec("CREATE TABLE thread_spawn_edges (parent_thread_id TEXT NOT NULL, child_thread_id TEXT NOT NULL PRIMARY KEY, status TEXT NOT NULL)")
	spawnHookMust(t, err)
	line := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	lines := []string{line(map[string]any{"type": "session_meta", "payload": map[string]any{"id": v2ChainSession}})}
	for call, child := range calls {
		source, _ := json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": v2ChainSession, "depth": 1, "agent_path": v2ChainPath}}})
		rollout := filepath.Join(codex, "rollout-"+child+".jsonl")
		spawnHookMust(t, os.WriteFile(rollout, []byte(line(map[string]any{"type": "session_meta", "payload": map[string]any{"id": child, "forked_from_id": v2ChainSession}})+"\n"), 0o600))
		_, err = db.Exec("INSERT INTO threads VALUES (?,?,?,0,?,?,?,?)", child, rollout, string(source), "You are the orchestrator thread of a benchmark run.", "gpt-6.1-sol", "high", v2ChainPath)
		spawnHookMust(t, err)
		_, err = db.Exec("INSERT INTO thread_spawn_edges VALUES (?,?,'open')", v2ChainSession, child)
		spawnHookMust(t, err)
		lines = append(lines,
			line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": call, "arguments": "{}"}}),
			line(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": v2ChainSession,
				"item": map[string]any{"type": "SubAgentActivity", "id": call, "kind": "started", "agent_thread_id": child, "agent_path": v2ChainPath}}}),
			line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": call, "output": `{"task_name":"` + v2ChainPath + `"}`}}))
	}
	parent := filepath.Join(codex, "rollout-parent.jsonl")
	spawnHookMust(t, os.WriteFile(parent, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	_, err = db.Exec("INSERT INTO threads VALUES (?,?,?,0,?,?,?,?)", v2ChainSession, parent, "cli", "", "", "", "")
	spawnHookMust(t, err)
}

func v2ChainStdin(ws, tool, input string) string { return spawnReapplyPayload(ws, tool, input) }

func v2ChainRead(t *testing.T, file string) (issued bool, tool string) {
	t.Helper()
	var d struct {
		Attempts []struct {
			SpawnIssued bool    `json:"spawnIssued"`
			ToolUseID   *string `json:"toolUseId"`
		} `json:"attempts"`
	}
	data, err := os.ReadFile(file)
	spawnHookMust(t, err)
	spawnHookMust(t, json.Unmarshal(data, &d))
	a := d.Attempts[len(d.Attempts)-1]
	if a.ToolUseID != nil {
		tool = *a.ToolUseID
	}
	return a.SpawnIssued, tool
}

func TestSpawnHookV2CiphertextSpawnIssuesTheSessionsClaimedAttempt(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	for _, fork := range []string{`"none"`, `"2"`} {
		t.Run("fork_turns "+fork, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			file := v2ChainLedger(t, rig.ws, "review-1", "reviewer")
			codex, _ := rig.env("CODEX_HOME")
			input := `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":` + fork + `,"message":` + cipher + `}`
			answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "native-1", input), rig.env)
			if !strings.Contains(answer, `"permissionDecision":"allow"`) {
				t.Fatalf("hook = %q", answer)
			}
			if issued, tool := v2ChainRead(t, file); !issued || tool != "native-1" {
				t.Fatalf("issued = %v to %q", issued, tool)
			}
			if !strings.Contains(spawnReapplyUpdated(t, answer, input), cipher) {
				t.Fatal("the task ciphertext was changed")
			}
			// The same event delivered again is the same call.
			if again := RunSpawnAttachHook(v2ChainStdin(rig.ws, "native-1", input), rig.env); !strings.Contains(again, `"permissionDecision":"allow"`) {
				t.Fatalf("redelivered hook = %q", again)
			}

			v2ChainNative(t, codex, map[string]string{"native-1": v2ChainChild})
			created := map[string]any{"action": "report", "sessionId": v2ChainSession, "dispatchId": "review-1", "attemptId": "att-1", "outcome": "created", "agentId": v2ChainPath}
			out, err := role.CheckedDispatch(context.Background(), rig.ws, created, rig.env, nil)
			spawnHookMust(t, err)
			r := out.Attempts[0].Receipt
			if r == nil || r.Correlation != "spawn-result" || !r.Issuance.Recorded || r.Child.AgentID != v2ChainChild || r.Host.Model == nil || *r.Host.Model != "gpt-6.1-sol" {
				t.Fatalf("receipt = %+v", r)
			}
			created["outcome"] = "complete"
			if done, err := role.CheckedDispatch(context.Background(), rig.ws, created, rig.env, nil); err != nil || done.Action != "complete" {
				t.Fatalf("complete = %q %v", done.Action, err)
			}
		})
	}
}

// The call is tied only when the session's records leave one choice, and never for what is not a managed child.
func TestSpawnHookV2CiphertextSpawnLeavesAnUndecidedCallUnissued(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	reviewer := `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"2","message":` + cipher + `}`
	for _, tc := range []struct {
		name   string
		extra  func(t *testing.T, ws string) // more records of the session
		input  string
		reader string // the dispatch whose attempt must stay unissued
	}{
		{"two claimed attempts", func(t *testing.T, ws string) { v2ChainLedger(t, ws, "review-2", "reviewer") }, reviewer, "review-1"},
		{"a full-history fork", nil, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"all","message":` + cipher + `}`, "review-1"},
		{"another role's call", nil, `{"task_name":"x","agent_type":"architect","fork_turns":"2","message":` + cipher + `}`, "review-1"},
		{"a record that cannot be read", func(t *testing.T, ws string) {
			spawnHookMust(t, os.WriteFile(filepath.Join(ws, ".crw", "dispatches", v2ChainSession, "broken.json"), []byte("{"), 0o644))
		}, reviewer, "review-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			file := v2ChainLedger(t, rig.ws, "review-1", "reviewer")
			if tc.extra != nil {
				tc.extra(t, rig.ws)
			}
			before, err := os.ReadFile(file)
			spawnHookMust(t, err)
			if answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "native-1", tc.input), rig.env); strings.Contains(answer, `"permissionDecision":"deny"`) {
				t.Fatalf("an undecided call was denied: %q", answer)
			}
			if after, _ := os.ReadFile(file); string(after) != string(before) {
				t.Fatal("the attempt was issued")
			}
		})
	}
}
