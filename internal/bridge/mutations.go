package bridge

import (
	"context"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

// expectedSettingsKeys is bridge.py EXPECTED_SETTINGS_KEYS, sorted as its refusal lists them.
var expectedSettingsKeys = []string{"approval_policy", "cwd", "expected_sandbox_policy", "model", "reasoning_effort", "runtime_workspace_roots", "sandbox"}

func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

type SendMessage struct {
	RequestID, ThreadID, Message, Exception, Role string
	Expected                                      map[string]any
}

func (b *Bridge) SendMessageToThread(ctx context.Context, in SendMessage) (ledger.Receipt, error) {
	if err := nonempty(in.ThreadID, "thread_id", 128); err != nil {
		return nil, err
	}
	if err := nonempty(in.Message, "message", 100000); err != nil {
		return nil, err
	}
	expected := copyMap(in.Expected)
	unknown := []string{}
	for name := range expected {
		if !slices.Contains(expectedSettingsKeys, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, &Invalid{fmt.Sprintf("expected_settings has unknown keys %s; supported keys are %s", pyRepr(anyStrings(unknown)), pyRepr(anyStrings(expectedSettingsKeys)))}
	}
	if policy := object(expected["expected_sandbox_policy"]); policy != nil || expected["expected_sandbox_policy"] != nil {
		if err := validateSandboxPolicy(policy); err != nil {
			return nil, err
		}
	}
	if roots, ok := expected["runtime_workspace_roots"].([]string); ok {
		expected["runtime_workspace_roots"] = anyStrings(roots)
	}
	params := map[string]any{"threadId": in.ThreadID, "message": in.Message}
	if in.Expected != nil {
		params["expected_settings"] = expected
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
		auth, err = b.Policy.Authorize(execution.Input{Model: expected["model"], Effort: expected["reasoning_effort"], CWD: text(expected["cwd"]), Exception: in.Exception, Role: in.Role})
		if err != nil {
			return err
		}
		contract = settings.Contract{CWD: text(expected["cwd"]), Sandbox: text(expected["sandbox"]), ExpectedPolicy: object(expected["expected_sandbox_policy"]), Model: auth.Model, ReasoningEffort: auth.Effort}
		if roots, ok := expected["runtime_workspace_roots"].([]any); ok {
			for _, root := range roots {
				contract.Roots = append(contract.Roots, text(root))
			}
			if contract.Roots == nil {
				contract.Roots = []string{}
			}
		}
		if approval, ok := expected["approval_policy"]; ok {
			contract.ApprovalPolicy = text(approval)
			if approval == nil {
				// Python checks None against the same list as any other undeclarable value.
				return &Invalid{"approval_policy must be one of ['never', 'on-request', 'untrusted']; a granular policy has no name a caller can declare"}
			}
		}
		return contract.Validate()
	}
	return b.mutate(ctx, mutation{in.RequestID, "send_message_to_thread", params, validate, func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		receipt["threadId"] = in.ThreadID
		receipt["executionPolicy"] = auth.Receipt
		// Where this connection's server-request stream stood before anything was sent, so the
		// requests this dispatch met can be told apart from another thread's.
		requests, _ := b.RPC.(serverRequests)
		var mark uint64
		if requests != nil {
			mark = requests.RequestMark()
		}
		if _, err := b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		state, err := b.call(ctx, "thread/read", map[string]any{"threadId": in.ThreadID, "includeTurns": false})
		if err != nil {
			return err
		}
		status := text(object(object(state["thread"])["status"])["type"])
		if status == "active" {
			message := "Thread is active; message withheld. This path starts a new turn and an active thread already has one. To instruct the turn that is running, read get_active_turn and call steer_thread with that turn id. Waiting is for when there is nothing to say yet."
			return &appserver.RPCError{Method: "thread/read", Message: message, Object: map[string]any{"code": "thread_busy", "message": message}}
		}
		receipt["statusBeforeResume"] = status
		if status == "notLoaded" {
			receipt["echoIndependence"] = "not_established"
			if in.Role != "" && auth.Provenance != "role_pair" {
				message := "Thread is not loaded and this request's model and effort were not checked against a declared role pair; message withheld"
				return &appserver.RPCError{Method: "thread/read", Message: message, Object: map[string]any{"code": "unverified_pair_for_unloaded_thread", "message": message}}
			}
		}
		resumed, err := b.dispatch(ctx, "thread/resume", contract.ResumeParams(in.ThreadID), effects)
		if err != nil {
			return err
		}
		receipt["resumed"] = resumed
		receipt["settings"] = contract.Receipt(resumed, "resume")
		receipt["approvals"] = contract.Approvals(resumed)
		if _, err = b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		if findings := contract.Findings(resumed); len(findings) > 0 {
			first := findings[0]
			message := findingText(first.Code, first.Field, first.Returned, first.Expected) + "; message withheld"
			if first.Code == settings.UnsupportedApproval {
				message = fmt.Sprintf("Thread approval policy is %s; this request declared %s. Message withheld and NOT delivered; no turn was started. This bridge preserves a thread's approval policy and never sets one, so the way to deliver here is a NEW request id declaring the policy the thread is actually on. Declaring it does not make this bridge an approver: it answers no approval request, and the thread's own client decides every one the turn raises.", pyRepr(first.Returned), pyRepr(first.Expected))
			}
			return &appserver.RPCError{Method: "thread/resume", Message: message, Object: map[string]any{"code": first.Code, "message": message}}
		}
		turn, err := b.dispatch(ctx, "turn/start", map[string]any{"threadId": in.ThreadID, "input": []any{map[string]any{"type": "text", "text": in.Message}}}, effects)
		if err != nil {
			return err
		}
		receipt["turnId"] = id(turn, "turn")
		// Only the window this receipt actually spans. A turn outlives it, and the note says so.
		if requests != nil {
			receipt["approvalRequests"] = requests.RequestsSince(mark, in.ThreadID)
		}
		return nil
	}, nil, nil})
}

// serverRequests is the part of appserver.Client that records server-to-client requests.
type serverRequests interface {
	RequestMark() uint64
	RequestsSince(mark uint64, threadID string) appserver.RequestReport
}

func (b *Bridge) SteerThread(ctx context.Context, requestID, threadID, turnID, message string) (ledger.Receipt, error) {
	for _, pair := range [][2]string{{threadID, "thread_id"}, {turnID, "expected_turn_id"}, {message, "message"}} {
		max := 128
		if pair[1] == "message" {
			max = 100000
		}
		if err := nonempty(pair[0], pair[1], max); err != nil {
			return nil, err
		}
	}
	params := map[string]any{"threadId": threadID, "expectedTurnId": turnID, "message": message}
	return b.mutate(ctx, mutation{requestID, "steer_thread", params, func() error { return nil }, func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		receipt["threadId"] = threadID
		receipt["expectedTurnId"] = turnID
		receipt["clientUserMessageId"] = "steer:" + requestID
		receipt["settings"] = map[string]any{"verification": "not_observable", "reason": "steer performs no resume"}
		if _, err := b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		state, err := b.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
		if err != nil {
			return err
		}
		status := text(object(object(state["thread"])["status"])["type"])
		if status != "active" {
			code, message := "thread_not_steerable", "Thread status "+status+"; steer withheld."
			switch status {
			case "idle":
				code, message = "thread_idle", "Thread is idle; steer withheld. An idle thread takes send_message_to_thread."
			case "notLoaded":
				code, message = "thread_not_loaded", "Thread is not loaded; steer withheld. Read it again before choosing a delivery path."
			case "systemError":
				code, message = "thread_system_error", "Thread reports a system error; steer withheld and no delivery path is recommended."
			}
			return &appserver.RPCError{Method: "thread/read", Message: message, Object: map[string]any{"code": code, "message": message}}
		}
		response, err := b.dispatch(ctx, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "clientUserMessageId": "steer:" + requestID, "input": []any{map[string]any{"type": "text", "text": message}}}, effects)
		if err != nil {
			return err
		}
		receipt["steeredTurnId"] = response["turnId"]
		if response["turnId"] != turnID {
			message := fmt.Sprintf("turn/steer returned turn %q, not the guarded %q. The instruction cannot be reported as delivered to the observed turn; read the thread again and reclassify.", response["turnId"], turnID)
			return &appserver.RPCError{Method: "turn/steer", Message: message, Object: map[string]any{"code": "steered_turn_mismatch", "message": message}}
		}
		receipt["delivery"] = "accepted_not_applied"
		receipt["deliveryMeaning"] = "The host accepted this input into the guarded turn. It does not say the peer read it, and it does not say the peer acted on it."
		return nil
	}, nil, nil})
}
func (b *Bridge) PauseGoal(ctx context.Context, requestID, threadID string) (ledger.Receipt, error) {
	if err := nonempty(threadID, "thread_id", 128); err != nil {
		return nil, err
	}
	return b.mutate(ctx, mutation{requestID, "pause_goal", map[string]any{"threadId": threadID, "status": "paused"}, func() error { return nil }, func(ctx context.Context, receipt ledger.Receipt, effects *[]string) error {
		receipt["threadId"] = threadID
		if _, err := b.Ledger.Save(ctx, receipt); err != nil {
			return err
		}
		response, err := b.call(ctx, "thread/goal/get", map[string]any{"threadId": threadID})
		if err != nil {
			return err
		}
		before := object(response["goal"])
		if before == nil {
			message := "Thread has no goal to pause."
			return &appserver.RPCError{Method: "thread/goal/get", Message: message, Object: map[string]any{"code": "no_goal", "message": message}}
		}
		receipt["goalBefore"] = clipped(before, 4000, false)
		if before["status"] == "paused" {
			receipt["pause"] = "already_paused"
			receipt["delivery"] = "no_change"
			receipt["goalAfter"] = receipt["goalBefore"]
			return nil
		}
		if before["status"] != "active" {
			message := fmt.Sprintf("Goal status is %q; pause withheld. Only an active goal is paused, so a goal that already ended is never overwritten.", before["status"])
			return &appserver.RPCError{Method: "thread/goal/get", Message: message, Object: map[string]any{"code": "goal_not_active", "message": message}}
		}
		afterResponse, err := b.dispatch(ctx, "thread/goal/set", map[string]any{"threadId": threadID, "status": "paused"}, effects)
		if err != nil {
			return err
		}
		after := object(afterResponse["goal"])
		receipt["goalAfter"] = clipped(after, 4000, false)
		receipt["concurrency"] = "no_host_precondition_for_goal_status"
		receipt["concurrencyMeaning"] = "thread/goal/set takes no expected status, so this pause is not atomic. Read the goal again afterwards rather than trusting this receipt as exclusive."
		if after["objective"] != before["objective"] || after["tokenBudget"] != before["tokenBudget"] {
			message := "The host returned a different objective or tokenBudget than the goal read moments earlier."
			return &appserver.RPCError{Method: "thread/goal/set", Message: message, Object: map[string]any{"code": "goal_changed_under_pause", "message": message}}
		}
		if after["status"] != "paused" {
			message := fmt.Sprintf("The host reported status %q after the pause request; it is not recorded as applied.", after["status"])
			return &appserver.RPCError{Method: "thread/goal/set", Message: message, Object: map[string]any{"code": "goal_not_paused", "message": message}}
		}
		receipt["delivery"] = "applied_by_host"
		receipt["pause"] = "goal_paused_turn_may_still_be_running"
		return nil
	}, nil, nil})
}
