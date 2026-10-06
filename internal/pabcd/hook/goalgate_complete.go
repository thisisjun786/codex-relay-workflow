package hook

import (
	"os"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// GOAL-COMPLETE-GATE-01 (pabcd-state/src/goal-gate.ts:174-313, CXC v0.2.40, 3c1459ac): the PreToolUse guard that
// denies update_goal status complete while the session's own durable state says the work is not closed. The order,
// the texts and the fail-open catch are the oracle's.
//
// Two renames are applied as the port applies them. The invocation prefix: a backtick-anchored command prefix
// becomes the command crw runs as on this machine (host.Invocation), the way goalCompleteDenyEnvelope calls
// cxcInvocation, and the words after it follow the CLI name table (contract/schema/cxc/name-substitution.json):
// crw pabcd orchestrate, crw pabcd loop validate, crw pabcd evidence resolve. The state directory: .crw/ is
// crwdir.DirName, where the oracle says .codexclaw/.
//
// Every package-level name of this file carries the goalComplete prefix (the port's naming rule).

// goalCompleteUnresolvableAgent is what the oracle prints for a tombstone entry that carries no agent id
// (goal-gate.ts:271).
const goalCompleteUnresolvableAgent = "<no agent id>"

// goalCompleteDeps is the readers the guard uses, so that a test can fail one and reach the fail-open catch.
// Invocation is host.Invocation, carried as a function so the caller owns the environment it resolves from, as
// the oracle resolves cxcInvocation per emission; ReadState is state.ReadStateStrict.
type goalCompleteDeps struct {
	Invocation func() (string, error)
	ReadState  func(cwd, sessionID string) (state.State, bool)
}

// goalCompleteProcessDeps is the readers a hook process has: this machine's crw command, and the session state
// under the payload's cwd.
func goalCompleteProcessDeps() goalCompleteDeps {
	return goalCompleteDeps{
		Invocation: func() (string, error) { return host.Invocation(os.LookupEnv) },
		ReadState:  state.ReadStateStrict,
	}
}

// goalCompleteDenyEnvelope is goalCompleteDenyEnvelope (goal-gate.ts:174-186): one PreToolUse deny envelope whose
// reason and additional context are the same text, with a trailing newline. The invocation rename touches every
// backtick-anchored command prefix in the reason, as the oracle's global replace does; a resolver that fails
// leaves the reason as it is, which is the oracle's fail-open.
func goalCompleteDenyEnvelope(reason string, invocation func() (string, error)) (out string) {
	// The oracle wraps this resolution in its own try/catch (goal-gate.ts:176-181): a resolver that throws
	// leaves the reason as it is and never changes the deny. A panic must be caught here rather than reach the
	// guard's outer recover, where it would turn a deny into a pass.
	if invocation != nil {
		func() {
			defer func() { _ = recover() }()
			if inv, err := invocation(); err == nil {
				reason = strings.ReplaceAll(reason, "`crw ", "`"+inv+" ")
			}
		}()
	}
	return editAnswer("deny", reason, reason)
}

// goalCompleteApplyGuard is applyGoalCompleteGuard (goal-gate.ts:201-299). pabcdEnabled is readPabcdEnabled for the
// payload's cwd, read once by the caller as the oracle reads it at :318. It answers nothing for every update_goal
// the gate does not deny, and also when an unexpected error escapes a reader: the oracle's catch is total and
// fails open so this gate never traps a session.
func goalCompleteApplyGuard(p goalGatePreToolUse, pabcdEnabled bool, deps goalCompleteDeps) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	if p.ToolName != goalGateUpdateGoalToolName {
		return ""
	}
	input, isObject := p.ToolInput.(map[string]any)
	if !isObject || input["status"] != "complete" {
		return ""
	}
	s, unreadable := deps.ReadState(p.Cwd, p.SessionID)
	// A clean default also means no unresolved verdicts, so unreadable session state cannot be treated as a clean
	// bill of health: it is exactly the case where an unresolved subagent failure would be invisible. Deny
	// completion; blocked stays available. (An ABSENT state file is not unreadable: a session that never
	// delegated anything still completes normally.)
	if unreadable {
		return goalCompleteDenyEnvelope(
			"GOAL-COMPLETE-GATE-01: this session's state is unreadable, so unresolved subagent evidence failures cannot be ruled out. Restore or reset the session state after verifying the delegated work, or use update_goal status \"blocked\".",
			deps.Invocation)
	}
	if pabcdEnabled && s.OrchestrationActive && s.Phase != state.PhaseIdle && s.Phase != state.PhaseI {
		return goalCompleteDenyEnvelope(
			"GOAL-COMPLETE-GATE-01: a PABCD cycle is in flight at phase "+string(s.Phase)+". Close the cycle first (advance to D via `crw pabcd orchestrate ... --session "+p.SessionID+"`, or `crw pabcd orchestrate reset --session "+p.SessionID+"`), then mark the goal complete. If an external blocker prevents closing, use update_goal status \"blocked\" instead.",
			deps.Invocation)
	}
	// EVIDENCE-TERMINAL-01 (260826): the SubagentStop gate no longer blocks a child forever when it cannot
	// produce a receipt; it releases and records the unresolved verdict here, which is where that verdict is
	// enforced. Checked before the goalplan branch so it also holds at IDLE and with no bound goalplan.
	if s.UnverifiedCorrupt {
		return goalCompleteDenyEnvelope(
			"GOAL-COMPLETE-GATE-01: the subagent verification record for this session is unreadable or overflowed, so unresolved evidence failures cannot be ruled out. Re-verify the delegated work, or use update_goal status \"blocked\".",
			deps.Invocation)
	}
	// A verdict that existed but could not be persisted must not read as no verdict, and neither must an
	// unreadable marker directory.
	marker := evidence.UnrecordableVerdictStatus(p.Cwd, p.SessionID)
	if marker.Present || marker.Unreadable {
		return goalCompleteDenyEnvelope(
			"GOAL-COMPLETE-GATE-01: a delegated subagent failed evidence verification but the verdict could not be confirmed (see .crw/evidence-unrecordable/). Re-verify that work and clear the marker, or use update_goal status \"blocked\".",
			deps.Invocation)
	}
	// Independent durable signal: a retry counter still at the cap means an agent exhausted verification and was
	// never verified. It is written during NORMAL operation (calls 1..3) and cleared only by a valid receipt, so
	// it survives a transient failure at terminal time even if the filesystem later recovers.
	unresolved := s.UnverifiedSubagents
	if len(unresolved) > 0 {
		named := make([]string, 0, 3)
		for i := 0; i < len(unresolved) && i < 3; i++ {
			if unresolved[i].Resolvable {
				named = append(named, unresolved[i].AgentID)
			} else {
				named = append(named, goalCompleteUnresolvableAgent)
			}
		}
		return goalCompleteDenyEnvelope(
			"GOAL-COMPLETE-GATE-01: "+strconv.Itoa(len(unresolved))+" delegated subagent completion(s) exhausted evidence verification without a valid receipt ("+strings.Join(named, ", ")+"). Their work is unverified. Re-run or verify it and record a receipt with `crw pabcd evidence resolve --session "+p.SessionID+" --agent <agent-id> --receipt <path>`, or use update_goal status \"blocked\" if an external blocker prevents it.",
			deps.Invocation)
	}
	// Checked AFTER the tombstone list so the specific verdict speaks first. This is the fallback signal for the
	// case a tombstone could not be written.
	if evidence.HasSpentBudget(p.Cwd, p.SessionID) {
		return goalCompleteDenyEnvelope(
			"GOAL-COMPLETE-GATE-01: a delegated subagent exhausted its evidence-verification budget without a valid receipt. Re-verify that work and record a receipt with `crw pabcd evidence resolve --session "+p.SessionID+" --agent <agent-id> --receipt <path>`, or use update_goal status \"blocked\".",
			deps.Invocation)
	}
	if pabcdEnabled && s.Slug != "" {
		if _, err := sourcesession.Resolve(p.Cwd, p.SessionID); err != nil {
			return goalCompleteDenyEnvelope("SOURCE-ROOT: "+err.Error(), deps.Invocation)
		}
		plan := goalplan.ReadGoalplan(p.Cwd, s.Slug)
		if plan == nil {
			return goalCompleteDenyEnvelope(
				"GOAL-COMPLETE-GATE-01: session has a bound goalplan slug '"+s.Slug+"' but the plan could not be read (missing or malformed). Restore the goalplan or use update_goal status \"blocked\".",
				deps.Invocation)
		}
		// Completion must use the same marker/source/receipt-aware validation as crw pabcd loop validate; omitting
		// this context turns a removed schemaVersion field into a downgrade path around the v2 final gate.
		verdict := goalplan.ValidateGoalplan(plan, &goalplan.GoalplanValidationCtx{
			Cwd:                   p.Cwd,
			CaptureSourceIdentity: func(cwd string) goalplan.SourceIdentity { return goalCompleteCaptureSourceIdentity(cwd, p.SessionID) },
			CompareSource: func(a, b goalplan.SourceIdentity) source.Comparison {
				return source.Compare(a.Identity(), b.Identity())
			},
			ReadReceipt: func(path string, expectedKind gate.ReceiptKind) (goalplan.GoalplanReceiptEvidence, error) {
				receipt, err := gate.ParseSourceBoundReceipt(path, p.Cwd, expectedKind)
				if err != nil {
					return goalplan.GoalplanReceiptEvidence{}, err
				}
				return goalplan.GoalplanReceiptEvidence{SourceIdentity: receipt.SourceIdentity, ArtifactManifest: receipt.ArtifactManifest}, nil
			},
		})
		if !verdict.OK {
			reasons := verdict.Reasons
			if len(reasons) > 4 {
				reasons = reasons[:4]
			}
			return goalCompleteDenyEnvelope(
				"GOAL-COMPLETE-GATE-01: the session-bound goalplan '"+s.Slug+"' fails the E8 quality/integrity gate: "+strings.Join(reasons, "; ")+". Repair invalid dependency, outcome, and criteria references first; then finish remaining work and record fresh capturedEvidence in .crw/goalplans/"+s.Slug+"/goalplan.json (check with `crw pabcd loop validate --session "+p.SessionID+" --slug \""+s.Slug+"\"`), or use update_goal status \"blocked\" if an external blocker prevents completion. Do not shrink the objective to escape the gate (LOOP-CONTINUE-01).",
				deps.Invocation)
		}
	}
	return ""
}

// goalCompleteCaptureSourceIdentity is captureSessionSourceIdentity (session-source-identity.ts:6-10): the identity
// of the session's source worktree, carrying that worktree's root, in the stored form the goalplan validator's
// context takes. A resolution failure panics exactly as the oracle's call throws, and the validator's
// finalGateCaptureCurrent turns that into its catch-arm reason.
func goalCompleteCaptureSourceIdentity(cwd, sessionID string) goalplan.SourceIdentity {
	id, err := sourcesession.Capture(cwd, sessionID, sourcesession.CaptureOptions{})
	if err != nil {
		panic(err)
	}
	out := goalplan.SourceIdentity{Kind: id.Kind, CommitSha: id.CommitSha, Dirty: id.Dirty, CapturedAt: id.CapturedAt, SourceRoot: id.SourceRoot}
	if id.TreeHash != "" {
		out.TreeHash = &id.TreeHash
	}
	return out
}
