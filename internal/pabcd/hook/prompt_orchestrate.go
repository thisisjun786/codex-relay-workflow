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
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// promptOrchestrateHandle is handleOrchestrateCommand (hook.ts:860-937, 1287-1399). current is the
// session state the leading section read, turn is the payload's turn id, and command is the parsed
// chat command. seams carries the four commit points of a bound D-close the oracle's own handler
// takes as HookDcloseCommitHooks; nil is a production run. handled is false only for the one path
// the oracle answers with null, the goal-mode Interview suppression.
func promptOrchestrateHandle(p PromptSubmitPayload, current state.State, turn string, env host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error, command *fsm.OrchestrateCommand, seams *promptDcloseSeams) (string, bool) {
	if env == nil {
		env = os.LookupEnv
	}
	verb := command.Verb

	// The goal-mode Interview suppression is checked on the parser path too, before any state or
	// ledger write (HOTL boundary). Unhandled: the loose path below applies the same firewall.
	if verb == fsm.VerbI && host.SuppressesInterview(sessionHookGoalStatus(p.SessionID, env)) {
		return "", false
	}

	// CRW-1109: a recognized command whose form is broken, or whose --attest is explicitly malformed, is
	// refused before it is judged or applied (the prompt entry has already stamped the session's turn; the command itself writes
	// nothing). The oracle never reads attestError and reads a command line holding a CR, U+2028 or U+2029 as chat, so both used to pass: the free transition went
	// ahead without the attestation the user typed, or the command was silently ignored. An attestation the
	// user simply left out is still valid.
	if command.FormError != "" {
		return promptOrchestrateRefusal(command.FormError + ". This command was not applied."), true
	}
	if command.AttestError != "" {
		return promptOrchestrateRefusal("the --attest of this command is malformed: " + command.AttestError + ". Fix it or leave --attest out; this command was not applied."), true
	}

	// SOURCE-ROOT: every verb but status and reset needs a session source that resolves. status is a
	// read and reset is the operator's stand-down, so neither is refused by a broken binding.
	if verb != fsm.VerbStatus && verb != fsm.VerbReset {
		if _, err := session.Resolve(p.Cwd, p.SessionID); err != nil {
			// A bound D-close reaches this before the bound handler, and a matching retry may already
			// have published its marker and committed its goalplan, so the refusal names them rather
			// than deny them (CRW-930, d1).
			if verb == fsm.VerbD && current.Slug != "" {
				closePhaseID := ""
				if command.Attest != nil {
					closePhaseID = text.Trim(command.Attest.WorkPhaseID)
				}
				// The entry text carries no "Nothing was written." claim of its own, so the publication
				// sentences are appended rather than substituted.
				return promptDcloseRefusalNaming(promptOrchestrateRefusal("SOURCE-ROOT: "+err.Error()),
					promptDcloseRecoveryPublishedAt(state.MatchesDcloseRecovery(current, closePhaseID))), true
			}
			return promptOrchestrateRefusal("SOURCE-ROOT: " + err.Error()), true
		}
	}

	// The bound D-close owns its own transition, plan commit and goalplan rows.
	if verb == fsm.VerbD && current.Slug != "" {
		return promptOrchestrateBoundDclose(p, current, turn, env, lock, command, seams)
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

	// State-changing command: persist the phase and the 1355-1382 fields and record the row. Both happen
	// only if the locked write landed; otherwise nothing is written at all. A write that published the
	// state and then failed the directory sync landed too, and carries a warning.
	//
	// The row is the one of the transition applied to the state the lock found, not the one the pre-lock
	// read produced: a participating writer that lands between the handler's read and its lock changes
	// what the locked re-application decides (the I>P interview gate's override and scan evidence, for
	// one), so the row must describe what was actually applied (docs/port-cxc/known-defects/CRW-845.md).
	//
	// The oracle writes the state and appends the row afterwards (:1360 then :1395) with nothing in
	// between, so an append that fails, or a writer that dies, leaves an applied phase change that no row
	// records and nothing repairs. The port keeps the state-first order and adds the session's ledger
	// outbox (CRW-1097): the row is prepared as a pending event inside the session lock before the state
	// is published and is appended in the same lock, and an event a dead writer or a failed append left
	// behind is recorded by the next locked writer of the session.
	next, landed, warning, rowPending, writeErr := promptOrchestrateWrite(lock, p, current, verb, command, turn, seams)
	if !landed {
		if writeErr != nil {
			return promptSubmitNotApplied(p.Cwd, p.SessionID, verb, writeErr), true
		}
		return "[crw — refused: the session state changed or cannot be rewritten without losing a stored record, so this command was not applied. Nothing was written.]", true
	}

	answer := ""
	switch {
	case result.Control == fsm.ControlReset:
		answer = "[crw — reset → IDLE]"
	case result.Control == fsm.ControlDone:
		// done: the chat D-close. Inject the DONE summary directive this turn; the resting state is
		// already IDLE, so the footer surfaces IDLE.
		answer = WithFooter(PhaseDirective(state.PhaseD, nil), state.PhaseIdle)
	case next.Phase == state.PhaseI:
		answer = WithFooter(InterviewDirective(env), next.Phase)
	default:
		answer = WithFooter(PhaseDirective(next.Phase, ActiveWorkPhaseOpts(p.Cwd, current.Slug)), next.Phase)
	}
	if warning != "" {
		// The state reached its final path and only the directory sync failed, so the command was
		// applied; the durability warning rides the success answer, as the CLI writers report one
		// (CRW-744/793/811) and as the bound D-close does (CRW-797).
		answer += "\n" + warning
	}
	if rowPending != "" {
		// The transition was applied and its row is pending in the outbox: the answer says so, where
		// the oracle answered nothing at all (CRW-1097).
		answer += "\n" + rowPending
	}
	return answer, true
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

// promptOrchestrateWrite applies the command to the session state inside the lock and reports the
// state it wrote, whether the write landed, the durability warning of a write that published the state
// and then failed the directory sync, and the warning of a row it left pending. The state the lock finds
// must still be the one the handler read - same phase, same slug - and the transition is applied to that
// fresh state rather than to a copy of the stale one, so an update a participating writer landed in
// between survives and the row describes what was really applied. A write that does not land leaves the
// file exactly as it was and nothing pending. The last answer is the lock or pre-publication write error
// (CRW-1094), nil when the locked state simply no longer matches the handler's read.
//
// Inside the lock, in order (CRW-1097): the session's pending ledger events are drained, so an earlier
// transition's row goes first; the row of this transition is prepared as a pending event; the state is
// published (a failure before the rename drops the event again); and the drain records the row. The
// seam afterOrchestratePublish stops the write right after the publication, as a writer killed there
// would, so a test can show the next writer recording the row.
func promptOrchestrateWrite(lock func(cwd, sessionID string, fn func() error) error, p PromptSubmitPayload, current state.State, verb fsm.OrchestrateVerb, command *fsm.OrchestrateCommand, turn string, seams *promptDcloseSeams) (state.State, bool, string, string, error) {
	next, landed, rowPending := state.State{}, false, ""
	var publishedErr error
	err := lock(p.Cwd, p.SessionID, func() error {
		DrainSessionLedger(p.Cwd, p.SessionID)
		fresh, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
		if unreadable || !promptSubmitRewritable(p.Cwd, p.SessionID, fresh) {
			return nil
		}
		if fresh.Phase != current.Phase || fresh.Slug != current.Slug {
			return nil
		}
		applied := fsm.ApplyHumanTransition(fresh, verb, command.Attest)
		if !applied.OK || applied.State == nil {
			return nil
		}
		fields, ok := promptOrchestrateFields(p, *applied.State, fresh, current, command.Attest, turn)
		if !ok {
			return nil
		}
		var event *state.LedgerEvent
		if applied.Ledger != nil {
			ev, err := state.NewLedgerEvent(p.Cwd, fresh, fields, applied.Ledger, nil)
			if err == nil {
				err = state.PrepareLedgerEvent(p.Cwd, ev)
			}
			if err != nil {
				return err
			}
			event = &ev
		}
		if err := state.WriteState(p.Cwd, fields); err != nil {
			if !state.Published(err) {
				if event != nil {
					_ = state.AbortLedgerEvent(p.Cwd, *event)
				}
				return err
			}
			// Renamed into place and only the directory sync failed: the transition counts as applied.
			publishedErr = err
		}
		next, landed = fields, true
		if promptOrchestrateAfterPublish(seams) {
			return nil
		}
		rowPending = promptOrchestrateRecordRow(p, event)
		return nil
	})
	if !landed {
		return state.State{}, false, "", "", err
	}
	// The state is at its final path, so the command was applied; only its durability may be in
	// question (a *state.PublishedError, from the write or reported by the lock), and the caller
	// reports the warning on the success answer.
	if publishedErr == nil && state.Published(err) {
		publishedErr = err
	}
	_, warning := promptDcloseWriteLanded(publishedErr)
	return next, true, warning, rowPending, nil
}

// promptOrchestrateAfterPublish runs the test seam that stops a write right after its publication;
// production holds none and goes on.
func promptOrchestrateAfterPublish(seams *promptDcloseSeams) bool {
	return seams != nil && seams.afterOrchestratePublish != nil && seams.afterOrchestratePublish()
}

// promptOrchestrateRecordRow drains the session's outbox with event marked as just published and
// answers the warning for a row left pending, "" when the row is in the ledger.
func promptOrchestrateRecordRow(p PromptSubmitPayload, event *state.LedgerEvent) string {
	report := DrainSessionLedger(p.Cwd, p.SessionID, promptOrchestrateEventIDs(event)...)
	if event != nil && LedgerEventStillPending(report, event.ID) {
		return promptOrchestrateRowPendingWarning(report)
	}
	return ""
}

// promptOrchestrateEventIDs is the id of event, if there is one.
func promptOrchestrateEventIDs(event *state.LedgerEvent) []string {
	if event == nil {
		return nil
	}
	return []string{event.ID}
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
