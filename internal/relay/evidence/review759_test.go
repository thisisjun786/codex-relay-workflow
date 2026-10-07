package evidence

// CRW-946, the three P1 holes the blind post-merge review of PR #759 found in the light-run
// refusal (CRW-824). Each hole let a run that never tested the head stand in for one that did.
//
//  1. testedElsewhere accepted any other successful run of the same check on the same head as
//     "tested elsewhere", so a workflow that holds no test job at all (a dev-gate-only run) erased
//     the refusal. A substitute must have actually run the tests the judged run skipped.
//  2. An unknown provider on the substitute run filled the pinned-integration condition indirectly,
//     because the comparison was lenient on both sides. Under a pinned integration the substitute
//     run's gate provider must be known and inside the pinned set.
//  3. The collector's jobTestSkipped treated a successful mirror step as proof that the leg's tests
//     ran, but scripts/ci/edit_mirror.sh exits zero with mirrored=false whenever its lookup fails or
//     finds no candidate, so a null or skipped test step was exempted. The leg's own test step
//     decides; a leg the mirror carried over is exempted by the substitute-run rule of (1).

import (
	"strings"
	"testing"
)

// Two complementary light runs must not excuse each other. Each holds a leg the other skipped, so
// under the "answers every skipped leg" rule alone each would be the other's substitute and both
// gates would pass although neither run ran the whole suite. A substitute must also hold no skipped
// leg of its own (CRW-946).
func TestEvidenceReview759ComplementaryLightRunsAreNoSubstitute(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
		crw824Entry("workflow-run:600:go-product (test-2)#0", "go-product (test-2)", "success", 1, false),
		crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-2)#0", "go-product (test-2)", "success", 1, true),
	}
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("two complementary light runs are no substitute, want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light refusal is the answer, got %q", problems[0].Detail)
	}
	// The contrast: one run that ran every leg is the evidence for both light runs.
	complete := append(append([]any{}, checks...),
		crw824Entry("workflow-run:602:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:602:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
		crw824Entry("workflow-run:602:go-product (test-2)#0", "go-product (test-2)", "success", 1, false),
	)
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, complete); len(problems) != 0 {
		t.Fatalf("a run that ran every leg is the evidence, want no problem, got %v", problems)
	}
}

// The judged light run: dev-gate succeeded and the test leg skipped its work.
func review759LightRun() []any {
	return []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
	}
}

// A run that holds no test leg at all answered the required check successfully, but it never ran
// the tests the judged run skipped. The absence of a testSkipped mark on such a run is not evidence
// that the head was tested, so the light refusal stays.
func TestEvidenceReview759GateOnlyRunIsNoSubstitute(t *testing.T) {
	checks := append(review759LightRun(),
		crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
	)
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("a run with no test leg is not a substitute, want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light refusal is the answer, got %q", problems[0].Detail)
	}
	if VerdictOf(problems) != NotReady {
		t.Fatalf("checks_stale must stay not ready, got %s", VerdictOf(problems))
	}
}

// Under a pinned integration a substitute run whose gate provider is unknown does not fill the
// condition: the collector records a null provider when the check-run listing filtered to the
// latest run does not hold that entry's check-run, and a null is unknown rather than an integration.
// The same run carrying the pinned integration on its gate is the evidence.
func TestEvidenceReview759UnknownProviderIsNoPinnedSubstitute(t *testing.T) {
	pinned := map[string][]string{"dev-gate": {"42"}}
	checks := append(review759LightRun(),
		crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
	)
	// The judged light run carries the pinned integration; the substitute run's entries carry the
	// collector's null, which is unknown.
	checks[0].(map[string]any)["provider"] = "42"
	checks[1].(map[string]any)["provider"] = "42"
	problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, checks, true, pinned)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("an unknown provider is not the pinned substitute, want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light refusal is the answer, got %q", problems[0].Detail)
	}
	// The contrast: the same substitute run carrying the pinned integration is the evidence.
	checks[2].(map[string]any)["provider"] = "42"
	checks[3].(map[string]any)["provider"] = "42"
	if problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, checks, true, pinned); len(problems) != 0 {
		t.Fatalf("the pinned full run is the evidence, want no problem, got %v", problems)
	}
}

// review759MirroredJob is a go-product test leg whose mirror step concluded success and whose test
// step carries the given conclusion, the shape a body-only pull request edit produces.
func review759MirroredJob(id int, testConclusion any) map[string]any {
	return map[string]any{"id": id, "name": "go-product (test-1)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-07T06:00:00Z", "steps": []any{
		map[string]any{"name": lightMirrorStep, "conclusion": "success", "started_at": "2026-10-07T06:00:01Z"},
		map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": testConclusion, "started_at": "2026-10-07T06:00:02Z"},
	}}
}

// review759Collect collects one run holding the given job and returns the snapshot.
func review759Collect(job map[string]any) map[string]any {
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 8, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
		jobs: map[int][]any{8: {job}},
	}), "owner/name", 7)
	return snapshot
}

// review759Marked reports whether any check entry of the snapshot carries testSkipped true.
func review759Marked(snapshot map[string]any) bool {
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		if flag, isBool := mapOf(raw)["testSkipped"].(bool); isBool && flag {
			return true
		}
	}
	return false
}

// review759Unreadable returns the unreadable problem details of the snapshot.
func review759Unreadable(snapshot map[string]any) []string {
	var out []string
	for _, raw := range listOf(snapshot["problems"]) {
		entry := mapOf(raw)
		if strOf(entry["code"]) == UnreadableCode {
			out = append(out, strOf(entry["detail"]))
		}
	}
	return out
}

// A mirror step that concluded success does not vouch for a leg whose test step conclusion cannot
// be read: the leg is unreadable, so the collector records the existing unreadable problem and does
// not call the leg a skipped one. A skipped test step is a leg that skipped its tests whatever the
// mirror step says. And a leg the mirror carried over is exempted by the earlier run of the same
// head that actually ran its tests, not by the mirror step.
func TestEvidenceReview759MirrorStepIsNotEvidence(t *testing.T) {
	// The test step concluded null: whether the leg ran its tests cannot be told.
	unreadable := review759Collect(review759MirroredJob(51, nil))
	want := "go-product (test-1) of workflow run 8 came back without a readable step list, so whether it ran its tests cannot be told"
	found := false
	for _, detail := range review759Unreadable(unreadable) {
		if detail == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("a null test step must leave the unreadable problem %q, got %v", want, review759Unreadable(unreadable))
	}
	if review759Marked(unreadable) {
		t.Fatalf("a leg whose test step cannot be read is not marked testSkipped: %v", unreadable["handoff"])
	}

	// The test step concluded skipped: the leg skipped its tests.
	skipped := review759Collect(review759MirroredJob(52, "skipped"))
	if !review759Marked(skipped) {
		t.Fatalf("a skipped test step is testSkipped whatever the mirror step says: %v", skipped["handoff"])
	}

	// The contrast, end to end: a real body-only edit produces a run whose leg is marked (mirror
	// step success, test step skipped) beside an earlier run of the same head that ran that leg's
	// tests. The substitute rule exempts it, so the collector's own reading finds nothing.
	exempted, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{
			map[string]any{"id": 8, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request", "run_started_at": "2026-10-07T06:00:00Z"},
			map[string]any{"id": 9, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request", "run_started_at": "2026-10-07T07:00:00Z"},
		},
		jobs: map[int][]any{
			8: {review759RanLeg(61)},
			9: {review759MirroredJob(62, "skipped")},
		},
	}), "owner/name", 7)
	if exempted["verdict"] != Ready {
		t.Fatalf("the earlier run's own test run exempts the mirrored leg, want %s, got %s: %v",
			Ready, exempted["verdict"], exempted["problems"])
	}
}

// review759RanLeg is a go-product test leg whose test step ran and concluded success.
func review759RanLeg(id int) map[string]any {
	return map[string]any{"id": id, "name": "go-product (test-1)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-07T06:00:00Z", "steps": []any{
		map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": "success", "started_at": "2026-10-07T06:00:02Z"},
	}}
}

// Where the branch rule pins no integration the lenient rule stands: an unknown on either side is
// not evidence of a different integration, so a substitute that ran the skipped leg is the
// evidence, while two known and differing integrations are a refusal.
func TestEvidenceReview759UnpinnedProviderKeepsTheLenientRule(t *testing.T) {
	build := func(judged, substitute string) []any {
		checks := []any{
			crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
			crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
			crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
			crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
		}
		// The judged light gate and the substitute run's gate, as the collector records them.
		checks[0].(map[string]any)["provider"] = judged
		checks[2].(map[string]any)["provider"] = substitute
		return checks
	}
	// Nothing is pinned: an unknown on either side does not refuse the substitute.
	for _, pair := range [][2]string{{"", ""}, {"", "42"}, {"42", ""}} {
		if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, build(pair[0], pair[1])); len(problems) != 0 {
			t.Fatalf("with no integration pinned, providers %q/%q must not refuse the substitute, got %v",
				pair[0], pair[1], problems)
		}
	}
	// Two known and differing integrations are a refusal.
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, build("42", "99"))
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("two known differing integrations must refuse the substitute, want one %s, got %v",
			ChecksStale, problems)
	}
}
