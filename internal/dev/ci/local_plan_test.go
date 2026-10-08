//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964: crw-dev ci local runs every job and step of .github/workflows/ci.yml locally, in the
// same order, at the same pinned versions, and leaves a verification-record/1. The step table
// (local_plan.go) is held to the workflow itself here: a step ci.yml grows, or a command that
// drifts, is a red test rather than an unverified step.

// localWorkflow is .github/workflows/ci.yml as text.
func localWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// C1: the table covers every ci.yml job and step, positionally, with nothing missing and nothing
// invented.
func TestLocalPlan_covers_every_workflow_step(t *testing.T) {
	workflow, err := parseWorkflow(localWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	if problems := localPlanProblems(localPlan(), workflow); len(problems) > 0 {
		t.Errorf("the local plan does not cover ci.yml:\n%s", strings.Join(problems, "\n"))
	}
}

// C1: a run step's local command is the ci.yml run text, so the local run performs the same work
// the runner performs.
func TestLocalPlan_commands_match_the_workflow_run_text(t *testing.T) {
	workflow, err := parseWorkflow(localWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range localPlan() {
		other := workflowJobNamed(workflow, job.name)
		if other == nil {
			t.Errorf("the plan names job %q, which ci.yml does not have", job.name)
			continue
		}
		for i, step := range job.steps {
			if i >= len(other.steps) {
				break
			}
			want := other.steps[i].run
			if step.command != want {
				t.Errorf("%s step %d (%q): command = %q, ci.yml run = %q", job.name, i, step.name, step.command, want)
			}
		}
	}
}

// C1: light mode and the body-only edit mirror are never applied locally, and the table says so
// rather than leaving the steps out.
func TestLocalPlan_never_applies_light_mode_or_the_edit_mirror(t *testing.T) {
	for _, job := range localPlan() {
		for _, step := range job.steps {
			switch {
			case step.name == "Light mode notice":
				if step.kind != localNotApplicable {
					t.Errorf("the light mode notice is %q, want %q: light mode is never applied locally", step.kind, localNotApplicable)
				}
			case strings.HasPrefix(step.name, "Mirror the jobs this head already ran"):
				if step.kind != localNotApplicable {
					t.Errorf("the edit mirror is %q, want %q: the mirror is a hosted lookup", step.kind, localNotApplicable)
				}
			}
		}
	}
	for _, job := range localPlan() {
		for _, step := range job.steps {
			if strings.Contains(step.command, "edit_mirror.sh") && step.kind != localNotApplicable {
				t.Errorf("%s: the edit mirror would run locally (%q)", job.name, step.command)
			}
		}
	}
}

// C1: a workflow the table does not cover is refused rather than run partially. The fixture adds
// one step to the validate job.
func TestLocalPlan_refuses_a_workflow_it_does_not_cover(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "internal", "dev", "ci", "testdata", "local", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := parseWorkflow(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if problems := localPlanProblems(localPlan(), workflow); len(problems) == 0 {
		t.Error("a workflow with an extra step is covered by the plan")
	}
}
