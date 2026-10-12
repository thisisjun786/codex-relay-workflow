package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the close of the reviewer threads of a milestone (CRW-1206, F13): the audit of every acceptance the milestone
// rests on (internal/flow/audit), and the archive of the threads of the acceptances that passed it. A reviewer thread is a
// persistent thread of the host, so it can be read again - which is the whole point of the audit - and it is archived only after
// the audit has read it: an acceptance whose audit is mismatch or unobserved keeps its thread.

// ---- crw.audit ----

// auditOp is crw.audit(milestone): it audits each acceptance of the milestone's nodes against the host and records the results.
// The row is the evidence the archive reads. A host that is not wired, or does not answer, is not a failure of the run: the
// results are unobserved.
type auditOp struct{ l *Lane }

// AuditOutcome is the answer of crw.audit.
type AuditOutcome struct {
	Milestone  string         `json:"milestone"`
	Results    []audit.Result `json:"results"`
	OK         int            `json:"ok"`
	Mismatch   int            `json:"mismatch"`
	Unobserved int            `json:"unobserved"`
}

func (o *auditOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil || ms == "" {
		return engine.OpResult{}, fmt.Errorf("crw.audit(milestone): a milestone is required: %v", err)
	}
	events := req.Journal.Events()
	var reader audit.Reader
	if o.l.d.Threads != nil {
		reader = o.l.d.Threads
	}
	results := []audit.Result{}
	for _, s := range audit.Subjects(events) {
		if s.Milestone == ms {
			results = append(results, audit.Audit(ctx, reader, events, s))
		}
	}
	sum := audit.Summarize(results)
	out := AuditOutcome{Milestone: ms, Results: results, OK: sum.OK, Mismatch: sum.Mismatch, Unobserved: sum.Unobserved}
	data := audit.RowData(ms, results)
	delete(data, "result")
	return engine.OpResult{Result: result(out), Data: data}, nil
}

func (o *auditOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.archive ----

// archiveOp is crw.archive(milestone): it archives the reviewer thread of each acceptance of the milestone that has an audit
// pass row in the journal, and no other. It reads the rows, not what the script says. A thread already archived is not archived
// again, and an archive the host refuses is recorded and does not stop the close (the thread is kept, and the next close tries).
type archiveOp struct{ l *Lane }

// ArchivedThread is one thread the close archived or kept.
type ArchivedThread struct {
	AcceptanceID string `json:"acceptance_id"`
	Node         string `json:"node"`
	Thread       string `json:"thread"`
	Reason       string `json:"reason"`
}

// ArchiveOutcome is the answer of crw.archive.
type ArchiveOutcome struct {
	Milestone string           `json:"milestone"`
	Archived  []ArchivedThread `json:"archived"`
	Kept      []ArchivedThread `json:"kept"`
}

func (o *archiveOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil || ms == "" {
		return engine.OpResult{}, fmt.Errorf("crw.archive(milestone): a milestone is required: %v", err)
	}
	events := req.Journal.Events()
	passed := audit.Passed(events)
	already := archivedThreads(events)
	out := ArchiveOutcome{Milestone: ms, Archived: []ArchivedThread{}, Kept: []ArchivedThread{}}
	for _, s := range audit.Subjects(events) {
		if s.Milestone != ms {
			continue
		}
		pass, ok := passed[s.AcceptanceID]
		entry := ArchivedThread{AcceptanceID: s.AcceptanceID, Node: s.Node, Thread: pass.Thread}
		switch {
		case !ok:
			entry.Thread = ""
			entry.Reason = "no audit pass row"
			out.Kept = append(out.Kept, entry)
		case already[pass.Thread]:
			entry.Reason = "already archived"
			out.Kept = append(out.Kept, entry)
		case o.l.d.Threads == nil:
			entry.Reason = "no host is wired"
			out.Kept = append(out.Kept, entry)
		default:
			if err := o.l.d.Threads.ArchiveThread(ctx, pass.Thread); err != nil {
				entry.Reason = "the host refused: " + err.Error()
				out.Kept = append(out.Kept, entry)
				continue
			}
			already[pass.Thread] = true
			out.Archived = append(out.Archived, entry)
		}
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "archived": out.Archived, "kept": len(out.Kept)}}, nil
}

func (o *archiveOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// archivedThreads are the threads an earlier crw.archive row archived.
func archivedThreads(events []engine.Event) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != OpArchive || ev.Event != engine.EvCompleted {
			continue
		}
		raw, err := json.Marshal(ev.Data["archived"])
		if err != nil {
			continue
		}
		var list []ArchivedThread
		if json.Unmarshal(raw, &list) == nil {
			for _, a := range list {
				out[a.Thread] = true
			}
		}
	}
	return out
}
