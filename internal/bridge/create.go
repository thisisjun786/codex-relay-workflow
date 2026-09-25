package bridge

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"path/filepath"
)

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type CreateThread struct {
	RequestID, CWD, Prompt, Title, Sandbox, Model, ProjectID, Effort, Exception, Role string
	Roots                                                                             []string
	Policy                                                                            map[string]any
}

func (b *Bridge) CreateThread(ctx context.Context, in CreateThread) (ledger.Receipt, error) {
	if err := nonempty(in.CWD, "cwd", 100000); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(in.CWD) {
		return nil, &Invalid{"cwd must be an existing absolute directory on the App Server host"}
	}
	if in.Sandbox != "read-only" && in.Sandbox != "workspace-write" && in.Sandbox != "danger-full-access" {
		return nil, &Invalid{"Unsupported sandbox"}
	}
	for _, pair := range [][3]any{{in.Prompt, "prompt", 100000}, {in.Title, "title", 500}} {
		if value := text(pair[0]); value != "" {
			if err := nonempty(value, pair[1].(string), pair[2].(int)); err != nil {
				return nil, err
			}
		}
	}
	if in.Policy != nil {
		if err := validateSandboxPolicy(in.Policy); err != nil {
			return nil, err
		}
	}
	params := map[string]any{"cwd": in.CWD, "sandbox": in.Sandbox, "approvalPolicy": "never", "ephemeral": false, "prompt": nullable(in.Prompt), "title": nullable(in.Title)}
	if in.Model != "" {
		params["model"] = in.Model
	}
	if in.ProjectID != "" {
		params["projectId"] = in.ProjectID
	}
	if in.Effort != "" {
		params["reasoning_effort"] = in.Effort
	}
	if in.Policy != nil {
		params["expected_sandbox_policy"] = in.Policy
	}
	if in.Roots != nil {
		roots := make([]any, len(in.Roots))
		for i, root := range in.Roots {
			roots[i] = root
		}
		params["runtime_workspace_roots"] = roots
	}
	if in.Exception != "" {
		params["policy_exception"] = in.Exception
	}
	if in.Role != "" {
		params["role"] = in.Role
	}
	var authorized execution.Authorized
	var contract settings.Contract
	var launch map[string]any
	validate := func() error {
		cwd, err := directory(in.CWD)
		if err != nil {
			return err
		}
		authorized, err = b.Policy.Authorize(execution.Input{Model: execution.Optional(in.Model), Effort: execution.Optional(in.Effort), CWD: cwd, Exception: in.Exception, Role: in.Role})
		if err != nil {
			return err
		}
		contract = settings.Contract{CWD: cwd, Sandbox: in.Sandbox, Model: authorized.Model, ReasoningEffort: authorized.Effort, Roots: in.Roots, ExpectedPolicy: in.Policy}
		if err := contract.Validate(); err != nil {
			return err
		}
		launch = map[string]any{"cwd": cwd, "sandbox": in.Sandbox, "approvalPolicy": "never", "ephemeral": false}
		for k, v := range contract.StartParams() {
			launch[k] = v
		}
		if in.ProjectID != "" {
			launch["projectId"] = in.ProjectID
		}
		return nil
	}
	return b.mutate(ctx, mutation{requestID: in.RequestID, method: "create_thread", params: params, validate: validate, action: func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		receipt["executionPolicy"] = authorized.Receipt
		if _, err := b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		if in.ProjectID != "" {
			if _, err := b.call(ctx, "project/read", map[string]any{"projectId": in.ProjectID}); err != nil {
				return err
			}
		}
		created, err := b.dispatch(ctx, "thread/start", launch, effects)
		if err != nil {
			return err
		}
		threadID := id(created, "thread")
		receipt["threadId"] = threadID
		receipt["creation"] = created
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		receipt["settings"] = contract.Receipt(created, "creation")
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		if findings := contract.Findings(created); len(findings) > 0 {
			first := findings[0]
			message := findingText(first.Code, first.Field, first.Returned, first.Expected) + "; initial prompt withheld. Inspect creation receipt. The thread remains retained."
			return &appserver.RPCError{Method: "thread/start", Message: message, Object: map[string]any{"code": first.Code, "message": message}}
		}
		if in.Title != "" {
			if _, err = b.dispatch(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": in.Title}, effects); err != nil {
				return err
			}
			receipt["title"] = in.Title
			if _, err = b.Ledger.Save(ctx, receipt); err != nil {
				return err
			}
		}
		if in.Prompt != "" {
			turn, err := b.dispatch(ctx, "turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": in.Prompt}}}, effects)
			if err != nil {
				return err
			}
			receipt["turnId"] = id(turn, "turn")
		}
		receipt["desktopProjectAssociation"] = "unverified; check Desktop listing"
		return nil
	}, legacy: func() map[string]any {
		old := copyMap(params)
		if resolved, err := filepath.EvalSymlinks(in.CWD); err == nil {
			old["cwd"] = resolved
		}
		return old
	}})
}
