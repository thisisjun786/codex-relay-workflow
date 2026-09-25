// Package bridge implements the Codex thread bridge operations independently of MCP.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/worktrees"
)

type RPC interface {
	Call(context.Context, string, map[string]any) (json.RawMessage, error)
}
type ExecutionPolicy interface {
	Authorize(execution.Input) (execution.Authorized, error)
	Summary() map[string]any
}

type Bridge struct {
	RPC    RPC
	Ledger *ledger.Ledger
	Policy ExecutionPolicy
	mu     sync.Mutex
	// waiting, when set by a test, runs as a mutation starts to wait for the mutation lock.
	waiting func(requestID string)
}

func New(rpc RPC, store *ledger.Ledger, policy ExecutionPolicy) *Bridge {
	return &Bridge{RPC: rpc, Ledger: store, Policy: policy}
}

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }
func nonempty(value, name string, max int) error {
	if strings.TrimSpace(value) == "" || len(value) > max {
		return &Invalid{fmt.Sprintf("%s must contain 1–%d characters", name, max)}
	}
	return nil
}
func directory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", &Invalid{"cwd must be an existing absolute directory on the App Server host"}
	}
	absolute, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", &Invalid{"cwd must be an existing absolute directory on the App Server host"}
	}
	return absolute, nil
}
func (b *Bridge) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	raw, err := b.RPC.Call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	if roots, ok := result["runtimeWorkspaceRoots"].([]any); ok {
		typed := make([]string, 0, len(roots))
		for _, root := range roots {
			value, ok := root.(string)
			if !ok {
				break
			}
			typed = append(typed, value)
		}
		if len(typed) == len(roots) {
			result["runtimeWorkspaceRoots"] = typed
		}
	}
	return result, nil
}
func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func text(value any) string           { result, _ := value.(string); return result }
func copyMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for k, v := range source {
		result[k] = v
	}
	return result
}
func id(response map[string]any, entity string) string { return text(object(response[entity])["id"]) }

// errorText renders an error as Python's f"{type(error).__name__}: {error}": the bare type
// name of the innermost non-wrapper error, so receipts start with e.g. "ResponseTooLarge:".
func errorText(err error) string {
	named := err
	for e := err; e != nil; e = errors.Unwrap(e) {
		named = e
		if name := fmt.Sprintf("%T", e); name != "*fmt.wrapError" && name != "*fmt.wrapErrors" {
			break
		}
	}
	name := strings.TrimPrefix(fmt.Sprintf("%T", named), "*")
	return name[strings.LastIndexByte(name, '.')+1:] + ": " + err.Error()
}
func (b *Bridge) GetOperation(ctx context.Context, requestID string) (ledger.Receipt, error) {
	return b.Ledger.Get(ctx, requestID)
}

type mutation struct {
	requestID, method string
	params            map[string]any
	validate          func() error
	action            func(context.Context, ledger.Receipt, *[]string) error
	legacy            func() map[string]any
	// reconcile corrects fields an action wrote in advance, once status and effects exist.
	reconcile func(ledger.Receipt)
}

func (b *Bridge) mutate(ctx context.Context, op mutation) (ledger.Receipt, error) {
	if b.waiting != nil {
		b.waiting(op.requestID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	retained, err := b.Ledger.Lookup(ctx, op.requestID, op.method, op.params, op.legacy)
	if err != nil {
		return nil, err
	}
	if retained != nil && retained["status"] != "not_attempted" {
		replay := copyMap(retained)
		replay["replayed"] = true
		return replay, nil
	}
	if err := op.validate(); err != nil {
		return nil, err
	}
	fresh, receipt, err := b.Ledger.Begin(ctx, op.requestID, op.method, op.params, op.legacy)
	if err != nil {
		return nil, err
	}
	if !fresh {
		receipt["replayed"] = true
		return receipt, nil
	}
	effects := []string{}
	tracked := appserver.WithSendHook(ctx, func(method string) {
		switch method {
		case "initialize", "project/read", "thread/read", "thread/list", "thread/turns/list", "thread/items/list", "thread/goal/get":
			// A lost observation cannot imply that a mutation reached the host.
		default:
			effects = append(effects, method)
		}
	})
	err = op.action(tracked, receipt, &effects)
	switch {
	case err == nil:
		receipt["status"] = "accepted"
	default:
		var rpc *appserver.RPCError
		var local *Invalid
		var gitFailure *worktrees.Error
		if errors.As(err, &rpc) {
			receipt["status"] = "failed"
			receipt["error"] = err.Error()
			if rpc.Object != nil {
				receipt["rpcError"] = rpc.Object
			} else {
				receipt["rpcError"] = map[string]any{"code": rpc.Code, "message": rpc.Message}
			}
		} else if errors.As(err, &local) || errors.As(err, &gitFailure) {
			receipt["status"] = "failed"
			receipt["error"] = err.Error()
		} else if len(effects) == 0 {
			receipt["status"] = "not_attempted"
			receipt["retrySafe"] = true
			receipt["error"] = errorText(err)
		} else {
			receipt["status"] = "outcome_unknown"
			receipt["retrySafe"] = false
			receipt["error"] = errorText(err)
		}
	}
	receipt["attemptedEffects"] = effects
	if op.reconcile != nil {
		op.reconcile(receipt)
	}
	if op.method == "send_message_to_thread" {
		delivery := "not_delivered"
		if slices.Contains(effects, "turn/start") {
			switch receipt["status"] {
			case "accepted":
				delivery = "turn_started"
			case "failed":
				delivery = "rejected"
			default:
				delivery = "outcome_unknown"
			}
		}
		receipt["delivery"] = delivery
		receipt["deliveryMeaning"] = map[string]string{
			"not_delivered":   "No turn/start left this process, so this message was not delivered and nothing on the host is holding it.",
			"turn_started":    "The host accepted this message into a new turn. That is delivery, not completion: it does not say the peer read it, acted on it, or finished anything.",
			"rejected":        "The turn/start went out and the host refused it. The message was not delivered.",
			"outcome_unknown": "A turn/start went out and no answer came back. It may have started a turn. Do not send this message again.",
		}[delivery]
	}
	// Cancellation must not prevent the outcome classification from being durable.
	settled, saveErr := b.Ledger.Save(context.WithoutCancel(ctx), receipt)
	if saveErr == nil && errors.Is(err, context.Canceled) {
		// Recorded first, then propagated, as Python re-raises CancelledError after saving.
		return settled, err
	}
	if saveErr != nil || err != nil || settled["status"] != "accepted" || settled["turnId"] == nil {
		return settled, saveErr
	}
	contract, ok := settled["settings"].(map[string]any)
	if !ok {
		return settled, nil
	}
	state, readErr := b.call(ctx, "thread/read", map[string]any{"threadId": settled["threadId"], "includeTurns": false})
	if readErr != nil {
		if ctx.Err() != nil {
			return settled, ctx.Err()
		}
		return settled, nil
	}
	settled["settingsAfterDispatch"] = settings.Annotation(object(contract["actual"]), object(state["thread"]))
	return b.Ledger.Save(context.WithoutCancel(ctx), settled)
}
func (b *Bridge) dispatch(ctx context.Context, method string, params map[string]any, effects *[]string) (map[string]any, error) {
	return b.call(ctx, method, params)
}
