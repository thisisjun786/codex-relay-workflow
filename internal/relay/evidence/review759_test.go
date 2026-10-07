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
	// tests. Both runs carry the required dev-gate, so the light reading actually fires on the
	// judged run and the substitute rule is what decides. Without the earlier run the same shape is
	// refused, which is what makes this a discriminating contrast rather than a vacuous one.
	run := func(id int, started string) map[string]any {
		return map[string]any{"id": id, "name": "CI", "head_sha": collectorHead, "workflow_id": 100,
			"event": "pull_request", "run_started_at": started}
	}
	devGate := func(id int) map[string]any {
		return map[string]any{"id": id, "name": "dev-gate", "run_attempt": 1, "status": "completed",
			"conclusion": "success", "started_at": "2026-10-07T06:00:00Z"}
	}
	collect := func(runs []any, jobs map[int][]any) map[string]any {
		snapshot, _ := Collect(fixedForge(&collectorScript{
			threads: 1, unresolved: map[int]bool{}, runs: runs, jobs: jobs,
		}), "owner/name", 7)
		return snapshot
	}
	exempted := collect(
		[]any{run(8, "2026-10-07T06:00:00Z"), run(9, "2026-10-07T07:00:00Z")},
		map[int][]any{8: {devGate(60), review759RanLeg(61)}, 9: {devGate(62), review759MirroredJob(63, "skipped")}},
	)
	if exempted["verdict"] != Ready {
		t.Fatalf("the earlier run's own test run exempts the mirrored leg, want %s, got %s: %v",
			Ready, exempted["verdict"], exempted["problems"])
	}
	// The negative control: the mirrored run alone, with nothing on the head that ran that leg.
	alone := collect(
		[]any{run(9, "2026-10-07T07:00:00Z")},
		map[int][]any{9: {devGate(62), review759MirroredJob(63, "skipped")}},
	)
	if alone["verdict"] != NotReady {
		t.Fatalf("a mirrored leg with no earlier run that ran its tests must stay not ready, got %s: %v",
			alone["verdict"], alone["problems"])
	}
	// The refusal is the light reading itself, which is what makes the pair above discriminating.
	codes := map[string]bool{}
	for _, raw := range listOf(alone["problems"]) {
		codes[strOf(mapOf(raw)["code"])] = true
	}
	if !codes[ChecksStale] {
		t.Fatalf("the negative control must refuse through %s, got %v", ChecksStale, alone["problems"])
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

// review759EntryMarked reports whether the check entry of the snapshot with the given name carries
// the given boolean mark set true.
func review759EntryMarked(snapshot map[string]any, name, mark string) bool {
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		entry := mapOf(raw)
		if strOf(entry["name"]) != name {
			continue
		}
		if flag, isBool := entry[mark].(bool); isBool && flag {
			return true
		}
	}
	return false
}

// A success leg whose test step concluded null carries no testSkipped mark, so the check row alone
// cannot tell it from a leg that ran -- and the reading merge-turn-check uses sees only those rows.
// The collector marks such a leg testUnreadable, and the substitute rule refuses to count it as
// having answered the judged run's skipped leg (CRW-946, the parent's correction of PR #875).
func TestEvidenceReview759UnreadableTestStepIsNoSubstitute(t *testing.T) {
	unreadable := review759Collect(review759MirroredJob(51, nil))
	if !review759EntryMarked(unreadable, "go-product (test-1)", "testUnreadable") {
		t.Fatalf("a leg whose test step cannot be read must carry testUnreadable: %v", unreadable["handoff"])
	}
	if review759EntryMarked(unreadable, "go-product (test-1)", "testSkipped") {
		t.Fatalf("testUnreadable must not be reported as testSkipped: %v", unreadable["handoff"])
	}
	// The contrast: a leg that ran its tests carries neither mark.
	ran := review759Collect(review759RanLeg(52))
	if review759EntryMarked(ran, "go-product (test-1)", "testUnreadable") {
		t.Fatalf("a leg that ran its tests is not unreadable: %v", ran["handoff"])
	}

	// The reading merge-turn-check uses: the judged run skipped its leg, and the only other run on
	// the head holds that leg at a success conclusion whose test step could not be read. A row that
	// says nothing about whether the tests ran is not the substitute.
	checks := append(review759LightRun(),
		crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
	)
	checks[3].(map[string]any)["testUnreadable"] = true
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("an unreadable test step is no substitute, want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light refusal is the answer, got %q", problems[0].Detail)
	}
	// The contrast: the same substitute run with a readable test step is the evidence.
	checks[3].(map[string]any)["testUnreadable"] = false
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("a readable test step is the evidence, want no problem, got %v", problems)
	}

	// The same reading on the collector's own rows, which is what the merge turn restates: a light
	// run and a second run on the same head whose leg concluded success with an unreadable test
	// step. The rows are the only thing the merge turn sees, and they do not accept the second run.
	runs := []any{
		map[string]any{"id": 8, "name": "CI", "head_sha": collectorHead, "workflow_id": 100,
			"event": "pull_request", "run_started_at": "2026-10-07T06:00:00Z"},
		map[string]any{"id": 9, "name": "CI", "head_sha": collectorHead, "workflow_id": 100,
			"event": "pull_request", "run_started_at": "2026-10-07T07:00:00Z"},
	}
	gate := func(id int) map[string]any {
		return map[string]any{"id": id, "name": "dev-gate", "run_attempt": 1, "status": "completed",
			"conclusion": "success", "started_at": "2026-10-07T06:00:00Z"}
	}
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{}, runs: runs,
		jobs: map[int][]any{8: {gate(60), review759SkippedLeg(61)}, 9: {gate(62), review759MirroredJob(63, nil)}},
	}), "owner/name", 7)
	rows := listOf(mapOf(snapshot["handoff"])["checks"])
	if problems := ChecksProblems(collectorHead, []string{"dev-gate"}, rows); len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("the collector's own rows must not accept an unreadable substitute, want one %s, got %v", ChecksStale, problems)
	}
}

// review759SkippedLeg is a go-product test leg that concluded success with its test step skipped,
// the light-mode shape.
func review759SkippedLeg(id int) map[string]any {
	return map[string]any{"id": id, "name": "go-product (test-1)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-07T06:00:00Z", "steps": []any{
		map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": "skipped", "started_at": nil},
	}}
}

// review759Entry is one check entry of the collector's shape with the given marks.
func review759Entry(run, name, conclusion string, marks map[string]bool) map[string]any {
	entry := map[string]any{"runId": run, "name": name, "headSha": crw824Head, "conclusion": conclusion, "attempt": 1}
	for mark, set := range marks {
		if set {
			entry[mark] = true
		}
	}
	return entry
}

// The provider the collector fills is read from the check-run listing filtered to the LATEST run, so
// the gate of an older light run carries none. Filtering the required check by integration before
// asking whether that run's tests ran drops exactly the run whose tests did not run, and a later
// gate-only run then answers both the required name and the integration. The run's tests are read
// before the integration filter, so the light refusal stays (CRW-946, the pre-merge evaluation's d1).
func TestEvidenceReview759LightGateWithUnknownProviderIsStillRefused(t *testing.T) {
	pinned := map[string][]string{"dev-gate": {"42"}}
	checks := []any{
		// The light run: its gate is the pinned integration, but the collector could not fill the
		// provider from the latest check-run listing, so the row carries none.
		review759Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", nil),
		review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", map[string]bool{"testSkipped": true}),
		// A later run on the same head holding only the gate, from the pinned integration.
		review759Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", nil),
	}
	checks[2].(map[string]any)["provider"] = "42"
	problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, checks, true, pinned)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("a light run whose gate provider is unknown must still be refused, want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light refusal is the answer, got %q", problems[0].Detail)
	}
	// The contrast: the same shape with the later run actually running the leg the light run
	// skipped is the evidence, whatever the light gate's provider reads.
	checks[2] = review759Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", nil)
	checks[2].(map[string]any)["provider"] = "42"
	checks = append(checks, review759Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", nil))
	checks[3].(map[string]any)["provider"] = "42"
	if problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, checks, true, pinned); len(problems) != 0 {
		t.Fatalf("the labeled full run is the evidence, want no problem, got %v", problems)
	}
}

// A run whose test step could not be read did not confirm that it ran the tests, so it is not merge
// evidence by itself: the merge turn receives only the check rows, and a row that says nothing about
// whether the tests ran must not read as a head that was tested (CRW-946, the evaluation's d2).
func TestEvidenceReview759UnreadableRunIsNoEvidenceOnItsOwn(t *testing.T) {
	checks := []any{
		review759Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", nil),
		review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", map[string]bool{"testUnreadable": true}),
	}
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("a run that could not confirm its tests ran is no evidence, want one %s, got %v", ChecksStale, problems)
	}
	if VerdictOf(problems) != NotReady {
		t.Fatalf("checks_stale must stay not ready, got %s", VerdictOf(problems))
	}
	// The contrast: a leg that ran its tests leaves the run as evidence.
	checks[1] = review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", nil)
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("a run that confirmed its tests is evidence, want no problem, got %v", problems)
	}
// And an unreadable leg beside a substitute that ran that leg is answered by the substitute.
	checks[1] = review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", map[string]bool{"testUnreadable": true})
	checks = append(checks,
		review759Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", nil),
		review759Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", nil),
	)
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("a run that ran the unconfirmed leg is the evidence, want no problem, got %v", problems)
	}
}

// A workflow run that holds no test leg at all never showed that it ran the tests, so its gate
// cannot be the success of an integration. The shape a dev-gate-only workflow has answers the
// required name without testing anything, and reading it as an integration success lets it fill a
// pinned integration the light run was exempted from (CRW-946, the evaluation's d1).
func TestEvidenceReview759GateOnlyRunIsNoIntegrationSuccess(t *testing.T) {
	pinned := map[string][]string{"dev-gate": {"42", "99"}}
	// run 600 (integration 42) is light, run 601 (integration 99) ran the tests and exempts it,
	// and run 602 (integration 42) holds only the gate.
	checks := []any{
		review759Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", nil),
		review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", map[string]bool{"testSkipped": true}),
		review759Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", nil),
		review759Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", nil),
		review759Entry("workflow-run:602:dev-gate#0", "dev-gate", "success", nil),
	}
	for i, provider := range []string{"42", "42", "99", "99", "42"} {
		checks[i].(map[string]any)["provider"] = provider
	}
	problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, checks, true, pinned)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("a gate-only run must not answer the integration, want one %s, got %v", ChecksStale, problems)
	}
	// The contrast: run 602 also ran the tests, so its gate answers integration 42.
	ran := review759Entry("workflow-run:602:go-product (test-1)#0", "go-product (test-1)", "success", nil)
	ran["provider"] = "42"
	if problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, append(checks, ran), true, pinned); len(problems) != 0 {
		t.Fatalf("a run that ran the tests answers its integration, want no problem, got %v", problems)
	}
}

// A restated row that states testUnreadable as anything but a boolean cannot be read, and a mark
// the reader cannot read is not evidence that the leg ran its tests. The reading merge-turn-check
// uses receives only the rows and runs no shape check of its own, so the predicate itself must fail
// closed on the value (CRW-946, the evaluation's d2).
func TestEvidenceReview759MalformedUnreadableMarkFailsClosed(t *testing.T) {
	for _, value := range []any{"true", 1, []any{}, map[string]any{}} {
		checks := []any{
			review759Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", nil),
			review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", nil),
		}
		checks[1].(map[string]any)["testUnreadable"] = value
		problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
		if len(problems) != 1 || problems[0].Code != ChecksStale {
			t.Fatalf("testUnreadable %#v must fail closed, want one %s, got %v", value, ChecksStale, problems)
		}
	}
	// An explicit false is the collector's "this leg's tests were confirmed", and an absent field
	// is a leg the collector never marked: both stay evidence.
	for _, value := range []any{false, nil} {
		checks := []any{
			review759Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", nil),
			review759Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", nil),
		}
		if value != nil {
			checks[1].(map[string]any)["testUnreadable"] = value
		}
		if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
			t.Fatalf("testUnreadable %#v is a confirmed leg, want no problem, got %v", value, problems)
		}
	}
}

// The light gate a substitute exempted is not an integration success. The success counted for a
// pinned integration is the gate of a run that actually ran its tests, so the judged light run's own
// gate answers no integration (CRW-946, the parent's correction of a regression PR #875 introduced).
func TestEvidenceReview759ExemptedLightGateIsNoIntegrationSuccess(t *testing.T) {
	build := func(judgedProvider, substituteProvider string, judgedLight bool) []any {
		checks := []any{
			crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
			crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, judgedLight),
			crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
			crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
		}
		checks[0].(map[string]any)["provider"] = judgedProvider
		checks[1].(map[string]any)["provider"] = judgedProvider
		checks[2].(map[string]any)["provider"] = substituteProvider
		checks[3].(map[string]any)["provider"] = substituteProvider
		return checks
	}
	// providers={dev-gate:[42,99]}: run 600 (42) is light and run 601 (99) ran the tests. 600 is
	// exempted by 601, but integration 42 is not answered -- the exempted light gate is not its
	// success, whatever it concluded.
	pinned := map[string][]string{"dev-gate": {"42", "99"}}
	problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, build("42", "99", true), true, pinned)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("an exempted light gate must not answer the integration, want one %s, got %v", ChecksStale, problems)
	}
	// The contrast: the same two integrations, with the judged run's own leg actually run, are both
	// answered by their own full run.
	if problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, build("42", "99", false), true, pinned); len(problems) != 0 {
		t.Fatalf("two full runs answer both integrations, want no problem, got %v", problems)
	}
	// The documented repair is untouched: with only integration 42 pinned, the labeled full run 601
	// from that same integration is the evidence for the light run 600.
	same := map[string][]string{"dev-gate": {"42"}}
	if problems := ChecksProblemsWith(crw824Head, []string{"dev-gate"}, build("42", "42", true), true, same); len(problems) != 0 {
		t.Fatalf("the labeled full run from the same integration is the evidence, want no problem, got %v", problems)
	}
}

// The shape check refuses a testUnreadable mark the collector cannot produce: a value that is not a
// boolean, a true one on a job that is not a go-product test leg, a true one on a conclusion other
// than success, and one set together with testSkipped, which is the opposite answer (CRW-946).
func TestEvidenceReview759ShapeRefusesAnUnreadableMarkTheCollectorCannotProduce(t *testing.T) {
	leg := func() map[string]any {
		return crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, false)
	}
	good := leg()
	good["testUnreadable"] = true
	if problems := ShapeProblems(cleanReview(), []any{good}, nil, nil); len(problems) != 0 {
		t.Fatalf("a boolean testUnreadable on a successful test leg must pass the shape check, got %v", problems)
	}
	plain := leg()
	if problems := ShapeProblems(cleanReview(), []any{plain}, nil, nil); len(problems) != 0 {
		t.Fatalf("an absent testUnreadable must pass the shape check, got %v", problems)
	}
	for _, value := range []any{"true", 1, nil, []any{}} {
		entry := leg()
		entry["testUnreadable"] = value
		if problems := ShapeProblems(cleanReview(), []any{entry}, nil, nil); len(problems) != 1 || problems[0].Code != Malformed {
			t.Fatalf("testUnreadable %#v must be malformed, got %v", value, problems)
		}
	}
	both := leg()
	both["testUnreadable"] = true
	both["testSkipped"] = true
	notLeg := crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false)
	notLeg["testUnreadable"] = true
	failed := crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "failure", 1, false)
	failed["testUnreadable"] = true
	for _, entry := range []map[string]any{both, notLeg, failed} {
		if problems := ShapeProblems(cleanReview(), []any{entry}, nil, nil); len(problems) != 1 || problems[0].Code != Malformed {
			t.Fatalf("testUnreadable %v must be malformed, got %v", entry, problems)
		}
	}
}
