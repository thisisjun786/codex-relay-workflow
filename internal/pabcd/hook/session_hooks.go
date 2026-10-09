// The SessionStart, PostCompact and PostToolUse(interview) legs of the pabcd-state hooks: CXC
// v0.2.40 pabcd-state/src/hook.ts (commit 3c1459ac) handleSessionStart (:604-612),
// handlePostToolUse (:1951-1992) and handlePostCompact (:2025-2033), under the CRW names of
// contract/schema/cxc/name-substitution.json. The three registrations are the harness leg rows
// session-start-bootstrapping-pabcd-state, post-tool-use-capturing-interview-answers and
// post-compact-resetting-reinject-cursor. Harness parses each payload as cli.ts does (parse.ts) and
// hands a handler what the parse produced, so the event name and the required string fields are
// already checked when these run.
package hook

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// SessionHookSessionStartPayload is what the harness hands SessionHookSessionStart: the
// SessionStart fields hook.ts SessionStartPayload declares.
type SessionHookSessionStartPayload struct{ Cwd, SessionID string }

// SessionHookPostCompactPayload is what the harness hands SessionHookPostCompact: the PostCompact
// fields hook.ts PostCompactPayload declares.
type SessionHookPostCompactPayload struct{ Cwd, SessionID string }

// SessionHookPostToolUsePayload is what the harness hands SessionHookPostToolUse: the PostToolUse
// fields handlePostToolUse reads. TurnID is the payload's turn_id, or empty when it is absent or
// not a string (the oracle's `?? ""`).
type SessionHookPostToolUsePayload struct {
	Cwd, SessionID, ToolName, TurnID string
	ToolInput, ToolResponse          any
}

// SessionHookSessionStart is handleSessionStart (hook.ts:604-612): it bootstraps the
// SessionStart-bound FSM by creating the session's state file, before an agent can invoke the
// explicit-session CLI, and answers nothing. ensureState leaves an existing file, valid or corrupt,
// untouched; a failure leaves no state and is silent, as the oracle's dispatch catch is.
//
// CRW-1140 (port: fixed): the oracle bootstraps in whatever cwd the payload names, so a thread
// resumed at another cwd gets an empty IDLE state beside the one it left in flight. A relay-managed
// thread (one a CRW resume path anchored at its native root, stateroot.Guard) is judged with the
// same resolution the resume paths use: when its root holds work in flight and the payload cwd is
// another directory, nothing is created (a state already there, a legacy IDLE one included, is left
// alone and does not exempt it) and the answer tells the agent where its state is. A thread
// with no anchor, a standalone terminal session, bootstraps exactly as before; the anchor is a local
// file, so no hook needs a running relay.
func SessionHookSessionStart(p SessionHookSessionStartPayload) string {
	return sessionHookSessionStart(p, os.LookupEnv)
}

func sessionHookSessionStart(p SessionHookSessionStartPayload, env host.LookupEnv) string {
	if conflict := stateroot.Bootstrap(env, p.Cwd, p.SessionID); conflict != nil {
		return sessionHookAnswer("SessionStart", sessionHookStateRootContext(conflict))
	}
	_, _ = state.EnsureState(p.Cwd, p.SessionID)
	stateroot.Bootstrapped(env, p.Cwd, p.SessionID)
	return ""
}

// sessionHookStateRootContext is what SessionStart tells an agent whose thread runs away from the
// root that holds its work in flight.
func sessionHookStateRootContext(c *stateroot.Conflict) string {
	held := "is in flight at " + c.StatePath + " (phase " + c.Phase + ")"
	if c.Unreadable {
		held = "is at " + c.StatePath + " and cannot be read, so it may be in flight"
	}
	return "[crw: PABCD state root]\n" +
		"This thread's PABCD state " + held + ", in its native cwd " + c.NativeCwd + ". This session started at " + c.TargetCwd +
		", so no state was created here: an empty IDLE state would detach the work in flight. Nothing was moved.\n" +
		"Run PABCD commands for this thread with `--cwd " + c.NativeCwd + "`, or resume the thread at that cwd. " +
		"Moving the work is an explicit handover to a thread started at the new cwd; a bound source worktree is not a native cwd."
}

// SessionHookPostCompact is handlePostCompact (hook.ts:2025-2033): a context compaction resets the
// reinjection cursor of an in-flight cycle, so the first eligible same-phase prompt injects the full
// phase directive (mode 2) instead of the short stage header (mode 3). Nothing else is touched — not
// the phase, the flags, the stagnation counters, the goalplan or the goal database — and the handler
// answers nothing.
//
// The oracle reads the state and writes it back with no lock, so an update a participating writer
// lands between the two is lost (a data-loss defect, fixed here by decision, as the idle-edit counter
// is): the eligibility is judged again inside the session lock and the file is written from that read.
// A state whose stored records the reader or the writer would change is left alone rather than
// rewritten from a lossy read, and a lock that cannot be taken or a write that fails leaves the file
// as it was.
func SessionHookPostCompact(p SessionHookPostCompactPayload) string {
	return sessionHookPostCompact(p, state.WithSessionLock)
}

// sessionHookPostCompact takes the lock as an argument so that a test can land a participating
// writer's update between the handler's read and its write.
func sessionHookPostCompact(p SessionHookPostCompactPayload, lock func(cwd, sessionID string, fn func() error) error) string {
	// No-op unless an orchestrated cycle is in flight, and no write when the cursor is already reset.
	if !sessionHookPostCompactEligible(state.ReadState(p.Cwd, p.SessionID)) {
		return ""
	}
	_ = lock(p.Cwd, p.SessionID, func() error {
		fresh, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
		if unreadable || !sessionHookPostCompactEligible(fresh) {
			return nil
		}
		raw, err := os.ReadFile(state.StatePath(p.Cwd, p.SessionID))
		if err != nil {
			return nil
		}
		fresh.LastInjectedPhase = nil
		if !state.RewriteKeepsStored(raw, fresh) || state.DcloseRecoveryLegacy(fresh) {
			return nil
		}
		return state.WriteState(p.Cwd, fresh)
	})
	return ""
}

// sessionHookPostCompactEligible is the oracle's guard: an orchestrated cycle is in flight and the
// reinjection cursor still holds a phase, so there is something to reset.
func sessionHookPostCompactEligible(s state.State) bool {
	return s.OrchestrationActive && s.Phase != state.PhaseIdle && s.LastInjectedPhase != nil
}

// SessionHookPostToolUse is handlePostToolUse (hook.ts:1951-1992): a request_user_input round is
// recorded in the interview ledger, and, when the session is in an interactive I phase (no goal that
// suppresses the interview), the post-answer rescan directive is reinjected as PostToolUse
// additionalContext so the Mind loop runs after every answer. It only acts on request_user_input;
// everything else is a no-op. The capture is a pure recorder and always runs; the reinjection is
// best effort, so a failure after it answers nothing, as the oracle's catch does.
func SessionHookPostToolUse(p SessionHookPostToolUsePayload, env host.LookupEnv) string {
	if p.ToolName != "request_user_input" {
		return ""
	}
	ledger.CaptureInterviewAnswers(ledger.CaptureInput{Cwd: p.Cwd, SessionID: p.SessionID, TurnID: p.TurnID,
		ToolInput: p.ToolInput, ToolResponse: p.ToolResponse})
	// L18: the goal firewall suppresses the whole interview, so nothing is reinjected.
	if host.SuppressesInterview(sessionHookGoalStatus(p.SessionID, env)) {
		return ""
	}
	if state.ReadState(p.Cwd, p.SessionID).Phase != state.PhaseI {
		return ""
	}
	return sessionHookPostToolUseAnswer(ResolveCRWInDirective(sessionHookRescanReinjectDirective, env))
}

// sessionHookRescanReinjectDirective is RESCAN_REINJECT_DIRECTIVE (hook.ts:1929-1937) under the
// declared name substitution (rules R1, R23 and R33, and the cli table's verb pass). The text is
// what the model acts on, so it is frozen byte for byte after that substitution; the invocation of
// the backticked command is resolved at emission, never in the constant.
const sessionHookRescanReinjectDirective = "[crw: INTERVIEW — post-answer rescan]\n" +
	"An answer was recorded. Apply this pointer and $crw:crw-interview only within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"INTERVIEW-SCAN-01: rescan contradictions before the next question or advancement. If required work or tracker writes are forbidden, report them as unmet; do not record a completed scan or claim readiness.\n" +
	"Only when dispatch is authorized: give each read-only Mind the current plan/tracker position; cap 3, lowest-scoring dimensions first. Discover spawn_agent if needed.\n" +
	"Minds return contradictions only, never ask, edit or write state. Inline reasoning is not evidence that independent Minds ran.\n" +
	"Triage high contradictions into user questions and low/medium into OPEN ASSUMPTIONS; record only actual authorized work with `crw pabcd scan record --session <id> [--contradictions N] [--high N]`."

// sessionHookGoalStatus is getGoalActiveStatus(sessionID) with no injected deps: the goals database
// is the host's, at CODEX_SQLITE_HOME, else CODEX_HOME, else the account home. A path that cannot be
// resolved reads as unreadable, which suppresses the interview exactly as the oracle's throwing
// resolveGoalsDbPath does inside the handler's try.
func sessionHookGoalStatus(sessionID string, env host.LookupEnv) host.GoalStatus {
	path, err := host.GoalsDBPath(env)
	if err != nil {
		return host.GoalUnreadable
	}
	return host.GoalActiveStatus(sessionID, path)
}

// sessionHookPostToolUseAnswer is the answer handlePostToolUse builds inline (hook.ts:1978-1986):
// JSON.stringify of one hookSpecificOutput object, then a newline.
func sessionHookPostToolUseAnswer(context string) string {
	return sessionHookAnswer("PostToolUse", context)
}

// sessionHookAnswer is one hookSpecificOutput object carrying context for event, then a newline.
func sessionHookAnswer(event, context string) string {
	type output struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	}
	b, err := role.Stringify(struct {
		Output output `json:"hookSpecificOutput"`
	}{output{event, context}}, "")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}
