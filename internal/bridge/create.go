package bridge

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"path/filepath"
	"sync"
	"time"
)

// createAckBound is the acknowledgement bound every App Server call of the create path uses. Under
// load the host's answers to thread/start, thread/name/set and turn/start, and to the profile
// reads this path makes (config/read, plugin/installed, mcpServerStatus/list), have taken longer
// than the default 20 s: three of about eighty creations on 2026-10-06 hit it, and one answered
// after 25 s. A creation whose answer is lost leaves an outcome_unknown receipt although the host
// did the work, so the create path waits longer than every other caller of the client.
const createAckBound = 60 * time.Second

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type CreateThread struct {
	RequestID, CWD, Prompt, Title, Sandbox, Model, ProjectID, Effort, Exception, Role, MCPProfile string
	Roots                                                                                         []string
	Policy                                                                                        map[string]any
}

// CreationWatch is the turn watch a creation opened on its thread, handed to the caller instead of being let go
// as the creation returns (CreateThreadWatched). The watch holds the thread's mutation gate and its
// subscription until Release, so the caller can wait for the standby turn's turn/completed on the connection
// that carried the creation.
type CreationWatch struct {
	watch   *appserver.TurnWatch
	receipt ledger.Receipt
	once    sync.Once
}

// TurnCompleted reports whether the watch has received turn/completed for turn.
func (w *CreationWatch) TurnCompleted(turn string) bool { return w.watch.Completed(turn) }

// Release lets the watch go, as the creation does when nobody keeps it. It is idempotent.
func (w *CreationWatch) Release() {
	w.once.Do(func() { finishSubscription(w.watch, w.receipt, true) })
}

type keepWatchKey struct{}

// CreateThreadWatched is CreateThread that keeps the creation's turn watch open: the returned CreationWatch is
// nil when the creation opened none (a scripted transport) or failed, and otherwise the caller releases it.
func (b *Bridge) CreateThreadWatched(ctx context.Context, in CreateThread) (ledger.Receipt, *CreationWatch, error) {
	keep := &CreationWatch{}
	receipt, err := b.CreateThread(context.WithValue(ctx, keepWatchKey{}, keep), in)
	if keep.watch == nil {
		return receipt, nil, err
	}
	return receipt, keep, err
}

func (b *Bridge) CreateThread(ctx context.Context, in CreateThread) (receipt ledger.Receipt, failure error) {
	var watch *appserver.TurnWatch
	var result ledger.Receipt
	defer func() {
		// Only an accepted creation hands its watch over: a creation that failed or is unknown goes on to the standby recovery, which
		// takes the thread's gate itself.
		if keep, ok := ctx.Value(keepWatchKey{}).(*CreationWatch); ok && watch != nil && failure == nil && result["status"] == "accepted" {
			keep.watch, keep.receipt = watch, result
			return
		}
		finishSubscription(watch, result, true)
	}()
	// One bound on this path's context covers every call it makes, before and inside the mutation.
	ctx = appserver.WithAckBound(ctx, createAckBound)
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
		if value := pyjson.Text(pair[0]); value != "" {
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
	if in.MCPProfile != "" {
		params["mcp_profile"] = in.MCPProfile
	}
	var authorized execution.Authorized
	var contract settings.Contract
	var selection execution.MCPSelection
	var launch map[string]any
	validate := func() error {
		cwd, err := directory(in.CWD)
		if err != nil {
			return err
		}
		// CRW-1265: a sandboxed thread never starts in the account home, above it or above a project
		// root. Nothing has been written or sent yet, so the request ID stays usable.
		if err := sandboxroot.CheckAll(cwd, absolute(writableRoots(in.Roots, in.Policy)), effectiveSandbox(in.Sandbox, in.Policy)); err != nil {
			return err
		}
		authorized, err = b.Policy.Authorize(execution.Input{Model: execution.Optional(in.Model), Effort: execution.Optional(in.Effort), CWD: cwd, Exception: in.Exception, Role: in.Role})
		if err != nil {
			return err
		}
		contract = settings.Contract{CWD: cwd, Sandbox: in.Sandbox, Model: authorized.Model, ReasoningEffort: authorized.Effort, Roots: in.Roots, ExpectedPolicy: in.Policy, AutoCompactTokenLimit: authorized.AutoCompactTokenLimit}
		if err := contract.Validate(); err != nil {
			return err
		}
		if selection, err = b.selectMCP(in.Role, in.MCPProfile); err != nil {
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
	result, err := b.mutate(ctx, mutation{requestID: in.RequestID, method: "create_thread", params: params, validate: validate, action: func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		receipt["executionPolicy"] = authorized.Receipt
		if _, err := b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		if in.ProjectID != "" {
			if _, err := b.call(ctx, "project/read", map[string]any{"projectId": in.ProjectID}); err != nil {
				return err
			}
		}
		if selection.Name != "" {
			var info map[string]any
			var err error
			if contract.MCP, info, err = ResolveMCP(ctx, b.call, selection, contract.CWD); err != nil {
				return MCPRefusal("config/read", err)
			}
			receipt["mcpProfile"] = info
			launch["config"] = contract.Config()
		}
		created, err := b.dispatch(ctx, "thread/start", launch, effects)
		if err != nil {
			return err
		}
		threadID := id(created, "thread")
		receipt["threadId"] = threadID
		receipt["creation"] = created
		if watch, err = b.watchSubscription(ctx, threadID, true); err != nil {
			return err
		}
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		observed := created
		if contract.MCP != nil {
			if observed, err = b.observeMCP(ctx, contract.MCP, threadID, created, receipt); err != nil {
				return err
			}
		}
		receipt["settings"] = contract.Receipt(observed, "creation")
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		if findings := contract.Findings(observed); len(findings) > 0 {
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
	return result, err
}
