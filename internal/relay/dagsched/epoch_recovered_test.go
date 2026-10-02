package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Restart reads the frozen request of the intent that is open, the successor's after a close and a release again, and not the one of the closed release: a replacement parent that released the node
// again is the parent of the intent it can replay, and the tombstone of a start the operator released names the command that ends it.
func TestRestartReadsTheFrozenRequestOfARecoveredIntent(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	first := k.sched
	k.claim(first, "rp", "parent", "session-1")
	digest, request := k.abandon("rp", "A")

	second := k.session("rp", "parent", "session-2")
	node, found := k.resumeOf(second, "rp", "parent", "A")
	if !found || node.Resume != ResumeNeedsOperator || !strings.Contains(node.ResumeDetail, "dag-release-close") || !strings.Contains(node.ResumeDetail, "--request-id") {
		t.Fatalf("restart of an abandoned release says %+v", node)
	}
	if _, err := second.CloseRelease(ctx, "rp", "A", "parent", digest, "the operator released the start", request); err != nil {
		t.Fatalf("close: %v", err)
	}

	// the project changes hands and the new parent releases the node again; its intent is the successor's, frozen with its own name as the parent
	k.replaceParent("parent-2")
	third := k.session("rp", "parent-2", "session-3")
	real := third.Start
	third.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, errCrash }
	if _, err := third.Release(ctx, "rp", "A", "parent-2", k.request(false)); !errors.Is(err, errCrash) {
		t.Fatalf("the release again did not reach the host: %v", err)
	}
	third.Start = real
	if node, found := k.resumeOf(third, "rp", "parent-2", "A"); !found || node.Resume != ResumeReconcile {
		t.Fatalf("restart says %+v: the intent of the release again names parent-2 and parent-2 can replay it", node)
	}
	res, err := third.Release(ctx, "rp", "A", "parent-2", k.request(false))
	if err != nil || !res.Bound || !res.Replayed || res.ChildTaskID != "child-1" {
		t.Fatalf("replay of the successor's intent = %+v, %v", res, err)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("%d children", created)
	}
}
