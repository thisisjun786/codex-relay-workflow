package harness

import (
	"os"
	"slices"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
)

// Component is the oracle's name for the component whose hooks these legs are; it keys the record.
const Component = "pabcd-state"

// Stage is where in cli.ts' order a leg runs, which decides what an error in it does.
type Stage int

const (
	// Permission legs answer before the record is made and fail open; a payload over the limit gets silence.
	Permission Stage = iota
	// Guard legs follow the record and sit above the subagent exit, so a subagent's turn reaches them.
	Guard
	// FailClosed legs follow the PABCD gate; an error ends the process (exit 1), it never allows.
	FailClosed
	// Generic legs follow the gate and fail open.
	Generic
)

// Call is what a handler is given once the legs' checks have let it act.
type Call struct {
	Raw string // the hook's input, within the limit
	// PabcdEnabled is readPabcdEnabled for the payload's cwd. cli.ts reads it after the subagent exit
	// (cli.ts:427), so for a Permission or Guard leg it is false because it was not read, and only for
	// a FailClosed or Generic leg does false mean that PABCD is off.
	PabcdEnabled bool
}

// Handler is a leg's behaviour; its answer is written to stdout as it is.
type Handler func(Call) string

// Leg is one hook registration of the oracle's pabcd-state component, as the host runs it:
// crw hook <Event> --leg <ID>.
type Leg struct {
	ID             string // the declared registration, hook-declarations.json "leg"
	Event          string // the hook event, kebab-case
	Slug           string // the verb cli.ts dispatched on, which names the record and the oversize answer
	Stage          Stage
	Recover        bool    // Guard only: an error in the handler is swallowed (try/catch) rather than fatal
	SubagentExempt bool    // runs for a subagent's turn too (subagent-stop, subagent-stop-review)
	Gated          bool    // a member of PABCD_DISABLED_EVENTS: silent when PABCD is off
	Handle         Handler // nil until the issue that ports the leg registers it: the leg then answers nothing
}

// Legs is the table: the one place the legs and their positions in the order are registered. Other
// components' hooks (recall, bg-wake, cxc-ops, config-guard, provider-bridge, subagent-config) have
// their own ingress and are not here, and neither are the six events no manifest registers.
func Legs() []Leg {
	return []Leg{
		{"session-start-bootstrapping-pabcd-state", "session-start", "session-start", Generic, false, false, true, func(c Call) string {
			p, ok := ParseSessionStart(c.Raw)
			if !ok {
				return ""
			}
			return pabcdhook.SessionHookSessionStart(pabcdhook.SessionHookSessionStartPayload{Cwd: p.Cwd, SessionID: p.SessionID})
		}},
		{"session-start-advising-agent-thread-permissions", "session-start", "session-start-permission-advisory", Permission, false, false, false, func(c Call) string {
			return pabcdhook.HandleAgentThreadSessionStartAdvisory(c.Raw, os.LookupEnv)
		}},
		{"user-prompt-submit-checking-pabcd-trigger", "user-prompt-submit", "user-prompt-submit", Generic, false, false, false, nil},
		{"stop-checking-pabcd-continuation", "stop", "stop", Generic, false, false, true, nil},
		{"pre-tool-use-guarding-goal-budget", "pre-tool-use", "pre-tool-use", FailClosed, false, false, false, func(c Call) string {
			return pabcdhook.GoalGateHandlePreToolUseFailClosed(c.Raw, os.LookupEnv, c.PabcdEnabled)
		}},
		{"permission-request-allowing-agent-thread", "permission-request", "permission-request", Permission, false, false, false, func(c Call) string {
			return pabcdhook.HandleAgentThreadPermissionRequest(c.Raw, os.LookupEnv)
		}},
		{"pre-tool-use-guarding-interview-in-goal", "pre-tool-use", "pre-tool-use", FailClosed, false, false, false, func(c Call) string {
			return pabcdhook.GoalGateHandlePreToolUseFailClosed(c.Raw, os.LookupEnv, c.PabcdEnabled)
		}},
		{"pre-tool-use-guarding-goal-complete", "pre-tool-use", "pre-tool-use", FailClosed, false, false, false, func(c Call) string {
			return pabcdhook.GoalGateHandlePreToolUseFailClosed(c.Raw, os.LookupEnv, c.PabcdEnabled)
		}},
		{"post-tool-use-capturing-interview-answers", "post-tool-use", "post-tool-use", Generic, false, false, true, func(c Call) string {
			p, ok := ParsePostToolUse(c.Raw)
			if !ok {
				return ""
			}
			turn := ""
			if p.TurnID != nil {
				turn = *p.TurnID
			}
			return pabcdhook.SessionHookPostToolUse(pabcdhook.SessionHookPostToolUsePayload{Cwd: p.Cwd, SessionID: p.SessionID,
				ToolName: p.ToolName, TurnID: turn, ToolInput: p.ToolInput, ToolResponse: p.ToolResponse}, os.LookupEnv)
		}},
		{"subagent-stop-verifying-evidence", "subagent-stop", "subagent-stop", Generic, false, true, true, func(c Call) string {
			p, ok := ParseSubagentStop(c.Raw)
			if !ok {
				return ""
			}
			value := func(s *string) string {
				if s != nil {
					return *s
				}
				return ""
			}
			return pabcdhook.RunSubagentStopGate(pabcdhook.SubagentStopPayload{Cwd: p.Cwd, SessionID: p.SessionID,
				AgentType: p.AgentType, AgentID: value(p.AgentID), TurnID: value(p.TurnID), LastAssistantMessage: value(p.LastAssistantMessage)}, os.Getenv)
		}},
		{"subagent-stop-observing-review", "subagent-stop", "subagent-stop-review", Generic, false, true, true, nil},
		{"post-compact-resetting-reinject-cursor", "post-compact", "post-compact", Generic, false, false, true, func(c Call) string {
			p, ok := ParsePostCompact(c.Raw)
			if !ok {
				return ""
			}
			return pabcdhook.SessionHookPostCompact(pabcdhook.SessionHookPostCompactPayload{Cwd: p.Cwd, SessionID: p.SessionID})
		}},
		{"pre-tool-use-linting-apply-patch", "pre-tool-use", "pre-tool-use-edit", Generic, false, false, false, func(c Call) string {
			out := pabcdhook.HandleApplyPatchLint(c.Raw)
			if c.PabcdEnabled && out == "" {
				out = pabcdhook.HandleIdleEditAdvisory(c.Raw, os.LookupEnv)
			}
			return out
		}},
		{"post-tool-use-tracking-render-observations", "post-tool-use", "post-tool-use-render-observation", Generic, false, false, true, func(call Call) string {
			if p, ok := ParsePostToolUse(call.Raw); ok {
				payload := pabcdhook.RenderPayload{Event: "PostToolUse", Cwd: p.Cwd, SessionID: p.SessionID, ToolName: p.ToolName, Input: p.ToolInput, Response: p.ToolResponse}
				pabcdhook.HandleRenderObservationCapture(payload)
				pabcdhook.HandleRenderArtifactCapture(payload)
			}
			return ""
		}},
		{"session-start-detecting-managed-worktree", "session-start", "worktree-guard", Generic, false, false, false, func(c Call) string {
			return ContextOutput(pabcdhook.HandleWorktreeGuard(c.Raw, os.LookupEnv))
		}},
		{"user-prompt-submit-guiding-worktree-rename", "user-prompt-submit", "worktree-guard", Generic, false, false, false, func(c Call) string {
			return ContextOutput(pabcdhook.HandleWorktreeGuard(c.Raw, os.LookupEnv))
		}},
		{"pre-tool-use-guarding-managed-worktree-deletion", "pre-tool-use", "worktree-guard-pretool", Guard, true, false, false, func(c Call) string {
			return pabcdhook.HandleWorktreeGuardPreTool(c.Raw, os.LookupEnv)
		}},
		{"pre-tool-use-guarding-memory-write", "pre-tool-use", "pre-tool-use-memory-write", Guard, true, false, false, func(c Call) string {
			return pabcdhook.HandleMemoryWriteGate(c.Raw, os.LookupEnv)
		}},
		{"pre-tool-use-guarding-automation-ownership", "pre-tool-use", "pre-tool-use-automation-ownership", Guard, false, false, false, nil},
	}
}

// ClaimsHook is whether crw hook's arguments name one of these events, so that the harness answers
// them; anything else (--plugin-launch, a settings path, no argument) stays with the Stop adapter.
func ClaimsHook(args []string) bool {
	return len(args) > 0 && slices.ContainsFunc(Legs(), func(l Leg) bool { return l.Event == args[0] })
}
