package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The results a summary_notice_settled row carries.
const (
	// SettledAccepted and SettledQueued: the notice was accepted by the thread, or queued for the pump.
	SettledAccepted = "accepted"
	SettledQueued   = "queued"
	// SettledObsolete: the entry was confirmed or superseded before the notice went; nothing is sent (a newer entry has its own notice).
	SettledObsolete = "obsolete"
	// SettledRefused: the delivery was refused; the run escalates.
	SettledRefused = "refused"
)

// SummaryEnv is what the hand-off of the plan summary works with (CRW-1204). The run owes the document of its plan a summary after every
// committed batch: it enqueues the entry under the orchestrator's task id - the project's registered parent, the one actor the outbox
// lets enqueue and claim - and tells the orchestrator thread that an entry is owed. It never claims: the orchestrator does, with its own
// Linear connector. Deduplication of the entry (the relay's Replayed) and the telling of the thread are separate things: whether the
// thread is told is decided by the rows below, not by Replayed.
//
//	summary_notice (summary id, plan, document, logical id summary-<id>): the intent to tell, once for each summary id
//	summary_notice_settled (summary id, result, request id): accepted or queued, obsolete (the entry was confirmed or superseded first), or
//	  refused (and the run escalates); a delivery whose outcome is unknown or that got no answer writes none and is made again with the
//	  same logical id
type SummaryEnv struct {
	Store *store.Store
	Sched *dagsched.Scheduler
	Rows  Appender
	// Run is the run the rows belong to.
	Run string
	// Plan and Document name the stream; Actor is the orchestrator's task id and its thread.
	Plan, Document, Actor string
	Sender                Sender
	// Wake proves that a send to the orchestrator thread wakes it; nil proves nothing, and the notice is queued.
	Wake func(ctx context.Context, thread string) bool
}

// SummaryOutcome is what one reconcile did.
type SummaryOutcome struct {
	// Enqueued is set when this call asked for an entry (the newest was not up to date); Replayed that the relay answered an entry it held.
	Enqueued, Replayed bool
	SummaryID          string
	// Intents are the notice intents this call wrote, Sent the notices it sent, Settled the settled rows it wrote.
	Intents, Sent, Settled int
}

// SummaryNoticeText is the notice that an entry is owed.
func SummaryNoticeText(id, plan, document string) string {
	return fmt.Sprintf("[crw flow] summary_owed summary=%s plan=%s document=%s\nThe plan summary is owed to the document. You are the parent of the project: take the newest entry (`crw relay dag-summary-status --plan %s`), claim it (`dag-summary-claim`), write the document with your Linear connector, read it back and complete it (`dag-summary-complete`). The run and the manager do not claim.",
		id, plan, document, plan)
}

// ReconcileSummary brings the summary of the stream up to date and tells the orchestrator thread what is owed. It is safe to call at any
// time and as often as wanted: the run calls it when it starts (a resume too) and after each committed batch, `crw flow apply` after each
// verdict, so an entry owed because a process died between two of the steps is found by the next call.
func ReconcileSummary(ctx context.Context, env SummaryEnv) (SummaryOutcome, error) {
	var out SummaryOutcome
	if env.Sched == nil || env.Rows == nil || env.Plan == "" || env.Document == "" || env.Actor == "" {
		return out, errors.New("report: a summary needs the scheduler, the row writer, the plan, the document and the orchestrator")
	}
	stream, found, err := streamOf(ctx, env)
	if err != nil {
		return out, err
	}
	if !found || !stream.UpToDate {
		enq, err := env.Sched.EnqueueSummary(ctx, env.Plan, env.Actor, env.Document, false)
		if err != nil {
			return out, fmt.Errorf("report: enqueue the summary: %w", err)
		}
		out.Enqueued, out.Replayed = true, enq.Replayed
		if stream, found, err = streamOf(ctx, env); err != nil || !found {
			return out, firstError(err, errors.New("report: the summary stream is empty after the enqueue"))
		}
	}
	newest := stream.Newest
	out.SummaryID = newest.SummaryID
	state := map[string]string{}
	for _, e := range stream.History {
		state[e.SummaryID] = e.State
	}
	notices, settled, err := noticeRows(ctx, env)
	if err != nil {
		return out, err
	}
	// (a) the intent: the newest entry is owed and has none
	if owed(newest.State) && !slices.Contains(notices, newest.SummaryID) {
		wrote, _, err := env.Rows.AppendOnce(ctx, NewRow{Kind: KindSummaryNotice, Payload: map[string]any{
			"summary_id": newest.SummaryID, "plan": env.Plan, "document": env.Document, "logical_id": "summary-" + newest.SummaryID, "run": env.Run,
		}}, "summary_id")
		if err != nil {
			return out, err
		}
		if wrote {
			out.Intents++
		}
		notices = append(notices, newest.SummaryID)
	}
	// (b), (c) every intent without a settlement: obsolete, or sent under its recorded logical id
	for _, id := range notices {
		if slices.Contains(settled, id) {
			continue
		}
		if !owed(state[id]) {
			if err := settle(ctx, env, id, SettledObsolete, "", &out); err != nil {
				return out, err
			}
			continue
		}
		// the entry is read again just before the send: an entry that was confirmed or superseded meanwhile is not told
		if fresh, found, err := streamOf(ctx, env); err != nil {
			return out, err
		} else if found {
			current := ""
			for _, e := range fresh.History {
				if e.SummaryID == id {
					current = e.State
				}
			}
			if !owed(current) {
				if err := settle(ctx, env, id, SettledObsolete, "", &out); err != nil {
					return out, err
				}
				continue
			}
		}
		sent, err := env.send(ctx, id)
		out.Sent++
		if err != nil {
			// no answer: no settlement; the next reconcile sends it again under the same logical id
			continue
		}
		switch sent.Class {
		case "accepted":
			err = settle(ctx, env, id, SettledAccepted, sent.RequestID, &out)
		case "queued":
			err = settle(ctx, env, id, SettledQueued, sent.RequestID, &out)
		case "refused":
			if err = settle(ctx, env, id, SettledRefused, sent.RequestID, &out); err == nil {
				_, _, err = env.Rows.AppendOnce(ctx, NewRow{Kind: engine.EvEscalation, Payload: map[string]any{
					"escalation_id": "esc_" + shortHash(env.Run, "summary_notice", id), "reason": "summary_notice_refused", "class": "decision", "summary_id": id,
					"detail": "the notice that the plan summary is owed was refused for thread " + env.Actor + ": " + sent.Reason,
				}}, "escalation_id")
			}
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func owed(state string) bool {
	return state == dagsched.SummaryPending || state == dagsched.SummaryClaimed || state == dagsched.SummaryFailed
}

func streamOf(ctx context.Context, env SummaryEnv) (dagsched.SummaryStream, bool, error) {
	st, err := env.Sched.SummaryStatus(ctx, env.Plan, env.Document, true)
	if err != nil {
		return dagsched.SummaryStream{}, false, fmt.Errorf("report: read the summary status: %w", err)
	}
	for _, s := range st.Streams {
		if s.Document == env.Document {
			return s, true, nil
		}
	}
	return dagsched.SummaryStream{}, false, nil
}

// noticeRows are the summary ids of the stream that have an intent and those that have a settlement, from the rows of every run, each id
// once: a live run and a concurrent `crw flow apply` can both have written the intent of one summary id (the journal's check and its later
// flush are not one transaction), and a duplicate row must neither send twice nor settle twice.
func noticeRows(ctx context.Context, env SummaryEnv) (notices, settled []string, err error) {
	rows, err := ReadRows(ctx, env.Store.DB, "", KindSummaryNotice, KindSummarySettled)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		if r.Str("plan") != env.Plan || r.Str("document") != env.Document {
			continue
		}
		switch r.Kind {
		case KindSummaryNotice:
			if id := r.Str("summary_id"); !slices.Contains(notices, id) {
				notices = append(notices, id)
			}
		case KindSummarySettled:
			if id := r.Str("summary_id"); !slices.Contains(settled, id) {
				settled = append(settled, id)
			}
		}
	}
	return notices, settled, nil
}

func (env SummaryEnv) send(ctx context.Context, id string) (SendResult, error) {
	if env.Sender == nil {
		return SendResult{}, errors.New("report: a sender is required to tell the orchestrator thread")
	}
	queue := true
	if env.Wake != nil && env.Wake(ctx, env.Actor) {
		queue = false
	}
	return env.Sender.SendParent(ctx, ParentSend{Thread: env.Actor, LogicalID: "summary-" + id, Text: SummaryNoticeText(id, env.Plan, env.Document), Queue: queue})
}

func settle(ctx context.Context, env SummaryEnv, id, result, request string, out *SummaryOutcome) error {
	wrote, _, err := env.Rows.AppendOnce(ctx, NewRow{Kind: KindSummarySettled, Payload: map[string]any{
		"summary_id": id, "plan": env.Plan, "document": env.Document, "result": result, "request_id": request, "logical_id": "summary-" + id, "run": env.Run,
	}}, "summary_id")
	if wrote {
		out.Settled++
	}
	return err
}

func firstError(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// SummaryTarget finds the run that keeps the plan summary of a plan for an orchestrator: the newest run of the plan whose actor is the
// orchestrator and that recorded a summary document (`crw flow run --summary-document`). `crw flow apply` reconciles the stream through
// it every time it runs. ok is false when no run recorded one.
func SummaryTarget(ctx context.Context, db DB, plan, actor string) (run, document string, ok bool, err error) {
	var payload string
	err = db.QueryRowContext(ctx, `SELECT e.run_id, e.payload_json FROM flow_events e JOIN flow_runs r ON r.run_id = e.run_id
 WHERE e.kind = ? AND r.plan_id = ? AND r.actor_task_id = ? ORDER BY e.rowid DESC LIMIT 1`, engine.EvSummaryDocument, plan, actor).Scan(&run, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return "", "", false, err
	}
	document, _ = doc["document"].(string)
	return run, document, document != "", nil
}
