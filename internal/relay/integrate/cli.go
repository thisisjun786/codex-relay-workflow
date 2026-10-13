package integrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay commands of the integration batch (CRW-965). dag-integrate merges the plan's ready accepted candidates onto
// a local integration branch and verifies the merged tree; dag-integrate-push moves the remote branch to that commit by a
// fast-forward. Neither opens a pull request or touches the merge lane.

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
	argv, err := splitCommand(args.Text("verify"))
	if err != nil {
		return nil, err
	}
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	sched := &dagsched.Scheduler{Store: s, ExpectedEpoch: epoch}
	result, err := sched.IntegrateBatch(ctx, dagsched.IntegrationBatchInput{
		Plan: args.Text("plan"), Actor: args.Text("actor"), Checkout: args.Text("checkout"),
		IntegrationRef: args.Text("integration-ref"), BaseRef: args.Text("base"), Nodes: args.Strings("node"),
		RetryNotProven: args.Bool("retry-not-proven"),
	}, dagsched.IntegrationDeps(dagsched.CommandVerifier(argv)))
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
	pending := make([]any, len(result.Pending))
	for i, p := range result.Pending {
		pending[i] = p
	}
	targets := make([]any, len(result.Targets))
	for i, t := range result.Targets {
		targets[i] = t
	}
	events := make([]any, len(result.MarkedEvents))
	for i, e := range result.MarkedEvents {
		events[i] = e
	}
	already := make([]any, len(result.AlreadyContained))
	for i, c := range result.AlreadyContained {
		already[i] = contract.OrderedObject{{Key: "node_id", Value: c.NodeID}, {Key: "acceptance_id", Value: c.AcceptanceID}, {Key: "head_sha", Value: c.HeadSHA}, {Key: "contained_in", Value: c.ContainedIn}, {Key: "marked_event", Value: c.MarkedEvent}, {Key: "reason", Value: c.Reason}}
	}
	unverified := make([]any, len(result.ContainedUnverified))
	for i, c := range result.ContainedUnverified {
		unverified[i] = contract.OrderedObject{{Key: "node_id", Value: c.NodeID}, {Key: "acceptance_id", Value: c.AcceptanceID}, {Key: "head_sha", Value: c.HeadSHA}, {Key: "contained_in", Value: c.ContainedIn}}
	}
	reconciled := make([]any, len(result.Reconciled))
	for i, b := range result.Reconciled {
		reconciled[i] = b
	}
	abandoned := make([]any, len(result.Abandoned))
	for i, b := range result.Abandoned {
		abandoned[i] = b
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaIntegrate}, {Key: "plan_id", Value: result.Plan}, {Key: "batch_id", Value: result.BatchID},
		{Key: "checkout", Value: result.Checkout}, {Key: "integration_ref", Value: result.Ref}, {Key: "base_ref", Value: result.BaseRef},
		{Key: "old_head", Value: result.OldHead}, {Key: "new_head", Value: result.NewHead},
		{Key: "merged", Value: merged}, {Key: "split", Value: split}, {Key: "pending_marks", Value: pending},
		{Key: "verification", Value: contract.OrderedObject{{Key: "result", Value: result.Verification.Result}, {Key: "tree", Value: result.Verification.TreeHash}, {Key: "digest", Value: result.VerificationDigest}}},
		{Key: "marked_events", Value: events}, {Key: "already_contained", Value: already}, {Key: "contained_unverified", Value: unverified}, {Key: "reconciled_batches", Value: reconciled}, {Key: "abandoned_batches", Value: abandoned}, {Key: "targets", Value: targets},
	}, nil
}

func runPush(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	// the store answers whether the relay moved the branch onto the tip (CRW-965, parent decision D6)
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	sched := &dagsched.Scheduler{Store: s}
	// the remote head the push reads is compared with the heads the relay recorded for the integration ref (CRW-1301)
	result, err := dagsched.PushIntegrationChecked(ctx, args.Text("checkout"), args.Text("remote"), args.Text("remote-ref"), args.Text("integration-ref"), sched.VerifiedMoveOnto, sched.StaleIntegratedNodes, sched.CheckIntegrationTarget)
	if err != nil {
		return nil, err
	}
	return pushObject(result), nil
}

// pushObject is the document dag-integrate-push prints. stale_nodes is output only (CRW-1026, d2): the integrated nodes of the
// pushed head whose acceptance the plan no longer stands behind, each named by plan, node and acceptance; the checkout and the
// integration ref are the push's own fields. stale_nodes_error is null unless the list could not be read. target_moved is
// present only when the remote held a head the relay never recorded for the integration ref, and target_check_error only when
// the recorded heads could not be read (CRW-1301), so a push on the integration path prints what it printed before.
func pushObject(result dagsched.PushResult) contract.OrderedObject {
	stale := make([]any, len(result.StaleNodes))
	for i, n := range result.StaleNodes {
		stale[i] = contract.OrderedObject{{Key: "plan_id", Value: n.PlanID}, {Key: "node_id", Value: n.NodeID}, {Key: "acceptance_id", Value: n.AcceptanceID},
			{Key: "reason", Value: n.Reason}, {Key: "accepted_criteria_digest", Value: nullableText(n.AcceptedCriteria)}, {Key: "current_criteria_digest", Value: nullableText(n.CurrentCriteria)}}
	}
	doc := contract.OrderedObject{
		{Key: "ok", Value: result.Outcome != dagsched.PushDeferred}, {Key: "schema", Value: dagsched.SchemaIntegrationPush},
		{Key: "remote", Value: result.Remote}, {Key: "remote_ref", Value: result.RemoteRef},
		{Key: "local_head", Value: result.LocalHead}, {Key: "remote_head", Value: result.RemoteHead},
		{Key: "outcome", Value: result.Outcome}, {Key: "detail", Value: result.Detail},
		{Key: "stale_nodes", Value: stale}, {Key: "stale_nodes_error", Value: nullableText(result.StaleNodesError)},
	}
	if m := result.TargetMoved; m != nil {
		expected := make([]any, len(m.Expected))
		for i, head := range m.Expected {
			expected[i] = head
		}
		doc = append(doc, pyjson.Field{Key: "target_moved", Value: contract.OrderedObject{
			{Key: "fault_class", Value: dagsched.ClassIntegrationTargetMoved}, {Key: "fault_id", Value: nullableText(m.FaultID)}, {Key: "fault_error", Value: nullableText(m.FaultError)},
			{Key: "integration_ref", Value: m.IntegrationRef}, {Key: "remote_head", Value: m.RemoteHead},
			{Key: "expected_heads", Value: expected}, {Key: "expected_total", Value: m.ExpectedTotal},
		}})
	}
	if result.TargetCheckError != "" {
		doc = append(doc, pyjson.Field{Key: "target_check_error", Value: result.TargetCheckError})
	}
	return doc
}

// nullableText is a string that prints as null when empty.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// splitCommand splits a command line into an argument vector. Single and double quotes group words and a backslash
// escapes the next character; an unterminated quote or escape, or an empty command, is refused, so a command the caller
// typed is never handed to a shell.
func splitCommand(command string) ([]string, error) {
	var argv []string
	var current strings.Builder
	started := false
	var quote rune
	escaped := false
	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			started = true
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if started {
				argv = append(argv, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, refuse(contract.RefusalMalformedReceipt, "--verify carries an unterminated quote or escape")
	}
	if started {
		argv = append(argv, current.String())
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, refuse(contract.RefusalMalformedReceipt, "--verify names no command")
	}
	return argv, nil
}

// refuse is a refusal under an existing reason, with the relay's exit code 2.
func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &store.RefusedError{Reason: string(reason), Detail: fmt.Sprintf(format, args...)}
}
