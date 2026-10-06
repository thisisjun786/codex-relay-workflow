package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The transition half of CXC v0.2.40 orchestrate-cli.ts (3c1459ac, :549-1131): what a mutation
// RunOrchestrateRead delegated (OrchestrateReadResult.SessionID) does to the session. The read prefix
// (:466-548) is that library's; this file starts at the state it reads. Ported as-is, texts with the
// authorized name substitution only (.codexclaw -> .crw, cxc orchestrate -> crw pabcd orchestrate).
//
// Three pieces belong to other issues: the A>B review binding (:49-92) and the B>C source gate (:701-717) are
// pass-throughs CRW-755 fills, and the D close (:725-1064) is a refusal CRW-756 and CRW-757 fill. No verb row
// reaches this code yet, so nothing user-visible depends on those gaps (CRW-758 connects it).

// orchestrateTransitionTimestampLayout is Date.prototype.toISOString, as a ledger or goalplan row prints a time.
const orchestrateTransitionTimestampLayout = "2006-01-02T15:04:05.000Z"

// orchestrateTransitionPlanBinding is the {unit, epoch} the P>A edge minted, so A can bind a review round to
// the unit that edge validated (REVIEW-BINDING-01) instead of deriving a unit the caller names.
type orchestrateTransitionPlanBinding struct {
	unit  string
	epoch string
}

func orchestrateTransitionTimestamp() string {
	return time.Now().UTC().Format(orchestrateTransitionTimestampLayout)
}

// orchestrateTransitionMintEpoch is mintEpoch (:96-103): one nonce per edge, the UTC instant to the second
// plus three random bytes. The instant alone would collide on a fast re-plan, the case the epoch tells apart.
func orchestrateTransitionMintEpoch(prefix string) (string, error) {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + time.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(raw[:]), nil
}

// orchestrateTransitionPlaceholderDid is PLACEHOLDER_DID of attest.ts:66, which the oracle keeps private to the
// I>P override path: ASCII folding, as a JavaScript /i without the u flag compares.
func orchestrateTransitionPlaceholderDid(s string) bool {
	folded := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		folded = append(folded, c)
	}
	switch string(folded) {
	case "tbd", "todo", "na", "n/a", "none", "done", "ok":
		return true
	}
	return s != "" && (strings.Trim(s, ".") == "" || strings.Trim(s, "-") == "")
}

// orchestrateTransitionStoredIdentity is a capture in the form a session file stores (state.SourceIdentity).
func orchestrateTransitionStoredIdentity(id source.Identity) state.SourceIdentity {
	out := state.SourceIdentity{Kind: id.Kind, CommitSha: id.CommitSha, Dirty: id.Dirty, CapturedAt: id.CapturedAt, SourceRoot: id.SourceRoot}
	if id.TreeHash != "" {
		out.TreeHash = &id.TreeHash
	}
	return out
}

// orchestrateTransitionWithArchitectHint is withArchitectHint (:456-464): a terminal entry reaches P without a
// UserPromptSubmit turn, so the formal-P consultation pointer is repeated there. Advice only; no gate.
func orchestrateTransitionWithArchitectHint(phase state.Phase, output string) string {
	if phase != state.PhaseP {
		return output
	}
	return output + " [formal P: architect proposal -> main executable plan -> same-architect reflection before A (crw-pabcd phase-plan)]"
}

// orchestrateTransitionPlanBindingOf is :1073-1074: the P>A edge keeps the unit and epoch it just minted,
// entering A another way keeps the session's own, and anywhere else drops both.
func orchestrateTransitionPlanBindingOf(binding *orchestrateTransitionPlanBinding, cur state.State, phase state.Phase) (unit, epoch *string) {
	if binding != nil {
		u, e := binding.unit, binding.epoch
		return &u, &e
	}
	if phase == state.PhaseA {
		return cur.PlanUnit, cur.PlanEpoch
	}
	return nil, nil
}

// orchestrateTransitionCheckEpoch is :1075-1077: entering C mints a check epoch, staying in C keeps the
// session's own, and anywhere else drops it, so re-checking invalidates the earlier receipt.
func orchestrateTransitionCheckEpoch(from, to state.Phase, current *string) (*string, error) {
	if to != state.PhaseC {
		return nil, nil
	}
	if from == state.PhaseC {
		return current, nil
	}
	epoch, err := orchestrateTransitionMintEpoch("c")
	if err != nil {
		return nil, err
	}
	return &epoch, nil
}

// orchestrateTransitionSupersedeStaleRounds is the P>A housekeeping of :1085-1118: a fresh plan epoch orphans
// every open plan_audit round this session owns, so they are closed under the goalplan write lock before the
// new binding lands. The stranded epoch is read from the rounds, because this edge is entered from P, where
// the A-only binding has already been normalised to null. Fail-open, as the oracle's catch is.
func orchestrateTransitionSupersedeStaleRounds(cwd, slug, sessionID, epoch string) {
	_, _ = goalplan.WithGoalplanWriteLock(cwd, slug, func(plan *goalplan.Goalplan) (struct{}, error) {
		stranded := ""
		for i := range plan.ReviewRounds {
			r := plan.ReviewRounds[i]
			if r.Purpose == goalplan.PurposePlanAudit && r.OwnerSessionID == sessionID && r.PlanEpoch != "" && r.PlanEpoch != epoch &&
				r.Status != goalplan.ReviewApproved && r.Status != goalplan.ReviewChangesRequested && r.Status != goalplan.ReviewInconclusive {
				stranded = r.PlanEpoch
				break
			}
		}
		swept, closed := review.SupersedeStaleRounds(plan, goalplan.PurposePlanAudit, sessionID, stranded)
		if len(closed) == 0 {
			return struct{}{}, nil
		}
		if err := goalplan.WriteGoalplan(cwd, swept); err != nil {
			return struct{}{}, err
		}
		for _, roundID := range closed {
			row := roundID
			err := goalplan.AppendGoalplanLedger(cwd, slug, goalplan.GoalplanLedgerEntry{
				Ts: orchestrateTransitionTimestamp(), Slug: slug, Event: goalplan.EventReviewRoundSuperseded,
				Detail: "the plan was re-planned, so this round can no longer be spent", RoundID: &row,
			})
			if err != nil {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	}, nil)
}

// orchestrateTransitionReviewBinding is the A>B review binding check (validateReviewBinding, :49-92, :688-691).
// CRW-755 ports it, the security fix for a recorded verdict without a binding included; until then it passes.
func orchestrateTransitionReviewBinding(cur state.State, a OrchestrateCliArgs, sessionID string) *CliResult {
	return nil
}

// orchestrateTransitionSourceGate is the B>C source gate (:701-717): the B entry snapshot must exist and the
// source must have changed during B. CRW-755 ports it; until then it passes.
func orchestrateTransitionSourceGate(cwd, sessionID string, cur state.State) *CliResult {
	return nil
}

// orchestrateTransitionDClose is the D close (:725-1064), which CRW-756 and CRW-757 port. This issue refuses
// the edge rather than half-close a cycle.
func orchestrateTransitionDClose(cwd, sessionID, closePhaseID string, cur state.State, att *attest.Attestation, recovering bool) (CliResult, error) {
	return CliResult{}, errors.New("orchestrate D close is not ported yet")
}

// orchestrateTransitionStateWritable is this writer's half of the port's data-loss rule for the session file
// (docs/port-cxc/known-defects.md, "Found by the CRW-609 lossless rewrite guard"): the oracle publishes the
// state the reader rebuilt, so a stored lone surrogate, invalid UTF-8, a receipt past the cap or a record past
// the list cap is replaced or dropped. The port refuses instead, as every other session writer here does.
func orchestrateTransitionStateWritable(cwd, sessionID string, kept state.State) bool {
	return cliVerdictsIntact(cwd, sessionID, len(kept.UnverifiedSubagents))
}

// orchestrateTransitionStateRefusal is the refusal shared by the three writes this file owns.
func orchestrateTransitionStateRefusal(verb fsm.OrchestrateVerb) CliResult {
	return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) +
		": session state holds records this rewrite would change; refusing to overwrite it. Nothing was written."}
}

// RunOrchestrateTransition ports the transition half of orchestrate-cli.ts (:549-1131) on the mutation
// RunOrchestrateRead delegated. It does its own state, goalplan and ledger IO and returns the oracle's
// CliResult; an error is a Go-level IO failure the oracle would have thrown, which no refusal path uses.
//
// The whole read, gate and write runs under the session's exclusive lock. The oracle takes no lock, so a hook
// that writes the same session meanwhile is overwritten by this command's earlier copy and its update is lost;
// the port fixes that (the data-loss class the parity rule revision of 2026-10-03 fixes during the port). A
// lock that cannot be taken is an error, never a silent success.
func RunOrchestrateTransition(a OrchestrateCliArgs, sessionID string) (CliResult, error) {
	var out CliResult
	err := state.WithSessionLock(a.Cwd, sessionID, func() error {
		var inner error
		out, inner = orchestrateTransitionApply(a, sessionID)
		return inner
	})
	if err != nil {
		return CliResult{}, err
	}
	return out, nil
}

// orchestrateTransitionApply is the ported body, run while the caller holds the session lock.
func orchestrateTransitionApply(a OrchestrateCliArgs, sessionID string) (CliResult, error) {
	cwd, verb := a.Cwd, a.Verb
	cur := state.ReadState(cwd, sessionID)
	// 050 wp5 §5: the fixed close target, and whether this D request is finishing a close that already started.
	// A matching marker means the first attempt spent the binding, transition and receipt gates already.
	closePhaseID := ""
	if verb == fsm.VerbD && a.Attest != nil {
		closePhaseID = text.Trim(a.Attest.WorkPhaseID)
	}
	recoveringDclose := verb == fsm.VerbD && state.MatchesDcloseRecovery(cur, closePhaseID)

	// reset: a control override, the same cleared-IDLE write as the human path.
	if verb == fsm.VerbReset {
		res := fsm.ApplyHumanTransition(cur, fsm.VerbReset, a.Attest)
		if res.Noop {
			return CliResult{Code: 0, Output: "orchestrate reset: " + RenderPhaseContext(cur, sessionID) + "; already IDLE"}, nil
		}
		if res.State != nil {
			if !orchestrateTransitionStateWritable(cwd, sessionID, cur) {
				return orchestrateTransitionStateRefusal(verb), nil
			}
			next := *res.State
			next.OrchestrationActive, next.LastInjectedPhase = false, nil
			next.StopBlockPhase, next.StopBlockCount = nil, 0
			if err := state.WriteState(cwd, next); err != nil {
				return CliResult{}, err
			}
			if res.Ledger != nil {
				if err := state.AppendLedger(cwd, *res.Ledger); err != nil {
					return CliResult{}, err
				}
			}
		}
		return CliResult{Code: 0, Output: "orchestrate reset: current=" + string(cur.Phase) + " -> IDLE (session " + sessionID + ")"}, nil
	}

	// A phase verb: agent-gated through the un-weakened transition(), validated before any write.
	to := state.Phase(verb)
	if _, err := session.Resolve(cwd, sessionID); err != nil {
		return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": SOURCE-ROOT: " + err.Error()}, nil
	}
	// #133: entry refusal for a goalplan-bound cycle with no resolvable source identity, so a bound non-git
	// session cannot enter P and strand at C, whose close needs a receipt that cannot be bound. Guarded on
	// state.slug, so unbound HITL cycles are untouched, and on the ENTRY edges only: A>P is a re-plan.
	if to == state.PhaseP && (cur.Phase == state.PhaseIdle || cur.Phase == state.PhaseI) && cur.Slug != "" {
		if gate := session.CheckBound(cwd, sessionID); !gate.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + gate.Reason + "\nNothing was written."}, nil
		}
	}
	// P>A plan-artifact gate (DIFFLEVEL-ROADMAP-01): the plan must exist as numbered on-disk docs before Audit.
	// It runs even with no attestation, so the first error names planUnit. Fail-closed on this edge only.
	var binding *orchestrateTransitionPlanBinding
	if cur.Phase == state.PhaseP && to == state.PhaseA {
		planCheck := attest.ValidatePlanArtifacts(a.Attest, cwd)
		if !planCheck.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + planCheck.Reason}, nil
		}
		epoch, err := orchestrateTransitionMintEpoch("e")
		if err != nil {
			return CliResult{}, err
		}
		binding = &orchestrateTransitionPlanBinding{unit: planCheck.Unit, epoch: epoch}
	}
	// Work-phase binding gate (LOOP-UNIT-CHAIN-01): on a gated edge of a bound session the attestation must
	// name the one effective active work-phase. Fail-open when no goalplan resolves.
	if attest.IsGated(cur.Phase, to) && cur.Slug != "" && !recoveringDclose {
		var effective *string
		if plan := goalplan.ReadGoalplan(cwd, cur.Slug); plan != nil {
			effective = goalplan.EffectiveActiveWorkPhaseID(plan)
		}
		if bindCheck := attest.ValidateWorkPhaseBinding(a.Attest, effective); !bindCheck.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + bindCheck.Reason}, nil
		}
	}
	// I>P: the interview soft gate. transition() has no override support, so this adds the equivalent of
	// applyHumanTransition's override for I>P only.
	if cur.Phase == state.PhaseI && to == state.PhaseP {
		gate := interview.EvaluateInterviewGate(cur.Interview, &interview.GateEvidence{
			BackedDimensions: ledger.DimensionsBackedByAnswers(cwd, sessionID),
		})
		switch {
		case gate.Ready:
			// Ready: the normal transition() path handles it (it derives flags.interview from the tracker).
		case a.Attest != nil && a.Attest.Override:
			if a.Attest.Did == "" || orchestrateTransitionPlaceholderDid(a.Attest.Did) {
				return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
					"; I\u2192P override requires a specific \"did\" narrative explaining why the interview is complete (not empty or placeholder)."}, nil
			}
			if a.Attest.From != state.PhaseI || a.Attest.To != state.PhaseP {
				return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
					"; I\u2192P override attest from/to must be I/P, got " + string(a.Attest.From) + "/" + string(a.Attest.To) + "."}, nil
			}
			next := cur
			next.Flags.Interview = true
			if ok, reason := fsm.CanEnter(to, next); !ok {
				return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + reason}, nil
			}
			// I>P is never B or A, so the snapshot and both bindings are cleared explicitly: this writer
			// bypasses transition() and would otherwise spread stale values.
			next.Phase, next.OrchestrationActive, next.LastInjectedPhase = to, true, &to
			next.StopBlockPhase, next.StopBlockCount = nil, 0
			next.PhaseEntrySource, next.PlanUnit, next.PlanEpoch, next.CheckEpoch = nil, nil, nil, nil
			if !orchestrateTransitionStateWritable(cwd, sessionID, cur) {
				return orchestrateTransitionStateRefusal(verb), nil
			}
			if err := state.WriteState(cwd, next); err != nil {
				return CliResult{}, err
			}
			hook.ResetRenderLedger(cwd)
			scan := state.ScanEvidence{HighContradictionCount: float64(gate.HighContradictionCount)}
			if cur.Interview != nil {
				scan.ScanRounds = float64(cur.Interview.ScanRounds)
			}
			yes := true
			did := a.Attest.Did
			from := cur.Phase
			if err := state.AppendLedger(cwd, state.LedgerEntry{
				TS: orchestrateTransitionTimestamp(), SessionID: cur.SessionID, From: &from, To: to, Reason: "cli",
				Actor: "agent", Override: &yes, ScanEvidence: &scan, Evidence: &did,
			}); err != nil {
				return CliResult{}, err
			}
			return CliResult{Code: 0, Output: orchestrateTransitionWithArchitectHint(state.PhaseP,
				"orchestrate P: I \u2192 P (agent override, session "+sessionID+")")}, nil
		default:
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
				"; interview soft-gate: " + strings.Join(gate.Warnings, "; ") + ". Pass override:true in --attest to proceed."}, nil
		}
	}
	// 050 wp5 §5: a marker-matched D retry resumes a transition the first attempt already made legally;
	// state.phase is IDLE by then, so transition() would refuse C>D on a session whose cycle is mid-close.
	var result fsm.TransitionResult
	if recoveringDclose {
		next := cur
		next.Phase = state.PhaseD
		result = fsm.TransitionResult{OK: true, State: &next}
	} else {
		result = fsm.Transition(cur, to, a.Attest)
	}
	if !result.OK || result.State == nil {
		reason := result.Reason
		if reason == "" {
			reason = "transition refused"
		}
		return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + reason}, nil
	}
	// LEAN-REVIEW-01: an open round is honoured, never required; the B>C source gates follow, then the D close.
	if cur.Phase == state.PhaseA && to == state.PhaseB && cur.Slug != "" {
		if refusal := orchestrateTransitionReviewBinding(cur, a, sessionID); refusal != nil {
			return *refusal, nil
		}
	}
	if cur.Phase == state.PhaseB && to == state.PhaseC {
		if refusal := orchestrateTransitionSourceGate(cwd, sessionID, cur); refusal != nil {
			return *refusal, nil
		}
	}
	if to == state.PhaseD {
		return orchestrateTransitionDClose(cwd, sessionID, closePhaseID, cur, a.Attest, recoveringDclose)
	}
	// The data-loss refusal, before any of this edge's writes, the goalplan housekeeping included.
	if !orchestrateTransitionStateWritable(cwd, sessionID, cur) {
		return orchestrateTransitionStateRefusal(verb), nil
	}

	// L6: a real transition is progress, so the Stop stagnation guard resets. SOURCE-DELTA-01: snapshot the
	// source on entry to B and clear it on every other edge, so no stale snapshot outlives its phase.
	var entrySource *state.SourceIdentity
	if result.State.Phase == state.PhaseB {
		exclude := true
		id, err := session.Capture(cwd, sessionID, session.CaptureOptions{ExcludeStateArtifacts: &exclude})
		if err != nil {
			return CliResult{}, err
		}
		stored := orchestrateTransitionStoredIdentity(id)
		entrySource = &stored
	}
	unit, epoch := orchestrateTransitionPlanBindingOf(binding, cur, result.State.Phase)
	checkEpoch, err := orchestrateTransitionCheckEpoch(cur.Phase, result.State.Phase, cur.CheckEpoch)
	if err != nil {
		return CliResult{}, err
	}
	// 032: a fresh epoch orphans every round the old one owned. Close them before the new binding lands.
	if binding != nil && cur.Slug != "" {
		orchestrateTransitionSupersedeStaleRounds(cwd, cur.Slug, sessionID, binding.epoch)
	}
	next := *result.State
	next.OrchestrationActive, next.LastInjectedPhase = result.State.Phase != state.PhaseIdle, &result.State.Phase
	next.StopBlockPhase, next.StopBlockCount = nil, 0
	next.PhaseEntrySource = entrySource
	if entrySource != nil && entrySource.SourceRoot != nil && *entrySource.SourceRoot != "" {
		next.BoundSourceRoot = entrySource.SourceRoot
	}
	next.PlanUnit, next.PlanEpoch, next.CheckEpoch = unit, epoch, checkEpoch
	if err := state.WriteState(cwd, next); err != nil {
		return CliResult{}, err
	}
	// C-RENDER-GROUNDING-01: a new cycle starts at P, so the render ledger is cleared (stale rows misfire).
	if result.State.Phase == state.PhaseP {
		hook.ResetRenderLedger(cwd)
	}
	from := cur.Phase
	row := state.LedgerEntry{TS: orchestrateTransitionTimestamp(), SessionID: cur.SessionID, From: &from, To: result.State.Phase, Reason: "cli"}
	if a.Attest != nil && a.Attest.Did != "" {
		did := a.Attest.Did
		row.Evidence = &did
	}
	if err := state.AppendLedger(cwd, row); err != nil {
		return CliResult{}, err
	}
	arrow := string(cur.Phase) + " \u2192 " + string(result.State.Phase)
	return CliResult{Code: 0, Output: orchestrateTransitionWithArchitectHint(result.State.Phase,
		"orchestrate "+VerbText(verb)+": current="+string(cur.Phase)+" -> "+string(result.State.Phase)+" ("+arrow+", session "+sessionID+")")}, nil
}
