//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// scripts/ci/edit_mirror.sh decides whether a body-only edit of a pull request has to run a
// job again. These tests run the script as CI does, in a temporary directory with a stand-in
// gh ahead of it on PATH: the stand-in answers the two endpoints the script reads from JSON
// files written here, and records every call. They hold which run and which job the script
// accepts, and that no answer fails the job. What GitHub's API returns is GitHub's own.
//
// The last test reads .github/workflows/ci.yml for the structure the change needs: the
// concurrency group, the mirror step and the steps that stand down behind it, and the jobs,
// names and permissions that must not move.

// editMirrorStandInGH answers the runs endpoint and a run's jobs endpoint from files the test
// writes, and fails when EDIT_MIRROR_TEST_FAIL is set, as an unreachable API does.
const editMirrorStandInGH = `#!/bin/sh
printf '%s\n' "$*" >> "$EDIT_MIRROR_TEST_DIR/calls"
if [ -n "${EDIT_MIRROR_TEST_FAIL:-}" ]; then
  echo 'stand-in gh: the API is unavailable' >&2
  exit 1
fi
url=
for arg in "$@"; do
  case $arg in repos/*) url=$arg ;; esac
done
if [ -z "$url" ]; then
  echo 'stand-in gh: no api url' >&2
  exit 2
fi
case $url in
  */actions/workflows/ci.yml/runs*)
    cat "$EDIT_MIRROR_TEST_DIR/runs.json" ;;
  */actions/runs/*/jobs*)
    run=${url#*/actions/runs/}
    run=${run%%/*}
    cat "$EDIT_MIRROR_TEST_DIR/jobs-$run.json" ;;
  *)
    echo "stand-in gh: unexpected url $url" >&2
    exit 2 ;;
esac
`

const (
	editMirrorRepository = "thisisjun786/codex-relay-workflow"
	editMirrorHead       = "3f1d2c0b9a8e7d6c5b4a39281706f5e4d3c2b1a0"
	editMirrorSelfRun    = 4242
)

// editMirrorRunFixture is one entry of the runs endpoint's workflow_runs.
type editMirrorRunFixture struct {
	id         int
	headSHA    string
	event      string
	status     string
	repository string
	path       string
	startedAt  string
}

// editMirrorRun is a candidate run as the API answers it: a completed pull_request run of
// this repository, on this head, of this workflow.
func editMirrorRun(id int, startedAt string) editMirrorRunFixture {
	return editMirrorRunFixture{
		id:         id,
		headSHA:    editMirrorHead,
		event:      "pull_request",
		status:     "completed",
		repository: editMirrorRepository,
		path:       ".github/workflows/ci.yml",
		startedAt:  startedAt,
	}
}

// editMirrorRunAs is editMirrorRun with the fields change sets.
func editMirrorRunAs(id int, startedAt string, change func(*editMirrorRunFixture)) editMirrorRunFixture {
	run := editMirrorRun(id, startedAt)
	change(&run)
	return run
}

func (r editMirrorRunFixture) json() string {
	return fmt.Sprintf(`{"id":%d,"head_sha":%q,"event":%q,"status":%q,"path":%q,"head_repository":{"full_name":%q},"run_started_at":%q}`,
		r.id, r.headSHA, r.event, r.status, r.path, r.repository, r.startedAt)
}

// editMirrorJobFixture is one entry of a run's jobs: one attempt of one job name.
type editMirrorJobFixture struct {
	name       string
	attempt    int
	conclusion string
}

func editMirrorJob(name, conclusion string, attempt int) editMirrorJobFixture {
	return editMirrorJobFixture{name: name, attempt: attempt, conclusion: conclusion}
}

func (j editMirrorJobFixture) json() string {
	return fmt.Sprintf(`{"name":%q,"run_attempt":%d,"conclusion":%q,"status":"completed"}`, j.name, j.attempt, j.conclusion)
}

// editMirrorCase is one run of the script: what the API answers and the decision it must reach.
type editMirrorCase struct {
	name    string
	runs    []editMirrorRunFixture
	jobs    map[int][]editMirrorJobFixture
	jobName string // the job the script speaks for; "" is validate
	apiFail bool
	want    string // the mirrored= value
	wantRun int    // the run id mirrored= carries, 0 when it is not mirrored
}

// editMirrorOutcome is one run's result: the script's exit code and what it answered.
type editMirrorOutcome struct {
	code     int
	stderr   string
	summary  string
	calls    []string
	mirrored string
	runID    string
}

// runEditMirror answers the API as c says and runs scripts/ci/edit_mirror.sh with the
// variables ci.yml passes it.
func runEditMirror(t *testing.T, c editMirrorCase) editMirrorOutcome {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(editMirrorStandInGH), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runs := make([]string, len(c.runs))
	for i, run := range c.runs {
		runs[i] = run.json()
	}
	write("runs.json", `{"total_count":`+strconv.Itoa(len(c.runs))+`,"workflow_runs":[`+strings.Join(runs, ",")+`]}`)
	for id, jobs := range c.jobs {
		parts := make([]string, len(jobs))
		for i, job := range jobs {
			parts[i] = job.json()
		}
		write(fmt.Sprintf("jobs-%d.json", id), `{"total_count":`+strconv.Itoa(len(jobs))+`,"jobs":[`+strings.Join(parts, ",")+`]}`)
	}
	jobName := c.jobName
	if jobName == "" {
		jobName = "validate"
	}
	output, summary := filepath.Join(dir, "output"), filepath.Join(dir, "summary")
	var env []string
	for _, kv := range os.Environ() {
		// The tests may themselves run under Actions, which sets these.
		if !strings.HasPrefix(kv, "GITHUB_OUTPUT=") && !strings.HasPrefix(kv, "GITHUB_STEP_SUMMARY=") &&
			!strings.HasPrefix(kv, "REPOSITORY=") && !strings.HasPrefix(kv, "HEAD_SHA=") &&
			!strings.HasPrefix(kv, "JOB_NAME=") && !strings.HasPrefix(kv, "RUN_ID=") &&
			!strings.HasPrefix(kv, "EDIT_MIRROR_TEST_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY="+editMirrorRepository, "HEAD_SHA="+editMirrorHead,
		"JOB_NAME="+jobName, "RUN_ID="+strconv.Itoa(editMirrorSelfRun),
		"EDIT_MIRROR_TEST_DIR="+dir, "GITHUB_OUTPUT="+output, "GITHUB_STEP_SUMMARY="+summary)
	if c.apiFail {
		env = append(env, "EDIT_MIRROR_TEST_FAIL=1")
	}
	script := filepath.Join(repoRoot(), "scripts", "ci", "edit_mirror.sh")
	res := runEnv(t, dir, env, "bash", "--noprofile", "--norc", "-eo", "pipefail", script)
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	out := editMirrorOutcome{code: res.code, stderr: res.stderr, summary: read(summary)}
	out.calls = strings.Split(read(filepath.Join(dir, "calls")), "\n")
	for _, line := range strings.Split(read(output), "\n") {
		if value, ok := strings.CutPrefix(line, "mirrored="); ok {
			out.mirrored = value
		}
		if value, ok := strings.CutPrefix(line, "run-id="); ok {
			out.runID = value
		}
	}
	return out
}

// check holds the script to the decision c expects, and to never failing the job.
func (o editMirrorOutcome) check(t *testing.T, c editMirrorCase) {
	t.Helper()
	if o.code != 0 {
		t.Errorf("%s: exit %d, want 0\n%s", c.name, o.code, o.stderr)
	}
	if o.mirrored != c.want {
		t.Errorf("%s: mirrored=%q, want %q\n%s", c.name, o.mirrored, c.want, o.summary)
	}
	wantRun := ""
	if c.wantRun != 0 {
		wantRun = strconv.Itoa(c.wantRun)
	}
	if o.runID != wantRun {
		t.Errorf("%s: run-id=%q, want %q", c.name, o.runID, wantRun)
	}
	if c.want == "true" && !strings.Contains(o.summary, wantRun) {
		t.Errorf("%s: the summary does not name the mirrored run: %q", c.name, o.summary)
	}
}

// A body-only edit mirrors the job whose same-named job concluded success in the newest
// completed run of this repository for this head, and names that run.
func TestEditMirror_a_successful_same_named_job_is_mirrored(t *testing.T) {
	c := editMirrorCase{
		name:    "the newest run succeeded at this job",
		runs:    []editMirrorRunFixture{editMirrorRun(1111, "2026-10-06T06:00:00Z")},
		jobs:    map[int][]editMirrorJobFixture{1111: {editMirrorJob("secrets", "success", 1), editMirrorJob("validate", "success", 1)}},
		want:    "true",
		wantRun: 1111,
	}
	runEditMirror(t, c).check(t, c)
}

// Everything but a successful same-named job in a candidate run leaves the job to run in
// full: the script answers mirrored=false and exits 0, so a lookup that cannot establish the
// answer never narrows the run.
func TestEditMirror_nothing_else_is_mirrored(t *testing.T) {
	const older, newer = 1111, 2222
	for _, c := range []editMirrorCase{
		{name: "no candidate run at all", jobs: map[int][]editMirrorJobFixture{}},
		{
			name: "only this run",
			runs: []editMirrorRunFixture{editMirrorRun(editMirrorSelfRun, "2026-10-06T06:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{editMirrorSelfRun: {editMirrorJob("validate", "success", 1)}},
		},
		{
			name: "another head",
			runs: []editMirrorRunFixture{editMirrorRunAs(older, "2026-10-06T06:00:00Z", func(r *editMirrorRunFixture) { r.headSHA = strings.Repeat("a", 40) })},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "success", 1)}},
		},
		{
			name: "another workflow",
			runs: []editMirrorRunFixture{editMirrorRunAs(older, "2026-10-06T06:00:00Z", func(r *editMirrorRunFixture) { r.path = ".github/workflows/release.yml" })},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "success", 1)}},
		},
		{
			name: "a fork's head",
			runs: []editMirrorRunFixture{editMirrorRunAs(older, "2026-10-06T06:00:00Z", func(r *editMirrorRunFixture) { r.repository = "someone/codex-relay-workflow" })},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "success", 1)}},
		},
		{
			name: "not a pull_request run",
			runs: []editMirrorRunFixture{editMirrorRunAs(older, "2026-10-06T06:00:00Z", func(r *editMirrorRunFixture) { r.event = "push" })},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "success", 1)}},
		},
		{
			name: "not completed",
			runs: []editMirrorRunFixture{editMirrorRunAs(older, "2026-10-06T06:00:00Z", func(r *editMirrorRunFixture) { r.status = "in_progress" })},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "success", 1)}},
		},
		{
			name: "the job failed",
			runs: []editMirrorRunFixture{editMirrorRun(older, "2026-10-06T06:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "failure", 1)}},
		},
		{
			name: "the job was cancelled",
			runs: []editMirrorRunFixture{editMirrorRun(older, "2026-10-06T06:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "cancelled", 1)}},
		},
		{
			name: "the job was skipped",
			runs: []editMirrorRunFixture{editMirrorRun(older, "2026-10-06T06:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "skipped", 1)}},
		},
		{
			name: "no job of this name",
			runs: []editMirrorRunFixture{editMirrorRun(older, "2026-10-06T06:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("secrets", "success", 1)}},
		},
		{
			name: "the newest attempt of this job failed after an older one succeeded",
			runs: []editMirrorRunFixture{editMirrorRun(older, "2026-10-06T06:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "success", 1), editMirrorJob("validate", "failure", 2)}},
		},
		{
			name: "the newer of two candidate runs failed where the older succeeded",
			runs: []editMirrorRunFixture{editMirrorRun(older, "2026-10-06T06:00:00Z"), editMirrorRun(newer, "2026-10-06T07:00:00Z")},
			jobs: map[int][]editMirrorJobFixture{
				older: {editMirrorJob("validate", "success", 1)},
				newer: {editMirrorJob("validate", "failure", 1)},
			},
		},
		{name: "the API cannot be read", apiFail: true, jobs: map[int][]editMirrorJobFixture{}},
	} {
		c.want = "false"
		runEditMirror(t, c).check(t, c)
	}
}

// The newest of two candidate runs decides, so a later red run of the same head is never
// covered by an earlier green one.
func TestEditMirror_the_newest_candidate_run_decides(t *testing.T) {
	const older, newer = 1111, 2222
	c := editMirrorCase{
		name:    "the newer run succeeded",
		runs:    []editMirrorRunFixture{editMirrorRun(newer, "2026-10-06T07:00:00Z"), editMirrorRun(older, "2026-10-06T06:00:00Z")},
		jobs:    map[int][]editMirrorJobFixture{older: {editMirrorJob("validate", "failure", 1)}, newer: {editMirrorJob("validate", "success", 1)}},
		want:    "true",
		wantRun: newer,
	}
	runEditMirror(t, c).check(t, c)
}

// The script asks for the runs of this workflow on this head, and never reads its own run.
func TestEditMirror_reads_the_runs_of_this_workflow_on_this_head(t *testing.T) {
	c := editMirrorCase{
		name:    "one candidate",
		runs:    []editMirrorRunFixture{editMirrorRun(1111, "2026-10-06T06:00:00Z")},
		jobs:    map[int][]editMirrorJobFixture{1111: {editMirrorJob("validate", "success", 1)}},
		want:    "true",
		wantRun: 1111,
	}
	out := runEditMirror(t, c)
	out.check(t, c)
	calls := strings.Join(out.calls, "\n")
	for _, want := range []string{
		"actions/workflows/ci.yml/runs?head_sha=" + editMirrorHead,
		"event=pull_request", "status=completed",
		"actions/runs/1111/jobs",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("the script never asked for %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "actions/runs/"+strconv.Itoa(editMirrorSelfRun)) {
		t.Errorf("the script read its own run:\n%s", calls)
	}
}

// The workflow mirrors a body-only edit without cancelling a running run, and every step
// behind the mirror stands down only when the mirror answered. No job is skipped at job
// level, dev-gate keeps its place and its script, and the ten check names stay.
func TestWorkflow_the_body_only_edit_mirror_is_wired(t *testing.T) {
	const (
		bodyEdit = "${{ github.event.action == 'edited' && !github.event.changes.base }}"
		guard    = "steps.mirror.outputs.mirrored != 'true'"
	)
	jobs, order := workflowJobs(t)
	expectEqual(t, "the job names", sortedCopy(order), []string{"dev-gate", "go-product", "secrets", "validate"})
	expectEqual(t, "the go-product legs", sortedCopy(matrixValues(t, jobs["go-product"], "part")),
		[]string{"dist", "lint", "test-1", "test-2", "test-3", "test-4", "test-rest"})

	data, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	if !strings.Contains(workflow, "types: [opened, reopened, synchronize, ready_for_review, edited]") {
		t.Error("the pull_request types no longer carry edited")
	}
	if !strings.Contains(workflow, "\npermissions:\n  contents: read\n") {
		t.Error("the workflow's own permissions changed")
	}
	// A body-only edit joins the pull request's main group, where it waits behind a running
	// run instead of cancelling it; every other event still cancels.
	_, concurrency, found := strings.Cut(workflow, "\nconcurrency:\n")
	if !found {
		t.Fatal("the workflow has no concurrency block")
	}
	concurrency, _, found = strings.Cut(concurrency, "\njobs:\n")
	if !found {
		t.Fatal("the concurrency block does not end at jobs")
	}
	if strings.Contains(concurrency, "-edit") {
		t.Error("the concurrency group still has an -edit suffix")
	}
	if !strings.Contains(concurrency, "group: workflow-skills-ci-${{ github.event.pull_request.number || github.ref }}") {
		t.Error("the concurrency group is not the pull request's main group")
	}
	if !strings.Contains(concurrency, "cancel-in-progress: ${{ !(github.event.action == 'edited' && !github.event.changes.base) }}") {
		t.Error("cancel-in-progress does not keep a body-only edit from cancelling a running run")
	}

	for _, job := range []string{"validate", "secrets", "go-product"} {
		body := jobs[job]
		if regexp.MustCompile(`(?m)^    if:`).MatchString(body) {
			t.Errorf("%s carries a job-level if, which could skip it", job)
		}
		if !strings.Contains(body, "    permissions:\n      contents: read\n      actions: read\n") {
			t.Errorf("%s does not add exactly actions: read", job)
		}
		if !strings.Contains(body, "\n          sparse-checkout: scripts/ci\n") || !strings.Contains(body, "\n          fetch-depth: 1\n") {
			t.Errorf("%s does not check out scripts/ci alone for the mirror", job)
		}
		steps := workflowSteps(t, body)
		mirror := -1
		for i, step := range steps {
			if step["id"] == "mirror" {
				mirror = i
			}
		}
		if mirror < 0 {
			t.Fatalf("%s has no mirror step", job)
		}
		if got := steps[mirror]["run"]; got != "bash scripts/ci/edit_mirror.sh" {
			t.Errorf("%s runs %q as its mirror step", job, got)
		}
		if got := steps[mirror]["if"]; got != bodyEdit {
			t.Errorf("%s runs the mirror if %q, want %q", job, got, bodyEdit)
		}
		if mirror == 0 || steps[mirror-1]["uses"] == "" || steps[mirror-1]["if"] != bodyEdit {
			t.Errorf("%s does not check out the script on a body-only edit before the mirror", job)
		}
		for i, step := range steps[mirror+1:] {
			if !strings.Contains(step["if"], guard) {
				t.Errorf("%s: step %d after the mirror runs if %q, which does not carry %q", job, mirror+1+i, step["if"], guard)
			}
		}
	}
	// The mirror speaks for the job's check name, the one the earlier run reported.
	for job, name := range map[string]string{
		"validate":   "          JOB_NAME: validate\n",
		"secrets":    "          JOB_NAME: secrets\n",
		"go-product": "          JOB_NAME: go-product (${{ matrix.part }})\n",
	} {
		if !strings.Contains(jobs[job], name) {
			t.Errorf("%s does not ask the mirror for %q", job, strings.TrimSpace(name))
		}
	}

	// dev-gate is untouched: it still runs after every other job and decides on their results.
	gate := jobs["dev-gate"]
	if !regexp.MustCompile(`(?m)^    if: always\(\)` + "$").MatchString(gate) {
		t.Error("dev-gate no longer runs always")
	}
	if !regexp.MustCompile(`(?m)^    needs: \[validate, secrets, go-product\]` + "$").MatchString(gate) {
		t.Error("dev-gate's prerequisites changed")
	}
	if strings.Contains(gate, guard) {
		t.Error("dev-gate takes the mirror condition")
	}
	if !strings.Contains(gate, "${{ toJSON(needs) }}") {
		t.Error("dev-gate no longer decides on the needs results")
	}
}
