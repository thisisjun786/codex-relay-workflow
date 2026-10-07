package manage

import (
	"strings"
	"testing"
)

// The correction generation 2 (PR #870, needs_changes) fixes two defects inside this issue's
// criteria. d1: the pair key was the display string repository#baseRef, which merges two different
// valid targets, because a base ref may contain '#' (git check-ref-format --branch 'release#dev'
// succeeds) and a local absolute repository path may contain '#'. d2: the promise that the
// single-target output stays byte for byte the same (decided answer 3) was not pinned by a test.

// dagReviewReview826TargetTurn records a landed merge turn with the exact repository, base ref and
// target key a test names. The shared laneTurn helper fixes the target to owner/repo#dev, and the
// d1 case needs two targets whose display strings collide.
func dagReviewReview826TargetTurn(f *dagReviewFixture, turnID, repository, baseRef, relationshipID string, prNumber int, at string) {
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, declared_ready, state, tenure, requested_at, held_at, closed_at, updated_at) VALUES (?,?,?,?,'project',?,'host',?,?,'head',1,'landed',1,?,?,?,?)",
		turnID, repository+"#"+baseRef, repository, baseRef, "holder-"+turnID, relationshipID, prNumber, at, at, dagReviewNull(at), at)
}

// The collision the pair key must not merge: repository '/work/repo#release' with base 'dev' and
// repository '/work/repo' with base 'release#dev' both display as '/work/repo#release#dev'. With
// neither landing observed, two anomalies are reported and each detail names its own display
// string. A key built from the display string reports one anomaly instead.
func TestDagReviewReview826CollidingDisplayStringsStayApart(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(5))
	dagReviewReview826TargetTurn(f, "turn-hash-in-repo", "/work/repo#release", "dev", "relationship-A", 42, dagReviewAt(30))
	dagReviewReview826TargetTurn(f, "turn-hash-in-ref", "/work/repo", "release#dev", "relationship-A", 43, dagReviewAt(35))
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved)
	if len(found) != 2 {
		t.Fatalf("landed_not_observed = %+v, want one per (node, repository, base ref)", found)
	}
	// The report order is node, then the display string, then the repository.
	for i, want := range []string{"/work/repo#release#dev", "/work/repo#release#dev"} {
		if found[i].Node != "A" || found[i].Issue != "CRW-A" {
			t.Errorf("anomaly %d does not name the node: %+v", i, found[i])
		}
		if !strings.Contains(found[i].Detail, want) {
			t.Errorf("anomaly %d detail does not name %s: %q", i, want, found[i].Detail)
		}
	}
}

// The same two colliding targets, with only the earlier landing observed: the later, unobserved
// landing is still reported. Under a display-string key the two landings merge, the earliest one
// is observed, and the later unobserved landing disappears.
func TestDagReviewReview826LaterCollidingTargetSurvivesAnObservation(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptanceHead("plan-1", "A", "acceptance-A", "relationship-A", "head-A", dagReviewAt(5))
	dagReviewReview826TargetTurn(f, "turn-hash-in-repo", "/work/repo#release", "dev", "relationship-A", 42, dagReviewAt(30))
	dagReviewReview826TargetTurn(f, "turn-hash-in-ref", "/work/repo", "release#dev", "relationship-A", 43, dagReviewAt(35))
	// An observation of the earlier target only (09:32Z, after its landing and before the later one).
	dagReviewReview826Observe(f, "acceptance-A", "/work/repo#release", "dev", dagReviewAt(32), 1)
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved)
	if len(found) != 1 {
		t.Fatalf("landed_not_observed = %+v, want the later unobserved target alone", found)
	}
	if !strings.Contains(found[0].Detail, "/work/repo#release#dev") {
		t.Errorf("the anomaly does not name the later target: %q", found[0].Detail)
	}
}

// One target and one landed turn is the case decided answer 3 fixes: the whole anomaly and the
// command's JSON and --text output are compared byte for byte against expected literals.
func TestDagReviewReview826SingleTargetOutputIsByteIdentical(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(10))
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "landed", "relationship-A", 42, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	f.close()

	review := dagReviewRunReview(t, f, nil, 0)
	found := dagReviewFind(review, dagReviewKindLandedNotObserved)
	want := DagReviewAnomaly{
		Kind: "landed_not_observed", Plan: "plan-1", Node: "A", Issue: "CRW-A",
		Detail: "the lane landed and no integration was observed in owner/repo#dev within 15m0s",
	}
	if len(found) != 1 {
		t.Fatalf("landed_not_observed = %+v, want one", found)
	}
	if found[0] != want {
		t.Errorf("the anomaly = %+v, want %+v", found[0], want)
	}

	const wantJSON = "{\"plans\":[{\"plan\":\"plan-1\",\"revision\":1,\"nodes\":1,\"edges\":0,\"released\":1,\"accepted\":1,\"integrated\":0}],\"anomalies\":[{\"kind\":\"landed_not_observed\",\"plan\":\"plan-1\",\"node\":\"A\",\"issue\":\"CRW-A\",\"detail\":\"the lane landed and no integration was observed in owner/repo#dev within 15m0s\"}],\"checks\":[{\"name\":\"released_before_predecessor\",\"state\":\"unmeasured\",\"detail\":\"the scheduler exports no edge reading with the time it became satisfied\"}]}\n"
	const wantText = "plan plan-1 revision 1: 1 nodes, 0 edges, 1 released, 1 accepted, 0 integrated\nanomalies: 1\n - landed_not_observed plan-1 A CRW-A: the lane landed and no integration was observed in owner/repo#dev within 15m0s\ncheck released_before_predecessor: unmeasured (the scheduler exports no edge reading with the time it became satisfied)\n"

	stdout, stderr, code := dagReviewRunCommand(t, f.dir, nil)
	if code != dagReviewAnomalyExit || stderr != "" || stdout != wantJSON {
		t.Errorf("JSON: code=%d stderr=%q\n got %q\nwant %q", code, stderr, stdout, wantJSON)
	}
	stdoutText, stderrText, codeText := dagReviewRunCommand(t, f.dir, []string{"--text"})
	if codeText != dagReviewAnomalyExit || stderrText != "" || stdoutText != wantText {
		t.Errorf("--text: code=%d stderr=%q\n got %q\nwant %q", codeText, stderrText, stdoutText, wantText)
	}
}
