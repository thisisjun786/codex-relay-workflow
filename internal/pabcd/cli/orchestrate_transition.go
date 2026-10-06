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
// Three pieces of the oracle's file belong to other issues and are not ported here: the A>B review
// binding (:49-92) and the B>C source gates (:701-717) are pass-throughs CRW-755 fills, and the D close
// (:725-1064) is a refusal CRW-756 (normal path) and CRW-757 (recovery) fill. No verb row reaches this
// code yet, so nothing user-visible depends on those gaps (CRW-758 connects the row).

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
// plus three random bytes. The instant alone would collide on a fast re-plan, which is exactly the case the
// epoch exists to tell apart. A random source that cannot answer is an error, not a silent short nonce.
func orchestrateTransitionMintEpoch(prefix string) (string, error) {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + time.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(raw[:]), nil
}

// orchestrateTransitionPlaceholderDid is PLACEHOLDER_DID of attest.ts:66 (/^(tbd|todo|n\/?a|none|done|ok|\.+|-+)$/i),
// which the oracle keeps private to the I>P override path. ASCII folding, as a JavaScript /i without the u flag compares.
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
// A capture omits an empty tree hash, so the stored field is present only when the capture has one.
func orchestrateTransitionStoredIdentity(id source.Identity) state.SourceIdentity {
	out := state.SourceIdentity{Kind: id.Kind, CommitSha: id.CommitSha, Dirty: id.Dirty, CapturedAt: id.CapturedAt, SourceRoot: id.SourceRoot}
	if id.TreeHash != "" {
		out.TreeHash = &id.TreeHash
	}
	return out
}

// orchestrateTransitionWithArchitectHint is withArchitectHint (:456-464): a terminal entry can reach P without
// a UserPromptSubmit turn, so the formal-P consultation pointer is repeated there. Advice only; no gate.
func orchestrateTransitionWithArchitectHint(phase state.Phase, output string) string {
	if phase != state.PhaseP {
		return output
	}
	return output + " [formal P: architect proposal -> main executable plan -> same-architect reflection before A (crw-pabcd phase-plan)]"
}

// orchestrateTransitionPlanBindingOf is :1073-1074: the P>A edge keeps the unit and epoch it just minted,
// entering A another way keeps the session's own (A-only, so the state read at P has already normalised it
// away), and anywhere else drops both so a re-plan cannot leave an old approval looking current.
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
// session's own, and anywhere else drops it, so re-checking invalidates the receipt of the earlier check.
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

// orchestrateTransitionSupersedeStaleRounds is the P>A housekeeping of :1085-1118. A fresh plan epoch orphans
// every open plan_audit round this session owns, so they are closed under the goalplan write lock before the
// new binding lands; otherwise the gate would wait on a round no sign-off can reach. The stranded epoch is
// read from the rounds, not from state, because this edge is entered from P, where the A-only binding has
// already been normalised to null. Fail-open: a lock that cannot be taken, an unreadable plan or a failed
// write leaves the edge to proceed, as the oracle's catch does.
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

// orchestrateTransitionReviewBinding is the A>B review binding check (validateReviewBinding, :49-92 and
// :688-691). CRW-755 ports it, including the security fix for a recorded verdict that carries no binding;
// until then the edge passes, as the oracle does for a round that proves nothing.
func orchestrateTransitionReviewBinding(cur state.State, a OrchestrateCliArgs, sessionID string) *CliResult {
	return nil
}

// orchestrateTransitionSourceGate is the B>C source gate (:701-717): the B entry snapshot must exist and the
// source must have changed during B. CRW-755 ports it; until then the edge passes.
func orchestrateTransitionSourceGate(cwd, sessionID string, cur state.State) *CliResult {
	return nil
}

// orchestrateTransitionDClose is the D close (:725-1064): the unbound close, and the bound one with its
// goalplan lock, receipt gate and recovery branch. CRW-756 (normal path) and CRW-757 (recovery) port it;
// this issue refuses the edge rather than half-close a cycle.
func orchestrateTransitionDClose(cwd, sessionID, closePhaseID string, cur state.State, att *attest.Attestation, recovering bool) (CliResult, error) {
	return CliResult{}, errors.New("orchestrate D close is not ported yet")
}

// orchestrateTransitionStateWritable is this writer's half of the port's data-loss rule for the session file
// (docs/port-cxc/known-defects.md, "Found by the CRW-609 lossless rewrite guard"): the oracle's writeState
// publishes the state the reader rebuilt, so a stored lone surrogate, invalid UTF-8, a receipt past the cap
// or a record past the list cap is replaced or dropped on the way through, and the oracle writes anyway. The
// port refuses instead, as every other session writer here does (cliVerdictsIntact, the hook's rewritable
// check, the evidence guard), because over-refusal is never loss. A file that does not exist stores nothing.
func orchestrateTransitionStateWritable(cwd, sessionID string, kept state.State) bool {
	return cliVerdictsIntact(cwd, sessionID, len(kept.UnverifiedSubagents))
}

// orchestrateTransitionStateRefusal is the refusal shared by the three writes this file owns. It is the
// port's own text: the oracle has no refusal here, because it never checks (the known-defects line above).
func orchestrateTransitionStateRefusal(verb fsm.OrchestrateVerb) CliResult {
	return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) +
		": session state holds records this rewrite would change; refusing to overwrite it. Nothing was written."}
}

// RunOrchestrateTransition ports the transition half of orchestrate-cli.ts (:549-1131) on the mutation
// RunOrchestrateRead delegated. It does its own state, goalplan and ledger IO, as the oracle's
// runOrchestrateCli does, and returns the oracle's CliResult; an error is a Go-level IO failure the oracle
// would have thrown, which no refusal path uses.
func RunOrchestrateTransition(a OrchestrateCliArgs, sessionID string) (CliResult, error) {
	cwd, verb := a.Cwd, a.Verb
	cur := state.ReadState(cwd, sessionID)
	// 050 wp5 §5: the fixed close target, and whether this D request is finishing a close that already
	// started. A matching marker means the first attempt already spent the binding, transition and receipt
	// gates, so re-consuming them would refuse a retry for gates it has no way to satisfy twice.
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

	// A phase verb: agent-gated through the un-weakened transition(). Validate before any phase or goalplan
	// write; identity and artifact cwd stay native.
	to := state.Phase(verb)
	if _, err := session.Resolve(cwd, sessionID); err != nil {
		return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": SOURCE-ROOT: " + err.Error()}, nil
	}
	// #133: entry refusal for a goalplan-bound cycle with no resolvable source identity. Same neighbourhood as
	// the SOURCE-ROOT check, for the same reason: without it a bound non-git session enters P, passes A and B
	// (B>C is deliberately fail-open) and strands at C, whose close needs a receipt that cannot be bound.
	// Guarded on state.slug, the bound-session condition the work-phase and receipt gates already use, so
	// unbound HITL cycles are untouched, and on the ENTRY edges only: A>P is a re-plan inside a cycle already
	// in flight, and refusing it would strand the session rather than protect it.
	if to == state.PhaseP && (cur.Phase == state.PhaseIdle || cur.Phase == state.PhaseI) && cur.Slug != "" {
		if gate := session.CheckBound(cwd, sessionID); !gate.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + gate.Reason + "\nNothing was written."}, nil
		}
	}
	// P>A plan-artifact gate (DIFFLEVEL-ROADMAP-01): the plan must exist as numbered on-disk docs before
	// Audit. It runs even with no attestation, so the first error names planUnit. Fail-closed on this edge only.
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
	// Work-phase binding gate (LOOP-UNIT-CHAIN-01): on every gated edge of a goalplan-bound session the
	// attestation must name the one effective active work-phase. Fail-open when no goalplan resolves.
	if attest.IsGated(cur.Phase, to) && cur.Slug != "" && !recoveringDclose {
		var effective *string
		if plan := goalplan.ReadGoalplan(cwd, cur.Slug); plan != nil {
			effective = goalplan.EffectiveActiveWorkPhaseID(plan)
		}
		if bindCheck := attest.ValidateWorkPhaseBinding(a.Attest, effective); !bindCheck.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + bindCheck.Reason}, nil
		}
	}
	// I>P: the interview soft gate. The agent CLI path uses the un-weakened transition(), which has no
	// override support, so this adds the equivalent of applyHumanTransition's override for I>P only.
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
			// I>P is never B, so the snapshot is cleared explicitly rather than spread, and it is neither B
			// nor A, so both bindings are cleared: this writer bypasses transition() and would otherwise
			// spread stale values.
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
	// 050 wp5 §5: a marker-matched D retry is resuming a transition the first attempt already made legally.
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
	// LEAN-REVIEW-01: an open round is honoured, never required. A recorded verdict adds provenance on top of
	// the attest. The B>C source gates follow, and the D close takes the cycle to IDLE.
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
	// The port's data-loss refusal, before any of this edge's writes (the goalplan housekeeping below
	// included), so a refusal leaves state, both ledgers and the goalplan untouched.
	if !orchestrateTransitionStateWritable(cwd, sessionID, cur) {
		return orchestrateTransitionStateRefusal(verb), nil
	}

	// L6: a real CLI transition is progress, so the Stop stagnation guard resets. SOURCE-DELTA-01: snapshot the
	// source on entry to B and clear it on every other edge, so a stale snapshot cannot outlive its phase.
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
	// C-RENDER-GROUNDING-01: a new cycle starts at P, so the render ledger is cleared and the Stop advisory
	// judges this cycle's rows only (stale rows both suppress and misfire).
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
