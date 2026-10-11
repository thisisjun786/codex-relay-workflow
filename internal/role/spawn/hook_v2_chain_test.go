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
// invisible to the hook; the call names its dispatch by its plaintext task_name, the hook issues that dispatch's claimed attempt,
// and the created report by the agent path the call returned ends in a verified receipt that lets the review complete.

const (
	v2ChainSession = "rec-s1"
	v2ChainChild   = "01a128ac-1eb7-7910-9bb9-180b3dd53765"
	v2ChainOld     = "01a128ac-0000-7910-9bb9-180b3dd53765"
	v2ChainPath    = "/root/independent_review"
)

// v2ChainLedger writes a dispatch of the session whose one attempt was claimed with v2TaskName:true, the claim's opt-in to being
// named by a V2 call's task_name.
func v2ChainLedger(t *testing.T, ws, dispatch, kind string) string {
	t.Helper()
	return v2ChainLedgerOf(t, ws, dispatch, kind, `,"v2TaskName":true`)
}

// v2ChainLedgerOf writes the dispatch with extra appended to its attempt's members ("" for a claim without the opt-in).
func v2ChainLedgerOf(t *testing.T, ws, dispatch, kind, extra string) string {
	t.Helper()
	file := filepath.Join(ws, ".crw", "dispatches", v2ChainSession, dispatch+".json")
	spawnHookMust(t, os.MkdirAll(filepath.Dir(file), 0o755))
	spawnHookMust(t, os.WriteFile(file, []byte(`{"version":1,"sessionId":"`+v2ChainSession+`","id":"`+dispatch+`","role":"`+kind+`","candidates":[{"model":"gpt-6.1-sol","effort":"high"}],"attempts":[{"id":"att-1","candidate":{"model":"gpt-6.1-sol","effort":"high"},"claimed":true,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"claimed","reconciliation":null,"spawnIssued":false,"toolUseId":null`+extra+`}],"status":"active"}`), 0o644))
	return file
}

// v2ChainNative writes the host's thread database (the codex schema of the columns the boundary reads) with one child per id, and
// the parent's rollout in the form the host writes a V2 spawn: the function call, its SubAgentActivity event and the output that
// holds the agent path. An empty first makes each child a partial fork of the session (its rollout names the session it was forked
// from and its first user message is the parent's); otherwise each child is a fresh thread whose first user message is first.
func v2ChainNative(t *testing.T, codex string, calls map[string]string, first string) {
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
		meta, message := map[string]any{"id": child, "forked_from_id": v2ChainSession}, "You are the orchestrator thread of a benchmark run."
		if first != "" {
			meta, message = map[string]any{"id": child}, first
		}
		spawnHookMust(t, os.WriteFile(rollout, []byte(line(map[string]any{"type": "session_meta", "payload": meta})+"\n"), 0o600))
		_, err = db.Exec("INSERT INTO threads VALUES (?,?,?,0,?,?,?,?)", child, rollout, string(source), message, "gpt-6.1-sol", "high", v2ChainPath)
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

func TestSpawnHookV2CiphertextSpawnIssuesTheAttemptItsTaskNameNames(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	for _, tc := range []struct{ fork, first string }{
		{`"none"`, "[CRW-DISPATCH:independent_review:att-1]\nTASK: review the change"}, // a fresh child: its first message is the task
		{`"2"`, ""}, // a partial fork: its first message is the parent's
	} {
		fork := tc.fork
		t.Run("fork_turns "+fork, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			file := v2ChainLedger(t, rig.ws, "independent_review", "reviewer")
			v2ChainLedger(t, rig.ws, "review-2", "reviewer") // another claimed dispatch of the session, which the call does not name
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

			created := map[string]any{"action": "report", "sessionId": v2ChainSession, "dispatchId": "independent_review", "attemptId": "att-1", "outcome": "created", "agentId": v2ChainPath}
			if tc.first != "" {
				// The fresh child's first message is checked for the attempt's marker: one without it is refused, though the issued
				// call's result names it.
				v2ChainNative(t, codex, map[string]string{"native-1": v2ChainChild}, "TASK: review the change")
				if _, err := role.CheckedDispatch(context.Background(), rig.ws, created, rig.env, nil); err == nil || !strings.Contains(err.Error(), "carries no marker of this attempt") {
					t.Fatalf("a fresh child without the marker was accepted: %v", err)
				}
			}
			v2ChainNative(t, codex, map[string]string{"native-1": v2ChainChild}, tc.first)
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

// The call is tied only to the dispatch its task_name names while that dispatch waits for its spawn, and never for what is not a
// managed child.
func TestSpawnHookV2CiphertextSpawnLeavesAnUndecidedCallUnissued(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	for _, tc := range []struct {
		name   string
		extra  func(t *testing.T, ws string) // more records of the session
		input  string
		reader string // the dispatch whose attempt must stay unissued
	}{
		{"the one claimed attempt, not named", nil, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"2","message":` + cipher + `}`, "review-1"},
		{"a full-history fork", nil, `{"task_name":"review-1","agent_type":"reviewer","fork_turns":"all","message":` + cipher + `}`, "review-1"},
		{"another role's call", nil, `{"task_name":"review-1","agent_type":"architect","fork_turns":"2","message":` + cipher + `}`, "review-1"},
		{"a record that cannot be read", func(t *testing.T, ws string) {
			spawnHookMust(t, os.WriteFile(filepath.Join(ws, ".crw", "dispatches", v2ChainSession, "broken.json"), []byte("{"), 0o644))
		}, `{"task_name":"broken","agent_type":"reviewer","fork_turns":"2","message":` + cipher + `}`, "review-1"},
		{"a dispatch no longer waiting", func(t *testing.T, ws string) {
			file := filepath.Join(ws, ".crw", "dispatches", v2ChainSession, "review-1.json")
			data, err := os.ReadFile(file)
			spawnHookMust(t, err)
			spawnHookMust(t, os.WriteFile(file, []byte(strings.Replace(string(data), `"status":"active"`, `"status":"complete"`, 1)), 0o644))
		}, `{"task_name":"review-1","agent_type":"reviewer","fork_turns":"2","message":` + cipher + `}`, "review-1"},
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

// A ciphertext V2 call that does not name the claimed dispatch by its task_name is not the attempt's call, whatever its
// agent_type: it is not issued, so its child (here a partial fork the call really made) cannot be recorded without
// reconciliation and never satisfies the independent review (the guess the session's one claimed attempt once was).
func TestSpawnHookV2UnnamedCiphertextCallCannotSatisfyTheReview(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	for _, tc := range []struct{ name, input string }{
		{"a default-type helper", `{"task_name":"independent_review","agent_type":"default","fork_turns":"2","message":` + cipher + `}`},
		{"a reviewer-type helper", `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"2","message":` + cipher + `}`},
		{"no agent_type", `{"task_name":"independent_review","fork_turns":"2","message":` + cipher + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			file := v2ChainLedger(t, rig.ws, "review-1", "reviewer")
			answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "helper-call", tc.input), rig.env)
			if !strings.Contains(answer, `"permissionDecision":"allow"`) {
				t.Fatalf("hook = %q", answer)
			}
			if issued, _ := v2ChainRead(t, file); issued {
				t.Fatal("an unnamed call was issued the claimed attempt")
			}
			codex, _ := rig.env("CODEX_HOME")
			v2ChainNative(t, codex, map[string]string{"helper-call": v2ChainChild}, "")
			report := map[string]any{"action": "report", "sessionId": v2ChainSession, "dispatchId": "review-1", "attemptId": "att-1", "outcome": "created", "agentId": v2ChainPath}
			if _, err := role.CheckedDispatch(context.Background(), rig.ws, report, rig.env, nil); err == nil {
				t.Fatal("the unrelated child was recorded without reconciliation")
			}
			report["reconciliation"] = "spawned while the dispatch was claimed"
			out, err := role.CheckedDispatch(context.Background(), rig.ws, report, rig.env, nil)
			spawnHookMust(t, err)
			if r := out.Attempts[0].Receipt; r == nil || r.Correlation == "spawn-result" {
				t.Fatalf("receipt = %+v", r)
			}
			delete(report, "reconciliation")
			report["outcome"] = "complete"
			if done, err := role.CheckedDispatch(context.Background(), rig.ws, report, rig.env, nil); err == nil && done.Action == "complete" {
				t.Fatal("the unrelated child satisfied the independent review")
			}
		})
	}
}

// Parallel V2 calls that do not name a dispatch are both assembled before either is answered: neither is managed, so neither is
// denied and the claimed attempt stays unissued.
func TestSpawnHookV2ParallelUnnamedCallsAreNotDenied(t *testing.T) {
	c := spawnHookCase{}
	c.Env.Store = &[]string{`{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`}[0]
	rig := spawnHookNewRig(t, nil, c)
	file := v2ChainLedger(t, rig.ws, "review-1", "reviewer")
	before, err := os.ReadFile(file)
	spawnHookMust(t, err)
	input := `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"2","message":` + strconv.Quote(spawnV2Fernet()) + `}`
	assemble := func(call string) spawnHookAssembly {
		payload, ok := spawnHookRouteLoad(v2ChainStdin(rig.ws, call, input))
		if !ok {
			t.Fatal("payload")
		}
		a, deny, stop := spawnHookAssembleWith(spawnHookView(payload), rig.env, &spawnHookCommit{})
		if stop {
			t.Fatalf("assemble %s stopped: %q", call, deny)
		}
		return a
	}
	one, two := assemble("native-1"), assemble("native-2")
	for _, a := range []spawnHookAssembly{one, two} {
		if answer := spawnHookRoute(a, rig.env); !strings.Contains(answer, `"permissionDecision":"allow"`) {
			t.Fatalf("an unnamed parallel call was not allowed: %q", answer)
		}
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("the attempt was issued")
	}
}

// Two V2 calls that both name the claimed dispatch are two calls carrying its marker: the first is issued the attempt and the
// second is refused, as a second marker-carrying call is.
func TestSpawnHookV2TwoCallsNamingTheDispatch(t *testing.T) {
	c := spawnHookCase{}
	c.Env.Store = &[]string{`{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`}[0]
	rig := spawnHookNewRig(t, nil, c)
	file := v2ChainLedger(t, rig.ws, "review-1", "reviewer")
	input := `{"task_name":"review-1","agent_type":"reviewer","fork_turns":"2","message":` + strconv.Quote(spawnV2Fernet()) + `}`
	assemble := func(call string) spawnHookAssembly {
		payload, ok := spawnHookRouteLoad(v2ChainStdin(rig.ws, call, input))
		if !ok {
			t.Fatal("payload")
		}
		a, deny, stop := spawnHookAssembleWith(spawnHookView(payload), rig.env, &spawnHookCommit{})
		if stop || a.managed == nil {
			t.Fatalf("assemble %s: stop=%v deny=%q", call, stop, deny)
		}
		return a
	}
	one, two := assemble("native-1"), assemble("native-2")
	if answer := spawnHookRoute(one, rig.env); !strings.Contains(answer, `"permissionDecision":"allow"`) {
		t.Fatalf("first = %q", answer)
	}
	if issued, call := v2ChainRead(t, file); !issued || call != "native-1" {
		t.Fatalf("issued = %v to %q", issued, call)
	}
	if answer := spawnHookRoute(two, rig.env); !strings.Contains(answer, "attempt already issued to another native call") {
		t.Fatalf("second = %q", answer)
	}
}
