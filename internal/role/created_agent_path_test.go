package role

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// CRW-1260: multi-agent V2's spawn_agent returns the agent's path ("/root/<task_name>") and no thread id, so a created report
// names the child by that path. The checked boundary resolves it to the thread id in the native database (threads.agent_path
// of a thread_spawn_edges child of the session) and from there on works on the thread id, as it does for V1.

// agentPathRow is one thread of a native database as codex 0.154 writes a spawned child, with the columns the boundary reads.
type agentPathRow struct {
	id, parent, path, first, model, effort, role string
	// forkedFrom, when set, gives the child a rollout whose session_meta names the thread it was forked from (a partial fork
	// copies the parent's history, so the child's first user message is the parent's).
	forkedFrom string
	noEdge     bool
}

// agentPathNative seeds a native database with the codex schema of the columns the boundary reads, a row per child and its
// thread_spawn_edges row, and returns the environment that reads it. columns drops the agent_path column when false.
func agentPathNative(t *testing.T, env host.LookupEnv, agentPathColumn bool, rows ...agentPathRow) host.LookupEnv {
	t.Helper()
	native := t.TempDir()
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	columns := "id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0, first_user_message TEXT NOT NULL DEFAULT '', model TEXT, reasoning_effort TEXT, agent_role TEXT"
	if agentPathColumn {
		columns += ", agent_path TEXT"
	}
	_, err := db.Exec("CREATE TABLE threads (" + columns + ")")
	check(t, err)
	_, err = db.Exec("CREATE TABLE thread_spawn_edges (parent_thread_id TEXT NOT NULL, child_thread_id TEXT NOT NULL PRIMARY KEY, status TEXT NOT NULL)")
	check(t, err)
	for _, r := range rows {
		spawn := map[string]any{"parent_thread_id": r.parent, "depth": 1, "agent_nickname": "Inspector"}
		if r.path != "" {
			spawn["agent_path"] = r.path
		}
		source := string(must(json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": spawn}})))
		rollout := ""
		if r.forkedFrom != "" {
			rollout = filepath.Join(native, "rollout-"+r.id+".jsonl")
			meta := map[string]any{"timestamp": "2026-10-11T00:00:00Z", "type": "session_meta", "payload": map[string]any{"id": r.id, "forked_from_id": r.forkedFrom, "thread_source": "subagent"}}
			check(t, os.WriteFile(rollout, append(must(json.Marshal(meta)), '\n'), 0o600))
		}
		if agentPathColumn {
			_, err = db.Exec("INSERT INTO threads (id, rollout_path, source, first_user_message, model, reasoning_effort, agent_role, agent_path) VALUES (?,?,?,?,?,?,?,?)",
				r.id, rollout, source, r.first, r.model, r.effort, r.role, r.path)
		} else {
			_, err = db.Exec("INSERT INTO threads (id, rollout_path, source, first_user_message, model, reasoning_effort, agent_role) VALUES (?,?,?,?,?,?,?)",
				r.id, rollout, source, r.first, r.model, r.effort, r.role)
		}
		check(t, err)
		if !r.noEdge {
			_, err = db.Exec("INSERT INTO thread_spawn_edges VALUES (?,?,'open')", r.parent, r.id)
			check(t, err)
		}
	}
	return func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return env(k)
	}
}

const agentPathReviewer = "/root/independent_review"

// agentPathV2Call is one V2 spawn_agent call as the host writes it to the parent's rollout (the study's prep round 2 rollouts): a
// plain function_call whose arguments hold the task as ciphertext, the SubAgentActivity event the host writes once the thread
// exists (the only line that names the thread id), and the function_call_output that holds the agent path alone.
type agentPathV2Call struct{ call, child, path string }

// agentPathParentV2 gives env's native database a row for session-test whose rollout holds the V2 calls as the host writes them.
func agentPathParentV2(t *testing.T, env host.LookupEnv, calls ...agentPathV2Call) {
	t.Helper()
	native, _ := env("CODEX_HOME")
	rollout := filepath.Join(native, "rollout-session-test.jsonl")
	line := func(v map[string]any) string { return string(must(json.Marshal(v))) }
	lines := []string{line(map[string]any{"type": "session_meta", "payload": map[string]any{"id": "session-test"}})}
	for _, c := range calls {
		args := string(must(json.Marshal(map[string]any{"task_name": "independent_review", "fork_turns": "2", "message": "gAAAAABqyuyTEhaESnb0NxWN91zwF9OdePSHNsx8TuFTz4caREl98DDdcb9EuHhV2ArJTb4U8PGrSa_otZMnRDgtSrBX3YoZ7hjYjQnrtdveY3eHlCGDI-FFMvflhCi9yBlwXNhLLypb8RyRd_nKrX79uvPtrXX4LiuXfE7eU1w54Tyh_LhTfcExI9I5jSjsHIjaj0VYkx9sUSL_6HtFSWoFn7INcUASQ19rq_a-dLjFTDfl7izpoZUHPyeMQmPSmarZ-rh1pC0LtNodKL7pipICd-uz1dZxUI2iaEAR-t-NmYK-iV3VslCk_Z4a72cLYge36C6hUTZUjEZIesbf2jwb2WCSU7QlvTU5ec0kJvME04Ni2QBxiO84UfFafON9-_pBHvw"})))
		lines = append(lines,
			line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "spawn_agent", "namespace": "collaboration", "arguments": args, "call_id": c.call}}),
			line(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": "session-test", "turn_id": "turn-1",
				"item": map[string]any{"type": "SubAgentActivity", "id": c.call, "kind": "started", "agent_thread_id": c.child, "agent_path": c.path}}}),
			line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": c.call, "output": `{"task_name":"` + c.path + `"}`}}))
	}
	check(t, os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	_, err := db.Exec("INSERT INTO threads (id, rollout_path, source, archived, first_user_message, model, reasoning_effort) VALUES (?,?,?,0,?,?,?)", "session-test", rollout, "cli", "", "", "")
	check(t, err)
}

// agentPathReviewerEnv is an environment whose reviewer role is the pair gpt-6.1-sol high, the pair a served child is compared with.
func agentPathReviewerEnv(t *testing.T) host.LookupEnv {
	t.Helper()
	env, dir := home(t)
	writeStore(t, dir, `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`)
	return env
}

// V1 names the child by its thread id and V2 by its path; fresh and partial-fork children of both end in the same verified
// receipt, whose served pair is the pair the role asked for, and the review completes by either name.
func TestCreatedAgentPathReceiptMatchesThreadId(t *testing.T) {
	const inherited = "You are the orchestrator thread of a benchmark run. This turn is a PREPARATION turn."
	for _, tc := range []struct {
		name  string
		v2    bool
		fork  bool
		first func(marker string) string
	}{
		{"v1 fresh", false, false, func(m string) string { return "ROLE PROMPT\n" + m + "\nREVIEW" }},
		{"v1 partial fork", false, true, func(string) string { return inherited }},
		{"v2 fresh", true, false, func(string) string { return "" }},
		{"v2 fresh with the task as first message", true, false, func(m string) string { return m + "\nREVIEW" }},
		{"v2 partial fork", true, true, func(string) string { return inherited }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env := agentPathReviewerEnv(t)
			attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
			dispatchReceiptIssue(t, ws, marker, "call-1")
			row := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, first: tc.first(marker), model: "gpt-6.1-sol", effort: "high", role: "reviewer"}
			if tc.fork {
				row.forkedFrom = "session-test"
			}
			seen := agentPathNative(t, env, true, row)
			name := row.id
			if tc.v2 {
				name = agentPathReviewer
				agentPathParentV2(t, seen, agentPathV2Call{"call-1", row.id, agentPathReviewer})
			} else {
				dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{row.id}})
			}
			report := dispatchReceiptCreated("review-test", attempt, name)
			out, err := CheckedDispatch(context.Background(), ws, report, seen, nil)
			check(t, err)
			a := out.Attempts[0]
			if out.Action != "wait" || out.Reason != "" || a.AgentID == nil || *a.AgentID != row.id {
				t.Fatalf("created = %q %q agent %v", out.Action, out.Reason, a.AgentID)
			}
			r := a.Receipt
			if r == nil || r.Correlation != "spawn-result" || !r.Issuance.Recorded || r.Child.AgentID != row.id || r.Child.Parent != "session-test" || r.Child.Witness != "native-thread-database" {
				t.Fatalf("receipt = %+v", r)
			}
			if r.Host.Source != "native-thread-database" || r.Host.Model == nil || *r.Host.Model != "gpt-6.1-sol" || r.Host.Effort == nil || *r.Host.Effort != "high" ||
				a.Candidate.Model == nil || *a.Candidate.Model != *r.Host.Model || a.Candidate.Effort == nil || string(*a.Candidate.Effort) != *r.Host.Effort {
				t.Fatalf("served pair %+v is not the role's candidate %+v", r.Host, a.Candidate)
			}
			file := dispatchReceiptFile(ws, "review-test")
			before := must(os.ReadFile(file))
			if again, err := CheckedDispatch(context.Background(), ws, report, seen, nil); err != nil || again.Action != "wait" {
				t.Fatalf("replayed created = %q %v", again.Action, err)
			}
			if string(before) != string(must(os.ReadFile(file))) {
				t.Fatal("a replayed created report changed the record")
			}
			report["outcome"] = "complete"
			if done, err := CheckedDispatch(context.Background(), ws, report, seen, nil); err != nil || done.Action != "complete" {
				t.Fatalf("complete by %s = %q %v", name, done.Action, err)
			}
			if stored := must(dispatchRead(file, "session-test", "review-test")); *stored.Attempts[0].AgentID != row.id {
				t.Fatalf("the record holds %q, not the thread id", *stored.Attempts[0].AgentID)
			}
		})
	}
}

// A path the boundary cannot resolve to a child of this session is refused with its reason and the correction, and writes nothing.
func TestCreatedAgentPathRefusals(t *testing.T) {
	const child = "01a128af-384b-72e0-9cbe-e6f6cd6decb4"
	for _, tc := range []struct {
		name, agent, want string
		rows              []agentPathRow
		noPathColumn      bool
		noDatabase        bool
	}{
		{"no such child", "/root/other_task", "names no child of this session", []agentPathRow{{id: child, parent: "session-test", path: agentPathReviewer}}, false, false},
		{"another session's child", agentPathReviewer, "names no child of this session", []agentPathRow{{id: child, parent: "session-other", path: agentPathReviewer}}, false, false},
		{"not a spawn edge of the session", agentPathReviewer, "names no child of this session", []agentPathRow{{id: child, parent: "session-test", path: agentPathReviewer, noEdge: true}}, false, false},
		{"two children of the path", agentPathReviewer, "names more than one child", []agentPathRow{
			{id: child, parent: "session-test", path: agentPathReviewer}, {id: "01a128af-0000-72e0-9cbe-e6f6cd6decb5", parent: "session-test", path: agentPathReviewer}}, false, false},
		{"no agent_path column", agentPathReviewer, "records no agent paths", []agentPathRow{{id: child, parent: "session-test"}}, true, false},
		{"no database", agentPathReviewer, "could not resolve", nil, false, true},
		{"relative", "root/independent_review", "invalid agentId", nil, false, false},
		{"root only", "/", "invalid agentId", nil, false, false},
		{"empty segment", "/root//x", "invalid agentId", nil, false, false},
		{"trailing slash", "/root/x/", "invalid agentId", nil, false, false},
		{"dot segment", "/root/../x", "invalid agentId", nil, false, false},
		{"control", "/root/x\n", "invalid agentId", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env := agentPathReviewerEnv(t)
			attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
			dispatchReceiptIssue(t, ws, marker, "call-1")
			seen := agentPathNative(t, env, !tc.noPathColumn, tc.rows...)
			if tc.noDatabase {
				seen = func(k string) (string, bool) {
					if k == "CODEX_HOME" {
						return t.TempDir(), true
					}
					return env(k)
				}
			}
			file := dispatchReceiptFile(ws, "review-test")
			before := must(os.ReadFile(file))
			_, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, tc.agent), seen, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("created of %q = %v, want %q", tc.agent, err, tc.want)
			}
			if tc.want != "invalid agentId" && !strings.Contains(err.Error(), "copy agentId") {
				t.Fatalf("refusal lacks the correction: %v", err)
			}
			if string(before) != string(must(os.ReadFile(file))) {
				t.Fatal("a refused created report wrote state")
			}
		})
	}
}

// A task name the session used twice names several threads: the one carrying this attempt's marker is the child; without one
// the path is ambiguous and refused.
func TestCreatedAgentPathReusedNameIsNarrowedByMarker(t *testing.T) {
	ws := t.TempDir()
	env := agentPathReviewerEnv(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	dispatchReceiptIssue(t, ws, marker, "call-1")
	old := agentPathRow{id: "01a128af-0000-72e0-9cbe-e6f6cd6decb5", parent: "session-test", path: agentPathReviewer, first: "[CRW-DISPATCH:review-old:attempt-old]\nOLD"}
	mine := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, first: marker + "\nREVIEW"}
	seen := agentPathNative(t, env, true, old, mine)
	dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{mine.id}})
	out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, agentPathReviewer), seen, nil)
	check(t, err)
	if a := out.Attempts[0]; a.AgentID == nil || *a.AgentID != mine.id || a.Receipt == nil || a.Receipt.Correlation != "spawn-result" {
		t.Fatalf("created = %+v", a)
	}
}

// The failed and stopped reports name the recorded child by its path too.
func TestCreatedAgentPathFailedAndStoppedReports(t *testing.T) {
	for _, outcome := range []string{"failed", "stopped"} {
		t.Run(outcome, func(t *testing.T) {
			ws := t.TempDir()
			env := agentPathReviewerEnv(t)
			attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
			dispatchReceiptIssue(t, ws, marker, "call-1")
			row := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, model: "gpt-6.1-sol", effort: "high"}
			seen := agentPathNative(t, env, true, row)
			dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{row.id}})
			_, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, agentPathReviewer), seen, nil)
			check(t, err)
			report := dispatchReceiptCreated("review-test", attempt, agentPathReviewer)
			report["outcome"] = outcome
			report["executionState"] = "stopped"
			report["reconciliation"] = "the child ended"
			report["error"] = "insufficient_quota"
			out, err := CheckedDispatch(context.Background(), ws, report, seen, nil)
			if err != nil {
				t.Fatalf("%s by path: %v", outcome, err)
			}
			if out.Attempts[0].AgentID == nil || *out.Attempts[0].AgentID != row.id {
				t.Fatalf("recorded child = %v", out.Attempts[0].AgentID)
			}
		})
	}
}

// A child spawned without the hook (a full-history fork is never issued: the hook denies a managed full fork) is recorded
// through reconciliation, by its path as by its id, and cannot satisfy independent review.
func TestCreatedAgentPathFullForkRecordsOnlyByReconciliation(t *testing.T) {
	ws := t.TempDir()
	env := agentPathReviewerEnv(t)
	attempt, _ := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	row := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, first: "PREPARATION", forkedFrom: "session-test"}
	seen := agentPathNative(t, env, true, row)
	report := dispatchReceiptCreated("review-test", attempt, agentPathReviewer)
	if _, err := CheckedDispatch(context.Background(), ws, report, seen, nil); err == nil || !strings.Contains(err.Error(), "created requires this attempt's managed spawn issuance") {
		t.Fatalf("created of an unissued full fork = %v", err)
	}
	report["reconciliation"] = "full-history fork: the hook denies a managed one"
	out, err := CheckedDispatch(context.Background(), ws, report, seen, nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r == nil || r.Correlation != "unissued" || r.Child.AgentID != row.id || !strings.Contains(out.Reason, "cannot satisfy independent review") {
		t.Fatalf("receipt = %+v reason %q", r, out.Reason)
	}
}

// A first message without the attempt's marker is excused only for a child forked from this session, and the excuse does not
// tie it: without the host's result of the issued call it stays unverified, and a child forked from another thread, or not
// forked at all, is still refused.
func TestCreatedForkedChildFirstMessageExcuseIsNarrow(t *testing.T) {
	const inherited = "PREPARATION turn"
	ws := t.TempDir()
	env := agentPathReviewerEnv(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	dispatchReceiptIssue(t, ws, marker, "call-1")
	file := dispatchReceiptFile(ws, "review-test")
	before := must(os.ReadFile(file))
	for name, from := range map[string]string{"another thread": "session-other", "not forked": ""} {
		row := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, first: inherited, forkedFrom: from}
		seen := agentPathNative(t, env, true, row)
		dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{row.id}})
		if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, agentPathReviewer), seen, nil); err == nil || !strings.Contains(err.Error(), "carries no marker of this attempt") {
			t.Fatalf("created of a child %s = %v", name, err)
		}
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("a refused created report wrote state")
	}
	row := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, first: inherited, forkedFrom: "session-test"}
	out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, agentPathReviewer), agentPathNative(t, env, true, row), nil)
	check(t, err)
	if r := out.Attempts[0].Receipt; r == nil || r.Correlation != "unverified" || !strings.Contains(out.Reason, "cannot satisfy independent review") {
		t.Fatalf("created of a forked child without the call's result = %+v %q", out.Attempts[0].Receipt, out.Reason)
	}
}

// V2's host result of an issued call: another call's thread ties nothing, an activity of another session is not read, and a
// call with no activity (the function_call alone) stays unverified.
func TestCreatedAgentPathV2ResultOfTheIssuedCallOnly(t *testing.T) {
	const child = "01a128af-384b-72e0-9cbe-e6f6cd6decb4"
	const other = "01a128af-0000-72e0-9cbe-e6f6cd6decb5"
	for _, tc := range []struct {
		name  string
		calls []agentPathV2Call
		want  string // a refusal's text, or the receipt correlation
	}{
		{"the issued call made the child", []agentPathV2Call{{"call-1", child, agentPathReviewer}}, "spawn-result"},
		{"the issued call made another child", []agentPathV2Call{{"call-1", other, "/root/other"}, {"call-2", child, agentPathReviewer}}, "names another child"},
		{"no activity of the issued call", []agentPathV2Call{{"call-2", child, agentPathReviewer}}, "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env := agentPathReviewerEnv(t)
			attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
			dispatchReceiptIssue(t, ws, marker, "call-1")
			seen := agentPathNative(t, env, true, agentPathRow{id: child, parent: "session-test", path: agentPathReviewer, model: "gpt-6.1-sol", effort: "high"})
			agentPathParentV2(t, seen, tc.calls...)
			out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, child), seen, nil)
			if tc.want == "spawn-result" || tc.want == "unverified" {
				check(t, err)
				if r := out.Attempts[0].Receipt; r == nil || r.Correlation != tc.want {
					t.Fatalf("receipt = %+v", r)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("created = %v, want %q", err, tc.want)
			}
		})
	}
}

// Two partial-fork children of one task name both inherit the parent's first message, so the marker names neither: the issued
// call's result names the current one, and the reports after it (complete, failed, stopped) find it by the id the record holds.
func TestCreatedAgentPathReusedNameOfPartialForkChildren(t *testing.T) {
	const inherited = "You are the orchestrator thread of a benchmark run. This turn is a PREPARATION turn."
	old := agentPathRow{id: "01a128af-0000-72e0-9cbe-e6f6cd6decb5", parent: "session-test", path: agentPathReviewer, first: inherited, forkedFrom: "session-test", model: "gpt-6.1-sol", effort: "high"}
	mine := agentPathRow{id: "01a128af-384b-72e0-9cbe-e6f6cd6decb4", parent: "session-test", path: agentPathReviewer, first: inherited, forkedFrom: "session-test", model: "gpt-6.1-sol", effort: "high"}
	for _, outcome := range []string{"complete", "failed", "stopped"} {
		t.Run(outcome, func(t *testing.T) {
			ws := t.TempDir()
			env := agentPathReviewerEnv(t)
			attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
			dispatchReceiptIssue(t, ws, marker, "call-2")
			seen := agentPathNative(t, env, true, old, mine)
			agentPathParentV2(t, seen, agentPathV2Call{"call-1", old.id, agentPathReviewer}, agentPathV2Call{"call-2", mine.id, agentPathReviewer})
			out, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, agentPathReviewer), seen, nil)
			check(t, err)
			if a := out.Attempts[0]; a.AgentID == nil || *a.AgentID != mine.id || a.Receipt == nil || a.Receipt.Correlation != "spawn-result" {
				t.Fatalf("created = %+v", a)
			}
			report := dispatchReceiptCreated("review-test", attempt, agentPathReviewer)
			report["outcome"] = outcome
			if outcome != "complete" {
				report["executionState"] = "stopped"
				report["reconciliation"] = "the child ended"
				report["error"] = "insufficient_quota"
			}
			done, err := CheckedDispatch(context.Background(), ws, report, seen, nil)
			if err != nil {
				t.Fatalf("%s by path: %v", outcome, err)
			}
			if done.Attempts[0].AgentID == nil || *done.Attempts[0].AgentID != mine.id {
				t.Fatalf("recorded child = %v", done.Attempts[0].AgentID)
			}
		})
	}
	t.Run("without the call's result the path stays ambiguous", func(t *testing.T) {
		ws := t.TempDir()
		env := agentPathReviewerEnv(t)
		attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
		dispatchReceiptIssue(t, ws, marker, "call-2")
		seen := agentPathNative(t, env, true, old, mine)
		file := dispatchReceiptFile(ws, "review-test")
		before := must(os.ReadFile(file))
		if _, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, agentPathReviewer), seen, nil); err == nil || !strings.Contains(err.Error(), "names more than one child") {
			t.Fatalf("created = %v", err)
		}
		if string(before) != string(must(os.ReadFile(file))) {
			t.Fatal("a refused created report wrote state")
		}
	})
}
