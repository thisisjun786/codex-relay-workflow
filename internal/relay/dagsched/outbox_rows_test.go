package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1216 (R08): the plan summary carries one row per issue (stage, newest verdict with its revision, the findings' gist, the acceptance, the integration batch) for the nodes an agent runs and for the
// nodes a managed child runs (the oversized issues), and the subject digest of an entry is the digest of the progress together with those rows. The tests run the real scheduler over temporary stores:
// the journal rows of an agent's reviewer calls and the relationship's verdict rows are written the way their writers write them, and nothing is seeded into a summary table.

// rowsOf is the rows the summary of the plan states now.
func rowsOf(t *testing.T, k *agentKit) []IssueRow {
	t.Helper()
	ctx := context.Background()
	p, err := k.sched.ReadProgress(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	var subject summarySubjectOf
	if err := k.s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		subject, err = k.sched.summarySubject(txCtx, k.s.Q(txCtx), p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return subject.Rows
}

func rowOf(t *testing.T, rows []IssueRow, node string) IssueRow {
	t.Helper()
	for _, r := range rows {
		if r.NodeID == node {
			return r
		}
	}
	t.Fatalf("no row for node %s among %d rows", node, len(rows))
	return IssueRow{}
}

func enqueueG(t *testing.T, k *agentKit) SummaryEnqueued {
	t.Helper()
	res, err := k.sched.EnqueueSummary(context.Background(), "g", "parent", "doc-1", false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func statusG(t *testing.T, k *agentKit) SummaryStream {
	t.Helper()
	st, err := k.sched.SummaryStatus(context.Background(), "g", "doc-1", false)
	if err != nil || len(st.Streams) != 1 {
		t.Fatalf("status %+v %v", st, err)
	}
	return st.Streams[0]
}

// defect is a defect of a reviewer's verdict document (the VERDICT schema of the flow library).
func defect(ref, severity, kind, finding string) map[string]any {
	return map[string]any{"ref": ref, "kind": kind, "severity": severity, "introduced": true, "impact": "x", "inPromise": true, "file": "node.go", "line": 7, "finding": finding, "evidence_question": nil}
}

// reviewWith records a completed reviewer call of the node's current execution on its head with a verdict document that carries defects (journalCall carries a verdict only).
func (k *agentKit) reviewWith(node, verdict string, defects ...map[string]any) string {
	k.t.Helper()
	k.calls++
	id := "call-defects-" + node + "-" + string(rune('a'+k.calls))
	if k.count("SELECT COUNT(*) FROM flow_runs WHERE run_id = 'run-1'") == 0 {
		k.exec("INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			"run-1", "project-lane", dig("script"), "{}", dig("args"), "g", 1, "parent", "nonce", "", k.clock())
	}
	doc := map[string]any{"verdict": verdict, "criteria": []any{}, "defects": defects, "summary": "recorded by the test"}
	raw, _ := json.Marshal(doc)
	detail, _ := json.Marshal(map[string]string{"criteria_digest": releaseCriteriaDigest()})
	for seq, event := range []string{"prepared", "completed"} {
		var result any
		d := ""
		if event == "completed" {
			result, d = string(raw), string(detail)
		}
		k.exec("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, commit_sha, thread_id, turn_id, plan_id, node_id, execution_id, result_bytes, detail_json, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			id, seq+1, "run-1", "reviewer:"+node, 0, "agent", "reviewer", event, k.heads[node], "thread-"+id, "turn-"+id, "g", node, k.execution(node), result, d, k.clock())
	}
	return id
}

// ruleRelationship rules a further event of the relationship a managed child runs (the way supersedeReport adds a revision): the verdict row, its context and the findings.
func ruleRelationship(k *agentKit, node, label, verdict string, next int64, findings string) string {
	k.t.Helper()
	rid := "rel-g-" + node
	n, _ := nodeOf(k.snapshot("g"), node)
	now := k.clock()
	revision := dig("revision " + label + rid)
	event := "evt-" + label + "-" + rid
	k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, 1, ?, 'ready_for_review', 'child', 'child', 'turn-3', 'completed', '{}', 'final', ?, ?)", event, rid, revision, now, now)
	var nextGeneration any
	if next > 0 {
		nextGeneration = next
	}
	k.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?, '{}', ?, ?, 'verdict-turn-3', ?)", event, verdict, nextGeneration, now)
	var findingsText any
	if findings != "" {
		findingsText = findings
	}
	k.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, findings, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?, ?, '{}', ?, 'current', ?, ?, '{}', ?)",
		event, n.CriteriaSetDigest, findingsText, event, revision, now)
	return revision
}

// An agent node and a node a managed child runs both have a row in the summary, each with what the sync_outbox verdict summary held of the issue (the verdict, the revision, the child, the findings) and with
// the acceptance and the integration the progress document does not carry as text.
func TestSummaryRendersPerIssueRowForAgentAndOversized(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "r", files: fileOf("r"), relationship: true}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	entry := enqueueG(t, k).Entry
	rows := rowsOf(t, k)
	if len(rows) != 2 {
		t.Fatalf("%d rows for 2 issues: %+v", len(rows), rows)
	}
	agent, oversized := rowOf(t, rows, "a"), rowOf(t, rows, "r")
	if agent.Surface != ExecutionAgent || oversized.Surface != ExecutionRelationship {
		t.Fatalf("surfaces %q and %q", agent.Surface, oversized.Surface)
	}
	for _, r := range []IssueRow{agent, oversized} {
		if r.IssueKey == "" || r.Kind != "implementation" || r.Stage != StageAccepted || r.Verdict == nil || r.Verdict.Verdict != "verified" || r.AcceptanceID == "" {
			t.Fatalf("row %+v verdict %+v", r, r.Verdict)
		}
	}
	if agent.AcceptanceID != k.currentAcceptance("a").AcceptanceID || oversized.AcceptanceID != k.currentAcceptance("r").AcceptanceID {
		t.Fatalf("the acceptances of the rows are %s and %s", agent.AcceptanceID, oversized.AcceptanceID)
	}
	if agent.Verdict.Revision != k.heads["a"] || agent.Verdict.Generation != 1 || agent.Verdict.CriteriaSet != releaseCriteriaDigest() {
		t.Fatalf("the agent's verdict %+v", agent.Verdict)
	}
	if oversized.Verdict.ChildTaskID != "child-r" || oversized.Verdict.Revision == "" || oversized.Verdict.Generation != 1 {
		t.Fatalf("the oversized child's verdict %+v", oversized.Verdict)
	}
	if agent.IntegrationBatch != "" || oversized.IntegrationBatch != "" {
		t.Fatalf("a batch before any integration: %q, %q", agent.IntegrationBatch, oversized.IntegrationBatch)
	}
	// the text of the entry states the rows: the issue, its stage, the verdict with the revision and the acceptance
	text := entry.Summary
	if !strings.Contains(text, "\nIssues\n") {
		t.Fatalf("the summary has no rows:\n%s", text)
	}
	for _, want := range []string{agent.IssueKey, oversized.IssueKey, agent.AcceptanceID, oversized.AcceptanceID, "child-r", "verdict verified", k.heads["a"][:12], "agent", "relationship"} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, text)
		}
	}
	// after an integration both rows name the batch that merged them
	batch, err := k.integrate(t)
	if err != nil || len(batch.Merged) != 2 {
		t.Fatalf("integrate: %+v %v", batch, err)
	}
	after := rowsOf(t, k)
	for _, node := range []string{"a", "r"} {
		if got := rowOf(t, after, node).IntegrationBatch; got != batch.BatchID {
			t.Errorf("node %s names batch %q, want %q", node, got, batch.BatchID)
		}
	}
	if next := enqueueG(t, k); next.Replayed || !strings.Contains(next.Entry.Summary, batch.BatchID) {
		t.Fatalf("the integrated plan owes a new summary that names the batch: %+v", next)
	}
}

// The row of an issue carries its newest verdict: the status (verified or needs_changes), the revision it ruled, the findings and the digest of them; an issue that nobody ruled on has none.
func TestSummaryRowCarriesVerdictStatus(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "n", unreleased: true, files: fileOf("n")}, {name: "r", files: fileOf("r"), relationship: true}}
	k := newAgentKit(t, specs...)
	k.author("a", authorModel)
	k.reviewWith("a", "needs_changes", defect("D1", "P1", "correctness", "the loop never ends"), defect("D2", "P3", "maintainability", "name\nthe helper"))
	first := rowOf(t, rowsOf(t, k), "a")
	v := first.Verdict
	if v == nil || v.Verdict != "needs_changes" || v.Revision != k.heads["a"] || v.Generation != 1 || len(v.Findings) != 2 || v.FindingsDigest == "" {
		t.Fatalf("needs_changes row: %+v", v)
	}
	if !strings.Contains(v.Findings[0], "D1") || !strings.Contains(v.Findings[0], "P1") || !strings.Contains(v.Findings[0], "the loop never ends") || strings.Contains(v.Findings[1], "\n") {
		t.Fatalf("findings %q", v.Findings)
	}
	// the newest call decides: an approval after it leaves no findings
	k.reviewWith("a", "verified")
	second := rowOf(t, rowsOf(t, k), "a")
	if second.Verdict == nil || second.Verdict.Verdict != "verified" || len(second.Verdict.Findings) != 0 || second.Verdict.FindingsDigest == first.Verdict.FindingsDigest {
		t.Fatalf("verified row: %+v", second.Verdict)
	}
	// the node nobody released has no verdict, and says so in the text
	none := rowOf(t, rowsOf(t, k), "n")
	if none.Verdict != nil || none.Surface != "" || none.AcceptanceID != "" {
		t.Fatalf("an unreleased node: %+v", none)
	}
	if text := enqueueG(t, k).Entry.Summary; !strings.Contains(text, "no verdict") {
		t.Fatalf("the text does not say the unreleased issue has no verdict:\n%s", text)
	}
	// a managed child's ruling: needs_changes with findings and the correction that was queued, then the verified ruling that replaces it as the newest
	ruleRelationship(k, "r", "nc", "needs_changes", 2, `[{"id":"c1","verdict":"needs_changes","note":"the endpoint answers the wrong shape"},{"id":"c2","verdict":"verified"}]`)
	rel := rowOf(t, rowsOf(t, k), "r").Verdict
	if rel == nil || rel.Verdict != "needs_changes" || rel.NextGeneration != 2 || rel.ChildTaskID != "child-r" || rel.Generation != 1 || rel.Ruling != 1 || rel.CriteriaSet == "" || len(rel.Findings) != 2 {
		t.Fatalf("the managed child's needs_changes row: %+v", rel)
	}
	if rel.Findings[0] != "c1: needs_changes — the endpoint answers the wrong shape" || rel.Findings[1] != "c2: verified" {
		t.Fatalf("findings %q", rel.Findings)
	}
	text := enqueueG(t, k).Entry.Summary
	for _, want := range []string{"verdict needs_changes", "revision request queued under generation 2", "c1: needs_changes — the endpoint answers the wrong shape"} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, text)
		}
	}
	ruleRelationship(k, "r", "ok", "verified", 0, "")
	if got := rowOf(t, rowsOf(t, k), "r").Verdict; got == nil || got.Verdict != "verified" || got.NextGeneration != 0 || len(got.Findings) != 0 {
		t.Fatalf("the newest ruling is verified: %+v", got)
	}
}

// Only the project's registered parent drains the summary outbox: the task of the store seat (the supervisor's seat) is refused a claim, and the registered parent claims the same entry.
func TestSummaryClaimRefusesStoreSeat(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "a", files: fileOf("a")})
	now := k.clock()
	if err := storeseed.InsertScopeBinding(context.Background(), k.s, store.ScopeBindingsRow{BindingID: "bind-store", Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: "store-seat", HostID: "host",
		Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	entry := enqueueG(t, k).Entry
	ctx := context.Background()
	_, err := k.sched.ClaimSummary(ctx, entry.SummaryID, "store-seat")
	wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	if _, err := k.sched.EnqueueSummary(ctx, "g", "store-seat", "doc-1", false); err == nil {
		t.Fatal("the store seat enqueued a summary")
	} else {
		wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	}
	if got := entryBySeqK(k, 1); got.State != SummaryPending || got.ClaimedBy != "" {
		t.Fatalf("the refused claim changed the entry: %+v", got)
	}
	claim, err := k.sched.ClaimSummary(ctx, entry.SummaryID, "parent")
	if err != nil || claim.Entry.State != SummaryClaimed || claim.Entry.ClaimedBy != "parent" || claim.Token == "" {
		t.Fatalf("the registered parent's claim: %+v %v", claim, err)
	}
	for name, call := range map[string]func() error{
		"complete": func() error {
			_, err := k.sched.CompleteSummary(ctx, entry.SummaryID, "store-seat", claim.Token, "doc-1", "")
			return err
		},
		"fail": func() error {
			_, err := k.sched.FailSummary(ctx, entry.SummaryID, "store-seat", claim.Token, "x")
			return err
		},
		"retry": func() error { _, err := k.sched.RetrySummary(ctx, entry.SummaryID, "store-seat"); return err },
	} {
		if err := call(); err == nil || refusalReason(err) != string(contract.RefusalScopeRoleMismatch) {
			t.Errorf("%s by the store seat: %v", name, err)
		}
	}
}

func entryBySeqK(k *agentKit, seq int64) SummaryEntry {
	k.t.Helper()
	st, err := k.sched.SummaryStatus(context.Background(), "g", "", true)
	if err != nil {
		k.t.Fatal(err)
	}
	for _, stream := range st.Streams {
		for _, e := range stream.History {
			if e.Seq == seq {
				return e
			}
		}
	}
	k.t.Fatalf("no entry #%d", seq)
	return SummaryEntry{}
}

// The subject digest covers the rows: a verdict that changes while the node stays in its stage is a new entry that states it and supersedes the open one; nothing changed is the same entry.
func TestSummaryDigestCoversSameStageVerdictChange(t *testing.T) {
	t.Parallel()
	for _, side := range []string{"agent", "managed child"} {
		t.Run(side, func(t *testing.T) {
			t.Parallel()
			spec := agentSpec{name: "n", files: fileOf("n"), relationship: side == "managed child"}
			k := newAgentKit(t, spec)
			rule := func(label, verdict string, findings ...string) {
				t.Helper()
				if side == "agent" {
					var defects []map[string]any
					for i, f := range findings {
						defects = append(defects, defect("D"+string(rune('1'+i)), "P2", "other", f))
					}
					k.reviewWith("n", verdict, defects...)
					return
				}
				var list []string
				for i, f := range findings {
					list = append(list, `{"id":"c`+string(rune('1'+i))+`","verdict":"needs_changes","note":"`+f+`"}`)
				}
				text := ""
				if len(list) > 0 {
					text = "[" + strings.Join(list, ",") + "]"
				}
				next := int64(0)
				if verdict == "needs_changes" {
					next = 2
				}
				ruleRelationship(k, "n", label, verdict, next, text)
			}
			progressDigest := func() (string, string) {
				p, err := k.sched.ReadProgress(context.Background(), "g")
				if err != nil {
					t.Fatal(err)
				}
				for _, n := range p.Nodes {
					if n.NodeID == "n" {
						return p.Digest, n.Stage
					}
				}
				t.Fatal("no node n")
				return "", ""
			}
			if side == "agent" {
				k.author("n", authorModel)
			}
			rule("one", "needs_changes", "the first finding")
			d0, stage := progressDigest()
			first := enqueueG(t, k)
			if first.Replayed || first.Entry.Seq != 1 {
				t.Fatalf("first %+v", first)
			}
			rule("two", "verified")
			if d, s := progressDigest(); d != d0 || s != stage {
				t.Fatalf("the progress moved (stage %s -> %s): the test needs a verdict that changes under the same stage", stage, s)
			}
			second := enqueueG(t, k)
			if second.Replayed || second.Entry.Seq != 2 || second.Entry.SubjectDigest == first.Entry.SubjectDigest || len(second.Superseded) != 1 || second.Superseded[0] != first.Entry.SummaryID {
				t.Fatalf("the approval after needs_changes: %+v", second)
			}
			if !strings.Contains(second.Entry.Summary, "verdict verified") || strings.Contains(second.Entry.Summary, "the first finding") {
				t.Fatalf("the second summary does not state the newest verdict:\n%s", second.Entry.Summary)
			}
			if got := entryBySeqK(k, 1); got.State != SummarySuperseded {
				t.Fatalf("the open entry is %s, want superseded", got.State)
			}
			// two rulings that differ in their findings only
			rule("three", "needs_changes", "alpha problem")
			third := enqueueG(t, k)
			rule("four", "needs_changes", "beta problem")
			fourth := enqueueG(t, k)
			if third.Replayed || fourth.Replayed || fourth.Entry.Seq != 4 || third.Entry.SubjectDigest == fourth.Entry.SubjectDigest || len(fourth.Superseded) != 1 || fourth.Superseded[0] != third.Entry.SummaryID {
				t.Fatalf("findings-only change: %+v then %+v", third, fourth)
			}
			if !strings.Contains(fourth.Entry.Summary, "beta problem") || strings.Contains(fourth.Entry.Summary, "alpha problem") {
				t.Fatalf("the fourth summary:\n%s", fourth.Entry.Summary)
			}
			if d, s := progressDigest(); d != d0 || s != stage {
				t.Fatalf("the progress moved: %s", s)
			}
			// nothing changed: the entry is answered and the table does not grow
			rows := k.count("SELECT COUNT(*) FROM dag_summary_outbox")
			again := enqueueG(t, k)
			if !again.Replayed || again.Entry.SummaryID != fourth.Entry.SummaryID || k.count("SELECT COUNT(*) FROM dag_summary_outbox") != rows {
				t.Fatalf("asking again: %+v", again)
			}
		})
	}
}

// up_to_date is the comparison of the newest entry's subject digest with the subject of the plan now, so a findings-only change of a node in the same stage makes it false until the next enqueue.
func TestSummaryStatusUpToDateUsesSubjectDigest(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "n", files: fileOf("n")})
	k.author("n", authorModel)
	k.reviewWith("n", "needs_changes", defect("D1", "P2", "other", "one finding"))
	entry := enqueueG(t, k).Entry
	st := statusG(t, k)
	if !st.UpToDate || st.Newest.SummaryID != entry.SummaryID {
		t.Fatalf("right after the enqueue: %+v", st)
	}
	p, err := k.sched.ReadProgress(context.Background(), "g")
	if err != nil {
		t.Fatal(err)
	}
	all, err := k.sched.SummaryStatus(context.Background(), "g", "", false)
	if err != nil || all.ProgressDigest != p.Digest {
		t.Fatalf("the status keeps printing the progress digest: %q vs %q (%v)", all.ProgressDigest, p.Digest, err)
	}
	k.reviewWith("n", "needs_changes", defect("D1", "P2", "other", "another finding"))
	if st := statusG(t, k); st.UpToDate || !st.Owed {
		t.Fatalf("a verdict changed under the same stage and the entry still reads up to date: %+v", st)
	}
	if again := enqueueG(t, k); again.Replayed {
		t.Fatalf("the changed findings were not enqueued: %+v", again)
	}
	if st := statusG(t, k); !st.UpToDate {
		t.Fatalf("after the enqueue: %+v", st)
	}
}
