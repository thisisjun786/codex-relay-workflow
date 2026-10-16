package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ---- the fixture of the summary: an agent node with a reviewer verdict, written the way the run's journal writes it ----

// agentNode gives node A an agent execution (the node a run executes) so the summary carries its verdict row.
func (w *world) agentNode(node string) {
	w.t.Helper()
	exec := func(query string, args ...any) {
		if _, err := w.st.DB.ExecContext(ctx, query, args...); err != nil {
			w.t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES (?,?,?,?,0,?)`, "m-"+node, node, "{}", "{}", "t")
	exec(`INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at) VALUES (?,?,?,1,'initial',?,'executor','executor',?,0,'t')`,
		"exec-"+node, testPlan, node, "m-"+node, testOrch)
}

var verdictCalls int

// verdict records a completed reviewer call of the node with a verdict document that carries the defects.
func (w *world) verdict(node, verdict string, defects ...string) {
	w.t.Helper()
	verdictCalls++
	id := fmt.Sprintf("call-%s-%d", node, verdictCalls)
	var list []map[string]any
	for i, d := range defects {
		list = append(list, map[string]any{"ref": fmt.Sprintf("d%d", i+1), "kind": "correctness", "severity": "P1", "introduced": true, "impact": "x", "inPromise": true, "file": "a.go", "line": 7, "finding": d, "evidence_question": nil})
	}
	raw, _ := json.Marshal(map[string]any{"verdict": verdict, "criteria": []any{}, "defects": list, "summary": "s"})
	detail, _ := json.Marshal(map[string]string{"criteria_digest": "criteria-" + node})
	for seq, event := range []string{"prepared", "completed"} {
		var result any
		d := ""
		if event == "completed" {
			result, d = string(raw), string(detail)
		}
		if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, commit_sha, thread_id, turn_id, plan_id, node_id, execution_id, result_bytes, detail_json, recorded_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, seq+1, testRun, "reviewer:"+node, 0, "agent", "reviewer", event, "commit-"+id, "thread-"+id, "turn-"+id, testPlan, node, "exec-"+node, result, d, "2026-10-12T09:00:00Z"); err != nil {
			w.t.Fatal(err)
		}
	}
}

// documentRow records the summary document in the run's journal, as the engine does.
func (w *world) documentRow() {
	w.t.Helper()
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindFlow, Event: engine.EvSummaryDocument, Data: map[string]any{"document": testDocument}})
	if err := w.journal.Flush(); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) hook() *RunHook {
	return &RunHook{Store: w.st, Sched: w.sched, Journal: w.journal, Rows: JournalAppender{Journal: w.journal, RunID: testRun, Store: w.st}, Sender: w.sender, Run: testRun}
}

func (w *world) entries() []dagsched.SummaryEntry {
	w.t.Helper()
	st, err := w.sched.SummaryStatus(ctx, testPlan, "", true)
	if err != nil {
		w.t.Fatal(err)
	}
	var all []dagsched.SummaryEntry
	for _, s := range st.Streams {
		all = append(all, s.History...)
	}
	return all
}

func (w *world) upToDate() bool {
	w.t.Helper()
	st, err := w.sched.SummaryStatus(ctx, testPlan, testDocument, false)
	if err != nil {
		w.t.Fatal(err)
	}
	return len(st.Streams) == 1 && st.Streams[0].UpToDate
}

// distinct is the logical ids the orchestrator thread was sent, each once: the send of one logical id is idempotent in crw manage, so the
// fake counts ids, not calls.
func (f *fakeSender) distinctParent() []string {
	var out []string
	for _, p := range f.parentSent() {
		if !slices.Contains(out, p.LogicalID) {
			out = append(out, p.LogicalID)
		}
	}
	return out
}

// The run enqueues the summary of its plan under the orchestrator's task id - the project's registered parent, the one actor the outbox
// lets enqueue - at its start, and tells the orchestrator thread an entry is owed, queued: the run cannot prove a send wakes the thread.
func TestRunEnqueuesSummaryUnderOrchestratorTask(t *testing.T) {
	w := newWorld(t)
	w.documentRow()
	if err := w.hook().RunStarted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	entries := w.entries()
	if len(entries) != 1 || entries[0].EnqueuedBy != testOrch || entries[0].Document != testDocument || entries[0].State != dagsched.SummaryPending || entries[0].PlanID != testPlan {
		t.Fatalf("entries %+v", entries)
	}
	id := entries[0].SummaryID
	notices := w.rows(KindSummaryNotice)
	if len(notices) != 1 || notices[0].Str("summary_id") != id || notices[0].Str("plan") != testPlan || notices[0].Str("document") != testDocument || notices[0].Str("logical_id") != "summary-"+id {
		t.Fatalf("notice rows %+v", notices)
	}
	sent := w.sender.parentSent()
	if len(sent) != 1 || sent[0].Thread != testOrch || sent[0].LogicalID != "summary-"+id || !sent[0].Queue {
		t.Fatalf("sent %+v", sent)
	}
	for _, want := range []string{"summary_owed", "summary=" + id, "plan=" + testPlan, "document=" + testDocument} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("the notice lacks %q: %s", want, sent[0].Text)
		}
	}
	settled := w.rows(KindSummarySettled)
	if len(settled) != 1 || settled[0].Str("summary_id") != id || settled[0].Str("result") != SettledQueued || settled[0].Str("request_id") == "" {
		t.Fatalf("settled rows %+v", settled)
	}
	if !w.upToDate() {
		t.Error("the stream is behind the plan")
	}
	// a run that records no document keeps no summary
	v := newWorld(t)
	if err := v.hook().RunStarted(ctx, testRun); err != nil || len(v.entries()) != 0 || len(v.sender.parentSent()) != 0 {
		t.Errorf("no document: %v entries %d sent %d", err, len(v.entries()), len(v.sender.parentSent()))
	}
}

// A second verdict batch at the same stage, with other findings, is another subject: a new entry, a new notice intent and one more notice.
// The same batch again is the same entry and adds nothing.
func TestRunEnqueuesSummaryOnSameStageVerdictChange(t *testing.T) {
	w := newWorld(t)
	w.agentNode("A")
	w.documentRow()
	h := w.hook()
	w.verdict("A", "needs_changes", "the guard is missing")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	first := w.entries()
	if len(first) != 1 {
		t.Fatalf("entries %+v", first)
	}
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	if got := w.entries(); len(got) != 1 || len(w.rows(KindSummaryNotice)) != 1 || len(w.sender.distinctParent()) != 1 {
		t.Fatalf("the same batch again: entries %d notices %d sends %v", len(got), len(w.rows(KindSummaryNotice)), w.sender.distinctParent())
	}
	// the stage of the node is the same; its verdict and findings are not
	w.verdict("A", "needs_changes", "the guard is missing", "the loop never ends")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	second := w.entries()
	if len(second) != 2 || second[1].SummaryID == second[0].SummaryID || second[0].State != dagsched.SummarySuperseded || second[1].StateDigest != second[0].StateDigest {
		t.Fatalf("entries %+v", second)
	}
	notices := w.rows(KindSummaryNotice)
	if len(notices) != 2 || notices[1].Str("summary_id") != second[1].SummaryID {
		t.Fatalf("notice rows %+v", notices)
	}
	if got := w.sender.distinctParent(); len(got) != 2 || got[1] != "summary-"+second[1].SummaryID {
		t.Fatalf("sent %v", got)
	}
	// the replay of the second batch adds nothing
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	if len(w.entries()) != 2 || len(w.rows(KindSummaryNotice)) != 2 || len(w.sender.distinctParent()) != 2 || len(w.rows(KindSummarySettled)) != 2 {
		t.Errorf("the replay added: entries %d notices %d sent %v settled %d", len(w.entries()), len(w.rows(KindSummaryNotice)), w.sender.distinctParent(), len(w.rows(KindSummarySettled)))
	}
	// the first intent was settled when its entry was superseded (nothing owed for it), and the second by the queued notice
	settled := w.rows(KindSummarySettled)
	if r := map[string]string{settled[0].Str("summary_id"): settled[0].Str("result"), settled[1].Str("summary_id"): settled[1].Str("result")}; r[first[0].SummaryID] != SettledQueued || r[second[1].SummaryID] != SettledQueued {
		t.Errorf("settled %v", r)
	}
}

// The run process dies right after a batch committed and before the enqueue: the stream is behind the plan. A resume reconciles it at its
// start - one entry, one intent, one notice (logical id summary-<id>), one settlement - and so does the run that died after its last batch.
func TestSummaryHandoffCrashAfterBatchCommitBeforeEnqueue(t *testing.T) {
	for _, point := range []string{"RunStarted of the resume", "RunFinished of the run that never saw its last batch"} {
		w := newWorld(t)
		w.agentNode("A")
		w.documentRow()
		w.verdict("A", "needs_changes", "the guard is missing")
		// the process died: no hook call followed the batch
		if len(w.entries()) != 0 {
			t.Fatalf("%s: an entry exists", point)
		}
		before, err := w.sched.SummaryStatus(ctx, testPlan, testDocument, false)
		if err != nil || len(before.Streams) != 0 {
			t.Fatalf("%s: status before %+v %v", point, before, err)
		}
		h := w.hook()
		if point == "RunStarted of the resume" {
			err = h.RunStarted(ctx, testRun)
		} else {
			err = h.RunFinished(ctx, testRun, "completed")
		}
		if err != nil {
			t.Fatalf("%s: %v", point, err)
		}
		entries := w.entries()
		if len(entries) != 1 || !w.upToDate() {
			t.Fatalf("%s: entries %+v", point, entries)
		}
		if n := len(w.rows(KindSummaryNotice)); n != 1 {
			t.Errorf("%s: %d intents", point, n)
		}
		if got := w.sender.distinctParent(); !slices.Contains(got, "summary-"+entries[0].SummaryID) {
			t.Errorf("%s: sent %v", point, got)
		}
		settled := w.rows(KindSummarySettled)
		if len(settled) != 1 || settled[0].Str("result") != SettledQueued {
			t.Errorf("%s: settled %+v", point, settled)
		}
		// and a second reconcile finds everything done
		if err := h.BatchCommitted(ctx, testRun); err != nil || len(w.entries()) != 1 || len(w.rows(KindSummaryNotice)) != 1 || len(w.rows(KindSummarySettled)) != 1 {
			t.Errorf("%s: a second reconcile: %v", point, err)
		}
	}
}

// failing makes the rows of one kind fail to be written.
type failing struct {
	Appender
	kind string
	err  error
}

func (f failing) AppendOnce(ctx context.Context, row NewRow, key string) (bool, int64, error) {
	if row.Kind == f.kind {
		return false, 0, f.err
	}
	return f.Appender.AppendOnce(ctx, row, key)
}

// The run dies after the entry was enqueued and before the intent, between the intent and the send, after the send and before the
// settlement, or after the entry was superseded: every resume finds its place in the rows, and the orchestrator thread gets one notice for
// each entry that is owed, under one logical id. An entry that was superseded first is settled obsolete without a notice.
func TestSummaryHandoffCrashAfterEnqueueBeforeNotice(t *testing.T) {
	setup := func() *world {
		w := newWorld(t)
		w.agentNode("A")
		w.documentRow()
		w.verdict("A", "needs_changes", "the guard is missing")
		return w
	}
	t.Run("entry_before_intent", func(t *testing.T) {
		w := setup()
		enq, err := w.sched.EnqueueSummary(ctx, testPlan, testOrch, testDocument, false)
		if err != nil || enq.Replayed {
			t.Fatal(err)
		}
		if err := w.hook().RunStarted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
		if len(w.entries()) != 1 || len(w.rows(KindSummaryNotice)) != 1 || len(w.rows(KindSummarySettled)) != 1 || len(w.sender.parentSent()) != 1 || w.sender.parentSent()[0].LogicalID != "summary-"+enq.Entry.SummaryID {
			t.Fatalf("entries %d intents %d settled %d sent %+v", len(w.entries()), len(w.rows(KindSummaryNotice)), len(w.rows(KindSummarySettled)), w.sender.parentSent())
		}
	})
	t.Run("intent_before_send", func(t *testing.T) {
		w := setup()
		enq, _ := w.sched.EnqueueSummary(ctx, testPlan, testOrch, testDocument, false)
		id := enq.Entry.SummaryID
		if _, _, err := (JournalAppender{Journal: w.journal, RunID: testRun, Store: w.st}).AppendOnce(ctx, NewRow{Kind: KindSummaryNotice, Payload: map[string]any{
			"summary_id": id, "plan": testPlan, "document": testDocument, "logical_id": "summary-" + id, "run": testRun}}, "summary_id"); err != nil {
			t.Fatal(err)
		}
		if err := w.hook().RunStarted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
		if got := w.sender.distinctParent(); len(got) != 1 || got[0] != "summary-"+id || len(w.rows(KindSummaryNotice)) != 1 || len(w.rows(KindSummarySettled)) != 1 {
			t.Fatalf("sent %v intents %d settled %d", got, len(w.rows(KindSummaryNotice)), len(w.rows(KindSummarySettled)))
		}
	})
	t.Run("send_before_settlement", func(t *testing.T) {
		w := setup()
		h := w.hook()
		h.Rows = failing{Appender: h.Rows, kind: KindSummarySettled, err: errors.New("the process died")}
		if err := h.RunStarted(ctx, testRun); err == nil {
			t.Fatal("the failed settlement was not reported")
		}
		first := w.sender.parentSent()
		if len(first) != 1 || len(w.rows(KindSummaryNotice)) != 1 || len(w.rows(KindSummarySettled)) != 0 {
			t.Fatalf("after the death: sent %+v intents %d settled %d", first, len(w.rows(KindSummaryNotice)), len(w.rows(KindSummarySettled)))
		}
		// the resume sends it again under the logical id the intent recorded, and the settlement is written once
		if err := w.hook().RunStarted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
		if got := w.sender.distinctParent(); len(got) != 1 || len(w.sender.parentSent()) != 2 || len(w.rows(KindSummarySettled)) != 1 {
			t.Fatalf("after the resume: distinct %v calls %d settled %d", got, len(w.sender.parentSent()), len(w.rows(KindSummarySettled)))
		}
	})
	t.Run("superseded_before_send", func(t *testing.T) {
		w := setup()
		enq, _ := w.sched.EnqueueSummary(ctx, testPlan, testOrch, testDocument, false)
		old := enq.Entry.SummaryID
		if _, _, err := (JournalAppender{Journal: w.journal, RunID: testRun, Store: w.st}).AppendOnce(ctx, NewRow{Kind: KindSummaryNotice, Payload: map[string]any{
			"summary_id": old, "plan": testPlan, "document": testDocument, "logical_id": "summary-" + old, "run": testRun}}, "summary_id"); err != nil {
			t.Fatal(err)
		}
		w.verdict("A", "needs_changes", "the guard is missing", "and another thing")
		if err := w.hook().RunStarted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
		entries := w.entries()
		if len(entries) != 2 || entries[0].State != dagsched.SummarySuperseded {
			t.Fatalf("entries %+v", entries)
		}
		byID := map[string]string{}
		for _, r := range w.rows(KindSummarySettled) {
			byID[r.Str("summary_id")] = r.Str("result")
		}
		if byID[old] != SettledObsolete || byID[entries[1].SummaryID] != SettledQueued || len(byID) != 2 {
			t.Fatalf("settled %v", byID)
		}
		if got := w.sender.distinctParent(); len(got) != 1 || got[0] != "summary-"+entries[1].SummaryID {
			t.Fatalf("the superseded entry was told: %v", got)
		}
	})
	t.Run("refused", func(t *testing.T) {
		w := setup()
		w.sender.replyParent = func(ParentSend) (SendResult, error) {
			return SendResult{Class: "refused", RequestID: "r", Reason: "thread_gone"}, nil
		}
		if err := w.hook().RunStarted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
		settled := w.rows(KindSummarySettled)
		esc := w.rows(engine.EvEscalation)
		if len(settled) != 1 || settled[0].Str("result") != SettledRefused || len(esc) != 1 || esc[0].Str("reason") != "summary_notice_refused" {
			t.Fatalf("settled %+v escalations %+v", settled, esc)
		}
	})
	t.Run("unknown_writes_no_settlement", func(t *testing.T) {
		w := setup()
		w.sender.replyParent = func(ParentSend) (SendResult, error) { return SendResult{Class: "unknown"}, nil }
		if err := w.hook().RunStarted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
		if len(w.rows(KindSummaryNotice)) != 1 || len(w.rows(KindSummarySettled)) != 0 {
			t.Fatalf("intents %d settled %d", len(w.rows(KindSummaryNotice)), len(w.rows(KindSummarySettled)))
		}
		w.sender.replyParent = nil
		if err := w.hook().BatchCommitted(ctx, testRun); err != nil || len(w.rows(KindSummarySettled)) != 1 {
			t.Fatalf("the next reconcile: %v settled %d", err, len(w.rows(KindSummarySettled)))
		}
	})
}

// The orchestrator thread is the project's registered parent: it claims and completes the entry. The manager's store seat is refused the
// claim, so the run and the manager never drain the outbox.
func TestOrchestratorDrainsSummaryAndManagerClaimRefused(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	w.documentRow()
	if err := w.hook().RunStarted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	entries := w.entries()
	if len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	id := entries[0].SummaryID
	_, err := w.sched.ClaimSummary(ctx, id, "manager-seat")
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "scope_role_mismatch" {
		t.Fatalf("the manager's claim: %v", err)
	}
	if _, err := w.sched.EnqueueSummary(ctx, testPlan, "manager-seat", testDocument, false); !errors.As(err, &refused) || refused.Reason != "scope_role_mismatch" {
		t.Errorf("the manager's enqueue: %v", err)
	}
	claim, err := w.sched.ClaimSummary(ctx, id, testOrch)
	if err != nil || claim.Token == "" {
		t.Fatalf("the orchestrator's claim: %+v %v", claim, err)
	}
	done, err := w.sched.CompleteSummary(ctx, id, testOrch, claim.Token, testDocument, claim.Operation.Container)
	if err != nil {
		t.Fatalf("the orchestrator's completion: %v", err)
	}
	if done.Entry.State != dagsched.SummaryConfirmed {
		t.Errorf("entry %+v", done.Entry)
	}
	// the plan moves on: the run's next reconcile asks for the next entry, and the confirmed one is not told again
	w.bump()
	if err := w.hook().BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	if got := w.entries(); len(got) != 2 || got[1].State != dagsched.SummaryPending {
		t.Fatalf("entries %+v", got)
	}
	if got := w.sender.distinctParent(); len(got) != 2 {
		t.Errorf("sent %v", got)
	}
}

// Two writers can each have written the intent of one summary id (the run's check and its flush are not one transaction; before CRW-1416
// a live run and a concurrent `crw flow apply` did): the duplicate row neither sends twice nor settles twice.
func TestSummaryDuplicateIntentRowsSendAndSettleOnce(t *testing.T) {
	w := newWorld(t)
	w.agentNode("A")
	w.documentRow()
	w.verdict("A", "needs_changes", "the guard is missing")
	env := SummaryEnv{Store: w.st, Sched: w.sched, Rows: ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}, Run: testRun, Plan: testPlan, Document: testDocument, Actor: testOrch, Sender: w.sender}
	// the first reconcile writes the intent and gets no answer: no settlement
	w.sender.replyParent = func(ParentSend) (SendResult, error) { return SendResult{}, errors.New("no answer") }
	if out, err := ReconcileSummary(ctx, env); err != nil || out.Intents != 1 || out.Settled != 0 {
		t.Fatalf("first outcome %+v %v", out, err)
	}
	intents := w.rows(KindSummaryNotice)
	if len(intents) != 1 {
		t.Fatalf("intents %+v", intents)
	}
	// the other writer's copy of the same intent, in other words
	if _, err := (ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}).Append(ctx, NewRow{Kind: KindSummaryNotice, Payload: map[string]any{
		"summary_id": intents[0].Str("summary_id"), "plan": testPlan, "document": testDocument, "logical_id": intents[0].Str("logical_id"), "run": testRun,
		"text": "the other writer's words"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(w.rows(KindSummaryNotice)); n != 2 {
		t.Fatalf("%d intent rows", n)
	}
	w.sender.replyParent = nil
	before := len(w.sender.parentSent())
	if out, err := ReconcileSummary(ctx, env); err != nil || out.Sent != 1 || out.Settled != 1 {
		t.Fatalf("second outcome %+v %v", out, err)
	}
	if sent := len(w.sender.parentSent()) - before; sent != 1 {
		t.Errorf("%d sends for one summary id", sent)
	}
	if n := len(w.rows(KindSummarySettled)); n != 1 {
		t.Errorf("%d settlements", n)
	}
	// both sends carry the text of the earliest row, never the later copy's
	first := intents[0].Str("text")
	for i, p := range w.sender.parentSent() {
		if first == "" || p.Text != first {
			t.Errorf("send %d carried %q, want the earliest row's text %q", i, p.Text, first)
		}
	}
}

// A command outside the run (as `crw flow apply` was before CRW-1416) is a process of its own: each time it runs it finds the run that
// keeps the summary of the plan for the orchestrator (the run that recorded the document), reconciles the stream under the orchestrator's
// task id and writes its rows into that run - from the range commands use, so the live run's own numbers are never met.
func TestAForeignCommandReconcilesTheSummaryOfTheOrchestratorsRun(t *testing.T) {
	w := newWorld(t)
	w.agentNode("A")
	w.documentRow()
	// the child's run is the same plan's, of another actor: it keeps no summary
	seedRun(t, w.st, "run-child", testPlan, "child-task")
	run, document, ok, err := SummaryTarget(ctx, w.st.DB, testPlan, testOrch)
	if err != nil || !ok || run != testRun || document != testDocument {
		t.Fatalf("target %q %q %v %v", run, document, ok, err)
	}
	if _, _, ok, _ := SummaryTarget(ctx, w.st.DB, testPlan, "child-task"); ok {
		t.Error("the child's run keeps a summary")
	}
	w.verdict("A", "needs_changes", "the guard is missing")
	env := SummaryEnv{Store: w.st, Sched: w.sched, Rows: ForeignAppender{Store: w.st, Run: run, Now: fixedNow}, Run: run, Plan: testPlan, Document: document, Actor: testOrch, Sender: w.sender}
	out, err := ReconcileSummary(ctx, env)
	if err != nil || !out.Enqueued || out.Intents != 1 || out.Settled != 1 {
		t.Fatalf("outcome %+v %v", out, err)
	}
	for _, r := range w.rows(KindSummaryNotice, KindSummarySettled) {
		if r.Seq < ForeignSeqBase || r.Run != testRun {
			t.Errorf("row %s of run %s has sequence number %d", r.Kind, r.Run, r.Seq)
		}
	}
	// the live run goes on writing its own rows after the command's
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindFlow, Event: "log", Data: map[string]any{"text": "after"}})
	if err := w.journal.Flush(); err != nil || w.journal.Err() != nil {
		t.Fatalf("the run's journal met the command's rows: %v %v", err, w.journal.Err())
	}
	// the verdict changes again: the next reconcile asks for the next entry
	w.verdict("A", "needs_changes", "the guard is missing", "and the loop")
	if out, err = ReconcileSummary(ctx, env); err != nil || !out.Enqueued || out.Intents != 1 || len(w.entries()) != 2 {
		t.Fatalf("second outcome %+v %v", out, err)
	}
}

// CRW-1548: the summary notice names the 자식 (project session), the layer the project's registered owner is, and no bare role word. The
// words every earlier build sent stay byte for byte in summaryNoticeTextV0, the text a summary_notice row without a text of its own is
// sent in, and its one bare role word is the summary.go line of testdata/role_words_baseline.txt.
func TestSummaryNoticeNamesTheLayer(t *testing.T) {
	text := SummaryNoticeText("sum-1", "plan-1", "doc-1")
	if !strings.Contains(text, "자식") {
		t.Errorf("the summary notice does not name the 자식: %q", text)
	}
	if hits := layername.Find(text); len(hits) != 0 {
		t.Errorf("the summary notice carries the bare role words %v", hits)
	}
	for _, want := range []string{"summary_owed", "summary=sum-1", "plan=plan-1", "document=doc-1", "`crw relay dag-summary-status --plan plan-1`", "`dag-summary-claim`", "`dag-summary-complete`"} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary notice lacks %q: %q", want, text)
		}
	}
	legacy := summaryNoticeTextV0("sum-1", "plan-1", "doc-1")
	want := "[crw flow] summary_owed summary=sum-1 plan=plan-1 document=doc-1\nThe plan summary is owed to the document. You are the parent of the project: take the newest entry (`crw relay dag-summary-status --plan plan-1`), claim it (`dag-summary-claim`), write the document with your Linear connector, read it back and complete it (`dag-summary-complete`). The run and the manager do not claim."
	if legacy != want {
		t.Errorf("the words of the earlier builds changed; a retry of a row they wrote would be refused locally (CRW-1548):\n got %q\nwant %q", legacy, want)
	}
	if hits := layername.Find(legacy); len(hits) != 1 || hits[0].Word != "parent" {
		t.Errorf("the earlier words carry the bare role words %v, want the one \"parent\" its baseline line counts", hits)
	}
}

// summaryWorld is a world whose plan owes a summary entry; enqueue asks for it and returns its summary id.
func summaryWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.agentNode("A")
	w.documentRow()
	w.verdict("A", "needs_changes", "the guard is missing")
	return w
}

func (w *world) enqueue() string {
	w.t.Helper()
	enq, err := w.sched.EnqueueSummary(ctx, testPlan, testOrch, testDocument, false)
	if err != nil {
		w.t.Fatal(err)
	}
	return enq.Entry.SummaryID
}

// intentPayload is the payload of a summary_notice row for id; text "" leaves the text member out, as every earlier build wrote it.
func intentPayload(id, text string) map[string]any {
	p := map[string]any{"summary_id": id, "plan": testPlan, "document": testDocument, "logical_id": "summary-" + id, "run": testRun}
	if text != "" {
		p["text"] = text
	}
	return p
}

func (w *world) summaryEnv(rows Appender) SummaryEnv {
	return SummaryEnv{Store: w.st, Sched: w.sched, Rows: rows, Run: testRun, Plan: testPlan, Document: testDocument, Actor: testOrch, Sender: w.sender,
		Wake: func(context.Context, string) bool { return true }}
}

// CRW-1548: the manage outbox keys summary-<id> to the sha256 of the first text it carried, so a notice is always sent in the text its
// intent row holds, never in words composed again: on the first reconcile and on the one after a send that got no answer. The row the
// reconcile writes carries the text it sends.
func TestSummaryNoticeSendsTheTextItsIntentHolds(t *testing.T) {
	t.Run("the row's text is what goes", func(t *testing.T) {
		w := summaryWorld(t)
		id := w.enqueue()
		rows := ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}
		if wrote, _, err := rows.AppendOnce(ctx, NewRow{Kind: KindSummaryNotice, Payload: intentPayload(id, "X")}, "summary_id"); err != nil || !wrote {
			t.Fatalf("the intent: %v %v", wrote, err)
		}
		env := w.summaryEnv(rows)
		w.sender.replyParent = func(ParentSend) (SendResult, error) { return SendResult{}, errors.New("no answer") }
		if out, err := ReconcileSummary(ctx, env); err != nil || out.Intents != 0 || out.Sent != 1 || out.Settled != 0 {
			t.Fatalf("the first reconcile: %+v %v", out, err)
		}
		w.sender.replyParent = nil
		if out, err := ReconcileSummary(ctx, env); err != nil || out.Sent != 1 || out.Settled != 1 {
			t.Fatalf("the second reconcile: %+v %v", out, err)
		}
		sent := w.sender.parentSent()
		if len(sent) != 2 {
			t.Fatalf("%d sends, want 2", len(sent))
		}
		for i, p := range sent {
			if p.Text != "X" || p.LogicalID != "summary-"+id {
				t.Errorf("send %d carried %q under %q, want the intent's text \"X\" under summary-%s", i, p.Text, p.LogicalID, id)
			}
		}
	})
	for _, c := range []struct {
		name string
		rows func(w *world) Appender
	}{
		{"a command's row", func(w *world) Appender { return ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow} }},
		{"the run's journal", func(w *world) Appender { return JournalAppender{Journal: w.journal, RunID: testRun, Store: w.st} }},
	} {
		t.Run("the written row carries what is sent: "+c.name, func(t *testing.T) {
			w := summaryWorld(t)
			out, err := ReconcileSummary(ctx, w.summaryEnv(c.rows(w)))
			if err != nil || out.Intents != 1 || out.Sent != 1 || out.Settled != 1 {
				t.Fatalf("outcome %+v %v", out, err)
			}
			intents := w.rows(KindSummaryNotice)
			sent := w.sender.parentSent()
			if len(intents) != 1 || len(sent) != 1 {
				t.Fatalf("intents %d sends %d", len(intents), len(sent))
			}
			if got := intents[0].Str("text"); got == "" || got != sent[0].Text || got != SummaryNoticeText(out.SummaryID, testPlan, testDocument) {
				t.Errorf("the intent holds %q, the send carried %q", got, sent[0].Text)
			}
		})
	}
}

// racingAppender is the row writer of a run that loses the race for one summary id's intent (R1-4): on its first AppendOnce of a
// summary_notice, another writer stores its own intent for the same summary id first, so the write that follows finds it and writes nothing.
type racingAppender struct {
	inner, other ForeignAppender
	// text is the other writer's notice text; "" writes the row as the earlier builds did, without one
	text  string
	raced bool
}

func (a *racingAppender) Append(ctx context.Context, row NewRow) (int64, error) {
	return a.inner.Append(ctx, row)
}

func (a *racingAppender) AppendOnce(ctx context.Context, row NewRow, key string) (bool, int64, error) {
	if row.Kind == KindSummaryNotice && !a.raced {
		a.raced = true
		id, _ := row.Payload["summary_id"].(string)
		if _, err := a.other.Append(ctx, NewRow{Kind: KindSummaryNotice, Payload: intentPayload(id, a.text)}); err != nil {
			return false, 0, err
		}
	}
	return a.inner.AppendOnce(ctx, row, key)
}

// R1-4: another writer stores the intent of the newest summary id between this reconcile's read of the rows and its own write. The notice
// goes once, in the other writer's text (the earlier words when that row holds none), never in the words this build composes, and it
// settles once with no summary_notice_refused escalation.
func TestSummaryIntentWrittenByAnotherWriterFirstSendsItsText(t *testing.T) {
	for _, c := range []struct{ name, text string }{{"its row holds a text", "T1: the summary is owed, in the other writer's words"}, {"its row holds none", ""}} {
		t.Run(c.name, func(t *testing.T) {
			w := summaryWorld(t)
			rows := &racingAppender{inner: ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}, other: ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}, text: c.text}
			out, err := ReconcileSummary(ctx, w.summaryEnv(rows))
			if err != nil {
				t.Fatal(err)
			}
			if !rows.raced || out.Intents != 0 || out.Sent != 1 || out.Settled != 1 {
				t.Fatalf("outcome %+v (raced %v), want no intent of its own, one send and one settlement", out, rows.raced)
			}
			want := c.text
			if want == "" {
				want = summaryNoticeTextV0(out.SummaryID, testPlan, testDocument)
			}
			sent := w.sender.parentSent()
			if len(sent) != 1 || sent[0].Text != want {
				t.Fatalf("sent %+v, want one send of %q", sent, want)
			}
			if c.text != "" && sent[0].Text == SummaryNoticeText(out.SummaryID, testPlan, testDocument) {
				t.Error("the notice went in the words this build composes")
			}
			settled := w.rows(KindSummarySettled)
			if len(settled) != 1 || settled[0].Str("result") != SettledAccepted {
				t.Errorf("settled %+v, want accepted once", settled)
			}
			if esc := w.rows(engine.EvEscalation); len(esc) != 0 {
				t.Errorf("escalations %+v", esc)
			}
		})
	}
}
