package workflows

import (
	"bytes"
	"io/fs"
	"regexp"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
)

// Every built-in workflow loads: its header parses, its helpers resolve inside the embedded library and the strict-schema check of
// crw:lib/schemas passes.
func TestBuiltinWorkflowsLoad(t *testing.T) {
	names := Names()
	if !slices.Contains(names, "project-lane") {
		t.Fatalf("built-in workflows %v", names)
	}
	for _, name := range names {
		prog, err := Load(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if prog.Meta["name"] != name {
			t.Errorf("%s: meta.name is %v", name, prog.Meta["name"])
		}
	}
	if _, err := Load("no-such-workflow"); err == nil {
		t.Errorf("a workflow that does not exist loaded")
	}
}

// The issue-lane workflow of a `child` session is retired (CRW-1415): a node sized for a child escalates as a size split from the
// project lane, so no built-in workflow is named issue-lane and none loads under that name.
func TestBuiltinWorkflowsHaveNoIssueLane(t *testing.T) {
	if names := Names(); slices.Contains(names, "issue-lane") {
		t.Errorf("built-in workflows %v still name issue-lane", names)
	}
	if _, err := Load("issue-lane"); err == nil {
		t.Errorf("the issue-lane workflow still loads")
	}
}

// TestLegacyLaneWorkflowsAreTheLaneBuiltins (CRW-1429): the lane scope of the legacy refusal (engine.LegacyLaneWorkflows) is the built-in
// workflows that ran the lane library: each name is a built-in, and every built-in whose source imports crw:lib/lane or crw:lib/close,
// or nests the project lane, is in the list.
func TestLegacyLaneWorkflowsAreTheLaneBuiltins(t *testing.T) {
	names := Names()
	for _, name := range engine.LegacyLaneWorkflows {
		if !slices.Contains(names, name) {
			t.Errorf("engine.LegacyLaneWorkflows names %q, which is no built-in workflow (%v)", name, names)
		}
	}
	lane := regexp.MustCompile(`from 'crw:lib/(lane|close)'|workflow\('project-lane'`)
	for _, name := range names {
		src, err := Source(name)
		if err != nil {
			t.Fatal(err)
		}
		if lane.Match(src) && !slices.Contains(engine.LegacyLaneWorkflows, name) {
			t.Errorf("%s runs the lane library and is not in engine.LegacyLaneWorkflows %v", name, engine.LegacyLaneWorkflows)
		}
	}
}

// TestNoWorkflowCallsTheNodeReviewerAnyMore (CRW-1447): the node reviewer's write path is retired. No built-in workflow and no helper of
// crw:lib calls the node reviewer (crw.review and its composites crw.reviewHalt and crw.exploreEvidence, an agent call of role reviewer,
// the spawn decisions REV_1, REV_2 and EXPL_3), and the close archives no thread (crw.archive, crw.settleReviews): it audits the
// acceptances and reports. The lane registers no review op; the rows old runs wrote stay readable under ops.OpReview.
func TestNoWorkflowCallsTheNodeReviewerAnyMore(t *testing.T) {
	retired := []string{"crw.review(", "crw.reviewHalt(", "crw.exploreEvidence(", "crw.settleReviews(", "crw.archive(", "agentType: 'reviewer'", "'REV_1'", "'REV_2'", "'EXPL_3'"}
	sources := builtinSources(t)
	for name, src := range sources {
		for _, word := range retired {
			if bytes.Contains(src, []byte(word)) {
				t.Errorf("%s holds %q of the retired node reviewer path", name, word)
			}
		}
	}
	if _, ok := ops.NewLane(ops.Deps{}).Ops()["review"]; ok {
		t.Errorf("the lane still registers crw.review")
	}
}

// TestNoWorkflowCallsReviewerFinal (CRW-1450): the reviewer-final write path is retired. No built-in workflow and no helper of crw:lib
// calls reviewer-final (crw.reviewerFinal, an agent call of role reviewer-final, the spawn decision REVF_1) or hands its schema
// VERDICT_FINAL to a call, and the lane registers no reviewerFinal op; the rows old runs wrote stay readable under ops.OpReviewerFinal.
func TestNoWorkflowCallsReviewerFinal(t *testing.T) {
	retired := []string{"crw.reviewerFinal(", "agentType: 'reviewer-final'", "'REVF_1'", "VERDICT_FINAL"}
	for name, src := range builtinSources(t) {
		for _, word := range retired {
			if bytes.Contains(src, []byte(word)) {
				t.Errorf("%s holds %q of the retired reviewer-final path", name, word)
			}
		}
	}
	if _, ok := ops.NewLane(ops.Deps{}).Ops()["reviewerFinal"]; ok {
		t.Errorf("the lane still registers crw.reviewerFinal")
	}
}

// builtinSources are the texts of every built-in workflow (by <name>.js) and of every helper of crw:lib (by lib/<name>.js).
func builtinSources(t *testing.T) map[string][]byte {
	t.Helper()
	sources := map[string][]byte{}
	for _, name := range Names() {
		src, err := Source(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name+".js"] = src
	}
	helpers, err := fs.Glob(lib.FS, "*.js")
	if err != nil || len(helpers) == 0 {
		t.Fatalf("the helper library: %v %v", helpers, err)
	}
	for _, name := range helpers {
		src, err := fs.ReadFile(lib.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		sources["lib/"+name] = src
	}
	return sources
}
