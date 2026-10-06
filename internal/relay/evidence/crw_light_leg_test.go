package evidence

// CRW-824: CI light mode (CRW-790) skips a go-product test leg's steps while the job still
// concludes success, so dev-gate reads green on a head nobody tested. The collector marks such a
// leg testSkipped, the reading answers the existing checks_stale instead of taking the run as
// evidence, and a leg whose step list cannot be read is left unmarked and reported unreadable, so a
// success leg whose test run cannot be confirmed is never merge evidence either way. No new
// refusal name is introduced.

import (
	"strings"
	"testing"
	"time"
)

const crw824Head = "9f2c1d4b7a8e5f6031b2c4d5e6f70819a2b3c4d5"

// crw824Entry is one check entry of the collector's shape, with the optional testSkipped mark.
func crw824Entry(run, name, conclusion string, attempt int, testSkipped bool) map[string]any {
	entry := map[string]any{"runId": run, "name": name, "headSha": crw824Head, "conclusion": conclusion, "attempt": attempt}
	if testSkipped {
		entry["testSkipped"] = true
	}
	return entry
}

// crw824LightRun is the light-mode shape: the required dev-gate succeeded, and the two test legs of
// the same run and attempt concluded success while their test step was skipped.
func crw824LightRun() []any {
	return []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
		crw824Entry("workflow-run:600:go-product (test-2)#0", "go-product (test-2)", "success", 1, true),
	}
}

// The core hole: today every entry is a success, so dev-gate is taken as evidence. After the change
// the run holds light legs, so it answers checks_stale and names the leg that skipped its tests.
func TestCRW824LightLegRefusesTheRunAsMergeEvidence(t *testing.T) {
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, crw824LightRun())
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
	detail := problems[0].Detail
	if !strings.Contains(detail, "go-product (test-1)") {
		t.Fatalf("the detail must name the leg that skipped its tests: %q", detail)
	}
	if !strings.Contains(detail, "crw-lane") {
		t.Fatalf("the detail must name the label that repairs it: %q", detail)
	}
	if VerdictOf(problems) != NotReady {
		t.Fatalf("checks_stale must stay not ready, got %s", VerdictOf(problems))
	}
}

// A run whose legs ran their tests is unchanged: the same shape without the mark passes.
func TestCRW824AFullRunStaysEvidence(t *testing.T) {
	checks := crw824LightRun()
	checks[1] = crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, false)
	checks[2] = crw824Entry("workflow-run:600:go-product (test-2)#0", "go-product (test-2)", "success", 1, false)
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("want no problem, got %v", problems)
	}
}

// A light leg of another workflow run says nothing about this one: the required check's own run
// holds no skipped leg, so the head is still evidence.
func TestCRW824LightLegOfAnotherRunDoesNotCount(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
	}
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("want no problem, got %v", problems)
	}
}

// A partial rerun (gh run rerun --failed) leaves the successful skipped legs at attempt 1 while the
// failed job and its dependent dev-gate rerun at attempt 2. The skipped leg is still the leg's own
// newest attempt, so it still says the head's tests did not run: reading each leg at its own newest
// attempt rather than at the judged check's attempt is what keeps this from being accepted.
func TestCRW824ASkippedLegSurvivesTheGatesPartialRerun(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 2, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
	}
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light detail is the answer, got %q", problems[0].Detail)
	}
}

// The same leg rerun at a newer attempt that ran its tests is the leg's answer, not the skipped
// older attempt.
func TestCRW824ASupersededLightLegDoesNotCount(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 2, false),
	}
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("want no problem, got %v", problems)
	}
}

// The lane's own repair for a light run is to label the pull request crw-lane, which starts a full
// run on the same head. That run's dev-gate is the evidence, so the earlier light run no longer
// refuses the head.
func TestCRW824ALabeledFullRunSupersedesTheEarlierLightRun(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
		crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
	}
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("the labeled full run is the evidence, want no problem, got %v", problems)
	}
}

// The full run answers only for the check it holds: a required check the full run does not carry is
// still refused by the light run that does.
func TestCRW824ALabeledFullRunMustAnswerTheSameCheck(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
		crw824Entry("workflow-run:601:other-gate#0", "other-gate", "success", 1, false),
	}
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light detail is the answer, got %q", problems[0].Detail)
	}
}

// A full run whose own legs skipped their tests is not the evidence that repairs a light run.
func TestCRW824ALightRunDoesNotSupersedeAnother(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
		crw824Entry("workflow-run:601:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:601:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
	}
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

// A light leg answers checks_stale rather than checks_not_run even when the same run attempt also
// holds a step-less job: a light leg is not a rerunnable runner problem, and the light reading
// dominates.
func TestCRW824ALightLegBeatsTheNotRunReading(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "failure", 1, false),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
	}
	stepLess := crw824Entry("workflow-run:600:validate#0", "validate", "cancelled", 1, false)
	stepLess["notRun"] = true
	checks = append(checks, stepLess)
	problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
	if !strings.Contains(problems[0].Detail, "crw-lane") {
		t.Fatalf("the light detail is the answer, got %q", problems[0].Detail)
	}
}

// The light reading does not depend on the order the forge enumerated the entries in.
func TestCRW824TheLightAnswerDoesNotDependOnOrder(t *testing.T) {
	forward := crw824LightRun()
	reversed := []any{forward[2], forward[1], forward[0]}
	for _, checks := range [][]any{forward, reversed} {
		problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks)
		if len(problems) != 1 || problems[0].Code != ChecksStale {
			t.Fatalf("want one %s in either order, got %v", ChecksStale, problems)
		}
		if !strings.Contains(problems[0].Detail, "go-product (test-1)") {
			t.Fatalf("the named leg must not move with the enumeration: %q", problems[0].Detail)
		}
	}
}

// A required check that stands in another run with no light leg of its own is untouched by a light
// leg elsewhere, and an optional light leg beside a green required one is not a refusal.
func TestCRW824AnOptionalLightLegBesideTheRequiredOne(t *testing.T) {
	checks := []any{
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
		crw824Entry("workflow-run:602:go-product (test-1)#0", "go-product (test-1)", "success", 1, true),
	}
	if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("want no problem, got %v", problems)
	}
}

// A leg the collector did not mark is not read as one, whatever the record spells: the flag is a
// strict boolean, so a string, a number and a nil all read false and cannot make the lane treat an
// untested head as tested.
func TestCRW824ANonBooleanTestSkippedReadsFalse(t *testing.T) {
	for _, value := range []any{"true", 1, nil, []any{}} {
		checks := []any{
			crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, false),
			crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, false),
		}
		checks[1].(map[string]any)["testSkipped"] = value
		if problems := ChecksProblems(crw824Head, []string{"dev-gate"}, checks); len(problems) != 0 {
			t.Fatalf("testSkipped %#v read as set: %v", value, problems)
		}
	}
}

// The shape check refuses a mark the collector cannot produce: a value that is not a boolean, a true
// one on a job that is not a go-product test leg, and a true one on a conclusion other than success.
func TestCRW824ShapeRefusesAMarkTheCollectorCannotProduce(t *testing.T) {
	good := crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, true)
	if problems := ShapeProblems(cleanReview(), []any{good}, nil, nil); len(problems) != 0 {
		t.Fatalf("a boolean testSkipped on a successful test leg must pass the shape check, got %v", problems)
	}
	plain := crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "success", 1, false)
	if problems := ShapeProblems(cleanReview(), []any{plain}, nil, nil); len(problems) != 0 {
		t.Fatalf("an absent or false testSkipped must pass the shape check, got %v", problems)
	}
	for _, entry := range []map[string]any{
		func() map[string]any { e := good; e["testSkipped"] = "true"; return e }(),
		func() map[string]any { e := good; e["testSkipped"] = 1; return e }(),
		crw824Entry("workflow-run:600:dev-gate#0", "dev-gate", "success", 1, true),
		crw824Entry("workflow-run:600:test-1#0", "test-1", "success", 1, true),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "failure", 1, true),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "cancelled", 1, true),
		crw824Entry("workflow-run:600:go-product (test-1)#0", "go-product (test-1)", "SUCCESS", 1, true),
	} {
		problems := ShapeProblems(cleanReview(), []any{entry}, nil, nil)
		if len(problems) != 1 || problems[0].Code != Malformed {
			t.Fatalf("testSkipped %v must be malformed, got %v", entry, problems)
		}
	}
}

// The collector marks the light leg and no other, on the check entry and the checkDetail entry
// alike, and a leg whose step list cannot be read is left unmarked and reported unreadable instead.
func TestCRW824CollectorMarksTheLightLeg(t *testing.T) {
	jobs := []any{
		map[string]any{"id": 31, "name": "dev-gate", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:00:00Z", "steps": []any{map[string]any{"name": "Prerequisites", "conclusion": "success", "started_at": "2026-10-06T06:00:01Z"}}},
		map[string]any{"id": 32, "name": "go-product (test-1)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:01:00Z", "steps": []any{map[string]any{"name": "Light mode notice", "conclusion": "success", "started_at": "2026-10-06T06:01:01Z"}, map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": "skipped", "started_at": nil}}},
		map[string]any{"id": 33, "name": "go-product (test-2)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:02:00Z", "steps": []any{map[string]any{"name": "Test and replay the contract corpus (test-2)", "conclusion": "success", "started_at": "2026-10-06T06:02:01Z"}}},
		map[string]any{"id": 34, "name": "go-product (test-3)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:03:00Z"},
		map[string]any{"id": 35, "name": "go-product (test-4)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:04:00Z", "steps": "not a list"},
		map[string]any{"id": 36, "name": "go-product (test-5)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:05:00Z", "steps": []any{"not an object"}},
		map[string]any{"id": 37, "name": "go-product (test-6)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:06:00Z", "steps": []any{map[string]any{"name": "Test and replay the contract corpus (test-6)", "started_at": "2026-10-06T06:06:01Z"}}},
		map[string]any{"id": 38, "name": "go-product (test-7)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T06:07:00Z", "steps": []any{map[string]any{"name": "actions/checkout", "conclusion": "success", "started_at": "2026-10-06T06:07:01Z"}}},
	}
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 8, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
		jobs: map[int][]any{8: jobs},
	}), "owner/name", 7)

	marked := map[string]bool{}
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		entry := mapOf(raw)
		if value, present := entry["testSkipped"]; present {
			flag, isBool := value.(bool)
			if !isBool || !flag {
				t.Fatalf("testSkipped must be true when present, got %v", value)
			}
			marked[strOf(entry["name"])] = true
		}
	}
	// test-1 skipped its test step and test-7 has no test step at all: both are legs whose test run
	// did not conclude success. Every other leg ran its tests or is a job that is not a test leg.
	for _, name := range []string{"go-product (test-1)", "go-product (test-7)"} {
		if !marked[name] {
			t.Fatalf("%s did not run its tests and must carry testSkipped: %v", name, marked)
		}
	}
	for _, name := range []string{"dev-gate", "go-product (test-2)"} {
		if marked[name] {
			t.Fatalf("%s must not carry testSkipped: %v", name, marked)
		}
	}

	detail := map[string]bool{}
	for _, raw := range listOf(snapshot["checkDetail"]) {
		entry := mapOf(raw)
		if value, present := entry["testSkipped"]; present {
			if flag, isBool := value.(bool); isBool && flag {
				detail[strOf(entry["name"])] = true
			}
		}
	}
	if !detail["go-product (test-1)"] || !detail["go-product (test-7)"] || detail["dev-gate"] {
		t.Fatalf("checkDetail must carry the same value: %v", detail)
	}

	// A leg whose step list cannot be read is not called testSkipped: it is the existing unreadable
	// problem, with the detail the issue fixes, and dev-gate is no evidence for it either.
	problems := mapOf(snapshot)
	var unreadable []string
	for _, raw := range listOf(problems["problems"]) {
		entry := mapOf(raw)
		if strOf(entry["code"]) == UnreadableCode {
			unreadable = append(unreadable, strOf(entry["detail"]))
		}
	}
	for _, name := range []string{"go-product (test-3)", "go-product (test-4)", "go-product (test-5)", "go-product (test-6)"} {
		want := name + " of workflow run 8 came back without a readable step list, so whether it ran its tests cannot be told"
		found := false
		for _, detail := range unreadable {
			found = found || detail == want
		}
		if !found {
			t.Fatalf("%s must leave the unreadable problem %q: %v", name, want, unreadable)
		}
		if marked[name] {
			t.Fatalf("%s cannot be read and must not be marked testSkipped: %v", name, marked)
		}
	}
}

// A body-only pull request edit mirrors a leg whose earlier run of this head already ran its tests:
// the job concludes success with its test step skipped, which is the shape a light leg has. The
// mirror's own step tells the two apart, so a mirrored leg is not marked and editing a description
// after a full run cannot turn the head into merge evidence.
func TestCRW824AMirroredLegIsNotALightLeg(t *testing.T) {
	jobs := []any{
		map[string]any{"id": 41, "name": "go-product (test-1)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T07:00:00Z", "steps": []any{
			map[string]any{"name": "Check out scripts/ci for the body-only edit mirror", "conclusion": "success", "started_at": "2026-10-06T07:00:01Z"},
			map[string]any{"name": lightMirrorStep, "conclusion": "success", "started_at": "2026-10-06T07:00:02Z"},
			map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": "skipped", "started_at": nil},
		}},
		map[string]any{"id": 42, "name": "go-product (test-2)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T07:01:00Z", "steps": []any{
			map[string]any{"name": "Test and replay the contract corpus (test-2)", "conclusion": "success", "started_at": "2026-10-06T07:01:01Z"},
		}},
	}
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 9, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
		jobs: map[int][]any{9: jobs},
	}), "owner/name", 7)
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		if _, present := mapOf(raw)["testSkipped"]; present {
			t.Fatalf("a mirrored leg ran its tests on this head and must not be marked: %v", raw)
		}
	}
}

// A leg whose mirror step did not succeed is not mirrored: its test step skipped without a mirror
// vouching for it, so it is a light leg.
func TestCRW824AMirrorThatDidNotSucceedIsStillALightLeg(t *testing.T) {
	job := map[string]any{"id": 43, "name": "go-product (test-1)", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T07:02:00Z", "steps": []any{
		map[string]any{"name": lightMirrorStep, "conclusion": "failure", "started_at": "2026-10-06T07:02:01Z"},
		map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": "skipped", "started_at": nil},
	}}
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 10, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
		jobs: map[int][]any{10: {job}},
	}), "owner/name", 7)
	marked := false
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		if value, present := mapOf(raw)["testSkipped"]; present {
			if flag, isBool := value.(bool); isBool && flag {
				marked = true
			}
		}
	}
	if !marked {
		t.Fatalf("a leg whose mirror did not succeed skipped its tests and must be marked: %v", snapshot["handoff"])
	}
}

// A run whose jobs read fails stays unreadable and marks nothing (a regression pin for the existing
// behavior).
func TestCRW824AFailedJobsReadStaysUnreadable(t *testing.T) {
	script := &collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 8, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
	}
	// The jobs endpoint fails while every other read answers: the collector must not turn that into
	// a mark or a pass.
	failing := NewForge(func(argv []string, timeout time.Duration) (int, string, string, error) {
		last := argv[len(argv)-1]
		if strings.Contains(last, "/jobs") {
			return 1, "", "HTTP 500", nil
		}
		return script.runner(argv, timeout)
	})
	failing.Now = func() string { return "2026-09-26T00:00:00+00:00" }
	snapshot, _ := Collect(failing, "owner/name", 7)
	if snapshot["verdict"] != UnknownVerdict {
		t.Fatalf("a failed jobs read must stay unknown, got %v", snapshot["verdict"])
	}
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		if _, present := mapOf(raw)["testSkipped"]; present {
			t.Fatalf("a failed jobs read must mark nothing: %v", raw)
		}
	}
}
