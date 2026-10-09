package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// CRW-1116: a plan_audit round whose reviewer answered GO-WITH-FIXES (blockers=N) is a required review with unresolved blockers.
// The A>B edge asks for the explicit disposition of those blockers; a round with no recorded verdict (an optional review that
// never ran or never parsed) and a legacy bare verdict ask for nothing more than the attest already does (LEAN-REVIEW-01).

func orchestrateReviewBlockersAttest(verdict, residual string) string {
	body := map[string]any{"from": "A", "to": "B", "did": "audited the plan", "auditOutput": "VERDICT: GO-WITH-FIXES (blockers=2)",
		"auditVerdict": verdict, "workPhaseId": "wp1"}
	if residual != "" {
		body["auditResidual"] = residual
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func orchestrateReviewBlockersRun(t *testing.T, name string, blockers int, attest string) *CliResult {
	t.Helper()
	cwd := orchestrateTransitionRoot(t)
	id := "review-blockers-" + name
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "near-pass", files)
	round["planSha256"] = hash
	if blockers > 0 {
		round["lane"].(map[string]any)["blockers"] = blockers
	}
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", attest)
	return &got
}

func TestOrchestrateReviewBlockersNeedAnExplicitDisposition(t *testing.T) {
	t.Run("a residual that disposes of nothing is refused", func(t *testing.T) {
		got := orchestrateReviewBlockersRun(t, "none", 2, orchestrateReviewBlockersAttest("near-pass", "noted the reviewer's remarks"))
		if got.Code != 1 || !strings.Contains(got.Output, "recorded GO-WITH-FIXES (blockers=2)") || !strings.Contains(got.Output, "Nothing was written.") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a residual that folds or rebuts each blocker advances", func(t *testing.T) {
		for _, residual := range []string{"GO-WITH-FIXES; 2 blockers folded back: (1) a (2) b", "blocker 1 folded, blocker 2 rebutted because the gate owns it"} {
			got := orchestrateReviewBlockersRun(t, "ok", 2, orchestrateReviewBlockersAttest("near-pass", residual))
			if got.Code != 0 {
				t.Fatalf("%q: %+v", residual, got)
			}
		}
	})
	t.Run("attesting pass over recorded blockers is refused", func(t *testing.T) {
		got := orchestrateReviewBlockersRun(t, "pass", 2, orchestrateReviewBlockersAttest("pass", ""))
		if got.Code != 1 || !strings.Contains(got.Output, `you attested "pass" but the reviewer recorded "near-pass"`) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a legacy bare near-pass records no count and asks for nothing more", func(t *testing.T) {
		got := orchestrateReviewBlockersRun(t, "bare", 0, orchestrateReviewBlockersAttest("near-pass", "noted the reviewer's remarks"))
		if got.Code != 0 {
			t.Fatalf("%+v", got)
		}
	})
}

// An optional review that left no verdict (still in flight, or its sign-off never parsed) never blocks, whatever the reviewer
// would have said.
func TestOrchestrateReviewBlockersOptionalReviewDoesNotBlock(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-blockers-optional"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "", files)
	round["planSha256"], round["status"] = hash, "in_flight"
	delete(round["lane"].(map[string]any), "verdict")
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBlockersAttest("near-pass", "folded"))
	if got.Code != 0 {
		t.Fatalf("%+v", got)
	}
}
