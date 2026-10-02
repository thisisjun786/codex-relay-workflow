package dagsched

import (
	"strings"
	"testing"
)

// CRW-281 meets the recovery of an abandoned release: a node the plan holds is never started again, whichever route the start takes, and the operator can still end an abandoned intent
// (closing starts nothing, so it is not work the plan stopped).

// resumeOf is the revision that lifts the hold of a case (a cancel has none: it is final).
func resumeOf(c lifecycleCase) doc {
	if c.reason == "defer:plan_paused" {
		return planOp("resume_plan")
	}
	return lifeOp("resume_node", "A")
}

// An abandoned release of a node the plan holds: the close is still allowed and returns the slot, the release that follows it is refused until the node resumes, and the resume releases it
// once, under the successor request.
func TestAnAbandonedReleaseOfAHeldNodeIsClosedButNotReleasedAgainUntilResumed(t *testing.T) {
	for _, c := range lifecycleCases() {
		if c.archived {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			digest, abandoned := k.abandon("rp", "A")
			l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
			l.put(c.change("A"))
			// the reading keeps what the operator must act on (the abandoned intent and the way out of it) and says the plan holds the node
			if n := k.read("rp").node("A"); n.Reason != BlockedReleaseAbandoned || !strings.Contains(n.Detail, "dag-release-close") || !strings.Contains(n.Detail, "a repeat of the release is refused") {
				t.Fatalf("the held node with an abandoned release reads %+v", n)
			}
			res := k.mustClose("rp", "A", digest)
			if res.RequestID != abandoned || !res.SlotReleased || k.heldSlots() != 0 {
				t.Fatalf("close = %+v, held slots %d", res, k.heldSlots())
			}
			if n := k.read("rp").node("A"); n.Reason != c.reason {
				t.Fatalf("after the close the held node reads %+v, want %s", n, c.reason)
			}
			// nothing is released again while the plan holds the node
			if _, err := k.release("rp", "A"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("the release after the close while held = %v, want disposition_conflict naming %s", err, c.reason)
			}
			if k.children() != 0 || k.heldSlots() != 0 || k.recoveries("rereleased") != 0 {
				t.Fatalf("a refused release left children %d, held slots %d, rereleased rows %d", k.children(), k.heldSlots(), k.recoveries("rereleased"))
			}
			if c.reason == "skip:node_cancelled" {
				return // a cancel is final: there is no resume
			}
			l.put(resumeOf(c))
			again := k.mustRelease("rp", "A")
			if !again.Bound || again.RequestID != RecoveryRequestID("rp", "A", digest, abandoned) || k.children() != 1 || k.recoveries("rereleased") != 1 {
				t.Fatalf("release after the resume = %+v, children %d, rereleased rows %d", again, k.children(), k.recoveries("rereleased"))
			}
		})
	}
}

// A recovered release (the successor request of a closed intent) whose child was never created is a continuation like any other: the hold stops the replay, the resume lets the same
// successor request continue, and exactly one child is created.
func TestARecoveredReleaseIsNotContinuedForAHeldNode(t *testing.T) {
	for _, c := range lifecycleCases() {
		if c.archived || c.reason == "skip:node_cancelled" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			digest, abandoned := k.abandon("rp", "A")
			k.mustClose("rp", "A", digest)
			k.host.loseFirstCreation = true
			if res, err := k.release("rp", "A"); err != nil || res.Bound {
				t.Fatalf("the recovered release = %v %+v", err, res)
			}
			successor := RecoveryRequestID("rp", "A", digest, abandoned)
			if k.recoveries("rereleased") != 1 {
				t.Fatalf("rereleased rows = %d", k.recoveries("rereleased"))
			}
			l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
			l.put(c.change("A"))
			_, sentBefore := k.host.counts()
			if _, err := k.release("rp", "A"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("the replay of the recovered release while held = %v", err)
			}
			if _, sent := k.host.counts(); sent != sentBefore || k.count("SELECT COUNT(*) FROM dag_node_executions") != 0 {
				t.Fatal("a child was started or bound for a held node")
			}
			l.put(resumeOf(c))
			res, err := k.release("rp", "A")
			if err != nil || !res.Bound || !res.Replayed || res.RequestID != successor {
				t.Fatalf("the replay after the resume = %v %+v, want the successor request %s", err, res, successor)
			}
			if k.children() != 1 {
				t.Fatalf("%d children were created for one recovered release", k.children())
			}
		})
	}
}
