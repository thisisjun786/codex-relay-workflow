package integrate

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay commands of the integration batch (CRW-965): dag-integrate merges the ready accepted candidates onto a
// local integration branch and dag-integrate-push moves the remote branch to that commit by a fast-forward. Both
// are plain relay commands; neither opens a pull request or touches the merge lane.

// SchemaIntegrate names the document dag-integrate prints.
const SchemaIntegrate = "dag-integrate/1"

func init() {
	dispatch.Register(nil,
		dispatch.Command{Name: "dag-integrate", Run: runIntegrate},
		dispatch.Command{Name: "dag-integrate-push", Run: runPush},
	)
}

func runIntegrate(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	epoch := args.Integer("expect-epoch")
	if epoch < 0 {
		return nil, &dispatch.UsageError{Detail: "--expect-epoch is a whole number, 0 or more", Code: contract.ExitUsage}
	}
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	sched := &dagsched.Scheduler{Store: s, ExpectedEpoch: epoch}
	result, err := Batch(ctx, sched, BatchInput{
		Plan: args.Text("plan"), Actor: args.Text("actor"), Checkout: args.Text("checkout"),
		IntegrationRef: args.Text("integration-ref"), BaseRef: args.Text("base"), Nodes: args.Strings("node"),
		Verify: args.Text("verify"), ExpectEpoch: epoch,
	})
	if err != nil {
		return nil, err
	}
	merged := make([]any, len(result.Merged))
	for i, m := range result.Merged {
		merged[i] = contract.OrderedObject{{Key: "node_id", Value: m.NodeID}, {Key: "acceptance_id", Value: m.AcceptanceID}, {Key: "head_sha", Value: m.HeadSHA}, {Key: "merge_commit", Value: m.MergeCommit}}
	}
	split := make([]any, len(result.Split))
	for i, c := range result.Split {
		split[i] = contract.OrderedObject{{Key: "node_id", Value: c.NodeID}, {Key: "acceptance_id", Value: c.AcceptanceID}, {Key: "head_sha", Value: c.HeadSHA}, {Key: "reason", Value: c.Reason}}
	}
	targets := make([]any, len(result.Targets))
	for i, t := range result.Targets {
		targets[i] = t
	}
	events := make([]any, len(result.MarkedEvents))
	for i, e := range result.MarkedEvents {
		events[i] = e
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaIntegrate}, {Key: "plan_id", Value: result.Plan},
		{Key: "checkout", Value: result.Checkout}, {Key: "integration_ref", Value: result.Ref}, {Key: "base_ref", Value: result.BaseRef},
		{Key: "old_head", Value: result.OldHead}, {Key: "new_head", Value: result.NewHead},
		{Key: "merged", Value: merged}, {Key: "split", Value: split},
		{Key: "verification", Value: contract.OrderedObject{{Key: "result", Value: result.Verification.Result}, {Key: "tree", Value: result.Verification.Tree}, {Key: "digest", Value: result.VerificationDigest}}},
		{Key: "marked_events", Value: events}, {Key: "targets", Value: targets},
	}, nil
}

func runPush(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	result, err := PushIntegration(ctx, args.Text("checkout"), args.Text("remote"), args.Text("remote-ref"), args.Text("integration-ref"))
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "ok", Value: result.Outcome != PushDeferred}, {Key: "schema", Value: SchemaIntegrationPush},
		{Key: "remote", Value: result.Remote}, {Key: "remote_ref", Value: result.RemoteRef},
		{Key: "local_head", Value: result.LocalHead}, {Key: "remote_head", Value: result.RemoteHead},
		{Key: "outcome", Value: result.Outcome}, {Key: "detail", Value: result.Detail},
	}, nil
}
