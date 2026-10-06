//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CRW-790's temporary CI light mode. While the repository variable CRW_CI_MODE is "light", a
// pull request that does not carry the crw-lane label skips the work of the five go-product test
// legs; when the merge lane labels the pull request, the labeled event runs the full CI on the
// same head. The condition is defined once, as a job-level env on go-product, and every step that
// would do a leg's work is guarded by a step condition, never by a job-level if: GitHub reports a
// skipped job's check as success, so a skipped leg could hide an earlier red run behind a green
// dev-gate. validate, secrets, lint, dist, a dev push and a manual dispatch always run in full.
//
// The workflow is read as text, with the helpers workflow_test.go already defines (workflowJobs,
// workflowSteps, matrixValues, sortedCopy, expectEqual), so this file adds no second parser.

const (
	lightEnvKey = "CRW_LIGHT_LEG"
	lightGuard  = "env.CRW_LIGHT_LEG != 'true'"
	lightOnly   = "env.CRW_LIGHT_LEG == 'true'"
	lightNotice = "light mode: this leg's tests run in full when the merge lane labels the pull request crw-lane"
)

// lightExpression is what the one job-level env holds: the pull request event, the repository
// variable, the negated crw-lane label check and the test-* part check, in that order. The first
// term keeps a dev push and a manual dispatch out, the third keeps a labeled pull request out and
// the fourth keeps lint and dist out, so only the five test legs of an unlabeled pull request run
// can ever be light.
const lightExpression = "github.event_name == 'pull_request' && vars.CRW_CI_MODE == 'light' && !contains(github.event.pull_request.labels.*.name, 'crw-lane') && startsWith(matrix.part, 'test-')"

// lightBodyEdit is the mirror pair's existing condition with the light guard appended inside the
// expression: a body-only edit of an unlabeled light run must not repeat a leg whose tests did not
// run, and the guard has to sit inside the braces because the text after them is not evaluated.
const lightBodyEdit = "${{ github.event.action == 'edited' && !github.event.changes.base && env.CRW_LIGHT_LEG != 'true' }}"

// lightWorkflow is .github/workflows/ci.yml as text.
func lightWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// jobEnv is a job body's job-level env keys, in order. A step's env sits eight spaces deep and is
// not read here.
func jobEnv(t *testing.T, body string) []string {
	t.Helper()
	var keys []string
	inEnv := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case line == "    env:":
			inEnv = true
		case inEnv && strings.HasPrefix(line, "      "):
			keys = append(keys, strings.TrimPrefix(line, "      "))
		case inEnv:
			inEnv = false
		}
	}
	return keys
}

// The light condition is defined exactly once, on go-product alone, as a job-level env whose value
// is the whole four-term expression: a dev push, a manual dispatch, a labeled pull request and the
// lint and dist legs can therefore never be light, and no step has to repeat the expression.
func TestLightMode_the_condition_is_defined_once_and_only_for_test_legs(t *testing.T) {
	jobs, _ := workflowJobs(t)
	got := jobEnv(t, jobs["go-product"])
	want := []string{lightEnvKey + ": ${{ " + lightExpression + " }}"}
	expectEqual(t, "go-product job-level env", got, want)
	if n := strings.Count(lightWorkflow(t), "${{ "+lightExpression+" }}"); n != 1 {
		t.Errorf("the light expression appears %d times in the workflow, want exactly one", n)
	}
	for _, term := range []string{
		"github.event_name == 'pull_request'",
		"vars.CRW_CI_MODE == 'light'",
		"!contains(github.event.pull_request.labels.*.name, 'crw-lane')",
		"startsWith(matrix.part, 'test-')",
	} {
		if !strings.Contains(lightExpression, term) {
			t.Errorf("the light expression does not hold %s", term)
		}
	}
}

// The notice step runs first and writes the exact line to the step summary; every other step of the
// leg carries the light guard, joined with the condition it already had, so a light leg does none of
// the leg's work and still concludes success under its own name.
func TestLightMode_the_notice_step_is_first_and_every_other_step_is_guarded(t *testing.T) {
	jobs, _ := workflowJobs(t)
	steps := workflowSteps(t, jobs["go-product"])
	if len(steps) == 0 {
		t.Fatal("go-product has no steps")
	}
	if got := steps[0]["name"]; got != "Light mode notice" {
		t.Errorf("the first step is %q, want the light mode notice", got)
	}
	if got := steps[0]["if"]; got != lightOnly {
		t.Errorf("the notice step runs if %q, want %q", got, lightOnly)
	}
	if got := steps[0]["run"]; got != "|" {
		t.Errorf("the notice step runs %q, want a literal block scalar", got)
	}
	line := "\n          printf '%s\\n' \"" + lightNotice + "\" >> \"$GITHUB_STEP_SUMMARY\"\n"
	if !strings.Contains(lightWorkflow(t), line) {
		t.Errorf("the notice step does not write the exact line to the step summary")
	}
	for i, step := range steps[1:] {
		if !strings.Contains(step["if"], lightGuard) {
			t.Errorf("step %d (%q) runs if %q, which does not carry %q", i+1, step["name"], step["if"], lightGuard)
		}
	}
	// The mirror pair keeps its body-only edit condition, with the guard inside the braces; a
	// guard appended after }} would never be evaluated.
	if got := steps[1]["if"]; got != lightBodyEdit {
		t.Errorf("the sparse checkout runs if %q, want %q", got, lightBodyEdit)
	}
	if got := steps[2]["if"]; got != lightBodyEdit {
		t.Errorf("the mirror step runs if %q, want %q", got, lightBodyEdit)
	}
	// The test step keeps its own condition and the mirror guard, then the light guard.
	testStep := "startsWith(matrix.part, 'test-') && steps.mirror.outputs.mirrored != 'true' && " + lightGuard
	found := false
	for _, step := range steps {
		if step["name"] == "Test and replay the contract corpus (${{ matrix.part }})" {
			found = true
			if got := step["if"]; got != testStep {
				t.Errorf("the test step runs if %q, want %q", got, testStep)
			}
		}
	}
	if !found {
		t.Error("go-product has no test step")
	}
}

// pull_request.types gains labeled after the five earlier types, so the merge lane's label starts a
// full run on the same head; the concurrency expression is unchanged, so that labeled run joins the
// pull request's main group and cancels the light run in progress.
func TestLightMode_the_labeled_trigger_is_added_and_the_concurrency_is_unchanged(t *testing.T) {
	workflow := lightWorkflow(t)
	if !strings.Contains(workflow, "    types: [opened, reopened, synchronize, ready_for_review, edited, labeled]\n") {
		t.Error("pull_request.types does not carry labeled after its five earlier types")
	}
	_, concurrency, found := strings.Cut(workflow, "\nconcurrency:\n")
	if !found {
		t.Fatal("the workflow has no concurrency block")
	}
	concurrency, _, found = strings.Cut(concurrency, "\njobs:\n")
	if !found {
		t.Fatal("the concurrency block does not end at jobs")
	}
	for _, want := range []string{
		"  group: workflow-skills-ci-${{ github.event.pull_request.number || github.ref }}\n",
		"  cancel-in-progress: ${{ !(github.event.action == 'edited' && !github.event.changes.base) }}\n",
	} {
		if !strings.Contains(concurrency, want) {
			t.Errorf("the concurrency block no longer carries %q", strings.TrimSpace(want))
		}
	}
}

// Only go-product reads the light condition. validate, secrets, skill-scripts-node and
// dev-gate name it nowhere, so they always run in full, and no job carries a job-level if,
// which could skip it. skill-scripts-node is the fifth job (CRW-353); it gates itself on the
// changed paths instead, and that gate is a step condition, not a job-level one.
func TestLightMode_only_the_go_product_job_reads_the_light_condition(t *testing.T) {
	jobs, _ := workflowJobs(t)
	for _, name := range []string{"validate", "secrets", "skill-scripts-node", "dev-gate"} {
		for _, word := range []string{lightEnvKey, "CRW_CI_MODE"} {
			if strings.Contains(jobs[name], word) {
				t.Errorf("%s names %q; only go-product reads the light condition", name, word)
			}
		}
	}
	for name, body := range jobs {
		if !regexp.MustCompile(`(?m)^    if:`).MatchString(body) {
			continue
		}
		if name != "dev-gate" || !regexp.MustCompile(`(?m)^    if: always\(\)$`).MatchString(body) {
			t.Errorf("%s carries a job-level if, which could skip the job", name)
		}
	}
}

// The mode changes no check name: the five jobs, the seven go-product legs and dev-gate's
// prerequisites are the eleven checks the branch protection requires. skill-scripts-node is
// the fifth job (CRW-353) and is a dev-gate prerequisite like the rest.
func TestLightMode_the_check_names_are_unchanged(t *testing.T) {
	jobs, order := workflowJobs(t)
	expectEqual(t, "the job names", sortedCopy(order), []string{"dev-gate", "go-product", "secrets", "skill-scripts-node", "validate"})
	expectEqual(t, "the go-product legs", sortedCopy(matrixValues(t, jobs["go-product"], "part")),
		[]string{"dist", "lint", "test-1", "test-2", "test-3", "test-4", "test-rest"})
	if !regexp.MustCompile(`(?m)^    needs: \[validate, secrets, skill-scripts-node, go-product\]$`).MatchString(jobs["dev-gate"]) {
		t.Error("dev-gate's prerequisites changed")
	}
}

// lightLeg is the light condition of the four terms above, evaluated rather than matched: the
// event, the repository variable, the pull request's labels and the leg's part. GitHub evaluates
// the same four terms in the same order, so this is what makes the claim that only an unlabeled
// pull request's five test legs can be light a behavioural check rather than a reading of text.
func lightLeg(event, mode string, labels []string, part string) bool {
	if event != "pull_request" {
		return false
	}
	if mode != "light" {
		return false
	}
	for _, label := range labels {
		if label == "crw-lane" {
			return false
		}
	}
	return strings.HasPrefix(part, "test-")
}

// The four terms evaluate to light for exactly the five test legs of an unlabeled pull request,
// and never for a dev push, a manual dispatch, a labeled pull request, lint or dist. Each term is
// also held to the workflow's own expression, so this evaluator cannot drift from ci.yml.
func TestLightMode_the_condition_evaluates_to_light_only_for_unlabeled_test_legs(t *testing.T) {
	for _, term := range []string{
		"github.event_name == 'pull_request'",
		"vars.CRW_CI_MODE == 'light'",
		"!contains(github.event.pull_request.labels.*.name, 'crw-lane')",
		"startsWith(matrix.part, 'test-')",
	} {
		if !strings.Contains(lightExpression, term) {
			t.Errorf("the evaluator term %s is not in the workflow's expression", term)
		}
	}
	legs := []string{"lint", "test-1", "test-2", "test-3", "test-4", "test-rest", "dist"}
	for _, row := range []struct {
		event, mode string
		labels      []string
		wantLight   []string
	}{
		{"pull_request", "light", nil, []string{"test-1", "test-2", "test-3", "test-4", "test-rest"}},
		{"pull_request", "light", []string{"crw-lane"}, nil},
		{"pull_request", "light", []string{"crw-lane", "other"}, nil},
		{"pull_request", "light", []string{"other"}, []string{"test-1", "test-2", "test-3", "test-4", "test-rest"}},
		{"pull_request", "", nil, nil},
		{"pull_request", "full", nil, nil},
		{"push", "light", nil, nil},
		{"workflow_dispatch", "light", nil, nil},
		{"schedule", "light", nil, nil},
	} {
		var got []string
		for _, part := range legs {
			if lightLeg(row.event, row.mode, row.labels, part) {
				got = append(got, part)
			}
		}
		expectEqual(t, row.event+" mode="+row.mode+" labels="+strings.Join(row.labels, ","), got, row.wantLight)
	}
}

// edit_mirror.sh looks for the leg's test step by name, so the name it builds from its own prefix
// and the leg's part has to be the name ci.yml gives that step. A rename on either side is a red
// test here rather than a leg mirrored without its tests.
func TestLightMode_the_mirror_script_looks_for_the_workflow_test_step(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "scripts", "ci", "edit_mirror.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^test_step_prefix='(.*)'$`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("edit_mirror.sh does not define test_step_prefix")
	}
	jobs, _ := workflowJobs(t)
	want := ""
	for _, step := range workflowSteps(t, jobs["go-product"]) {
		name := strings.ReplaceAll(step["name"], "${{ matrix.part }}", "test-2")
		if strings.HasPrefix(name, m[1]) {
			want = name
		}
	}
	if want == "" {
		t.Fatalf("go-product has no step whose name starts with %q", m[1])
	}
	if got := m[1] + "test-2)"; got != want {
		t.Errorf("edit_mirror.sh builds the test step name %q, but ci.yml names the step %q", got, want)
	}
}
