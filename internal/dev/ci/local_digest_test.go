//go:build dev

package ci

import (
	"strings"
	"testing"
)

// CRW-964 parent ruling 2, d2 and c1: a local plan step carries the digest of the ci.yml step it
// implements, so a changed job env, matrix leg, working directory or job order is plan drift and
// the run is refused, never run partially.

// localWorkflowDrift is the plan's verdict for a workflow text: the problems it reports.
func localWorkflowDrift(t *testing.T, text string) []string {
	t.Helper()
	workflow, err := parseWorkflow(text)
	if err != nil {
		t.Fatal(err)
	}
	return localPlanProblems(localPlan(), workflow)
}

func TestLocalDigest_a_changed_ci_yml_step_or_matrix_is_plan_drift(t *testing.T) {
	cases := map[string]struct{ from, to string }{
		"job env":     {"SKILLS_ROOT: port/cxc/skills", "SKILLS_ROOT: port/other/skills"},
		"matrix leg":  {"part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]", "part: [lint, test-1, test-2, test-3, test-4, test-rest, dist, extra]"},
		"working dir": {"working-directory: web", "working-directory: web/sub"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			text := localWorkflow(t)
			if !strings.Contains(text, c.from) {
				t.Fatalf("the ci.yml text %q is not there to change", c.from)
			}
			if problems := localWorkflowDrift(t, strings.Replace(text, c.from, c.to, 1)); len(problems) == 0 {
				t.Errorf("a changed %s is covered by the plan", name)
			}
		})
	}
}

func TestLocalDigest_a_swapped_job_order_is_plan_drift(t *testing.T) {
	workflow, err := parseWorkflow(localWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	workflow[0], workflow[1] = workflow[1], workflow[0]
	if problems := localPlanProblems(localPlan(), workflow); len(problems) == 0 {
		t.Error("two jobs swapped in ci.yml are covered by the plan")
	}
}

func TestLocalDigest_the_table_matches_the_real_workflow(t *testing.T) {
	if problems := localWorkflowDrift(t, localWorkflow(t)); len(problems) > 0 {
		t.Errorf("the plan does not match ci.yml:\n%s", strings.Join(problems, "\n"))
	}
}
