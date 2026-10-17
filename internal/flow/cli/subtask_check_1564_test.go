package cli

// CRW-1564: the shipped unit check wires a prompt-taking Codex sub-task profile for the scoped check of a split node's
// sub-task (codex-cli 0.163.0 refuses a PROMPT with --uncommitted or --base, so the prompt states the scope), beside the
// unchanged uncommitted/committed Codex profiles and the Bugbot quota fallback. The ops prove the op picks it for {sub: id}
// (TestScopedCheckPicksTheSubtaskProfile) and the scenario proves the lane scopes every sub-task
// (TestSubTasksAreCheckedScopedBeforeEachCommit).

import (
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// TestShippedUnitCheckWiresTheSubtaskProfile: shippedUnitCheck's SubtaskProfile is Codex's custom-instructions review
// profile at the resolved unit-check pair — it sends the scope prompt as `-- <prompt>` with no diff-scope flag, refuses a
// blank prompt and any range, and leaves the ordinary profiles and the Bugbot fallback as they were.
func TestShippedUnitCheckWiresTheSubtaskProfile(t *testing.T) {
	// a resolved pair that is not the compiled default: the profile must carry it (the captured cfg), like both Codex profiles do
	uc, err := shippedUnitCheck(&engine.Policy{UnitCheck: engine.Pair{Model: "o9", Effort: "medium"}})
	if err != nil || uc == nil {
		t.Fatalf("shipped unit check %+v %v", uc, err)
	}
	if uc.SubtaskProfile == nil {
		t.Fatal("the shipped unit check wires no sub-task profile")
	}
	p, err := uc.SubtaskProfile()
	if err != nil {
		t.Fatalf("the sub-task profile: %v", err)
	}
	if p.Name != "codex-review-subtask" || p.Model != "o9" || p.Effort != "medium" {
		t.Errorf("the sub-task profile %+v, want codex-review-subtask of o9 at medium", p)
	}
	for _, want := range []string{`model="o9"`, `review_model="o9"`, `model_reasoning_effort="medium"`, `sandbox_mode="read-only"`} {
		if !slices.Contains(p.Argv, want) {
			t.Errorf("the sub-task argv %v lacks %s", p.Argv, want)
		}
	}
	if slices.Contains(p.Argv, "--uncommitted") || slices.Contains(p.Argv, "--base") {
		t.Errorf("the sub-task argv carries a diff-scope flag: %v", p.Argv)
	}
	if p.Answer == nil {
		t.Error("the sub-task profile has no answer reader")
	}
	// the scope goes to the tool as -- <prompt>, one argv element
	args, err := p.Args(reviewadapter.Request{Prompt: "Review the uncommitted changes in this working tree.\nSUB-TASK 1 of 2 (one): write one\n"})
	if err != nil || !slices.Equal(args, []string{"--", "Review the uncommitted changes in this working tree.\nSUB-TASK 1 of 2 (one): write one\n"}) {
		t.Errorf("the sub-task args %v %v", args, err)
	}
	if _, err := p.Args(reviewadapter.Request{}); err == nil {
		t.Error("a blank scope prompt was accepted")
	}
	if _, err := p.Args(reviewadapter.Request{Prompt: "p", Base: "abc"}); err == nil {
		t.Error("a ranged request was accepted")
	}
	// the ordinary profiles and the fallback are unchanged
	uncommitted, err := uc.Profile("uncommitted")
	if err != nil || uncommitted.Name != "codex-review" || uncommitted.Model != "o9" {
		t.Errorf("the uncommitted profile %+v %v", uncommitted, err)
	}
	committed, err := uc.Profile("committed")
	if err != nil || committed.Name != "codex-review-committed" || committed.Model != "o9" {
		t.Errorf("the committed profile %+v %v", committed, err)
	}
	fallback, err := uc.Fallback("uncommitted")
	if err != nil || fallback.Name != reviewadapter.BugbotProfile().Name {
		t.Errorf("the uncommitted fallback %+v %v", fallback, err)
	}
}
