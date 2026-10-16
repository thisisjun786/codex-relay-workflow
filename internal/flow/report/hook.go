package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// RunHook is the engine's UpwardHook of a run (CRW-1204): what goes up from the run to the orchestrator thread that started it.
//
//   - the plan summary: at run start (a resume too) and after every committed batch the summary stream of the run's plan and document is
//     reconciled (ReconcileSummary), so an entry owed because the process died between two steps is found;
//   - the escalations of the run (dispatch_unknown, tiers_exhausted, blocked_scope, size_split, restore_snapshot_mismatch, a configuration
//     fault, ...) are told once each to the level above the run's project through the Sender: RelaySender stages them on the supervisor
//     channel (flow_escalation, CRW-1380), and a run whose plan names no project tells the orchestrator thread with `crw manage
//     send-parent`, queued unless the wake-up of the thread is proven (the run cannot prove it, so it does not probe the thread first);
//     the escalation_notice row names the thread the notice was addressed to;
//   - the completion digest is told the same way when the run finishes (flow_completion);
//   - a run meant to start nodes that finished completed and started none (Blocked over the run's record, the reading of `crw flow show`,
//     CRW-1320) is reported to the initiative lead as project_blocked through Reports, the writer `crw flow report-up` works through, once
//     for the run: the report id comes from the run, so a resume that ends the same way finds the report row and makes no other. A run not
//     meant to start nodes (a script run that names no plan) is never blocked and reports nothing.
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
	// Reports writes the run's upward reports to the initiative lead; Blocked reads, from the run's record (flow_runs) and rows, whether a
	// finished run meant to start nodes started none, and the reason of each item that failed. With either nil no project_blocked report
	// is made.
	Reports *Writer
	Blocked func(run RunInfo, events []engine.Event) (reasons []string, blocked bool)

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
	if cancelledEnd(h.Journal.Events()) {
		return nil
	}
	err := h.reconcileLocked(ctx)
	run, ok, rerr := ReadRun(ctx, h.Store.DB, h.Run)
	if rerr == nil {
		rec := run
		if !ok {
			rec = RunInfo{RunID: h.Run}
		}
		err = errors.Join(err, h.reportBlocked(ctx, rec, status))
	}
	if rerr != nil || !ok {
		return firstError(err, rerr)
	}
	events := h.Journal.Events()
	outcome := finishedOutcome(events)
	key := "done:" + h.Run + ":" + digestOfRows(events, status, outcome)
	if told, terr := h.recorded(ctx, KindCompletionNotice, "notice_key", key); terr != nil || told {
		// the completion of this run in this state was told: the send-parent queue path writes a flushed notice again, so it is not asked
		return firstError(err, terr)
	}
	digest := key[strings.LastIndex(key, ":")+1:]
	text := fmt.Sprintf("[crw flow] run finished run=%s workflow=%s status=%s digest=%s", h.Run, run.Workflow, status, digest)
	if outcome != nil {
		// the milestone state the script came to, never "completed" for an escalated, incomplete or held run (CRW-1487)
		text = fmt.Sprintf("[crw flow] run finished run=%s workflow=%s %s digest=%s", h.Run, run.Workflow, outcomeFields(outcome), digest)
	}
	res, serr := h.send(ctx, run, supervisor.KindFlowCompletion, "done-"+shortHash(key), text)
	if serr != nil {
		return firstError(err, serr)
	}
	if res.Class == "accepted" || res.Class == "queued" {
		payload := map[string]any{"notice_key": key, "status": status, "result": res.Class, "request_id": res.RequestID}
		if outcome != nil {
			payload["state"] = outcome.State
			if outcome.Reason != "" {
				payload["reason"] = outcome.Reason
			}
			if lanes := laneTexts(outcome); len(lanes) > 0 {
				list := make([]any, len(lanes))
				for i, l := range lanes {
					list[i] = l
				}
				payload["lanes"] = list
			}
		}
		_, _, aerr := h.Rows.AppendOnce(ctx, NewRow{Kind: KindCompletionNotice, Payload: payload}, "notice_key")
		err = firstError(err, aerr)
	}
	return err
}

// finishedOutcome is the outcome the finished row of the process that finishes now carries (engine.Outcome): the last finished row
// after the last started row, which the engine writes before RunFinished. nil when that row carries none or is missing, so the
// outcome of an older process is never told for this one.
func finishedOutcome(events []engine.Event) *engine.Outcome {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind != engine.KindRun {
			continue
		}
		switch e.Event {
		case "finished":
			return engine.FinishedOutcome(e.Data)
		case "started":
			return nil
		}
	}
	return nil
}

// outcomeFields are the outcome's members of the completion notice: state=, then reason=, escalation= and lanes= when there are any.
func outcomeFields(o *engine.Outcome) string {
	parts := []string{"state=" + noticeValue(o.State)}
	if o.Reason != "" {
		parts = append(parts, "reason="+noticeValue(o.Reason))
	}
	if o.Escalation != "" {
		parts = append(parts, "escalation="+noticeValue(o.Escalation))
	}
	if lanes := laneTexts(o); len(lanes) > 0 {
		parts = append(parts, "lanes="+strings.Join(lanes, ","))
	}
	return strings.Join(parts, " ")
}

// laneTexts are the lanes of the outcome that did not land as node:state/reason (node:state with no reason), and +N for the lanes
// beyond the outcome's cap.
func laneTexts(o *engine.Outcome) []string {
	var out []string
	for _, l := range o.Lanes {
		t := noticeValue(textOr(l.Node, "-")) + ":" + noticeValue(textOr(l.State, "-"))
		if l.Reason != "" {
			t += "/" + noticeValue(l.Reason)
		}
		out = append(out, t)
	}
	if o.LanesMore > 0 {
		out = append(out, fmt.Sprintf("+%d", o.LanesMore))
	}
	return out
}

// noticeValue is a value of the notice's key=value line with no white space and no comma in it (the outcome's strings are one line
// already), so the members of the line stay apart.
func noticeValue(s string) string {
	return strings.NewReplacer(" ", "_", ",", ";").Replace(s)
}

// BlockedReportID is the id of the project_blocked report of a run that started no node: one for the run, whatever process finished it.
func BlockedReportID(run string) string { return "rep_" + shortHash(run, KindProjectBlocked) }

// reportBlocked reports a run meant to start nodes that finished completed and started none to the initiative lead (project_blocked).
func (h *RunHook) reportBlocked(ctx context.Context, run RunInfo, status string) error {
	if status != engine.RunCompleted || h.Reports == nil || h.Blocked == nil {
		return nil
	}
	reasons, blocked := h.Blocked(run, h.Journal.Events())
	if !blocked {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s finished without starting a node: no crw.release released a node and nothing landed.\n\nThe items that failed:\n", h.Run)
	for _, r := range reasons {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	fmt.Fprintf(&b, "\n`crw flow show %s` reads the run.\n", h.Run)
	_, err := h.Reports.Up(ctx, UpRequest{ID: BlockedReportID(h.Run), Kind: KindProjectBlocked, Run: h.Run, Subject: "no node started", Body: []byte(b.String())})
	return err
}

// recorded says whether a row of the kind carries the value in its member key: in the run's journal, else in the store (any run).
func (h *RunHook) recorded(ctx context.Context, kind, key, value string) (bool, error) {
	for _, ev := range h.Journal.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == kind && fmt.Sprint(ev.Data[key]) == value {
			return true, nil
		}
	}
	var n int
	if err := h.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_events WHERE kind = ? AND json_extract(payload_json, '$.'||?) = ?`, kind, key, value).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (h *RunHook) reconcile(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reconcileLocked(ctx)
}

func (h *RunHook) reconcileLocked(ctx context.Context) error {
	if cancelledEnd(h.Journal.Events()) {
		return nil
	}
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
	errs = append(errs, h.escalations(ctx, run))
	return errors.Join(errs...)
}

// Cancellation is an explicit stop, not a request for the upper layer to recover.
// Read only this process's end, never a cancelled end preceding a later start.
func cancelledEnd(events []engine.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind != engine.KindRun {
			continue
		}
		if e.Event == "started" {
			return false
		}
		if e.Event == "finished" {
			return e.Data["error_kind"] == engine.ErrKindCancelled
		}
	}
	return false
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

// escalations tells the level above the run's project (through the supervisor channel; the orchestrator thread through the fallback)
// about each escalation row of the run that was not told yet. The escalation_notice row names the thread the notice was addressed to.
// An escalation that asks bundle_split_request (a node sized for a bundle split, CRW-1437) names the request in place of the line that
// the run waits for a decision: a bundle split of the node's issue (read from the run's plan, else the node is named by its id) is the
// 부모's to make, and the 자식 asks for it with `crw flow report-up --kind project_decision_request`.
func (h *RunHook) escalations(ctx context.Context, run RunInfo) error {
	const askBundleSplit = "bundle_split_request"
	var issues map[string]string // node -> issue of the run's plan, read once and only for a bundle split
	issueOf := func(node string) string {
		if issues == nil {
			issues = map[string]string{}
			if run.PlanID != "" {
				// a plan that cannot be read names no issue: the notice names the node and still goes out
				if snap, _, err := (&dag.Repo{Store: h.Store}).Snapshot(ctx, run.PlanID, 0); err == nil {
					for _, n := range snap.Nodes {
						issues[n.NodeID] = n.IssueKey
					}
				}
			}
		}
		return issues[node]
	}
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
		switch ask := textOr(ev.Data["ask"], ""); ask {
		case "":
		case askBundleSplit:
			node := textOr(ev.Data["node"], "-")
			what := "the issue of node " + node
			if issue := issueOf(node); issue != "" {
				what = "issue " + issue + " (node " + node + ")"
			}
			text += "\nThe node is sized for a bundle split: a bundle split of " + what + " is the 부모's to make (the repository session). The 자식 (the project session) asks the 부모 for it with `crw flow report-up --kind project_decision_request --run " + h.Run + " --evidence FILE` (" + ask + ")."
		default:
			text += "\nThe run waits for your decision (" + ask + ")."
		}
		res, err := h.send(ctx, run, supervisor.KindFlowEscalation, "esc-"+id, text)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch res.Class {
		case "accepted", "queued":
			// the recipient the channel staged the notice for (CRW-1390), else the orchestrator thread send-parent was given
			thread := firstText(res.Recipient, run.Actor)
			if _, _, err := h.Rows.AppendOnce(ctx, NewRow{Kind: KindEscalationNotice, Payload: map[string]any{"escalation_id": id, "result": res.Class, "request_id": res.RequestID, "thread": thread}}, "escalation_id"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// send tells the orchestrator thread: queued, with no look at the thread first, unless the wake-up is proven. The notice names its
// supervisor-channel kind, the run and the project of the run's plan: RelaySender stages it for the level above the project (CRW-1380,
// design CRW-1314 §3), ManageSender sends it to the orchestrator thread as before.
func (h *RunHook) send(ctx context.Context, run RunInfo, kind, logicalID, text string) (SendResult, error) {
	if h.Sender == nil {
		return SendResult{}, errors.New("report: a sender is required to tell the orchestrator thread")
	}
	thread := run.Actor
	queue := true
	if h.Wake != nil && h.Wake(ctx, thread) {
		queue = false
	}
	project := ""
	if run.PlanID != "" {
		project, _ = ProjectOf(ctx, h.Store, run.PlanID)
	}
	return h.Sender.SendParent(ctx, ParentSend{Thread: thread, LogicalID: logicalID, Text: text, Queue: queue, Kind: kind, Run: h.Run, Project: project})
}

func textOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

// digestOfRows is the digest of what the run came to: its completed operations and calls and the status, and the outcome when the
// finished row carries one, as one outcome= member holding the outcome's JSON (every member by its name: the state, reason, source,
// escalation, milestone, run state and lanes), so a resume that ends with an outcome that differs in any of them is told again. With
// no outcome the input is the one before CRW-1487, so the keys of the notices told before hold.
func digestOfRows(events []engine.Event, status string, outcome *engine.Outcome) string {
	var parts []string
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted {
			parts = append(parts, ev.CallID)
		}
	}
	parts = append(parts, "status="+status)
	if outcome != nil {
		// an Outcome is strings, lanes of strings and a count: it always encodes, its members in their declared order
		b, _ := json.Marshal(outcome)
		parts = append(parts, "outcome="+string(b))
	}
	return shortHash(parts...)
}
