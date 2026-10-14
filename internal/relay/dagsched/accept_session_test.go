package dagsched

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-1331 (CRW-1316 §4): the rows of a session call, as the flow journal stores them: prepared (backend session), step_published,
// thread_started of each attempt (the subagent's host-read pair, via session, its attempt, and the pairs of earlier attempts that
// differ as prior_pairs), turn_started and the terminal row. Agent acceptance reads the pair of a call from its newest
// thread_started row, and the pairs of an author call's earlier attempts - whose edits stay in the worktree and the head - from
// prior_pairs.

// sessionAttempt is one attempt of a session call: the pair its subagent was bound with.
type sessionAttempt struct{ model, effort string }

// recordSession writes the rows of a session call whose attempts were bound with the pairs given, the last one completing.
func (k *agentKit) recordSession(role, node, head, verdict, criteria string, attempts ...sessionAttempt) string {
	k.t.Helper()
	k.calls++
	id := fmt.Sprintf("call-%d", k.calls)
	run := "run-1"
	if k.count("SELECT COUNT(*) FROM flow_runs WHERE run_id = ?", run) == 0 {
		k.exec("INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			run, "project-lane", dig("script"), "{}", dig("args"), "g", 1, "parent", "nonce", "", k.clock())
	}
	execution := ""
	if k.count("SELECT COUNT(*) FROM dag_agent_executions WHERE plan_id = 'g' AND node_id = ?", node) > 0 {
		execution = k.execution(node)
	}
	seq := 0
	row := func(event, thread, turn string, result any, detail map[string]any) {
		seq++
		raw, _ := json.Marshal(detail)
		k.exec("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, commit_sha, thread_id, turn_id, plan_id, node_id, execution_id, result_bytes, detail_json, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			id, seq, run, role+":"+node, 0, "agent", role, event, head, thread, turn, "g", node, execution, result, string(raw), k.clock())
	}
	row("prepared", "", "", nil, map[string]any{"role": role, "node": node, "backend": "session", "model": attempts[len(attempts)-1].model, "effort": attempts[len(attempts)-1].effort})
	var prior []map[string]any
	thread, turn := "", ""
	for n, a := range attempts {
		attempt := n + 1
		op := fmt.Sprintf("%s/t1", id)
		row("step_published", "", "", nil, map[string]any{"k": 1, "op": op, "marker": fmt.Sprintf("%s-a%d", op, attempt), "attempt": attempt, "action": "spawn"})
		thread, turn = fmt.Sprintf("agent-%s-a%d", id, attempt), fmt.Sprintf("turn-%s-a%d", id, attempt)
		ts := map[string]any{"thread_id": thread, "model": a.model, "effort": a.effort, "cwd": "/w/" + node, "host_cwd": "/orch", "via": "session", "attempt": attempt}
		if len(prior) > 0 {
			ts["prior_pairs"] = prior
		}
		row("thread_started", thread, "", nil, ts)
		row("turn_started", thread, turn, nil, map[string]any{"k": 1, "op": op, "turn_id": turn, "attempt": attempt, "via": "session"})
		if n < len(attempts)-1 {
			row("session_attempt_lost", thread, turn, nil, map[string]any{"attempt": attempt, "agent_id": thread, "reason": "the thread is notLoaded"})
			seen := false
			for _, p := range prior {
				if p["model"] == a.model && p["effort"] == a.effort {
					seen = true
				}
			}
			if !seen && (a.model != attempts[len(attempts)-1].model || a.effort != attempts[len(attempts)-1].effort) {
				prior = append(prior, map[string]any{"model": a.model, "effort": a.effort})
			}
		}
	}
	var result any
	detail := map[string]any{}
	if verdict != "" {
		raw, _ := json.Marshal(map[string]any{"verdict": verdict, "summary": "recorded by the test"})
		result = string(raw)
		detail["criteria_digest"] = criteria
	}
	row("completed", thread, turn, result, detail)
	return id
}

// A node whose author and reviewer ran as session calls is accepted on their rows: the reviewer's pair is the one its subagent
// was bound with, read from the newest thread_started row of the call.
func TestAgentAcceptOnSessionRows(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("plain"), one("lost"), one("lostreviewer"))
	criteria := releaseCriteriaDigest()

	k.recordSession("executor", "plain", k.heads["plain"], "", "", sessionAttempt{authorModel, "high"})
	review := k.recordSession("reviewer", "plain", k.heads["plain"], "verified", criteria, sessionAttempt{reviewerModel, "high"})
	res, err := k.accept("plain")
	if err != nil {
		t.Fatalf("accept on session rows: %v", err)
	}
	row, found, err := loadAgentAcceptanceByID(t.Context(), k.s.Q(t.Context()), res.AcceptanceID)
	if err != nil || !found || row.ReviewerCallID != review || row.ReviewerThreadID != "agent-"+review+"-a1" || row.ReviewerModel != reviewerModel ||
		row.AuthorPairsDigest != authorPairsDigest([]agentPair{{authorModel, "high"}}) {
		t.Fatalf("the acceptance row %+v (%v %v)", row, found, err)
	}

	// an author whose first attempt ran on the reviewer's family and was lost: its edits stay in the worktree, so its pair is an
	// author pair and the reviewer is refused
	k.recordSession("executor", "lost", k.heads["lost"], "", "", sessionAttempt{reviewerModel, "high"}, sessionAttempt{authorModel, "high"})
	k.recordSession("reviewer", "lost", k.heads["lost"], "verified", criteria, sessionAttempt{reviewerModel, "high"})
	_, err = k.accept("lost")
	wantAgentRefusal(t, "an author attempt of the reviewer's family", err, contract.RefusalDispositionConflict, IndependenceRefused)

	// a reviewer whose first attempt was lost is judged on the pair of the attempt that answered
	k.recordSession("executor", "lostreviewer", k.heads["lostreviewer"], "", "", sessionAttempt{authorModel, "high"})
	k.recordSession("reviewer", "lostreviewer", k.heads["lostreviewer"], "verified", criteria, sessionAttempt{reviewerModel, "low"}, sessionAttempt{reviewerModel, "high"})
	res, err = k.accept("lostreviewer")
	if err != nil {
		t.Fatalf("a reviewer with a lost attempt: %v", err)
	}
	row, _, _ = loadAgentAcceptanceByID(t.Context(), k.s.Q(t.Context()), res.AcceptanceID)
	if row.ReviewerEffort != "high" {
		t.Errorf("the reviewer pair %s/%s, want the answering attempt's", row.ReviewerModel, row.ReviewerEffort)
	}
}
