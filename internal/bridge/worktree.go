package bridge

import (
	"context"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/worktrees"
)

type CreateWorktree struct {
	RequestID, Source, Revision, Destination, Mode, Sandbox, Prompt, Title, Model, Effort, ProjectID, Exception, Role string
	Policy                                                                                                            map[string]any
}

func (b *Bridge) CreateWorktreeThread(ctx context.Context, in CreateWorktree) (ledger.Receipt, error) {
	if in.Mode != "bridge-managed-retained" {
		return nil, &Invalid{"Explicit bridge-managed-retained worktree ownership is required"}
	}
	policy := copyMap(in.Policy)
	if err := validateSandboxPolicy(policy); err != nil {
		return nil, err
	}
	modes := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}
	if modes[in.Sandbox] == "" || policy["type"] != modes[in.Sandbox] {
		return nil, &Invalid{"sandbox and expected_sandbox_policy.type must agree"}
	}
	for _, field := range [...][2]string{{in.Source, "source_repository"}, {in.Revision, "starting_revision"}, {in.Destination, "destination"}, {in.Prompt, "prompt"}, {in.Title, "title"}, {in.ProjectID, "app_server_project_id"}} {
		// An empty Go string is Python's None for the optional fields; the three required
		// ones are always checked.
		if field[0] != "" || field[1] == "source_repository" || field[1] == "starting_revision" || field[1] == "destination" {
			if err := nonempty(field[0], field[1], 100000); err != nil {
				return nil, err
			}
		}
	}
	in.Policy = policy
	params := map[string]any{"source_repository": in.Source, "starting_revision": in.Revision, "destination": in.Destination, "worktree_mode": in.Mode, "sandbox": in.Sandbox, "expected_sandbox_policy": in.Policy, "prompt": nil, "title": nil, "model": nil, "reasoning_effort": nil, "app_server_project_id": nil}
	if in.Prompt != "" {
		params["prompt"] = in.Prompt
	}
	if in.Title != "" {
		params["title"] = in.Title
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	if in.Effort != "" {
		params["reasoning_effort"] = in.Effort
	}
	if in.ProjectID != "" {
		params["app_server_project_id"] = in.ProjectID
	}
	if in.Exception != "" {
		params["policy_exception"] = in.Exception
	}
	if in.Role != "" {
		params["role"] = in.Role
	}
	var auth execution.Authorized
	var contract settings.Contract
	validate := func() error {
		var err error
		auth, err = b.Policy.Authorize(execution.Input{Model: execution.Optional(in.Model), Effort: execution.Optional(in.Effort), CWD: in.Destination, Exception: in.Exception, Role: in.Role})
		if err != nil {
			return err
		}
		contract = settings.Contract{Sandbox: in.Sandbox, ExpectedPolicy: in.Policy, Model: auth.Model, ReasoningEffort: auth.Effort}
		return contract.Validate()
	}
	return b.mutate(ctx, mutation{in.RequestID, "create_worktree_thread", params, validate, func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		checkpoint := func(phase string) error {
			receipt["phase"] = phase
			_, err := b.Ledger.Save(context.WithoutCancel(ctx), receipt)
			return err
		}
		receipt["executionPolicy"] = auth.Receipt
		receipt["requestedCheckout"] = in.Destination
		receipt["initialPrompt"] = map[string]any{"state": "not_requested"}
		if in.Prompt != "" {
			receipt["initialPrompt"] = map[string]any{"state": "not_sent"}
		}
		receipt["desktopProjectAssociation"] = map[string]any{"status": "unverified", "sourceRepository": in.Source}
		if err := checkpoint("validating"); err != nil {
			return err
		}
		w, err := worktrees.Validate(ctx, in.Source, in.Revision, in.Destination)
		if err != nil {
			return err
		}
		if in.ProjectID != "" {
			if _, err = b.call(ctx, "project/read", map[string]any{"projectId": in.ProjectID}); err != nil {
				return err
			}
		}
		// Recovery is demanded from here, not above: everything above only asks questions, and
		// from this line on the destination can exist.
		receipt["worktree"] = w.Receipt()
		receipt["recoveryRequired"] = true
		receipt["recovery"] = worktreeRecovery
		if err = checkpoint("reserving_destination"); err != nil {
			return err
		}
		*effects = append(*effects, "worktree/reserve")
		if err = w.Reserve(); err != nil {
			return err
		}
		receipt["worktree"].(map[string]any)["state"] = "reserved"
		if err = checkpoint("creating_worktree"); err != nil {
			return err
		}
		*effects = append(*effects, "worktree/create")
		if err = w.Create(ctx); err != nil {
			return err
		}
		receipt["worktree"].(map[string]any)["state"] = "registered"
		if err = checkpoint("checking_out_worktree"); err != nil {
			return err
		}
		*effects = append(*effects, "worktree/checkout")
		if err = w.Checkout(ctx); err != nil {
			return err
		}
		receipt["worktree"].(map[string]any)["state"] = "created"
		if err = checkpoint("checking_worktree"); err != nil {
			return err
		}
		actual, err := w.Inspect(ctx)
		if err != nil {
			return err
		}
		receipt["worktree"].(map[string]any)["checkout"] = actual.Checkout
		receipt["worktree"].(map[string]any)["initialRevision"] = actual.InitialRevision
		receipt["worktree"].(map[string]any)["gitCommonDirectory"] = actual.GitCommonDirectory
		receipt["worktree"].(map[string]any)["detached"] = actual.Detached
		receipt["worktree"].(map[string]any)["clean"] = actual.Clean
		if err = checkpoint("worktree_checked"); err != nil {
			return err
		}
		if !w.Matches(ctx, actual) {
			return &Invalid{"Worktree placement/base mismatch; initial prompt withheld"}
		}
		launch := map[string]any{"cwd": w.Destination, "sandbox": in.Sandbox, "approvalPolicy": "never", "ephemeral": false, "runtimeWorkspaceRoots": []string{w.Destination}, "model": auth.Model}
		if config := contract.Config(); len(config) > 0 {
			launch["config"] = config
		}
		if in.ProjectID != "" {
			launch["projectId"] = in.ProjectID
		}
		if err = checkpoint("creating_thread"); err != nil {
			return err
		}
		created, err := b.dispatch(ctx, "thread/start", launch, effects)
		if err != nil {
			return err
		}
		threadID := id(created, "thread")
		placed := settings.Contract{CWD: w.Destination, Sandbox: in.Sandbox, ExpectedPolicy: in.Policy, Model: auth.Model, ReasoningEffort: auth.Effort, Roots: []string{w.Destination}}
		receipt["threadId"] = threadID
		receipt["permissionReceipt"] = map[string]any{"approvalPolicy": created["approvalPolicy"], "sandbox": created["sandbox"], "activePermissionProfile": created["activePermissionProfile"], "runtimeWorkspaceRoots": created["runtimeWorkspaceRoots"]}
		receipt["desktopProjectAssociation"] = map[string]any{"status": "unverified", "sourceRepository": in.Source, "checkout": created["cwd"], "appServerProjectId": object(created["thread"])["projectId"]}
		receipt["creation"] = created
		receipt["settings"] = placed.Receipt(created, "creation")
		if err = checkpoint("checking_environment"); err != nil {
			return err
		}
		if findings := placed.Findings(created); len(findings) > 0 {
			first := findings[0]
			return &worktrees.Error{Reason: findingText(first.Code, first.Field, first.Returned, first.Expected) + "; initial prompt withheld. Inspect creation receipt"}
		}
		if text(object(created["thread"])["cwd"]) != w.Destination || (in.ProjectID != "" && text(object(created["thread"])["projectId"]) != in.ProjectID) {
			return &Invalid{"Created thread placement differs; initial prompt withheld. Inspect creation receipt"}
		}
		if in.Title != "" {
			if err = checkpoint("naming_thread"); err != nil {
				return err
			}
			if _, err = b.dispatch(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": in.Title}, effects); err != nil {
				return err
			}
			receipt["title"] = in.Title
			if err = checkpoint("thread_named"); err != nil {
				return err
			}
		}
		before, err := w.Inspect(ctx)
		if err != nil {
			return err
		}
		receipt["checkoutBeforeDispatch"] = map[string]any{"checkout": before.Checkout, "initialRevision": before.InitialRevision, "gitCommonDirectory": before.GitCommonDirectory, "detached": before.Detached, "clean": before.Clean}
		if err = checkpoint("checking_before_dispatch"); err != nil {
			return err
		}
		if !w.Matches(ctx, before) {
			return &Invalid{"Worktree changed during thread startup; initial prompt withheld"}
		}
		if in.Prompt != "" {
			// Written before the frame goes out, so a process killed mid-dispatch cannot leave a
			// receipt claiming the prompt was withheld; reconcile corrects it once the operation
			// ends and there is evidence.
			receipt["initialPrompt"] = map[string]any{"state": "outcome_unknown"}
			if err = checkpoint("dispatching_initial_prompt"); err != nil {
				return err
			}
			turn, err := b.dispatch(ctx, "turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": in.Prompt}}}, effects)
			if err != nil {
				return err
			}
			receipt["turnId"] = id(turn, "turn")
			receipt["initialPrompt"] = map[string]any{"state": "accepted"}
			if err = checkpoint("initial_prompt_accepted"); err != nil {
				return err
			}
		}
		receipt["recoveryRequired"] = false
		return checkpoint("complete")
	}, nil, func(receipt ledger.Receipt) {
		// bridge.py reconcile: the prompt's advance outcome_unknown is corrected by what went out.
		if in.Prompt == "" || object(receipt["initialPrompt"])["state"] != "outcome_unknown" {
			return
		}
		effects, _ := receipt["attemptedEffects"].([]string)
		if !slices.Contains(effects, "turn/start") {
			receipt["initialPrompt"] = map[string]any{"state": "not_sent"}
		} else if receipt["status"] == "failed" {
			receipt["initialPrompt"] = map[string]any{"state": "rejected"}
		}
	}})
}

const worktreeRecovery = "Inspect this receipt, the destination and Git worktree list, and backend/Desktop tasks before manual recovery. Retain all artifacts; do not retry with a new request ID. Unknown thread/turn outcomes need reconciliation."
