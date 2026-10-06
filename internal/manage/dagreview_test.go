package manage

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// dagReviewFind returns the anomalies of one kind, so a test names what it is pinning rather
// than an index into a list that a later source may reorder.
func dagReviewFind(review Review, kind string) []DagReviewAnomaly {
	var found []DagReviewAnomaly
	for _, anomaly := range review.Anomalies {
		if anomaly.Kind == kind {
			found = append(found, anomaly)
		}
	}
	return found
}

// dagReviewRunReview runs the review over the fixture and fails the test on a read error.
func dagReviewRunReview(t *testing.T, f *dagReviewFixture, plans []string, stall int) Review {
	t.Helper()
	review, err := DagReview(context.Background(), dagReviewEnv(t), dagReviewConfig(t, f.dir, plans, stall))
	if err != nil {
		t.Fatalf("DagReview: %v", err)
	}
	return review
}

// dagReviewOnePlan is a plan with one live revision and one node, which each anomaly test then
// breaks in the one way it pins.
func dagReviewOnePlan(t *testing.T, f *dagReviewFixture, node string) {
	t.Helper()
	f.plan("plan-1", "project-1")
	f.revision("plan-1", 1, dagReviewAt(0))
	f.node("plan-1", node, "CRW-"+node)
}

// An integrated edge whose successor was released before the predecessor was observed
// integrated is an anomaly.
func TestDagReviewReleasedBeforePredecessor(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.node("plan-1", "B", "CRW-B")
	f.edge("plan-1", "e1", "A", "B", "integrated", 1)
	f.release("plan-1", "B", "manifest-B", dagReviewAt(5))
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(6))
	f.observation("acceptance-A", dagReviewAt(30), true, "")
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindReleasedBeforePredecessor)
	if len(found) != 1 || found[0].Node != "B" || found[0].Issue != "CRW-B" {
		t.Fatalf("released_before_predecessor = %+v, want one for B", found)
	}
}

// An edge introduced after the successor was already released orders the merge, not the release,
// so the same shape is not an anomaly.
func TestDagReviewEdgeAddedAfterTheReleaseIsNotAnAnomaly(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.node("plan-1", "B", "CRW-B")
	f.revision("plan-1", 2, dagReviewAt(20))
	f.edge("plan-1", "e1", "A", "B", "integrated", 2)
	f.release("plan-1", "B", "manifest-B", dagReviewAt(5))
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(6))
	f.observation("acceptance-A", dagReviewAt(30), true, "")
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindReleasedBeforePredecessor); len(found) != 0 {
		t.Errorf("an edge added after the release was reported: %+v", found)
	}
}

// A node released twice is an anomaly and its detail carries the count.
func TestDagReviewReleasedRepeatedlyCarriesTheCount(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.release("plan-1", "A", "manifest-1", dagReviewAt(5))
	f.release("plan-1", "A", "manifest-2", dagReviewAt(10))
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindReleasedRepeatedly)
	if len(found) != 1 {
		t.Fatalf("released_repeatedly = %+v, want one", found)
	}
	if !strings.Contains(found[0].Detail, "2") {
		t.Errorf("the detail does not carry the count: %q", found[0].Detail)
	}
}

// Two nodes in flight whose declared regions the scheduler grades exclusive are an anomaly.
func TestDagReviewExclusiveOverlapRunning(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.node("plan-1", "B", "CRW-B")
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.release("plan-1", "B", "manifest-B", dagReviewAt(6))
	f.region("plan-1", "A", "shared.go", "file", "", "delete", true)
	f.region("plan-1", "B", "shared.go", "file", "", "edit", false)
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindExclusiveOverlapRunning)
	if len(found) == 0 {
		t.Fatal("an exclusive overlap in flight was not reported")
	}
	if found[0].Plan != "plan-1" || found[0].Issue == "" {
		t.Errorf("the anomaly does not name the plan and the issue: %+v", found[0])
	}
}

// A lane that landed with no integration observed past the threshold is an anomaly.
func TestDagReviewLandedNotObserved(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(10))
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "landed", "relationship-A", 42, dagReviewAt(10), dagReviewAt(10), dagReviewAt(12))
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindLandedNotObserved)
	if len(found) != 1 || found[0].Node != "A" {
		t.Fatalf("landed_not_observed = %+v, want one for A", found)
	}
}

// A relationship of the plan's project, opened after the plan, with no DAG execution record is
// an anomaly.
func TestDagReviewChildWithoutRelease(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.relationship("relationship-B", "CRW-B", dagReviewAt(10))
	f.scope("relationship-B", "project-1")
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindChildWithoutRelease)
	if len(found) != 1 || found[0].Issue != "CRW-B" {
		t.Fatalf("child_without_release = %+v, want one for CRW-B", found)
	}
}

// A child the plan did release is not an anomaly, and neither is a relationship opened before
// the plan existed.
func TestDagReviewReleasedChildAndEarlierRelationshipAreNotAnomalies(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.plan("plan-1", "project-1")
	f.revision("plan-1", 1, dagReviewAt(10))
	f.node("plan-1", "A", "CRW-A")
	f.relationship("relationship-B", "CRW-B", dagReviewAt(20))
	f.scope("relationship-B", "project-1")
	f.execution("plan-1", "B", "relationship-B")
	f.relationship("relationship-old", "CRW-OLD", dagReviewAt(-10))
	f.scope("relationship-old", "project-1")
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindChildWithoutRelease); len(found) != 0 {
		t.Errorf("a released or pre-plan child was reported: %+v", found)
	}
}

// A lane holding with no update past the threshold is an anomaly whose detail carries the PR,
// the holder and the waiters.
func TestDagReviewLaneTurnStalled(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "holding", "", 77, dagReviewAt(1), dagReviewAt(1), "")
	f.laneTurn("turn-2", "owner/repo#dev", "holder-2", "waiting", "", 78, dagReviewAt(2), dagReviewAt(2), "")
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindLaneTurnStalled)
	if len(found) != 1 {
		t.Fatalf("lane_turn_stalled = %+v, want one", found)
	}
	for _, want := range []string{"77", "holder-1", "holding", "1 waiting"} {
		if !strings.Contains(found[0].Detail, want) {
			t.Errorf("the detail does not carry %q: %q", want, found[0].Detail)
		}
	}
}

// The normal state raises nothing: two nodes editing the same file locally may run together
// (the scheduler releases that on purpose), and a lane inside the threshold is not stalled.
func TestDagReviewNormalStateRaisesNothing(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.node("plan-1", "B", "CRW-B")
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.release("plan-1", "B", "manifest-B", dagReviewAt(6))
	f.region("plan-1", "A", "shared.go", "symbol", "Alpha", "edit", false)
	f.region("plan-1", "B", "shared.go", "symbol", "Beta", "edit", false)
	f.grade("plan-1", "A", "shared.go", "symbol", "Alpha", "local", "")
	f.grade("plan-1", "B", "shared.go", "symbol", "Beta", "local", "")
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "holding", "", 77, dagReviewAt(50), dagReviewAt(50), "")
	f.close()

	review := dagReviewRunReview(t, f, nil, 0)
	if len(review.Anomalies) != 0 {
		t.Fatalf("the normal state raised %+v", review.Anomalies)
	}
	if len(review.Plans) != 1 || review.Plans[0].Released != 2 {
		t.Errorf("the plan shape is wrong: %+v", review.Plans)
	}
}

// A store that predates the region grade and hold tables is still read, and the readings it
// cannot take are reported as unmeasured checks rather than as a failed review.
func TestDagReviewStoreWithoutTheGradeTablesReportsUnmeasuredChecks(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.region("plan-1", "A", "shared.go", "file", "", "edit", false)
	f.exec("DROP TABLE dag_node_region_grades")
	f.exec("DROP TABLE dag_node_region_holds")
	f.close()

	review := dagReviewRunReview(t, f, nil, 0)
	unmeasured := map[string]bool{}
	for _, check := range review.Checks {
		if check.State == dagReviewUnmeasured {
			unmeasured[check.Name] = true
		}
	}
	if !unmeasured["dag_node_region_grades"] || !unmeasured["dag_node_region_holds"] {
		t.Errorf("the checks do not report both absent tables: %+v", review.Checks)
	}
}

// The configured plan list narrows the review to those plans.
func TestDagReviewConfiguredPlansNarrowTheReview(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.plan("plan-2", "project-2")
	f.revision("plan-2", 1, dagReviewAt(0))
	f.node("plan-2", "C", "CRW-C")
	f.close()

	review := dagReviewRunReview(t, f, []string{"plan-2"}, 0)
	if len(review.Plans) != 1 || review.Plans[0].Plan != "plan-2" {
		t.Fatalf("the configured plan list was not honoured: %+v", review.Plans)
	}
}

// The default JSON output carries the three keys the issue fixes, and a review that finds
// nothing exits 0 while one that finds something exits 1.
func TestDagReviewCommandJSONOutputAndExitStatus(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.close()

	stdout, stderr, code := dagReviewRunCommand(t, f.dir, nil)
	if code != 0 {
		t.Fatalf("a clean review exited %d: %s", code, stderr)
	}
	decoded := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, stdout)
	}
	for _, key := range []string{"plans", "anomalies", "checks"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("the output has no %q key: %s", key, stdout)
		}
	}

	f2 := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f2, "A")
	f2.release("plan-1", "A", "manifest-1", dagReviewAt(5))
	f2.release("plan-1", "A", "manifest-2", dagReviewAt(10))
	f2.close()
	if _, _, code := dagReviewRunCommand(t, f2.dir, nil); code != dagReviewAnomalyExit {
		t.Errorf("a review that found something exited %d, want %d", code, dagReviewAnomalyExit)
	}
}

// --anomalies-only omits the plan shape and the checks, and --text prints one line per record.
func TestDagReviewAnomaliesOnlyAndTextOutput(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.release("plan-1", "A", "manifest-1", dagReviewAt(5))
	f.release("plan-1", "A", "manifest-2", dagReviewAt(10))
	f.close()

	stdout, stderr, code := dagReviewRunCommand(t, f.dir, []string{"--anomalies-only"})
	if code != dagReviewAnomalyExit {
		t.Fatalf("--anomalies-only exited %d: %s", code, stderr)
	}
	decoded := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, stdout)
	}
	for _, key := range []string{"plans", "checks"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("--anomalies-only kept the %q key: %s", key, stdout)
		}
	}
	if _, ok := decoded["anomalies"]; !ok {
		t.Errorf("--anomalies-only dropped the anomalies: %s", stdout)
	}

	textOut, _, textCode := dagReviewRunCommand(t, f.dir, []string{"--text"})
	if textCode != dagReviewAnomalyExit {
		t.Fatalf("--text exited %d", textCode)
	}
	lines := strings.Split(strings.TrimSpace(textOut), "\n")
	if len(lines) < 3 {
		t.Fatalf("--text printed %d lines:\n%s", len(lines), textOut)
	}
	if !strings.HasPrefix(lines[0], "plan plan-1 revision 1:") {
		t.Errorf("the first line does not describe the plan: %q", lines[0])
	}
	if !strings.Contains(textOut, dagReviewKindReleasedRepeatedly) {
		t.Errorf("--text does not name the anomaly kind:\n%s", textOut)
	}
}

// A review that cannot read the store exits 3 rather than reporting an empty result.
func TestDagReviewCommandWithoutAStoreExitsThree(t *testing.T) {
	coreTempHome(t)
	dir := t.TempDir()
	if _, _, code := dagReviewRunCommand(t, dir, nil); code != dagReviewStoreExit {
		t.Errorf("a missing store exited %d, want %d", code, dagReviewStoreExit)
	}
}

// An unknown argument is a usage error, and the help flag prints the usage line.
func TestDagReviewCommandUsage(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.close()
	if _, _, code := dagReviewRunCommand(t, f.dir, []string{"--nope"}); code != usageExit {
		t.Errorf("an unknown argument exited %d, want %d", code, usageExit)
	}
	stdout, _, code := dagReviewRunCommand(t, f.dir, []string{"--help"})
	if code != 0 || !strings.Contains(stdout, "usage: crw manage dag-review") {
		t.Errorf("--help: exit %d output %q", code, stdout)
	}
}

// dagReviewRunCommand runs the command against a fixture state directory with the clock fixed,
// and returns its stdout, stderr and status.
func dagReviewRunCommand(t *testing.T, state string, args []string) (string, string, int) {
	t.Helper()
	exe, _ := coreFakeCRW(t, state, 0)
	var stdout, stderr strings.Builder
	e := &Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Getenv: os.Getenv, Now: dagReviewNow, Executable: exe}
	code := dagReviewCommand.Run(context.Background(), e, args)
	return stdout.String(), stderr.String(), code
}
