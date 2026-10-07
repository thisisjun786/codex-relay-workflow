package manage

import "testing"

// A node the plan holds before any release (paused, cancelled or archived by a revision) has never
// been released, so it is not in the plan's released count and does not hold its declared regions.
// The stage alone cannot tell such a node from one whose relationship was paused after a release;
// the plan's own reading carries the release evidence (a relationship, an execution, or the managed
// start of a release) that can.
func TestDagReviewNodeThePlanHoldsBeforeReleaseIsNotReleasedOrInFlight(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewCanonPlan(t, f, "A", "B")
	// Revision 2 pauses B, which has never been released. Both declare the same exclusive place.
	f.revision("plan-1", 2, dagReviewAt(1))
	f.pauseNode("plan-1", "B", 2)
	f.release("plan-1", "A", "manifest-A", dagReviewAt(5))
	f.region("plan-1", "A", "shared.go", "file", "", "delete", true)
	f.region("plan-1", "B", "shared.go", "file", "", "edit", false)
	f.close()

	review := dagReviewRunReview(t, f, nil, 0)
	if len(review.Plans) != 1 {
		t.Fatalf("the review did not read the plan: %+v", review.Plans)
	}
	if review.Plans[0].Released != 1 {
		t.Errorf("the released count = %d, want 1: a node the plan holds before any release is not released", review.Plans[0].Released)
	}
	if found := dagReviewFind(review, dagReviewKindExclusiveOverlapRunning); len(found) != 0 {
		t.Errorf("a node the plan holds before any release was reported in flight: %+v", found)
	}
}

// The control: a node whose relationship the relay paused was released, so it still holds its
// regions and reads the paused stage with a relationship link beside it.
func TestDagReviewPausedRelationshipStillHoldsItsRegions(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewCanonPlan(t, f, "A", "B")
	dagReviewCanonObserved(f, "A", dagReviewExecutionInitial, 1)
	paused := dagReviewCanonObserved(f, "B", dagReviewExecutionInitial, 1)
	f.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", paused)
	f.region("plan-1", "A", "shared.go", "file", "", "delete", true)
	f.region("plan-1", "B", "shared.go", "file", "", "edit", false)
	f.close()

	review := dagReviewRunReview(t, f, nil, 0)
	if review.Plans[0].Released != 2 {
		t.Errorf("the released count = %d, want 2: both nodes were released", review.Plans[0].Released)
	}
	if found := dagReviewFind(review, dagReviewKindExclusiveOverlapRunning); len(found) == 0 {
		t.Error("a node whose relationship is paused was not reported in flight")
	}
}
