package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// CRW-1116: a plan_audit round whose reviewer answered GO-WITH-FIXES (blockers=N) is a required review with unresolved blockers.
// The A>B edge asks for the explicit disposition of those blockers; a round with no recorded verdict (an optional review that
// never ran or never parsed) and a legacy bare verdict ask for nothing more than the attest already does (LEAN-REVIEW-01).

func orchestrateReviewBlockersAttest(verdict, residual string, dispositions any) string {
	body := map[string]any{"from": "A", "to": "B", "did": "audited the plan", "auditOutput": "VERDICT: GO-WITH-FIXES (blockers=2)",
		"auditVerdict": verdict, "workPhaseId": "wp1"}
	if residual != "" {
		body["auditResidual"] = residual
	}
	if dispositions != nil {
		body["auditBlockers"] = dispositions
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// orchestrateReviewBlockersDisposition is one entry of the attest's auditBlockers.
func orchestrateReviewBlockersDisposition(blocker any, disposition, reason string) map[string]any {
	return map[string]any{"blocker": blocker, "disposition": disposition, "reason": reason}
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

// The disposition is structured and language-neutral: auditBlockers lists, for each of the N recorded blockers, whether it was
// folded into the plan or rebutted and the reason. The text of auditResidual is never searched, so a negation, an unrelated word
// or another language neither passes nor fails the edge; whether a reason is sound stays the main agent's judgment.
func TestOrchestrateReviewBlockersNeedAnExplicitDisposition(t *testing.T) {
	folded, rebutted := orchestrateReviewBlockersDisposition(1, "folded", "step 3 now names the rollback"), orchestrateReviewBlockersDisposition(2, "rebutted", "the gate owns it")
	refused := func(t *testing.T, name, attest string) {
		t.Helper()
		got := orchestrateReviewBlockersRun(t, name, 2, attest)
		if got.Code != 1 || !strings.Contains(got.Output, "recorded GO-WITH-FIXES (blockers=2)") || !strings.Contains(got.Output, "auditBlockers") || !strings.Contains(got.Output, "Nothing was written.") {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	t.Run("a residual that says anything in words, with no structured disposition, is refused", func(t *testing.T) {
		// The recorded counterexamples of the CRW-1116 verification: each carries a substring a text scan took for a disposition.
		for i, residual := range []string{
			"noted the reviewer's remarks",
			"blocker 1 not folded; blocker 2 still unresolved",
			"unfolded blockers remain",
			"manifold risks remain",
			"GO-WITH-FIXES; 2 blockers folded back: (1) a (2) b",
			"블로커 1과 2를 모두 계획에 반영했고 잔여 블로커 없음",
		} {
			refused(t, "text"+string(rune('a'+i)), orchestrateReviewBlockersAttest("near-pass", residual, nil))
		}
	})
	t.Run("a structured disposition of every blocker advances, whatever language the residual is in", func(t *testing.T) {
		for i, residual := range []string{"GO-WITH-FIXES; 2 blockers folded back: (1) a (2) b", "블로커 1과 2를 모두 계획에 반영했고 잔여 블로커 없음", "blockers 1 and 2 incorporated into the plan; no unresolved blockers", "unfolded blockers remain"} {
			got := orchestrateReviewBlockersRun(t, "ok"+string(rune('a'+i)), 2, orchestrateReviewBlockersAttest("near-pass", residual, []any{folded, rebutted}))
			if got.Code != 0 {
				t.Fatalf("%q: %+v", residual, got)
			}
		}
	})
	t.Run("a disposition that does not cover each blocker once is refused", func(t *testing.T) {
		for name, dispositions := range map[string]any{
			"short":        []any{folded},
			"duplicate":    []any{folded, orchestrateReviewBlockersDisposition(1, "rebutted", "again")},
			"out-of-range": []any{folded, orchestrateReviewBlockersDisposition(3, "folded", "x")},
			"zero":         []any{folded, orchestrateReviewBlockersDisposition(0, "folded", "x")},
			"fraction":     []any{folded, orchestrateReviewBlockersDisposition(1.5, "folded", "x")},
			"text-number":  []any{folded, orchestrateReviewBlockersDisposition("2", "folded", "x")},
			"unknown":      []any{folded, orchestrateReviewBlockersDisposition(2, "ignored", "x")},
			"no-reason":    []any{folded, orchestrateReviewBlockersDisposition(2, "folded", "  ")},
			"not-a-list":   map[string]any{"1": "folded"},
			"empty-list":   []any{},
		} {
			refused(t, "cover-"+name, orchestrateReviewBlockersAttest("near-pass", "noted", dispositions))
		}
	})
	t.Run("attesting pass over recorded blockers is refused", func(t *testing.T) {
		got := orchestrateReviewBlockersRun(t, "pass", 2, orchestrateReviewBlockersAttest("pass", "", nil))
		if got.Code != 1 || !strings.Contains(got.Output, `you attested "pass" but the reviewer recorded "near-pass"`) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a legacy bare near-pass records no count and asks for nothing more", func(t *testing.T) {
		got := orchestrateReviewBlockersRun(t, "bare", 0, orchestrateReviewBlockersAttest("near-pass", "noted the reviewer's remarks", nil))
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
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBlockersAttest("near-pass", "folded", nil))
	if got.Code != 0 {
		t.Fatalf("%+v", got)
	}
}
