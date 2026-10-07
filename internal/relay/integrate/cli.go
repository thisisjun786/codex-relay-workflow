package integrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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
	}, dagsched.IntegrationBatchDeps{Verify: commandVerifier(argv), Update: updateRef})
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
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaIntegrate}, {Key: "plan_id", Value: result.Plan}, {Key: "batch_id", Value: result.BatchID},
		{Key: "checkout", Value: result.Checkout}, {Key: "integration_ref", Value: result.Ref}, {Key: "base_ref", Value: result.BaseRef},
		{Key: "old_head", Value: result.OldHead}, {Key: "new_head", Value: result.NewHead},
		{Key: "merged", Value: merged}, {Key: "split", Value: split}, {Key: "pending_marks", Value: pending},
		{Key: "verification", Value: contract.OrderedObject{{Key: "result", Value: result.Verification.Result}, {Key: "tree", Value: result.Verification.TreeHash}, {Key: "digest", Value: result.VerificationDigest}}},
		{Key: "marked_events", Value: events}, {Key: "targets", Value: targets},
	}, nil
}

func runPush(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	result, err := dagsched.PushIntegration(ctx, args.Text("checkout"), args.Text("remote"), args.Text("remote-ref"), args.Text("integration-ref"))
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "ok", Value: result.Outcome != dagsched.PushDeferred}, {Key: "schema", Value: dagsched.SchemaIntegrationPush},
		{Key: "remote", Value: result.Remote}, {Key: "remote_ref", Value: result.RemoteRef},
		{Key: "local_head", Value: result.LocalHead}, {Key: "remote_head", Value: result.RemoteHead},
		{Key: "outcome", Value: result.Outcome}, {Key: "detail", Value: result.Detail},
	}, nil
}

// commandVerifier runs the one verification command, given as an argument vector (never a shell), in the directory
// the batch names, with the batch's CRW_VERIFY_* variables added to the inherited environment. A non-zero exit is a
// failing run.
func commandVerifier(argv []string) dagsched.IntegrationBatchVerifier {
	return func(ctx context.Context, dir string, env []string) error {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = dir
		// inherited GIT_* variables would redirect the command's git reads to another repository, so they are dropped
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				// the command never ran: a host failure, not a verification result
				return &dagsched.IntegrationVerifierHostError{Detail: fmt.Sprintf("the verification command could not start: %v", err)}
			}
			return fmt.Errorf("the verification command failed: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
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

// updateRef moves a local branch to newCommit only when it still holds oldCommit: git's own compare-and-swap. The
// all-zero oldCommit creates the branch and refuses when it already exists, so a ref that moved under the batch fails
// the update instead of being overwritten.
func updateRef(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", checkout, "update-ref", "refs/heads/"+ref, newCommit, oldCommit)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git update-ref refs/heads/%s: %v: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
