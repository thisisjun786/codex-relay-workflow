package manage

import (
	"strings"
	"testing"
)

// The four readings this issue fixes, each with the red test the issue body names, and the controls
// the issue body fixes. The fixture is dagReviewFixture: a plan is described and written through the
// DAG repository, so the scheduler's own reading (Progress) accepts it, and every other row is
// written directly.

// dagReviewCanonPlan describes a plan with the named implementation nodes at one revision, so a
// test can speak about nodes rather than about revisions.
func dagReviewCanonPlan(t *testing.T, f *dagReviewFixture, nodes ...string) {
	t.Helper()
	f.plan("plan-1", "project-1")
	f.revision("plan-1", 1, dagReviewAt(0))
	for _, node := range nodes {
		f.node("plan-1", node, "CRW-"+node)
	}
}

// dagReviewCanonObserved records a node released to an active relationship, with a recorded
// execution of the named kind and generation, and returns the relationship id. It is the shape the
// relay leaves after it released the node and bound a child to it.
func dagReviewCanonObserved(f *dagReviewFixture, node, kind string, generation int) string {
	return f.boundExecution("plan-1", node, kind, generation)
}

// C1: a node the relay executed twice (an initial execution and a correction generation) is
// reported released_repeatedly as executed 2 times. The correction generation is recorded in
// dag_node_executions alone, so a review that counts dag_releases rows reads one execution.
func TestDagReviewReleasedRepeatedlyCountsTheCorrectionGeneration(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewCanonPlan(t, f, "A")
	relationshipID := dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
	f.executionKind("plan-1", "A", relationshipID, 2, dagReviewExecutionCorrection)
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindReleasedRepeatedly)
	if len(found) != 1 {
		t.Fatalf("released_repeatedly = %+v, want one", found)
	}
	if found[0].Node != "A" || found[0].Issue != "CRW-A" {
		t.Errorf("the anomaly does not name the node: %+v", found[0])
	}
	if found[0].Detail != "executed 2 times (initial 1, correction 1)" {
		t.Errorf("detail = %q, want %q", found[0].Detail, "executed 2 times (initial 1, correction 1)")
	}
}

// C2: a node whose only positive integration observation is followed by a negative one is not
// integrated (the scheduler reads the first positive after the last negative), and the edge reading
// this review cannot take is an unmeasured check rather than a finding.
func TestDagReviewWithdrawnIntegrationIsNotCountedAndTheEdgeCheckIsUnmeasured(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewCanonPlan(t, f, "A", "B")
	f.edge("plan-1", "e1", "A", "B", "integrated", 1)
	relationshipID := dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
	f.acceptanceHead("plan-1", "A", "acceptance-A", relationshipID, "head-A", dagReviewAt(5))
	f.mergedMark(relationshipID, dagReviewAt(6))
	f.observation("acceptance-A", dagReviewAt(10), true, "")
	f.observation("acceptance-A", dagReviewAt(20), false, "")
	f.close()

	review := dagReviewRunReview(t, f, nil, 0)
	if len(review.Plans) != 1 || review.Plans[0].Integrated != 0 {
		t.Fatalf("the plan integrated count = %+v, want 0: a withdrawn landing is not an integration", review.Plans)
	}
	if found := dagReviewFind(review, dagReviewKindReleasedBeforePredecessor); len(found) != 0 {
		t.Errorf("released_before_predecessor raised %+v, want no anomaly", found)
	}
	if !dagReviewUnmeasuredCheck(review, dagReviewKindReleasedBeforePredecessor) {
		t.Errorf("released_before_predecessor is not an unmeasured check: %+v", review.Checks)
	}
}

// C3: a landing is observed in the target it landed on. An observation of the same node in another
// target does not resolve the turn, so the anomaly names the turn's own repository and base ref.
func TestDagReviewLandedNotObservedUsesTheTurnsTarget(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewCanonPlan(t, f, "A")
	relationshipID := dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
	f.acceptanceHead("plan-1", "A", "acceptance-A", relationshipID, "head-A", dagReviewAt(5))
	// The node was observed in another target only.
	f.observationIn("acceptance-A", "owner/repo", "release", dagReviewAt(30), true, "")
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "landed", relationshipID, 42, dagReviewAt(5), dagReviewAt(5), dagReviewAt(5))
	f.close()

	found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindLandedNotObserved)
	if len(found) != 1 || found[0].Node != "A" {
		t.Fatalf("landed_not_observed = %+v, want one for A", found)
	}
	if !strings.Contains(found[0].Detail, "owner/repo#dev") {
		t.Errorf("the detail does not name the turn's target: %q", found[0].Detail)
	}
}

// C4: a release ended by dag-release-close owns nothing (the scheduler reads the node as planned and
// then ready), so the node is not in flight and cannot overlap a running node exclusively.
func TestDagReviewClosedReleaseIsNotInFlight(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewCanonPlan(t, f, "A", "B")
	// A: a release whose managed start was abandoned and then closed.
	f.closedRelease("plan-1", "A", "manifest-A", dagReviewAt(10))
	// B: released and still holding the same exclusive place (its child is not bound yet, so the
	// scheduler reads it releasing).
	f.release("plan-1", "B", "manifest-B", dagReviewAt(6))
	f.region("plan-1", "A", "shared.go", "file", "", "delete", true)
	f.region("plan-1", "B", "shared.go", "file", "", "edit", false)
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindExclusiveOverlapRunning); len(found) != 0 {
		t.Errorf("a closed release was reported in flight: %+v", found)
	}
}

// The controls the issue body fixes: the readings that were already right stay right.
func TestDagReviewControlsAreUnchanged(t *testing.T) {
	t.Run("a node executed once is not reported repeatedly", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewCanonPlan(t, f, "A")
		dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
		f.close()
		if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindReleasedRepeatedly); len(found) != 0 {
			t.Errorf("a single execution was reported: %+v", found)
		}
	})

	t.Run("a parent handover is not a re-execution", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewCanonPlan(t, f, "A")
		dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
		// dag-adopt binds the successor relationship to the node as a parent_handover of the same
		// generation: the child was handed to another parent, it was not run again.
		f.relayReadRelationship("relationship-A-successor", "CRW-A", "active", "parent", "child-A", 1)
		f.executionKind("plan-1", "A", "relationship-A-successor", 1, "parent_handover")
		f.close()
		if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindReleasedRepeatedly); len(found) != 0 {
			t.Errorf("a parent handover was reported as a re-execution: %+v", found)
		}
	})

	t.Run("a landed head is counted integrated", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewCanonPlan(t, f, "A", "B")
		f.edge("plan-1", "e1", "A", "B", "integrated", 1)
		relationshipID := dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
		f.acceptanceHead("plan-1", "A", "acceptance-A", relationshipID, "head-A", dagReviewAt(5))
		f.mergedMark(relationshipID, dagReviewAt(6))
		f.observation("acceptance-A", dagReviewAt(10), true, "")
		f.close()
		review := dagReviewRunReview(t, f, nil, 0)
		if len(review.Plans) != 1 || review.Plans[0].Integrated != 1 {
			t.Fatalf("the plan integrated count = %+v, want 1", review.Plans)
		}
	})

	t.Run("a same-target observation resolves the turn", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewCanonPlan(t, f, "A")
		relationshipID := dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
		f.acceptanceHead("plan-1", "A", "acceptance-A", relationshipID, "head-A", dagReviewAt(5))
		f.observation("acceptance-A", dagReviewAt(30), true, "")
		f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "landed", relationshipID, 42, dagReviewAt(5), dagReviewAt(5), dagReviewAt(5))
		f.close()
		if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindLandedNotObserved); len(found) != 0 {
			t.Errorf("a same-target observation did not resolve the turn: %+v", found)
		}
	})

	t.Run("two nodes running on one exclusive place are reported", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewCanonPlan(t, f, "A", "B")
		f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
		f.release("plan-1", "B", "manifest-B", dagReviewAt(6))
		f.region("plan-1", "A", "shared.go", "file", "", "delete", true)
		f.region("plan-1", "B", "shared.go", "file", "", "edit", false)
		f.close()
		if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindExclusiveOverlapRunning); len(found) == 0 {
			t.Error("a real exclusive overlap in flight was not reported")
		}
	})
}
