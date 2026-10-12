package engine

import (
	"context"
)

// ForeignSeqBase is the first sequence number of the rows a command writes into the flow_events of a run (internal/flow/report): a
// process that is not the run, possibly while the run is alive, takes its numbers from here so it can never meet one the run's journal
// hands out. The run's own numbers count up from 1 and stay below it; a journal that reads the rows of its run counts after the largest
// number below the base.
const ForeignSeqBase int64 = 1 << 40

// lastRunSeq is the largest sequence number among the rows the run itself wrote (below ForeignSeqBase), 0 for none.
func lastRunSeq(rows []Event) int {
	last := 0
	for _, ev := range rows {
		if int64(ev.Seq) < ForeignSeqBase && ev.Seq > last {
			last = ev.Seq
		}
	}
	return last
}

// UpwardHook is what a run hands the things that go up from it to: the plan summary the orchestrator thread owes its document, the
// escalations of the run and its completion digest (CRW-1204). The engine calls it and knows nothing of what it does; the binary installs
// the hook that works on the relay store (internal/flow/report).
//
// RunStarted is called once the run's started row is written and, for a resumed run, once the reconcile pass is over, before the script
// runs: a run that died between a committed batch and what that batch owed finds it here. BatchCommitted is called after the journal
// committed a batch (a transaction of rows) and outside the journal's lock; it is not called again for a batch the hook itself caused.
// RunFinished is called after the run's finished row is written, with the status the run came to.
//
// A failing hook never fails a run: the engine journals the failure as a log row of the run and goes on, and the next call reconciles
// what the failed one left.
type UpwardHook interface {
	RunStarted(ctx context.Context, runID string) error
	BatchCommitted(ctx context.Context, runID string) error
	RunFinished(ctx context.Context, runID, status string) error
}

// CommitNotifier is implemented by a journal that can tell when it committed a batch: Notify is called after each commit, outside the
// journal's lock, with no arguments; SetOnCommit(nil) removes it.
type CommitNotifier interface {
	SetOnCommit(notify func())
}

// EvUpwardHookFailed is the log row a failed hook leaves.
const EvUpwardHookFailed = "upward_hook_failed"

// hookRun calls one entry of the hook and journals its failure. The hook runs one call at a time: a start or a finish waits for the call in
// progress, and a batch committed while a call is in progress (the rows the hook itself appends, or another call's batch) is not told - the
// call in progress or the next one reads the whole journal.
func (e *Engine) hookRun(point string, call func(h UpwardHook) error) {
	h := e.cfg.Upward
	if h == nil {
		return
	}
	if point == "batch" {
		select {
		case e.hookSem <- struct{}{}:
		default:
			return
		}
	} else {
		e.hookSem <- struct{}{}
	}
	// the failure row is appended before the hook is released: the commit it may cause is not told to a hook that just failed
	defer func() { <-e.hookSem }()
	if err := call(h); err != nil {
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvUpwardHookFailed, Data: map[string]any{"point": point, "error": err.Error()}})
	}
}

// EvSummaryDocument is the row a run records the document of its plan summary with.
const EvSummaryDocument = "summary_document"

// noteSummaryDocument records Config.SummaryDocument in the journal unless the run's last such row already names it.
func (e *Engine) noteSummaryDocument() {
	doc := e.cfg.SummaryDocument
	if doc == "" {
		return
	}
	last := ""
	for _, ev := range e.j.Events() {
		if ev.Kind == KindFlow && ev.RunID == e.runID && ev.Event == EvSummaryDocument {
			last, _ = ev.Data["document"].(string)
		}
	}
	if last != doc {
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvSummaryDocument, Data: map[string]any{"document": doc}})
	}
}

// batchCommitted is the journal's notice that a batch was committed. A batch committed while the run is being cancelled is left to the
// next reconcile.
func (e *Engine) batchCommitted() {
	if e.ctx.Err() != nil {
		return
	}
	e.hookRun("batch", func(h UpwardHook) error { return h.BatchCommitted(e.ctx, e.runID) })
}
