package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

func (k *releaseKit) decide(in DecisionInput) (DecisionResult, error) {
	k.t.Helper()
	return k.sched.RecordDecision(context.Background(), "dp", "parent", in)
}

// A decision edge opens only on a recorded decision with the digest the plan fixed, by an authority the edge names (authority text opaque, D-09). Recording is not authority.
func TestDecisionEdges(t *testing.T) {
	k := newReleaseKit(t)
	k.putPlan("dp", 0, "dp-r1", addRelNode("D", dag.NodeNonPR), addRelNode("L", dag.NodeNonPR), addEdge("dl", "D", "L", dag.EdgeDecision, nil))
	subject, digest := "merge holds", dig("subject dl")
	if n := k.read("dp").node("L"); n.Reason != DeferAuthorityPending {
		t.Fatalf("L = %+v", n)
	}
	// the wrong authority records and opens nothing
	first, err := k.decide(DecisionInput{Subject: subject, Digest: digest, Disposition: "approved", AuthorityKind: "intern", AuthorityRef: "ref-1"})
	if err != nil || first.Revision != 1 || first.Replayed {
		t.Fatalf("first = %v %+v", err, first)
	}
	if n := k.read("dp").node("L"); n.Reason != BlockedDecisionMismatch {
		t.Fatalf("L after a decision by the wrong authority = %+v", n)
	}
	// the right authority supersedes it and opens the edge
	second, err := k.decide(DecisionInput{Subject: subject, Digest: digest, Disposition: "approved", AuthorityKind: "owner", AuthorityRef: "ref-2"})
	if err != nil || second.Revision != 2 || second.SupersededID != first.DecisionID || second.DecisionID == first.DecisionID {
		t.Fatalf("second = %v %+v", err, second)
	}
	if n := k.read("dp").node("L"); n.Disposition != DispReady {
		t.Fatalf("L = %+v", n)
	}
	if again, err := k.decide(DecisionInput{Subject: subject, Digest: digest, Disposition: "approved", AuthorityKind: "owner", AuthorityRef: "ref-2"}); err != nil || !again.Replayed || again.DecisionID != second.DecisionID || again.Revision != 2 {
		t.Fatalf("repeat = %v %+v", err, again)
	}
	// another digest than the plan fixed records and does not open
	if _, err := k.decide(DecisionInput{Subject: subject, Digest: dig("another digest"), Disposition: "approved", AuthorityKind: "owner", AuthorityRef: "ref-3"}); err != nil {
		t.Fatal(err)
	}
	if n := k.read("dp").node("L"); n.Reason != BlockedDecisionMismatch {
		t.Fatalf("L after a decision of another digest = %+v", n)
	}
	// a rejection after an approval is a fact: the history keeps all of them
	if _, err := k.decide(DecisionInput{Subject: subject, Digest: digest, Disposition: "rejected", AuthorityKind: "owner", AuthorityRef: "ref-4"}); err != nil {
		t.Fatal(err)
	}
	if n := k.read("dp").node("L"); n.Reason != DeferAuthorityPending {
		t.Fatalf("L after a rejection = %+v", n)
	}
	if k.count("SELECT COUNT(*) FROM dag_decisions") != 4 || k.count("SELECT COUNT(*) FROM dag_decisions WHERE state = 'active'") != 1 {
		t.Fatalf("history: %d rows, %d active", k.count("SELECT COUNT(*) FROM dag_decisions"), k.count("SELECT COUNT(*) FROM dag_decisions WHERE state = 'active'"))
	}
}

func TestRecordDecisionRefusals(t *testing.T) {
	k := newReleaseKit(t)
	k.putPlan("dp", 0, "dp-r1", addRelNode("D", dag.NodeNonPR), addRelNode("L", dag.NodeNonPR), addEdge("dl", "D", "L", dag.EdgeDecision, nil))
	ok := DecisionInput{Subject: "merge holds", Digest: dig("x"), Disposition: "approved", AuthorityKind: "owner", AuthorityRef: "r"}
	for name, c := range map[string]struct {
		actor  string
		plan   string
		mutate func(in *DecisionInput)
		reason string
	}{
		"another task":       {actor: "intruder", plan: "dp", mutate: func(in *DecisionInput) {}, reason: "scope_role_mismatch"},
		"an unknown plan":    {actor: "parent", plan: "nope", mutate: func(in *DecisionInput) {}, reason: "unregistered_scope"},
		"no subject":         {actor: "parent", plan: "dp", mutate: func(in *DecisionInput) { in.Subject = "" }, reason: "malformed_receipt"},
		"no authority":       {actor: "parent", plan: "dp", mutate: func(in *DecisionInput) { in.AuthorityRef = "" }, reason: "malformed_receipt"},
		"an odd disposition": {actor: "parent", plan: "dp", mutate: func(in *DecisionInput) { in.Disposition = "chosen" }, reason: "malformed_receipt"},
		"a padded authority": {actor: "parent", plan: "dp", mutate: func(in *DecisionInput) { in.AuthorityKind = " owner" }, reason: "malformed_receipt"},
	} {
		in := ok
		c.mutate(&in)
		_, err := k.sched.RecordDecision(context.Background(), c.plan, c.actor, in)
		if refusalReason(err) != c.reason {
			t.Errorf("%s: %v, want %s", name, err, c.reason)
		}
	}
	if k.count("SELECT COUNT(*) FROM dag_decisions") != 0 {
		t.Fatal("a refused decision wrote a row")
	}
}
