package mergeturn

// CRW-897, answer 6: the ci.yml job-set parser (moved out of the pin test in train_test.go) and
// verify's comparison of the head's job set with the runtime's expected list. The parser's own
// fixture is the repository's .github/workflows/ci.yml; the refusal cases use a forge stand-in whose
// checkout answers a workflow the test writes.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestTrainExpectedJobsMatchCiYml pins TrainExpectedJobs to .github/workflows/ci.yml: the workflow's
// jobs and its go-product matrix. A change to ci.yml's jobs turns this red (CRW-768 c1, c2), and the
// parser it uses is the one verify reads the head's workflow with (CRW-897, answer 6).
func TestTrainExpectedJobsMatchCiYml(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := TrainJobsFromWorkflow(string(data))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	got := append([]string{}, TrainExpectedJobs...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TrainExpectedJobs = %v, and ci.yml's jobs are %v", got, want)
	}
}

// TestTrainJobsFromWorkflowReadsTheMatrix: the go-product job's matrix expands into the leg names a
// run reports, and a workflow without the job is unreadable rather than a short list.
func TestTrainJobsFromWorkflowReadsTheMatrix(t *testing.T) {
	workflow := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    runs-on: ubuntu\n    strategy:\n      matrix:\n        part: [lint, test-1]\n  dev-gate:\n    runs-on: ubuntu\n"
	jobs, err := TrainJobsFromWorkflow(workflow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"validate", "go-product (lint)", "go-product (test-1)", "dev-gate"}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs = %v, want %v", jobs, want)
	}
	// the go-product job last: the matrix still reads, because the scan stops at the job's end
	last := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    strategy:\n      matrix:\n        part: [dist]\n"
	jobs, err = TrainJobsFromWorkflow(last)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jobs, []string{"validate", "go-product (dist)"}) {
		t.Fatalf("jobs of a workflow ending in go-product = %v", jobs)
	}
	// a workflow with no jobs block is unreadable
	if _, err := TrainJobsFromWorkflow("name: nothing\n"); err == nil {
		t.Fatal("a workflow with no jobs block was read")
	}
	// a job header with a trailing YAML comment is still a job: missing it would let a head add a job
	// the runtime never checks, and a comment on an expected header would refuse a legitimate bundle
	commented := "\njobs:\n  validate: # the first gate\n    runs-on: ubuntu\n  go-product:\n    strategy:\n      matrix:\n        part: [lint]\n  audit: # added gate\n    runs-on: ubuntu\n"
	jobs, err = TrainJobsFromWorkflow(commented)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jobs, []string{"validate", "go-product (lint)", "audit"}) {
		t.Fatalf("jobs with trailing comments = %v", jobs)
	}
	// a '#' outside a quoted scalar opens a comment; one inside quotes is part of the key
	if got := trainStripComment("  name: build #1"); got != "  name: build" {
		t.Fatalf("a trailing comment was not stripped: %q", got)
	}
	if got := trainStripComment("  \"a # b\":"); got != "  \"a # b\":" {
		t.Fatalf("a '#' inside a quoted key was stripped: %q", got)
	}
	// a comment on the go-product header does not hide the matrix that follows
	commentedProduct := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product: # the matrix\n    strategy:\n      matrix:\n        part: [dist]\n"
	jobs, err = TrainJobsFromWorkflow(commentedProduct)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jobs, []string{"validate", "go-product (dist)"}) {
		t.Fatalf("jobs of a commented go-product header = %v", jobs)
	}
	// a go-product job with no matrix part list is unreadable, never an empty leg list
	if _, err := TrainJobsFromWorkflow("\njobs:\n  go-product:\n    runs-on: ubuntu\n"); err == nil {
		t.Fatal("a go-product job with no matrix was read")
	}
	// an unterminated matrix list is unreadable
	if _, err := TrainJobsFromWorkflow("\njobs:\n  go-product:\n    strategy:\n      matrix:\n        part: [lint, test-1\n"); err == nil {
		t.Fatal("an unterminated matrix list was read")
	}
}

// TestTrainVerifyRefusesAChangedJobSet: a head whose ci.yml renames or adds a job is refused by
// verify naming the names, and a workflow verify cannot read is merge_target_unreadable
// (CRW-897, answer 6).
func TestTrainVerifyRefusesAChangedJobSet(t *testing.T) {
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")

	// the head renamed a job: the expected name is missing and the new one is added
	renamed := strings.Replace(string(repository), "\n  gui:\n", "\n  gui-renamed:\n", 1)
	if renamed == string(repository) {
		t.Fatal("the fixture did not rename a job")
	}
	w.proof.workflow = renamed
	_, err = w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a renamed job: %v", err)
	}
	if detail := err.Error(); !strings.Contains(detail, "gui") || !strings.Contains(detail, "gui-renamed") {
		t.Fatalf("the refusal does not name both jobs: %v", err)
	}

	// the head added a job the runtime does not expect
	added := strings.Replace(string(repository), "\n  gui:\n", "\n  gui:\n  gui-extra:\n    runs-on: ubuntu\n", 1)
	w.proof.workflow = added
	_, err = w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("an added job: %v", err)
	}
	if !strings.Contains(err.Error(), "gui-extra") {
		t.Fatalf("the refusal does not name the added job: %v", err)
	}

	// no refusal wrote a verified event
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}

	// the head's workflow cannot be read at all: merge_target_unreadable
	w.proof.workflow = ""
	w.proof.fileErr = errUnreadableWorkflow{}
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("an unreadable workflow: %v", err)
	}
	w.proof.fileErr = nil

	// the workflow is readable but not parseable: merge_target_unreadable, never an empty pass
	w.proof.workflow = "name: no jobs here\n"
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("an unparsable workflow: %v", err)
	}

	// the head's own workflow is accepted
	w.proof.workflow = string(repository)
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
		t.Fatalf("the head's own workflow was refused: %v", err)
	}
}

// errUnreadableWorkflow is the checkout stand-in's refusal for a head whose workflow cannot be read.
type errUnreadableWorkflow struct{}

func (errUnreadableWorkflow) Error() string { return "the checkout holds no such path" }
