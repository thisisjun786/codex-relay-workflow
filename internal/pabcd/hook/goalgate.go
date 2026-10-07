package hook

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// GOAL-GATE (pabcd-state/src/goal-gate.ts, CXC v0.2.40, 3c1459ac): the PreToolUse guard that keeps a native goal
// unlimited and the Interview out of goal mode. Three registrations share one handler there, so they share one
// entry point here: the budget guard denies a create_goal whose tool_input carries any key but objective, the
// interview guard denies request_user_input while the host's goal owns the thread (active, or unreadable, which
// fails closed), and the goal-complete guard checks update_goal against the session's own durable state.
//
// Each guard is tool-name-scoped, so at most one fires (goal-gate.ts:320), and the harness registers all three
// rows against the same leg (pre-tool-use), as the oracle's declarations do.

// The tool names and the two deny texts, fixed as the oracle writes them (goal-gate.ts:32-34, :62-66). They carry
// no name the port renames, so contract/schema/cxc/name-substitution.json leaves them as they are.
const (
	goalGateCreateGoalToolName   = "create_goal"
	goalGateRequestUserInputTool = "request_user_input"
	goalGateUpdateGoalToolName   = "update_goal"
	goalGateCreateGoalWarning    = "Use create_goal with objective only. Omit token_budget so the goal stays unlimited, and put lifecycle status changes on update_goal."
	goalGateModeDenyReason       = "Goal mode denies blocking Interview / request_user_input, also when goal state is unreadable. For useful mid-work questions, use exposed and host-allowed request_user_input_async without expecting a reply; keep working with verified facts and authorized assumptions. Silence grants no approval."
)

// goalGatePreToolUse is what parsePreToolUse keeps (goal-gate.ts:91-102). The oracle also carries tool_use_id,
// turn_id, transcript_path, model and permission_mode, which no guard of this file reads; the event name is
// checked once at the parse, so a guard reached through it cannot see another event.
type goalGatePreToolUse struct {
	SessionID, Cwd, ToolName string
	ToolInput                any
}

// goalGateParsePreToolUse is parsePreToolUse (goal-gate.ts:76-103): the trimmed input must be one JSON object
// naming PreToolUse and carrying session_id, cwd and tool_name as strings. tool_input is carried as it is, the
// way the oracle carries its unknown-typed value; anything else (empty, unparseable, an array, a scalar, a
// missing or mistyped field) is not a payload. It never throws, which is what keeps a malformed input a
// pass-through rather than a deny.
func goalGateParsePreToolUse(raw string) (goalGatePreToolUse, bool) {
	object := editObject(text.Trim(raw))
	if object == nil || object["hook_event_name"] != "PreToolUse" {
		return goalGatePreToolUse{}, false
	}
	sessionID, hasSession := object["session_id"].(string)
	cwd, hasCwd := object["cwd"].(string)
	toolName, hasTool := object["tool_name"].(string)
	if !hasSession || !hasCwd || !hasTool {
		return goalGatePreToolUse{}, false
	}
	return goalGatePreToolUse{SessionID: sessionID, Cwd: cwd, ToolName: toolName, ToolInput: object["tool_input"]}, true
}

// goalGateHasInvalidCreateGoalInput is hasInvalidCreateGoalInput (goal-gate.ts:105-107): an object carrying any
// key but objective is invalid, and a value that is not an object is not invalid at all.
func goalGateHasInvalidCreateGoalInput(value any) bool {
	object, isObject := value.(map[string]any)
	if !isObject {
		return false
	}
	for key := range object {
		if key != "objective" {
			return true
		}
	}
	return false
}

// goalGateApplyGoalBudgetGuard is applyGoalBudgetGuard (goal-gate.ts:113-125): the deny envelope for a create_goal
// that names anything beside objective, else nothing. It never throws.
func goalGateApplyGoalBudgetGuard(p goalGatePreToolUse) string {
	if p.ToolName != goalGateCreateGoalToolName || !goalGateHasInvalidCreateGoalInput(p.ToolInput) {
		return ""
	}
	return editAnswer("deny", goalGateCreateGoalWarning, goalGateCreateGoalWarning)
}

// goalGateApplyGoalModeInterviewGuard is applyGoalModeInterviewGuard (goal-gate.ts:136-142): the deny envelope for
// request_user_input while goal mode owns the thread, else nothing. The tool name is checked first and the status
// is asked for only then, so another tool never reads the host's database, as the oracle reads it only past its
// own check. status is a function so that the caller owns that read and a test can fail it.
func goalGateApplyGoalModeInterviewGuard(p goalGatePreToolUse, status func() host.GoalStatus) string {
	if p.ToolName != goalGateRequestUserInputTool {
		return ""
	}
	state := status()
	if !host.SuppressesInterview(state) {
		return ""
	}
	return goalGateModeInterviewDenyEnvelope(state)
}

// goalGateModeInterviewDenyEnvelope is goalModeInterviewDenyEnvelope (goal-gate.ts:146-155): one envelope, shared
// by the guard and the fail-closed path so that both emit byte-identical output. The status is reported in the
// additional context, which is what tells an active goal from an unreadable one.
func goalGateModeInterviewDenyEnvelope(status host.GoalStatus) string {
	return editAnswer("deny", goalGateModeDenyReason, goalGateModeDenyReason+" (goal-active="+string(status)+")")
}

// goalGateRawLooksLikeRequestUserInput is rawLooksLikeRequestUserInput (goal-gate.ts:160-171): the loose detector
// the fail-closed path uses when the full parse or the status lookup could not answer. It requires only the event
// and the tool name, so it can recognise a payload the strict parse rejected. It never throws.
func goalGateRawLooksLikeRequestUserInput(raw string) bool {
	object := editObject(text.Trim(raw))
	return object != nil && object["hook_event_name"] == "PreToolUse" && object["tool_name"] == goalGateRequestUserInputTool
}

// goalGateApplyGoalCompleteGuard is the goal-complete place of the dispatcher (goal-gate.ts:325): the row
// CRW-379 registered, filled by CRW-752. The body is goalgate_complete.go; this call hands it the readers a hook
// process has, which is the oracle's own environment (cxcInvocation reads process.env).
func goalGateApplyGoalCompleteGuard(p goalGatePreToolUse, pabcdEnabled bool) string {
	return goalCompleteApplyGuard(p, pabcdEnabled, goalCompleteProcessDeps())
}

// goalGateDeps is the readers the dispatcher uses, so that a test can fail one. The oracle injects the same three
// through GoalActiveDeps (goal-active.ts:60-66) and its own environment; here they are the host readers the rest
// of this package already uses.
type goalGateDeps struct {
	PabcdEnabled func(cwd string) bool
	GoalStatus   func(sessionID string) host.GoalStatus
	Cwd          func() (string, error)
}

// goalGateRealDeps is the readers a hook uses: the project's crw.json for the payload's cwd, the host's goals
// database, and the process's working directory.
func goalGateRealDeps(env host.LookupEnv) goalGateDeps {
	return goalGateDeps{
		PabcdEnabled: func(cwd string) bool {
			return projectcfg.PabcdEnabled(cwd, func(key string) string { value, _ := env(key); return value })
		},
		GoalStatus: func(sessionID string) host.GoalStatus { return sessionHookGoalStatus(sessionID, env) },
		Cwd:        os.Getwd,
	}
}

// GoalGateHandlePreToolUseFailClosed is handlePreToolUseFailClosed (goal-gate.ts:314-325), the entry point the
// three pre-tool-use goal rows of internal/harness/legs.go register. pabcdEnabled is the read the harness already
// made for this payload's cwd (the same file the oracle reads at :318), so the normal path does not read it again.
//
// The budget guard runs first, then the interview guard while PABCD is on, then the goal-complete guard; each is
// tool-name-scoped, so at most one fires and the first non-empty answer stands.
func GoalGateHandlePreToolUseFailClosed(raw string, env host.LookupEnv, pabcdEnabled bool) string {
	return goalGateHandle(raw, pabcdEnabled, goalGateRealDeps(env))
}

// goalGateHandle is the dispatcher without its readers bound, so that a test can fail one and reach the recover.
//
// The deferred recover is the oracle's try/catch (:316, :321-324). Like the oracle's catch it is total: a panic
// below is answered here, never re-raised, so this handler never lets one reach the harness. It is fail-CLOSED
// for the case that matters — a payload that looks like a request_user_input call with PABCD on is denied with
// status unreadable rather than allowed — and for every other payload it answers nothing, which is what the
// oracle's catch does. A parse failure is not a panic at all (:316-317), so an unparseable payload answers
// nothing too, as the oracle's own test asserts (goal-gate.test.ts:260-262).
func goalGateHandle(raw string, pabcdEnabled bool, deps goalGateDeps) (out string) {
	defer func() {
		if recover() == nil {
			return
		}
		cwd, err := deps.Cwd()
		if err != nil {
			cwd = "" // a working directory that cannot be read is not enabled: PabcdEnabled defaults to true
		}
		if deps.PabcdEnabled(cwd) && goalGateRawLooksLikeRequestUserInput(raw) {
			out = goalGateModeInterviewDenyEnvelope(host.GoalUnreadable)
		} else {
			out = ""
		}
	}()
	payload, ok := goalGateParsePreToolUse(raw)
	if !ok {
		return ""
	}
	if answer := goalGateApplyGoalBudgetGuard(payload); answer != "" {
		return answer
	}
	if pabcdEnabled {
		if answer := goalGateApplyGoalModeInterviewGuard(payload, func() host.GoalStatus { return deps.GoalStatus(payload.SessionID) }); answer != "" {
			return answer
		}
	}
	return goalGateApplyGoalCompleteGuard(payload, pabcdEnabled)
}
