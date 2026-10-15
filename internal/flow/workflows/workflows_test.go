package workflows

import (
	"slices"
	"testing"
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

// The issue-lane workflow of an issue child is retired (CRW-1415): a node sized for a child escalates as a size split from the
// project lane, so no built-in workflow is named issue-lane and none loads under that name.
func TestBuiltinWorkflowsHaveNoIssueLane(t *testing.T) {
	if names := Names(); slices.Contains(names, "issue-lane") {
		t.Errorf("built-in workflows %v still name issue-lane", names)
	}
	if _, err := Load("issue-lane"); err == nil {
		t.Errorf("the issue-lane workflow still loads")
	}
}
