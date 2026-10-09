package cli

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The two gates CRW-384 left as pass-throughs: the A>B review binding check (CXC v0.2.40
// orchestrate-cli.ts validateReviewBinding :49-95, called at :688-691) and the B>C source gate
// (:701-717). Ported as they are, texts with the authorized name substitution only
// (.codexclaw -> .crw, cxc orchestrate -> crw pabcd orchestrate), except the one security fix the
// issue body names. Every package-level name here carries the orchestrateReviewBinding prefix
// (the packet's package-name rule).

// orchestrateReviewBindingRefuse is the oracle's refuse closure (:51-53), text for text. The verb is
// interpolated as the caller wrote it - a lowercase `b` prints as `b` - which is VerbText.
func orchestrateReviewBindingRefuse(cur state.State, verb fsm.OrchestrateVerb, sessionID, why string) *CliResult {
	return &CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
		"; " + why + " (LEAN-REVIEW-01). Either let the reviewer's exit record a verdict for this round, or close the round with `crw pabcd review-round abort --session <id>` and advance on the attest alone. Nothing was written."}
}

// orchestrateReviewBindingCheck ports validateReviewBinding (:49-95), the A>B gate that reads the
// latest plan_audit round a bound session left behind. A plan that does not resolve, no round, and a
// round with no RECORDED VERDICT all pass - the attest is the gate (LEAN-REVIEW-01) - while a round
// whose verdict was recorded must still be honest. nil means "advance".
func orchestrateReviewBindingCheck(cur state.State, a OrchestrateCliArgs, sessionID string) *CliResult {
	// readGoalplan's catch: an unreadable plan is no plan, and a plan with no round has nothing to check.
	return orchestrateReviewBindingCheckPlan(goalplan.ReadGoalplan(a.Cwd, cur.Slug), cur, a, sessionID)
}

// orchestrateReviewBindingCheckPlan is the check over a plan the caller already holds. The publication
// calls it with the plan it read inside the goalplan write lock (CRW-975), so a verdict recorded between
// the first, unlocked check and the write is judged too; nil is no plan.
func orchestrateReviewBindingCheckPlan(plan *goalplan.Goalplan, cur state.State, a OrchestrateCliArgs, sessionID string) *CliResult {
	if plan == nil {
		return nil
	}
	// A round the reader DROPPED is invisible here, exactly as it is to the oracle's latestRound: a
	// missing plan hash, a status outside the four, a lane that is not an object. That residual is kept
	// and recorded (known-defects/CRW-755.md); widening the refusal to it belongs to the reader.
	round := review.LatestRound(plan, goalplan.PurposePlanAudit)
	if round == nil {
		return nil
	}
	// A round with no RECORDED VERDICT is not a blocker: an unfinished round is indistinguishable from
	// a hook that never ran, and advancing past an open round is the agent's call. Keying on the
	// verdict rather than on status === "approved" matters, because a reviewer FAIL lands as
	// changes_requested - a real answer, honoured below, not an "unfinished" round to wave through.
	if round.Lane.Verdict == "" {
		return nil
	}
	// SECURITY FIX (CRW-755, port: fixed). The oracle returns null here (:78-80): a round whose binding
	// is incomplete - ownerSessionId, workPhaseId, planEpoch or a non-empty planFiles missing - reads as
	// "pre-binding" and checks nothing, so a reviewer FAIL is ignored. planFiles is the live case,
	// because revivePlanFiles drops the whole list when one entry is absolute or climbs out with ".."
	// (CRW-330). From here a reviewer answered, so the answer must hold.
	if round.OwnerSessionID == "" || round.WorkPhaseID == "" || round.PlanEpoch == "" || len(round.PlanFiles) == 0 {
		return &CliResult{Code: 1, Output: "orchestrate " + VerbText(a.Verb) + ": " + RenderPhaseContext(cur, sessionID) +
			"; the latest plan_audit round has a recorded verdict but no complete binding (owner session, work-phase, plan epoch and plan files), so its verdict cannot be checked. Re-run the audit round before entering B. Nothing was written."}
	}
	if round.OwnerSessionID != sessionID {
		return orchestrateReviewBindingRefuse(cur, a.Verb, sessionID,
			"round "+round.RoundID+" was approved for a different session, so it cannot be spent here")
	}
	// state.planEpoch is nullable: a stored null never equals the round's text, as !== does.
	if cur.PlanEpoch == nil || round.PlanEpoch != *cur.PlanEpoch {
		return orchestrateReviewBindingRefuse(cur, a.Verb, sessionID,
			"round "+round.RoundID+" approved an earlier plan — re-planning invalidated that approval")
	}
	activeWP := goalplan.EffectiveActiveWorkPhaseID(plan)
	if activeWP == nil || round.WorkPhaseID != *activeWP {
		return orchestrateReviewBindingRefuse(cur, a.Verb, sessionID,
			"round "+round.RoundID+" approved work-phase "+round.WorkPhaseID+", but "+orchestrateReviewBindingActiveWorkPhase(activeWP)+" is active")
	}
	if current := PlanFilesHash(Recomputed(a.Cwd, round.PlanFiles)); current != round.PlanSha256 {
		return orchestrateReviewBindingRefuse(cur, a.Verb, sessionID,
			"the plan changed after round "+round.RoundID+" approved it")
	}
	if a.Attest != nil {
		if attested := a.Attest.AuditVerdict; attested != "" && attested != string(round.Lane.Verdict) {
			return orchestrateReviewBindingRefuse(cur, a.Verb, sessionID,
				`you attested "`+attested+`" but the reviewer recorded "`+string(round.Lane.Verdict)+`"`)
		}
	}
	return nil
}

// orchestrateReviewBindingActiveWorkPhase is `${activeWp ?? "none"}` (:84): only an absent binding
// target reads as "none", never the empty string.
func orchestrateReviewBindingActiveWorkPhase(active *string) string {
	if active == nil {
		return "none"
	}
	return *active
}

// orchestrateReviewBindingRootsDiffer is the oracle's `a.sourceRoot !== b.sourceRoot`: an absent root
// (a legacy capture, or an unbound session's) equals another absent root and nothing else.
func orchestrateReviewBindingRootsDiffer(a, b *string) bool {
	if a == nil || b == nil {
		return (a == nil) != (b == nil)
	}
	return *a != *b
}

// orchestrateReviewBindingSourceGate ports the B>C block (:701-717): for a session whose bound source
// is not its native cwd, the B entry snapshot must exist; the source root must not have moved; and the
// source must have CHANGED during B. The oracle's two captures throw where this returns an error, which
// the caller reports as the IO failure it is (the convention CRW-384's port set for this file).
func orchestrateReviewBindingSourceGate(cwd, sessionID string, cur state.State) (*CliResult, error) {
	if cur.PhaseEntrySource == nil {
		// resolveSessionSource's throw is no refusal here: only a bound source other than cwd has no baseline.
		bound, err := session.Resolve(cwd, sessionID)
		if err == nil && bound != cwd {
			return &CliResult{Code: 1, Output: "orchestrate C: SOURCE-ROOT: bound source has no valid B baseline. Re-plan before continuing."}, nil
		}
		return nil, nil
	}
	exclude := true
	captured, err := session.Capture(cwd, sessionID, session.CaptureOptions{ExcludeStateArtifacts: &exclude})
	if err != nil {
		return nil, err
	}
	entry, now := cur.PhaseEntrySource.Identity(), captured
	if orchestrateReviewBindingRootsDiffer(entry.SourceRoot, now.SourceRoot) {
		return &CliResult{Code: 1, Output: "orchestrate C: SOURCE-ROOT: source binding changed since B began. Re-plan and capture a new baseline; nothing was written."}, nil
	}
	// Only "same" refuses, as the oracle's `cmp.kind === "same"` does: an "unavailable" comparison passes
	// and SOURCE-DELTA-01 cannot fire for that session (kept, known-defects/CRW-755.md).
	if source.Compare(entry, now).Kind == source.ComparisonSame {
		return &CliResult{Code: 1, Output: "orchestrate C: " + RenderPhaseContext(cur, sessionID) +
			"; the source is unchanged since B began (" + source.Describe(now) + "), so nothing was implemented in this B (SOURCE-DELTA-01). Implement inside B rather than carrying earlier work across the edge. Nothing was written."}, nil
	}
	return nil, nil
}
