package workflows

import (
	"regexp"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
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
