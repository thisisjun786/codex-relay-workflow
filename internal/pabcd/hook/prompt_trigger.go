// prompt_trigger.go holds the remainder of handleUserPromptSubmit, CXC v0.2.40
// pabcd-state/src/hook.ts:755-847 (commit 3c1459ac), and renderStatusLine (hook.ts:849-853). The
// leading section lives in prompt_submit.go; that unit stops where this one starts, and the trigger
// branch it leaves is the first statement here. The registration it answers is the harness leg row
// user-prompt-submit-checking-pabcd-trigger.
//
// What the range decides, in the oracle's order: the natural-language trigger branch, which injects
// the phase directive (the Interview directive when the trigger is I or the project's policy advises
// it), the TRIGGER-AUTHORITY-01 note resolved at emission and the phase footer, while it records only
// dedup and loop-arm bookkeeping and never moves the phase; the fail-closed branch that answers the
// search directive, or nothing, when orchestration was never activated; the loopArmSeen write that has
// to survive the passive branches below; the L17 goal-mode Interview firewall; the R-11
// transcript-grounded idempotency guards; and the passive re-injection modes 2 (the phase changed since
// the last injection) and 3 (the same phase, the short compaction-immune stage header). Texts are
// frozen byte for byte after contract/schema/cxc/name-substitution.json, and a backticked command is
// resolved at emission, never in a constant.
//
// The state writes here differ from the oracle in the way the memory gate, the idle-edit counter, the
// idle-edit rewrite guard and handlePostCompact already differ: the oracle reads the session state,
// changes fields and writes the whole state back with no lock, so an update a participating writer
// lands between the read and the write is lost, and the write-back rebuilds the state from the
// reader's normalised value, so a stored record the reader cannot keep is lost with it. Each write
// here goes through promptSubmitWriteState, which re-reads inside the session lock, applies the change
// to that read, and refuses a rewrite the reader would not keep whole (docs/port-cxc/known-defects.md).
// A write the oracle's own writeState would have thrown out of the handler - a lock that cannot be
// taken, or a write that fails - ends this handler in silence, as cli.ts's catch answers nothing
// there; a write this port's rewrite guard only skips still answers, because the oracle has no such
// guard and would have written and answered. A write that records an answer goes through
// promptSubmitClaim (CRW-1159): the answer goes out only when the lock finds the turn unrecorded and
// the phase, binding, cursor and bound work phase it was chosen from unmoved, so one turn is answered
// once and a stale decision is dropped instead of answered and recorded.
//
// The handler answers the context to hand the model, not the envelope: harness.ContextOutput wraps it
// (hook.ts:583-597 buildContextOutput), which is where the CRLF normalisation, the trim and the
// 32,000-unit cap live. This file has no package-level initializer and needs no Node at run time.
package hook

import (
	"slices"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// RenderStatusLine is renderStatusLine (hook.ts:849-853): the one-line human status the chat
// `orchestrate status` affordance prints. The label falls back to the phase itself, as the oracle's
// `STAGE_LABELS[phase] ?? phase` does, and the three flags are the session's derived gates.
func RenderStatusLine(phase state.Phase, interview, auditPassed, checkPassed bool) string {
	return "[crw status] IPABCD: " + string(phase) + " (" + stageLabel(phase) + ") · interview=" +
		strconv.FormatBool(interview) + " auditPassed=" + strconv.FormatBool(auditPassed) +
		" checkPassed=" + strconv.FormatBool(checkPassed)
}

// promptTriggerHandle is the remainder of handleUserPromptSubmit (hook.ts:755-847). current is the
// session state the leading section read after its own Stop-budget stamp, trigger is the advisory
// phase hint the loose detector produced (empty for none), adviseInterview is the interview-entry
// decision for it, and agbrowseRequested and loopArmRequested are the two prompt heuristics. It
// returns the context to inject, or "" for every path that injects nothing.
func promptTriggerHandle(p PromptSubmitPayload, env host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error, current state.State, trigger state.Phase, adviseInterview, agbrowseRequested, loopArmRequested bool) string {
	turn := p.TurnID

	// Natural-language triggers are advisory only. Explicit chat commands above or authorized agent CLI
	// calls own phase entry and advancement through real gates. Keep phase, orchestrationActive and
	// lastInjectedPhase unchanged, including from IDLE; only dedup and loop-arm bookkeeping is recorded.
	if trigger != "" {
		opts := ActiveWorkPhaseOpts(p.Cwd, current.Slug)
		directive := PhaseDirective(trigger, opts)
		inputs := promptClaimInputs{read: current, checkWork: true, work: opts}
		if trigger == state.PhaseI || adviseInterview {
			directive = InterviewDirective(env)
			inputs.checkWork = false
		}
		if turn != "" || loopArmRequested {
			if promptSubmitClaim(lock, p.Cwd, p.SessionID, turn, inputs, promptTriggerDedup(turn, loopArmRequested)) != promptClaimEmit {
				return ""
			}
		}
		guided := directive + "\n\n" + ResolveCRWInDirective(TriggerAuthorityNote, env)
		return WithFooter(guided, current.Phase)
	}

	// fail-closed: no trigger and orchestration never activated -> stay silent.
	if !current.OrchestrationActive {
		if agbrowseRequested {
			if promptSubmitClaim(lock, p.Cwd, p.SessionID, turn, promptClaimInputs{read: current}, promptTriggerDedup(turn, false)) != promptClaimEmit {
				return ""
			}
			return AgbrowseSearchDirective
		}
		return ""
	}

	// TRIGGER-AUTHORITY-01 (040): an armed session that asks for a loop keeps its phase and falls
	// through to the passive pipeline, but the flag must survive. The oracle persists it once here so
	// every later write spreads `working` instead of the stale snapshot; this port re-reads inside the
	// lock, so it only has to set the flag on the state the lock found. `working` alone is not enough:
	// the passive branches below can return without writing (context pressure, no turn id).
	if loopArmRequested && !current.LoopArmSeen {
		if promptTriggerWrite(lock, p.Cwd, p.SessionID, func(fresh *state.State) bool {
			fresh.LoopArmSeen = true
			return true
		}) == promptSubmitFailed {
			return ""
		}
	}

	// L17 firewall: the goal-active interview suppression must also cover the PASSIVE re-injection
	// paths (modes 2/3), not just the explicit trigger path above. If the session is sitting in phase I
	// and a native goal is (or becomes) active, do NOT re-inject any Interview directive - goal mode is
	// PABCD-only and the Interview never fires under a goal. Fail-closed: an unreadable goal database
	// also suppresses.
	if current.Phase == state.PhaseI && host.SuppressesInterview(sessionHookGoalStatus(p.SessionID, env)) {
		return ""
	}

	// R-11 transcript-grounded idempotency (passive modes only; the explicit trigger above already
	// injected). The local injectedTurns flag dedups within a turn, but turn_id can churn or reset after
	// compaction, so the stage marker a hook injected into the context the model still has (the records
	// after the last compaction) also counts as this phase's injection. CRW-1090 departs from the oracle
	// here (docs/port-cxc/known-defects/CRW-1090.md): the oracle matched marker and pressure text anywhere
	// in the raw tail, so a quote suppressed the injection, and the compacted record's own history made the
	// prompt after a compaction skip the directive the compaction had removed. A cursor PostCompact reset
	// (nil) is never set again from the tail: the full directive goes in. A prompt is the boundary of a
	// compaction's recovery window (host.TranscriptGeneration.ContextPressure), so the oracle's pressure
	// suppression has no case left here; the Stop leg keeps it.
	generation := host.ReadTranscriptGeneration(p.TranscriptPath, host.TailBytes)
	// Each passive answer below goes out only when the lock that records it finds the turn unrecorded and
	// the phase, the binding and the cursor it was chosen from unmoved (CRW-1159, promptSubmitClaim); a
	// stale decision is dropped rather than answered or recorded.
	passive := promptClaimInputs{read: current, cursor: true}
	if current.LastInjectedPhase != nil && generation.HasStageMarkerForPhase(string(current.Phase)) {
		if turn != "" {
			if promptSubmitClaim(lock, p.Cwd, p.SessionID, turn, passive, promptTriggerReinject(current.Phase, turn)) != promptClaimEmit {
				return ""
			}
		}
		if agbrowseRequested {
			return AgbrowseSearchDirective
		}
		return ""
	}

	// mode 2: the phase changed since the last injected phase -> the full directive.
	if current.LastInjectedPhase == nil || *current.LastInjectedPhase != current.Phase {
		opts := ActiveWorkPhaseOpts(p.Cwd, current.Slug)
		directive := PhaseDirective(current.Phase, opts)
		inputs := promptClaimInputs{read: current, cursor: true, checkWork: true, work: opts}
		if current.Phase == state.PhaseI {
			directive = InterviewDirective(env)
			inputs.checkWork = false
		}
		context := directive
		if agbrowseRequested {
			context = directive + "\n\n" + AgbrowseSearchDirective
		}
		if turn != "" {
			if promptSubmitClaim(lock, p.Cwd, p.SessionID, turn, inputs, promptTriggerReinject(current.Phase, turn)) != promptClaimEmit {
				return ""
			}
		}
		return WithFooter(context, current.Phase)
	}

	// mode 3: the same phase -> the short compaction-immune stage header every turn.
	if turn != "" {
		if promptSubmitClaim(lock, p.Cwd, p.SessionID, turn, passive, promptTriggerDedup(turn, false)) != promptClaimEmit {
			return ""
		}
	}
	header := BuildStageHeader(current.Phase)
	context := header
	if agbrowseRequested {
		context = header + "\n\n" + AgbrowseSearchDirective
	}
	return WithFooter(context, current.Phase)
}

// promptTriggerWrite applies one of this unit's state writes to the session state and reports what it
// did. It is promptSubmitWriteState, the locked writer the leading section established: the change
// lands on the state the lock found, and a file the reader would not keep whole is left as it is.
func promptTriggerWrite(lock func(cwd, sessionID string, fn func() error) error, cwd, sessionID string, change func(*state.State) bool) promptSubmitWriteOutcome {
	return promptSubmitWriteState(lock, cwd, sessionID, change)
}

// promptTriggerDedup is the oracle's `{ ...state, injectedTurns: turn ? appendTurn(state.injectedTurns,
// turn) : state.injectedTurns, ...(loopArmRequested ? { loopArmSeen: true } : {}) }`. The turn is
// appended only when the state the lock found does not already hold it, so two concurrent invocations
// for one turn store the turn once, as the oracle's own write also ends up storing it; a turnless
// payload leaves the list as it is.
func promptTriggerDedup(turn string, loopArmRequested bool) func(*state.State) bool {
	return func(fresh *state.State) bool {
		if turn != "" && !slices.Contains(fresh.InjectedTurns, turn) {
			fresh.InjectedTurns = promptSubmitAppendTurn(fresh.InjectedTurns, turn)
		}
		if loopArmRequested {
			fresh.LoopArmSeen = true
		}
		return true
	}
}

// promptTriggerReinject is the oracle's `{ ...working, lastInjectedPhase: state.phase, injectedTurns:
// appendTurn(state.injectedTurns, turn) }`: the injection cursor names the phase the handler read, and
// the turn joins the bounded list.
func promptTriggerReinject(phase state.Phase, turn string) func(*state.State) bool {
	return func(fresh *state.State) bool {
		fresh.LastInjectedPhase = &phase
		if !slices.Contains(fresh.InjectedTurns, turn) {
			fresh.InjectedTurns = promptSubmitAppendTurn(fresh.InjectedTurns, turn)
		}
		return true
	}
}
