package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// RunHook is the engine's UpwardHook of a run (CRW-1204): what goes up from the run to the orchestrator thread that started it.
//
//   - the plan summary: at run start (a resume too) and after every committed batch the summary stream of the run's plan and document is
//     reconciled (ReconcileSummary), so an entry owed because the process died between two steps is found;
//   - the escalations of the run (dispatch_unknown, tiers_exhausted, blocked_scope, size_split, restore_snapshot_mismatch, a configuration
//     fault, ...) are told to the orchestrator thread with `crw manage send-parent`, once each, queued unless the wake-up of the thread is
//     proven (the run cannot prove it, so it does not probe the thread first);
//   - the completion digest is told the same way when the run finishes.
//
// A failing step does not fail the run: the next call does it again.
type RunHook struct {
	Store   *store.Store
	Sched   *dagsched.Scheduler
	Journal engine.Journal
	Rows    Appender
	Sender  Sender
	Run     string
	// Wake proves that a send wakes the orchestrator thread; nil proves nothing and every notice is queued.
	Wake func(ctx context.Context, thread string) bool

	mu sync.Mutex
}

var _ engine.UpwardHook = (*RunHook)(nil)

// RunStarted implements engine.UpwardHook.
func (h *RunHook) RunStarted(ctx context.Context, _ string) error { return h.reconcile(ctx) }

// BatchCommitted implements engine.UpwardHook.
func (h *RunHook) BatchCommitted(ctx context.Context, _ string) error { return h.reconcile(ctx) }

// RunFinished implements engine.UpwardHook: the summary and the escalations once more, and the completion digest.
func (h *RunHook) RunFinished(ctx context.Context, _, status string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.reconcileLocked(ctx)
	run, ok, rerr := ReadRun(ctx, h.Store.DB, h.Run)
	if rerr != nil || !ok {
		return firstError(err, rerr)
	}
	key := "done:" + h.Run + ":" + digestOfRows(h.Journal.Events(), status)
	text := fmt.Sprintf("[crw flow] run finished run=%s workflow=%s status=%s digest=%s", h.Run, run.Workflow, status, key[strings.LastIndex(key, ":")+1:])
	res, serr := h.send(ctx, run.Actor, "done-"+shortHash(key), text)
	if serr != nil {
		return firstError(err, serr)
	}
	if res.Class == "accepted" || res.Class == "queued" {
		_, _, aerr := h.Rows.AppendOnce(ctx, NewRow{Kind: KindCompletionNotice, Payload: map[string]any{"notice_key": key, "status": status, "result": res.Class, "request_id": res.RequestID}}, "notice_key")
		err = firstError(err, aerr)
	}
	return err
}

func (h *RunHook) reconcile(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reconcileLocked(ctx)
}

func (h *RunHook) reconcileLocked(ctx context.Context) error {
	run, ok, err := ReadRun(ctx, h.Store.DB, h.Run)
	if err != nil || !ok {
		// the run record is written with the run's first batch: before it there is nothing to tell
		return err
	}
	var errs []error
	if document := h.document(); document != "" && run.PlanID != "" && h.Sched != nil {
		_, err := ReconcileSummary(ctx, SummaryEnv{Store: h.Store, Sched: h.Sched, Rows: h.Rows, Run: h.Run, Plan: run.PlanID, Document: document, Actor: run.Actor, Sender: h.Sender, Wake: h.Wake})
		errs = append(errs, err)
	}
	errs = append(errs, h.escalations(ctx, run.Actor))
	return errors.Join(errs...)
}

// document is the summary document the run recorded (the newest summary_document row of its journal).
func (h *RunHook) document() string {
	doc := ""
	for _, ev := range h.Journal.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == engine.EvSummaryDocument {
			if d, _ := ev.Data["document"].(string); d != "" {
				doc = d
			}
		}
	}
	return doc
}

// escalations tells the orchestrator thread about each escalation row of the run that was not told yet.
func (h *RunHook) escalations(ctx context.Context, thread string) error {
	var told = map[string]bool{}
	for _, ev := range h.Journal.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == KindEscalationNotice {
			told[fmt.Sprint(ev.Data["escalation_id"])] = true
		}
	}
	var errs []error
	for _, ev := range h.Journal.Events() {
		if ev.Kind != engine.KindFlow || ev.Event != engine.EvEscalation {
			continue
		}
		id, _ := ev.Data["escalation_id"].(string)
		if id == "" || told[id] {
			continue
		}
		told[id] = true
		text := fmt.Sprintf("[crw flow] run escalation run=%s node=%s reason=%s class=%s", h.Run, textOr(ev.Data["node"], "-"), textOr(ev.Data["reason"], "-"), textOr(ev.Data["class"], "-"))
		if d := textOr(ev.Data["detail"], ""); d != "" {
			text += "\n" + d
		}
		if ask := textOr(ev.Data["ask"], ""); ask != "" {
			text += "\nThe run waits for your decision (" + ask + ")."
		}
		res, err := h.send(ctx, thread, "esc-"+id, text)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch res.Class {
		case "accepted", "queued":
			if _, _, err := h.Rows.AppendOnce(ctx, NewRow{Kind: KindEscalationNotice, Payload: map[string]any{"escalation_id": id, "result": res.Class, "request_id": res.RequestID, "thread": thread}}, "escalation_id"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// send tells the orchestrator thread: queued, with no look at the thread first, unless the wake-up is proven.
func (h *RunHook) send(ctx context.Context, thread, logicalID, text string) (SendResult, error) {
	if h.Sender == nil {
		return SendResult{}, errors.New("report: a sender is required to tell the orchestrator thread")
	}
	queue := true
	if h.Wake != nil && h.Wake(ctx, thread) {
		queue = false
	}
	return h.Sender.SendParent(ctx, ParentSend{Thread: thread, LogicalID: logicalID, Text: text, Queue: queue})
}

func textOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

// digestOfRows is the digest of what the run came to: its completed operations and calls and the status.
func digestOfRows(events []engine.Event, status string) string {
	var parts []string
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted {
			parts = append(parts, ev.CallID)
		}
	}
	return shortHash(append(parts, "status="+status)...)
}
