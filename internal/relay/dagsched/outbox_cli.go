package dagsched

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// The seven relay commands of the project summary outbox (docs/relay/dag-outbox.md). Every answer is the relay's JSON envelope; a refusal is exit 2 with an existing reason, an option
// that is missing or a document that cannot be read is a usage error, and a store or progress that disagrees with itself is the host's failure (exit 3). status and reconcile only read:
// they open the store read-only and create nothing.

func init() {
	dispatch.Register(nil,
		dispatch.Command{Name: "dag-summary-enqueue", Run: runSummaryEnqueue},
		dispatch.Command{Name: "dag-summary-status", ReadOnly: true, Run: runSummaryStatus},
		dispatch.Command{Name: "dag-summary-claim", Run: runSummaryClaim},
		dispatch.Command{Name: "dag-summary-reconcile", ReadOnly: true, Run: runSummaryReconcile},
		dispatch.Command{Name: "dag-summary-complete", Run: runSummaryComplete},
		dispatch.Command{Name: "dag-summary-fail", Run: runSummaryFail},
		dispatch.Command{Name: "dag-summary-retry", Run: runSummaryRetry},
	)
}

// summaryFailure is the host's failure for a progress that cannot be printed honestly, and hostFailure for a plan that disagrees with itself.
func summaryFailure(err error) error {
	var broken *InvariantError
	if errors.As(err, &broken) {
		return dispatch.Host(broken.Error())
	}
	return hostFailure(err)
}

// entryObject is an entry as the commands print it: its identity, its state and its history, without the summary text (the claim answers the block).
func entryObject(e SummaryEntry) contract.OrderedObject {
	return contract.OrderedObject{{Key: "summary_id", Value: e.SummaryID}, {Key: "plan_id", Value: e.PlanID}, {Key: "project_key", Value: e.ProjectKey}, {Key: "document", Value: e.Document},
		{Key: "plan_revision", Value: e.PlanRevision}, {Key: "seq", Value: e.Seq}, {Key: "subject_digest", Value: e.SubjectDigest}, {Key: "state_digest", Value: e.StateDigest},
		{Key: "summary_sha256", Value: e.SummarySHA256}, {Key: "state", Value: e.State}, {Key: "attempts", Value: e.Attempts}, {Key: "last_error", Value: optionalText(e.LastError)},
		{Key: "claimed_by", Value: optionalText(e.ClaimedBy)}, {Key: "claimed_at", Value: optionalText(e.ClaimedAt)}, {Key: "confirmed_at", Value: optionalText(e.ConfirmedAt)},
		{Key: "enqueued_by", Value: e.EnqueuedBy}, {Key: "coordinator_epoch", Value: e.CoordinatorEpoch}, {Key: "created_at", Value: e.CreatedAt}, {Key: "updated_at", Value: e.UpdatedAt}}
}

func runSummaryEnqueue(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	result, err := sched.EnqueueSummary(ctx, args.Text("plan"), args.Text("actor"), args.Text("document"), args.Bool("again"))
	if err != nil {
		return nil, summaryFailure(err)
	}
	superseded := make([]any, len(result.Superseded))
	for i, id := range result.Superseded {
		superseded[i] = id
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-enqueue/1"}, {Key: "replayed", Value: result.Replayed},
		{Key: "superseded", Value: superseded}, {Key: "entry", Value: entryObject(result.Entry)}}, nil
}

func runSummaryStatus(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	s, err := openProgressStore(ctx, services.Selection.DBPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	status, err := (&Scheduler{Store: s}).SummaryStatus(ctx, args.Text("plan"), args.Text("document"), args.Bool("history"))
	if err != nil {
		return nil, summaryFailure(err)
	}
	documents := make([]any, len(status.Streams))
	for i, st := range status.Streams {
		counts := contract.OrderedObject{}
		for _, state := range []string{SummaryPending, SummaryClaimed, SummaryConfirmed, SummaryFailed, SummarySuperseded} {
			counts = append(counts, contract.Field{Key: state, Value: st.Counts[state]})
		}
		o := contract.OrderedObject{{Key: "document", Value: st.Document}, {Key: "owed", Value: st.Owed}, {Key: "up_to_date", Value: st.UpToDate}, {Key: "newest", Value: entryObject(st.Newest)},
			{Key: "counts", Value: counts}}
		if args.Bool("history") {
			entries := make([]any, len(st.History))
			for j, e := range st.History {
				entries[j] = entryObject(e)
			}
			o = append(o, contract.Field{Key: "entries", Value: entries})
		}
		documents[i] = o
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-status/1"}, {Key: "plan_id", Value: status.PlanID}, {Key: "project_key", Value: status.ProjectKey},
		{Key: "plan_revision", Value: status.PlanRevision}, {Key: "progress_digest", Value: status.ProgressDigest}, {Key: "documents", Value: documents}}, nil
}

func runSummaryClaim(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	claim, err := sched.ClaimSummary(ctx, args.Text("summary"), args.Text("actor"))
	if err != nil {
		return nil, summaryFailure(err)
	}
	op := claim.Operation
	protocol := make([]any, len(op.Protocol))
	for i, step := range op.Protocol {
		protocol[i] = step
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-claim/1"}, {Key: "claim_token", Value: claim.Token}, {Key: "entry", Value: entryObject(claim.Entry)},
		{Key: "operation", Value: contract.OrderedObject{{Key: "document", Value: op.Document}, {Key: "container_start", Value: op.ContainerStart}, {Key: "container_end", Value: op.ContainerEnd},
			{Key: "empty_container", Value: op.EmptyContainer}, {Key: "block", Value: op.Block}, {Key: "container", Value: op.Container}, {Key: "protocol", Value: protocol}}}}, nil
}

func runSummaryReconcile(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	observed, err := readDocument(args.Text("observed"))
	if err != nil {
		return nil, err
	}
	s, err := openProgressStore(ctx, services.Selection.DBPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	r, err := (&Scheduler{Store: s}).ReconcileSummary(ctx, args.Text("summary"), string(observed))
	if err != nil {
		return nil, summaryFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-reconcile/1"}, {Key: "summary_id", Value: r.SummaryID}, {Key: "state", Value: r.State},
		{Key: "outcome", Value: r.Outcome}, {Key: "detail", Value: r.Detail}, {Key: "writable", Value: r.Writable}, {Key: "again", Value: r.Again}, {Key: "container", Value: r.Container},
		{Key: "document_summary_id", Value: optionalText(r.DocumentSummaryID)}, {Key: "document_seq", Value: optionalSeq(r.DocumentSeq)}, {Key: "relation", Value: optionalText(r.Relation)},
		{Key: "previous_block", Value: optionalText(r.PreviousBlock)}}, nil
}

func optionalSeq(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func runSummaryComplete(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	readback, err := readDocument(args.Text("readback"))
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	done, err := sched.CompleteSummary(ctx, args.Text("summary"), args.Text("actor"), args.Text("claim-token"), args.Text("document"), string(readback))
	if err != nil {
		return nil, summaryFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-complete/1"}, {Key: "replayed", Value: done.Replayed}, {Key: "entry", Value: entryObject(done.Entry)}}, nil
}

func runSummaryFail(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	entry, err := sched.FailSummary(ctx, args.Text("summary"), args.Text("actor"), args.Text("claim-token"), args.Text("error"))
	if err != nil {
		return nil, summaryFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-fail/1"}, {Key: "entry", Value: entryObject(entry)}}, nil
}

func runSummaryRetry(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	retried, err := sched.RetrySummary(ctx, args.Text("summary"), args.Text("actor"))
	if err != nil {
		return nil, summaryFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-summary-retry/1"}, {Key: "replayed", Value: retried.Replayed}, {Key: "entry", Value: entryObject(retried.Entry)}}, nil
}
