package manage

import (
	"fmt"
	"strings"
	"testing"
)

// The issue body (CRW-948) fixes three answers this file pins: one landed_not_observed result per
// (node, target) pair; several turns of one pair are that pair's earliest landing; and the
// single-target output is unchanged byte for byte. The two-target case is the red test: the
// baseline collected its result in a map keyed by node id alone, so the last turn's target
// overwrote the earlier one and the other target disappeared.

// dagReviewReview826ReleaseTurn records a landed merge turn on owner/repo#release for the fixture.
// The shared laneTurn helper fixes the target to owner/repo#dev, and this issue's example needs a
// second target on the same repository, so the row is written here with the same shape.
func dagReviewReview826ReleaseTurn(f *dagReviewFixture, turnID, relationshipID string, prNumber int, at string) {
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, declared_ready, state, tenure, requested_at, held_at, closed_at, updated_at) VALUES (?,?,'owner/repo','release','project',?,'host',?,?,'head',1,'landed',1,?,?,?,?)",
		turnID, "owner/repo#release", "holder-"+turnID, relationshipID, prNumber, at, at, dagReviewNull(at), at)
}

// Both targets observed is the negative control for the two-target path: nothing is reported when
// every target the node landed on was observed.
func TestDagReviewReview826BothTargetsObservedIsClean(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptanceHead("plan-1", "A", "acceptance-A", "relationship-A", "head-A", dagReviewAt(5))
	f.laneTurn("turn-dev", "owner/repo#dev", "holder-dev", "landed", "relationship-A", 42, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	dagReviewReview826ReleaseTurn(f, "turn-release", "relationship-A", 43, dagReviewAt(35))
	dagReviewReview826Observe(f, "acceptance-A", "owner/repo", "dev", dagReviewAt(40), 1)
	dagReviewReview826Observe(f, "acceptance-A", "owner/repo", "release", dagReviewAt(41), 2)
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved); len(found) != 0 {
		t.Errorf("landed_not_observed = %+v, want none: both targets were observed", found)
	}
}

// dagReviewReview826Observe records one integration observation in a named target with an explicit
// observation id. The shared observationIn helper numbers a new observation by the count already
// stored for the (acceptance, target) pair, so its first observation of each of two targets of one
// acceptance both become observation-<acceptance>-1 and the second insert violates the primary key.
// This helper takes the sequence directly, which is what two targets of one acceptance need.
func dagReviewReview826Observe(f *dagReviewFixture, acceptanceID, repository, baseRef, observedAt string, seq int) {
	f.t.Helper()
	f.writePlans()
	var head string
	if err := f.store.DB.QueryRow("SELECT COALESCE(head_sha, '') FROM dag_acceptances WHERE acceptance_id = ?", acceptanceID).Scan(&head); err != nil {
		f.t.Fatalf("read the accepted head of %s: %v", acceptanceID, err)
	}
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		fmt.Sprintf("observation-%s-%d", acceptanceID, seq), acceptanceID, repository, baseRef, head, "tip", dagReviewFlag(true), "ancestry", seq, nil, observedAt)
}

// The issue's example: t1 landed owner/repo#dev at 09:30Z and t2 owner/repo#release at 09:35Z for
// one relationship, the review runs at 10:00Z with a 15 minute stall, and neither landing was
// observed. Both targets are reported, dev before release, each detail naming its own target.
func TestDagReviewReview826EveryTargetIsReported(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(5))
	f.laneTurn("turn-dev", "owner/repo#dev", "holder-dev", "landed", "relationship-A", 42, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	dagReviewReview826ReleaseTurn(f, "turn-release", "relationship-A", 43, dagReviewAt(35))
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved)
	if len(found) != 2 {
		t.Fatalf("landed_not_observed = %+v, want one per (node, target) pair", found)
	}
	for i, want := range []string{"owner/repo#dev", "owner/repo#release"} {
		if found[i].Node != "A" || found[i].Issue != "CRW-A" {
			t.Errorf("anomaly %d does not name the node: %+v", i, found[i])
		}
		if !strings.Contains(found[i].Detail, want) {
			t.Errorf("anomaly %d detail does not name %s: %q", i, want, found[i].Detail)
		}
	}
}

// A target whose landing was observed is not reported, while the other target of the same node is:
// the observation resolves the branch it was recorded in and no other.
func TestDagReviewReview826ObservedTargetIsNotReported(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptanceHead("plan-1", "A", "acceptance-A", "relationship-A", "head-A", dagReviewAt(5))
	f.laneTurn("turn-dev", "owner/repo#dev", "holder-dev", "landed", "relationship-A", 42, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	dagReviewReview826ReleaseTurn(f, "turn-release", "relationship-A", 43, dagReviewAt(35))
	f.observation("acceptance-A", dagReviewAt(40), true, "")
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved)
	if len(found) != 1 {
		t.Fatalf("landed_not_observed = %+v, want only the unobserved target", found)
	}
	if !strings.Contains(found[0].Detail, "owner/repo#release") {
		t.Errorf("the remaining anomaly does not name the unobserved target: %q", found[0].Detail)
	}
}

// Several turns of one (node, target) pair are that pair's single landing: they are one anomaly,
// not one per turn.
func TestDagReviewReview826SeveralTurnsOfOnePairAreOneLanding(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptance("plan-1", "A", "acceptance-A", "relationship-A", dagReviewAt(5))
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "landed", "relationship-A", 42, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	f.laneTurn("turn-2", "owner/repo#dev", "holder-2", "landed", "relationship-A", 43, dagReviewAt(35), dagReviewAt(35), dagReviewAt(35))
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved)
	if len(found) != 1 {
		t.Fatalf("landed_not_observed = %+v, want one for the (node, target) pair", found)
	}
	if found[0].Node != "A" || !strings.Contains(found[0].Detail, "owner/repo#dev") {
		t.Errorf("the anomaly does not name the pair: %+v", found[0])
	}
}

// The pair's landing is its earliest one, so an observation recorded at or after that landing
// resolves the pair even when a later turn of the same pair has no observation of its own: the
// node was observed integrated in that target, which is what this check asks.
func TestDagReviewReview826EarliestLandingDecides(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptanceHead("plan-1", "A", "acceptance-A", "relationship-A", "head-A", dagReviewAt(5))
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "landed", "relationship-A", 42, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	f.laneTurn("turn-2", "owner/repo#dev", "holder-2", "landed", "relationship-A", 43, dagReviewAt(35), dagReviewAt(35), dagReviewAt(35))
	// 09:32Z: after the earliest landing (09:30Z), before the later one (09:35Z).
	f.observation("acceptance-A", dagReviewAt(32), true, "")
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved); len(found) != 0 {
		t.Errorf("landed_not_observed = %+v, want none: the pair's earliest landing was observed", found)
	}
}

// The pair keeps its EARLIEST landing, not the first one read. The lanes are read ordered by turn
// id, so a test whose read order and landing order agree cannot tell the two rules apart: every
// other repeated-pair test gives the earlier turn id the earlier landing. Here turn-a is read first
// and landed later, turn-b is read second and landed earlier, and only the earliest landing is
// observed. Earliest-wins resolves the pair and reports nothing; keeping the first landing read
// would take turn-a's later instant, find the observation too early, and report the pair.
func TestDagReviewReview826EarliestLandingWinsOverReadOrder(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	f.acceptanceHead("plan-1", "A", "acceptance-A", "relationship-A", "head-A", dagReviewAt(5))
	f.laneTurn("turn-a", "owner/repo#dev", "holder-a", "landed", "relationship-A", 42, dagReviewAt(35), dagReviewAt(35), dagReviewAt(35))
	f.laneTurn("turn-b", "owner/repo#dev", "holder-b", "landed", "relationship-A", 43, dagReviewAt(30), dagReviewAt(30), dagReviewAt(30))
	// 09:32Z: at or after the earliest landing (09:30Z, turn-b), before the later one (09:35Z, turn-a).
	f.observation("acceptance-A", dagReviewAt(32), true, "")
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 15), dagReviewKindLandedNotObserved); len(found) != 0 {
		t.Errorf("landed_not_observed = %+v, want none: the pair's earliest landing was observed even though it was read second", found)
	}
}

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
