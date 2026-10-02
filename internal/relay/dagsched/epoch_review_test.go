package dagsched

import (
	"context"
	"reflect"
	"testing"
)

// Findings of the review of the first version of the epoch: what a retried claim says, and what restart prescribes for a merge whose effect is unknown.

// A session that retries its claim after losing the response is told what the first answer told it: the same epoch, and the same claim it replaced.
func TestAClaimReplayNamesTheClaimItReplaced(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	first, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-1"}); err != nil || !again.Replayed {
		t.Fatalf("replay of the first claim = %+v, %v", again, err)
	} else if again.PreviousEpoch != 0 || again.PreviousTask != "" {
		t.Fatalf("the first claim replaced nothing, its replay says epoch %d, task %q", again.PreviousEpoch, again.PreviousTask)
	}
	second, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-2"})
	if err != nil || second.PreviousEpoch != first.Epoch || second.PreviousTask != "parent" {
		t.Fatalf("second claim = %+v, %v", second, err)
	}
	again, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-2"})
	if err != nil || !again.Replayed {
		t.Fatalf("replay of the second claim = %+v, %v", again, err)
	}
	// the printed answer of the replay is the printed answer of the first call, but for the replayed flag
	replayed := again
	replayed.Replayed = false
	if !reflect.DeepEqual(second.Object(), replayed.Object()) {
		t.Fatalf("the replay answers differently:\n first  %v\n replay %v", second.Object(), again.Object())
	}
	// a replacement parent's claim names the previous parent task, and its replay too
	k.replaceParent("parent-2")
	third, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent-2", SessionNonce: "session-3"})
	if err != nil || third.PreviousTask != "parent" || third.PreviousEpoch != 2 {
		t.Fatalf("replacement claim = %+v, %v", third, err)
	}
	again, err = k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent-2", SessionNonce: "session-3"})
	if err != nil || !again.Replayed || again.PreviousTask != "parent" || again.PreviousEpoch != 2 {
		t.Fatalf("replay of the replacement claim = %+v, %v", again, err)
	}
}

// A merge turn whose effect is unknown is reconciled by observing where the head is, and only the parent of the accepted result's relationship can observe. A parent that holds the project and
// the epoch but not that relationship is told it needs an operator, not to run a command that refuses it.
func TestRestartDoesNotPrescribeAnObservationItsActorCannotMake(t *testing.T) {
	ctx := context.Background()
	k := newJudgeKit(t)
	k.claim(k.sched, "g", "parent", "session-1")
	if _, turn, err := k.sched.RequestMergeTurn(ctx, "g", "I", "parent", MergeRequestInput{Host: "host"}); err != nil || turn == nil {
		t.Fatalf("request = %v, %v", turn, err)
	}
	k.exec("UPDATE merge_turns SET state = 'unknown'")
	// the holder of the relationship reconciles
	if node, found := k.resumeOf(k.sched, "g", "parent", "I"); !found || node.Reason != BlockedEffectUnknown || node.Resume != ResumeReconcile {
		t.Fatalf("restart for the relationship's parent says %+v (found %v)", node, found)
	}
	// the project passes to another parent, who claims the epoch; the relationship stays where it was
	k.replaceParent("parent-2")
	second := k.session("g", "parent-2", "session-2")
	if _, err := second.ObserveIntegration(ctx, "g", "I", "parent-2", []Target{{Repository: k.repo.path, BaseRef: "dev"}}); refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("the premise: the replacement parent observing the landing = %v", err)
	}
	node, found := k.resumeOf(second, "g", "parent-2", "I")
	if !found || node.Reason != BlockedEffectUnknown {
		t.Fatalf("restart says %+v (found %v)", node, found)
	}
	if node.Resume != ResumeNeedsOperator {
		t.Fatalf("restart prescribes %q for an actor that cannot observe: %+v", node.Resume, node)
	}
}
