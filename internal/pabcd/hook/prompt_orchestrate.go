// prompt_orchestrate.go holds handleOrchestrateCommand, CXC v0.2.40 pabcd-state/src/hook.ts:860-874,
// 896-937, 1287-1300 and 1355-1399 (commit 3c1459ac), plus chatPlanBinding (105-126) and
// mintCheckEpoch (128-130). It fills the seam CRW-645 left in prompt_submit.go, which parses the
// line-anchored chat command and hands it here; handled=false still falls through to the loose
// detectTrigger path, as the oracle's null return does.
//
// What the range decides, in the oracle's order: the goal-mode Interview suppression, which must
// answer before any state or ledger write (HOTL boundary); the SOURCE-ROOT check every verb but
// status and reset runs; the bound D-close, which a successor issue ports and which this unit hands
// to promptOrchestrateBoundDclose; the human free-pass transition itself; the B>C source gates
// (SOURCE-DELTA-01), which the chat path must run or a phrasing would bypass the CLI's gate; the
// status line; the reset no-op; the state write, which sets the 1355-1382 fields; the PABCD ledger
// row, appended only after that write landed; and the directive, status line or reset line to
// inject. Texts are frozen byte for byte after contract/schema/cxc/name-substitution.json, and a
// backticked command is resolved at emission, never in a constant.
//
// The state write differs from the oracle in the way the memory gate, the idle-edit counter and
// handlePostCompact already differ, and for the same reason: the oracle reads the session state,
// changes fields and writes the whole state back with no lock, so an update a participating writer
// (another hook of the same session, the memory gate, the idle-edit counter) lands between the read
// and the write is lost, and the write-back rebuilds the state from the reader's normalised value,
// so a stored record the reader cannot keep is lost with it. This writer goes through
// promptSubmitWriteState: it re-reads inside the session lock, re-applies the transition to that
// read, and refuses the whole command - writing nothing - when the state moved or when the file
// holds a record the rewrite would not keep (docs/port-cxc/known-defects.md).
//
// One further departure, from the issue's step (5): the oracle lets a captureSessionSourceIdentity
// throw escape the handler, which cli.ts answers with silence; here the error is answered with the
// SOURCE-ROOT refusal that names it (the data-loss class the parity rule revision fixes during the
// port).
//
// The handler answers the context to hand the model, not the envelope: harness.ContextOutput wraps
// it (hook.ts:583-597 buildContextOutput), which is where the CRLF normalisation, the trim and the
// 32,000-unit cap live. This file has no package-level initializer and needs no Node at run time.
package hook

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptOrchestrateHandle is handleOrchestrateCommand (hook.ts:860-937, 1287-1399). current is the
// session state the leading section read, turn is the payload's turn id, and command is the parsed
// chat command. handled is false only for the two paths the oracle answers with null: the goal-mode
// Interview suppression, and a bound D-close whose seam has not been ported yet.
func promptOrchestrateHandle(p PromptSubmitPayload, current state.State, turn string, env host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error, command *fsm.OrchestrateCommand) (string, bool) {
	if env == nil {
		env = os.LookupEnv
	}
	verb := command.Verb

	// The goal-mode Interview suppression is checked on the parser path too, before any state or
	// ledger write (HOTL boundary). Unhandled: the loose path below applies the same firewall.
	if verb == fsm.VerbI && host.SuppressesInterview(sessionHookGoalStatus(p.SessionID, env)) {
		return "", false
	}

	// SOURCE-ROOT: every verb but status and reset needs a session source that resolves. status is a
	// read and reset is the operator's stand-down, so neither is refused by a broken binding.
	if verb != fsm.VerbStatus && verb != fsm.VerbReset {
		if _, err := session.Resolve(p.Cwd, p.SessionID); err != nil {
			return promptOrchestrateRefusal("SOURCE-ROOT: " + err.Error()), true
		}
	}

	// The bound D-close owns its own transition, plan commit and goalplan rows. This unit leaves the
	// seam answering unhandled, so a bound D falls through to the loose path exactly as it does today.
	if verb == fsm.VerbD && current.Slug != "" {
		return promptOrchestrateBoundDclose(p, current, turn, env, lock, command)
	}

	result := fsm.ApplyHumanTransition(current, verb, command.Attest)
	if !result.OK {
		// Refused (illegal adjacency): surface the reason, do not write state or ledger.
		return promptOrchestrateRefusal(result.Reason), true
	}

	// SOURCE-DELTA-01: the chat path gets the same B>C check as the CLI. Wiring only one of them
	// would leave a phrasing that bypasses the gate entirely, which is the class of hole this unit
	// exists to close.
	if current.Phase == state.PhaseB && verb == fsm.VerbC {
		if out, refused := promptOrchestrateSourceGate(p, current); refused {
			return out, true
		}
	}

	// status: read-only, no state change, no ledger.
	if result.Control == fsm.ControlStatus {
		return RenderStatusLine(current.Phase, current.Flags.Interview, current.Flags.AuditPassed, current.Flags.CheckPassed), true
	}

	// reset-from-IDLE no-op: recognized but nothing to write.
	if result.Noop {
		return "[crw — already IDLE]", true
	}

	// State-changing command: persist the phase and the 1355-1382 fields, then append the row. Both
	// happen only if the locked write landed; otherwise nothing is written at all.
	next, landed := promptOrchestrateWrite(lock, p, current, verb, command, turn)
	if !landed {
		return "[crw — refused: the session state changed or cannot be rewritten without losing a stored record, so this command was not applied. Nothing was written.]", true
	}
	if result.Ledger != nil {
		// The oracle writes the state and appends the row afterwards (:1360 then :1395), so an append
		// that fails leaves an applied phase change unrecorded and answers nothing. That order is kept
		// (the issue's step (9)), and so is the append's position outside the locked section, which
		// belongs to promptSubmitWriteState (docs/port-cxc/known-defects/CRW-385.md).
		if err := state.AppendLedger(p.Cwd, *result.Ledger); err != nil {
			return "", true
		}
	}

	if result.Control == fsm.ControlReset {
		return "[crw — reset → IDLE]", true
	}
	// done: the chat D-close. Inject the DONE summary directive this turn; the resting state is
	// already IDLE, so the footer surfaces IDLE.
	if result.Control == fsm.ControlDone {
		return WithFooter(PhaseDirective(state.PhaseD, nil), state.PhaseIdle), true
	}
	if next.Phase == state.PhaseI {
		return WithFooter(InterviewDirective(env), next.Phase), true
	}
	return WithFooter(PhaseDirective(next.Phase, ActiveWorkPhaseOpts(p.Cwd, current.Slug)), next.Phase), true
}

// promptOrchestrateBoundDclose is the seam for the bound D-close (hook.ts:939-1429), which the
// successor issue ports. It answers unhandled, as the seam does today, so a bound D keeps falling
// through to the loose path and nothing is half-closed.
func promptOrchestrateBoundDclose(_ PromptSubmitPayload, _ state.State, _ string, _ host.LookupEnv, _ func(cwd, sessionID string, fn func() error) error, _ *fsm.OrchestrateCommand) (string, bool) {
	return "", false
}

// promptOrchestrateSourceGate is the B>C source gate of hook.ts:920-936: the B entry snapshot must
// exist and the source must have moved during B. The first refusal is the chat path's own (it can
// read the bound source but holds no snapshot); the second and third are the CLI's.
func promptOrchestrateSourceGate(p PromptSubmitPayload, current state.State) (string, bool) {
	if current.PhaseEntrySource == nil {
		if resolved, err := session.Resolve(p.Cwd, p.SessionID); err == nil && resolved != p.Cwd {
			return promptOrchestrateRefusal("SOURCE-ROOT: bound source has no valid B baseline. Re-plan before continuing."), true
		}
		return "", false
	}
	now, err := session.Capture(p.Cwd, p.SessionID, session.CaptureOptions{ExcludeStateArtifacts: promptOrchestrateBoolPtr(true)})
	if err != nil {
		// The oracle lets this throw out of the handler, which cli.ts answers with silence. A command
		// that cannot judge the gate must say so rather than advance.
		return promptOrchestrateRefusal("SOURCE-ROOT: " + err.Error()), true
	}
	if !promptOrchestrateRootsEqual(current.PhaseEntrySource.SourceRoot, now.SourceRoot) {
		return promptOrchestrateRefusal("SOURCE-ROOT: source binding changed since B began. Re-plan and capture a new baseline; nothing was written."), true
	}
	if source.Compare(current.PhaseEntrySource.Identity(), now).Kind == source.ComparisonSame {
		return promptOrchestrateRefusal("the source is unchanged since B began (" + source.Describe(now) + "), so nothing was implemented in this B (SOURCE-DELTA-01). Nothing was written."), true
	}
	return "", false
}

// promptOrchestrateWrite applies the command to the session state inside the lock and reports
// whether the write landed. The state the lock finds must still be the one the handler read - same
// phase, same slug - and the transition is applied to that fresh state rather than to a copy of the
// stale one, so an update a participating writer landed in between survives. A write that does not
// land leaves the file exactly as it was.
func promptOrchestrateWrite(lock func(cwd, sessionID string, fn func() error) error, p PromptSubmitPayload, current state.State, verb fsm.OrchestrateVerb, command *fsm.OrchestrateCommand, turn string) (state.State, bool) {
	next, landed := state.State{}, false
	outcome := promptSubmitWriteState(lock, p.Cwd, p.SessionID, func(fresh *state.State) bool {
		if fresh.Phase != current.Phase || fresh.Slug != current.Slug {
			return false
		}
		applied := fsm.ApplyHumanTransition(*fresh, verb, command.Attest)
		if !applied.OK || applied.State == nil {
			return false
		}
		fields, ok := promptOrchestrateFields(p, *applied.State, *fresh, current, command.Attest, turn)
		if !ok {
			return false
		}
		// The change lands on the state the lock found, which is what promptSubmitWriteState writes.
		*fresh, next, landed = fields, fields, true
		return true
	})
	if outcome != promptSubmitWrote {
		return state.State{}, false
	}
	return next, landed
}

// promptOrchestrateFields is the object the oracle's write spreads over result.state
// (hook.ts:1355-1382): the B entry snapshot and its bound root, the P>A plan binding, the C check
// epoch, the two injection fields and the Stop stagnation reset. ok is false only when the check
// epoch cannot be minted, which the oracle's own randomBytes never reports; the caller then writes
// nothing rather than a state carrying a half-made nonce.
func promptOrchestrateFields(p PromptSubmitPayload, next, fresh, current state.State, att *attest.Attestation, turn string) (state.State, bool) {
	// Snapshot on entry to B, clear on every other edge: clearedIdle already nulls it for
	// reset and done, and this covers the forward edges.
	var entrySource *state.SourceIdentity
	if next.Phase == state.PhaseB {
		id, err := session.Capture(p.Cwd, p.SessionID, session.CaptureOptions{ExcludeStateArtifacts: promptOrchestrateBoolPtr(true)})
		if err != nil {
			// The oracle lets this throw out of the handler, which cli.ts answers with silence, and a
			// B entered without a baseline is a cycle the B>C gate can only refuse later. The command
			// is refused here with nothing written instead (the data-loss class, fixed).
			return state.State{}, false
		}
		stored := promptOrchestrateStoredIdentity(id)
		entrySource = &stored
	}
	// 060/032: a chat P>A mints the same plan binding the CLI does, with the CLI's checks, so a
	// cycle entered from chat reaches A bound to the unit that edge validated.
	var binding *promptOrchestrateBinding
	if current.Phase == state.PhaseP && next.Phase == state.PhaseA {
		binding = promptOrchestrateChatPlanBinding(p.Cwd, current.Slug, att)
	}
	keepBinding := next.Phase == state.PhaseA && current.Phase == state.PhaseA

	next.PhaseEntrySource = entrySource
	if entrySource != nil && entrySource.SourceRoot != nil && *entrySource.SourceRoot != "" {
		next.BoundSourceRoot = entrySource.SourceRoot
	}
	switch {
	case binding != nil:
		next.PlanUnit, next.PlanEpoch = &binding.unit, &binding.epoch
	case keepBinding:
		next.PlanUnit, next.PlanEpoch = fresh.PlanUnit, fresh.PlanEpoch
	default:
		next.PlanUnit, next.PlanEpoch = nil, nil
	}
	// 075: the same rule as the CLI - C mints, staying in C keeps, anywhere else drops.
	switch {
	case next.Phase != state.PhaseC:
		next.CheckEpoch = nil
	case current.Phase == state.PhaseC:
		next.CheckEpoch = fresh.CheckEpoch
	default:
		epoch, err := promptOrchestrateMintEpoch("c")
		if err != nil {
			return state.State{}, false
		}
		next.CheckEpoch = &epoch
	}
	// L6: a real chat transition is progress -> reset the Stop stagnation guard. The resting state
	// carries no injection cursor, so reset and done clear both.
	if next.Phase == state.PhaseIdle {
		next.OrchestrationActive, next.LastInjectedPhase = false, nil
	} else {
		phase := next.Phase
		next.OrchestrationActive, next.LastInjectedPhase = true, &phase
	}
	if turn != "" {
		next.InjectedTurns = promptSubmitAppendTurn(fresh.InjectedTurns, turn)
	}
	next.StopBlockPhase, next.StopBlockCount = nil, 0
	return next, true
}

// promptOrchestrateBinding is chatPlanBinding's {unit, epoch}: the plan unit the P>A edge validated
// and the nonce that tells this binding from a later one.
type promptOrchestrateBinding struct{ unit, epoch string }

// promptOrchestrateChatPlanBinding is chatPlanBinding (hook.ts:105-126): the binding a chat P>A
// earns, or nil when the attest does not earn one. Chat is a human free-pass for phase movement, but
// a binding is evidence, and evidence clears the same bar on both paths: the unit must hold numbered
// plan docs, and the attest must name the work-phase the goalplan says is active. Failing either
// moves the phase anyway and leaves the binding nil - the audit then refuses to open, which is the
// honest outcome. The whole function is fail-closed on the binding only.
func promptOrchestrateChatPlanBinding(cwd, slug string, att *attest.Attestation) *promptOrchestrateBinding {
	if att == nil {
		return nil
	}
	planCheck := attest.ValidatePlanArtifacts(att, cwd)
	if !planCheck.OK {
		return nil
	}
	if slug != "" {
		plan := goalplan.ReadGoalplan(cwd, slug)
		if plan == nil {
			return nil
		}
		if bind := attest.ValidateWorkPhaseBinding(att, goalplan.EffectiveActiveWorkPhaseID(plan)); !bind.OK {
			return nil
		}
	}
	epoch, err := promptOrchestrateMintEpoch("e")
	if err != nil {
		return nil
	}
	return &promptOrchestrateBinding{unit: planCheck.Unit, epoch: epoch}
}

// promptOrchestrateMintEpoch is mintEpoch (orchestrate-cli.ts:96-103) in the chat form chatPlanBinding
// and mintCheckEpoch use: the UTC instant to the second, then three random bytes as hex. The instant
// alone would collide on a fast re-plan, the case the epoch tells apart. The oracle's randomBytes
// does not fail; the error is reported so a caller can refuse instead of writing a made-up nonce.
func promptOrchestrateMintEpoch(prefix string) (string, error) {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + time.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(raw[:]), nil
}

// promptOrchestrateStoredIdentity is a capture in the form a session file stores. The exported
// converter lives in internal/pabcd/cli, which this package does not import; the shape is the one
// state.SourceIdentity documents (an absent tree hash is the empty one).
func promptOrchestrateStoredIdentity(id source.Identity) state.SourceIdentity {
	out := state.SourceIdentity{Kind: id.Kind, CommitSha: id.CommitSha, Dirty: id.Dirty, CapturedAt: id.CapturedAt, SourceRoot: id.SourceRoot}
	if id.TreeHash != "" {
		out.TreeHash = &id.TreeHash
	}
	return out
}

// promptOrchestrateRootsEqual is the oracle's sourceRoot comparison, where two absent roots are the
// same root and an absent one differs from any present one.
func promptOrchestrateRootsEqual(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// promptOrchestrateBoolPtr is the pointer CaptureOptions needs to tell "unset" from a value.
func promptOrchestrateBoolPtr(v bool) *bool { return &v }

// promptOrchestrateRefusal is buildContextOutput's text for a refusal. The oracle's own refusals
// carry the em dash and the bracketed prefix; the harness wraps the answer.
func promptOrchestrateRefusal(reason string) string {
	return "[crw — refused: " + reason + "]"
}
