package goalplan

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// The v2 final-gate checks of CXC v0.2.40 goalplan.ts:1772-1921 (commit 3c1459ac), ported as-is with the oracle's reason
// strings byte for byte: finalGateReasons, roundReasons, desktopArtifactCriterionIds, hasArtifactIdentityForCriterion and
// identityReasons. Behaviour is kept, oracle defects included; the divergences Go cannot reproduce are recorded in
// docs/port-cxc/known-defects.md. validateGoalplan, which calls finalGateReasons, belongs to a later issue.
//
// Every new package-level name carries the finalGate prefix. An absent optional the oracle interpolates prints "undefined"
// (finalGateJSText), and a JavaScript falsy test on an optional string is a nil-or-empty test here, because the revivers keep
// a stored "" apart from an absent key.

// finalGateReasons is finalGateReasons: the final-gate slice of the E8 validator, in the oracle's order.
func finalGateReasons(plan *Goalplan, ctx *GoalplanValidationCtx) []string {
	markerPath, markerPresent := "", false
	if ctx != nil {
		if p, err := SchemaMarkerPath(ctx.Cwd, plan.Slug); err == nil {
			markerPath = p
			if _, statErr := os.Stat(p); statErr == nil {
				markerPresent = true
			}
		}
	}
	version := EffectiveSchemaVersion(plan, markerPresent)
	if version < 2 {
		return []string{}
	}
	if ctx == nil {
		return []string{"this plan is schemaVersion >= 2 but validateGoalplan was called without a validation context, so the final gate could not be checked — this is a refusal, not a pass"}
	}
	if markerPresent && finalGateDeclaredSchemaVersion(plan) < 2 {
		declared := "none"
		if plan.SchemaVersion != nil {
			declared = finalGateJSNumber(*plan.SchemaVersion)
		}
		return []string{fmt.Sprintf("this plan was promoted to schemaVersion 2 (%s exists) but the plan file declares %s — restore \"schemaVersion\": 2 instead of downgrading", markerPath, declared)}
	}

	out := []string{}
	for _, c := range plan.Criteria {
		if c.Surface == "" {
			out = append(out, fmt.Sprintf("criterion %s has no valid surface (\"logic\" | \"web\" | \"tui\" | \"desktop\") — schemaVersion 2 requires it, since an unclassified criterion would silently escape the QA requirement", c.ID))
		}
	}
	g := plan.FinalGate
	if g == nil {
		// No verb in this build opens a final_gate round, so say the true state and name the escape that exists.
		out = append(out, fmt.Sprintf("schemaVersion %s requires an approved finalGate, and no command in this build opens a final-gate review round - either record the gate another way or declare schemaVersion 1, which new plans now use by default", finalGateJSNumber(version)))
		return out
	}
	if g.Status != GateApproved {
		out = append(out, fmt.Sprintf("final gate is %s, not approved — close the gate before completing the goal", finalGateJSText(string(g.Status))))
	}
	if g.Verdict == VerdictFail {
		out = append(out, "final gate verdict is fail")
	}
	if expectedQa := ComputeQaRequired(plan); expectedQa != g.QaRequired {
		out = append(out, fmt.Sprintf("final gate recorded qaRequired=%t but the plan now scans as %t — criteria changed after the gate opened, so re-open it", g.QaRequired, expectedQa))
	}
	out = append(out, finalGateRoundReasons(plan, g)...)
	out = append(out, finalGateIdentityReasons(plan, g, ctx)...)
	return out
}

// finalGateRoundReasons is roundReasons.
func finalGateRoundReasons(plan *Goalplan, g *FinalGateState) []string {
	if g.ReviewRoundID == nil || *g.ReviewRoundID == "" {
		return []string{"final gate has no reviewRoundId — an approval must name the round that produced it"}
	}
	roundID := *g.ReviewRoundID
	var round *ReviewRoundState
	for i := range plan.ReviewRounds {
		if plan.ReviewRounds[i].RoundID == roundID {
			round = &plan.ReviewRounds[i]
			break
		}
	}
	if round == nil {
		return []string{fmt.Sprintf("final gate names review round %s, which is not in the plan", roundID)}
	}
	out := []string{}
	if round.Purpose != PurposeFinalGate {
		out = append(out, fmt.Sprintf("review round %s has purpose \"%s\" — a plan audit cannot stand in for the final code gate", round.RoundID, finalGateJSText(string(round.Purpose))))
	}
	if round.Status != ReviewApproved {
		out = append(out, fmt.Sprintf("review round %s is %s, not approved", round.RoundID, finalGateJSText(string(round.Status))))
	}
	if round.Lane.Verdict == "" {
		out = append(out, fmt.Sprintf("review round %s recorded no verdict", round.RoundID))
	} else if round.Lane.Verdict != g.Verdict {
		out = append(out, fmt.Sprintf("review round %s says \"%s\" but the final gate says \"%s\"", round.RoundID, round.Lane.Verdict, finalGateJSText(string(g.Verdict))))
	}
	return out
}

// finalGateDesktopArtifactCriterionIDs is desktopArtifactCriterionIds: desktop criteria that must carry an artifact identity.
func finalGateDesktopArtifactCriterionIDs(plan *Goalplan) []string {
	out := []string{}
	for _, criterion := range plan.Criteria {
		if criterion.Surface == SurfaceDesktop && criterion.Presented != PresentedNative {
			out = append(out, criterion.ID)
		}
	}
	return out
}

// finalGateHasArtifactIdentityForCriterion is hasArtifactIdentityForCriterion. The basename is the oracle's path split on a
// slash or a backslash, not filepath.Base: a trailing separator yields "".
func finalGateHasArtifactIdentityForCriterion(manifest []gate.ArtifactDigest, criterionID string) bool {
	for _, entry := range manifest {
		if entry.Kind == gate.ArtifactIdentity && finalGateBaseName(entry.Path) == "artifact-identity.json" && slices.Contains(entry.CriterionIDs, criterionID) {
			return true
		}
	}
	return false
}

// finalGateIdentityReasons is identityReasons: collect every identity in play, then compare each against the tree now, so
// every structural reason precedes every "different source" reason.
func finalGateIdentityReasons(plan *Goalplan, g *FinalGateState, ctx *GoalplanValidationCtx) []string {
	out := []string{}
	current, captureReason := finalGateCaptureCurrent(ctx)
	if captureReason != "" {
		return []string{captureReason}
	}
	if g.SourceIdentity == nil {
		out = append(out, "final gate has no sourceIdentity — an approval must record the tree it approved")
	}

	type finalGateNamed struct {
		label    string
		identity *SourceIdentity
	}
	named := []finalGateNamed{{"the final gate", g.SourceIdentity}}
	if g.ReviewRoundID != nil {
		for i := range plan.ReviewRounds {
			if plan.ReviewRounds[i].RoundID == *g.ReviewRoundID {
				if plan.ReviewRounds[i].Lane.SourceIdentity != nil {
					named = append(named, finalGateNamed{"the reviewer", plan.ReviewRounds[i].Lane.SourceIdentity})
				}
				break
			}
		}
	}

	type finalGateSlot struct {
		label string
		path  *string
		kind  gate.ReceiptKind
	}
	slots := []finalGateSlot{{"the test receipt", g.TestReceiptPath, gate.ReceiptTest}}
	if g.QaRequired {
		slots = append(slots, finalGateSlot{"the QA receipt", g.QaReceiptPath, gate.ReceiptQA})
	}
	for _, slot := range slots {
		if slot.path == nil || *slot.path == "" {
			out = append(out, slot.label+" path is missing")
			continue
		}
		evidence, err, panicked := finalGateReadReceipt(ctx, *slot.path, slot.kind)
		if panicked != "" {
			out = append(out, fmt.Sprintf("%s could not be read: %s", slot.label, panicked))
			continue
		}
		if err != nil {
			out = append(out, fmt.Sprintf("%s is not usable: %s", slot.label, err.Error()))
			continue
		}
		if slot.kind == gate.ReceiptQA {
			for _, criterionID := range finalGateDesktopArtifactCriterionIDs(plan) {
				if !finalGateHasArtifactIdentityForCriterion(evidence.ArtifactManifest, criterionID) {
					out = append(out, fmt.Sprintf("the QA receipt artifactManifest has no artifact-identity.json entry for desktop criterion %s", criterionID))
				}
			}
		}
		identity := evidence.SourceIdentity
		named = append(named, finalGateNamed{slot.label, &identity})
	}

	for _, entry := range named {
		if entry.identity == nil {
			continue
		}
		cmp := ctx.CompareSource(*entry.identity, current)
		if cmp.Kind == source.ComparisonDifferent {
			detail := cmp.Detail
			if detail == "" {
				detail = "changed"
			}
			out = append(out, fmt.Sprintf("%s describes a different source than the tree right now (%s) — re-run the gate", entry.label, detail))
		} else if cmp.Kind == source.ComparisonUnavailable {
			out = append(out, fmt.Sprintf("%s cannot be compared because git could not resolve the source identity — a schemaVersion 2 plan cannot be certified without git; use the v1 flow instead", entry.label))
		}
	}
	return out
}

// finalGateCaptureCurrent runs the ctx capture, turning the oracle's thrown exception into its catch-arm reason.
func finalGateCaptureCurrent(ctx *GoalplanValidationCtx) (id SourceIdentity, reason string) {
	defer func() {
		if r := recover(); r != nil {
			reason = "could not capture the current source identity: " + finalGatePanicText(r)
		}
	}()
	id = ctx.CaptureSourceIdentity(ctx.Cwd)
	return
}

// finalGateReadReceipt runs the ctx reader, separating a thrown exception (panicked) from the oracle's {error} alternative.
func finalGateReadReceipt(ctx *GoalplanValidationCtx, path string, kind gate.ReceiptKind) (evidence GoalplanReceiptEvidence, err error, panicked string) {
	defer func() {
		if r := recover(); r != nil {
			panicked = finalGatePanicText(r)
		}
	}()
	evidence, err = ctx.ReadReceipt(path, kind)
	return
}

// finalGatePanicText is err instanceof Error ? err.message : String(err).
func finalGatePanicText(r any) string {
	if e, ok := r.(error); ok {
		return e.Error()
	}
	return fmt.Sprintf("%v", r)
}

// finalGateJSText is an optional string as the oracle interpolates it: an absent one prints "undefined".
func finalGateJSText(s string) string {
	if s == "" {
		return "undefined"
	}
	return s
}

// finalGateJSNumber is a JavaScript number as it prints in a template literal, for the schema versions this reaches.
func finalGateJSNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// finalGateDeclaredSchemaVersion is plan.schemaVersion ?? 1.
func finalGateDeclaredSchemaVersion(plan *Goalplan) float64 {
	if plan.SchemaVersion == nil {
		return 1
	}
	return *plan.SchemaVersion
}

// finalGateBaseName is the oracle's path split on a slash or a backslash, taking the last segment.
func finalGateBaseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
