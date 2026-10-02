package dagsched

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// Contract 8.2: a stale result or stale criteria never opens an edge. After a node's acceptance each change below must close what the acceptance opened (or, for an archived relationship,
// leave the durable acceptance standing), with the reason the contract names.
func TestStaleResultNeverOpensAnEdge(t *testing.T) {
	cases := []struct {
		name   string
		reason string // "" = still satisfied
		mutate func(k *releaseKit, a accepted)
	}{
		{name: "baseline", reason: ""},
		{name: "a newer revision supersedes the accepted one", reason: BlockedStaleHead, mutate: func(k *releaseKit, a accepted) { k.supersedeReport(a.Acceptance.RelationshipID, "A", "rp", "newer") }},
		{name: "the criteria were registered again", reason: BlockedStaleCriteria, mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE canonical_criteria SET set_digest = ?", dig("registered again"))
		}},
		{name: "a new generation opened", reason: BlockedStaleHead, mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE relationships SET execution_generation = 2")
		}},
		{name: "the relationship ended (archived): the durable acceptance stands", reason: "", mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE relationships SET status = 'archived'")
		}},
		{name: "the predecessor was cancelled", reason: BlockedPredecessorCancelled, mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE relationships SET status = 'cancelled'")
		}},
		{name: "the plan changed the node's criteria", reason: BlockedStaleCriteria, mutate: func(k *releaseKit, a accepted) {
			n := relNode("A", dag.NodeNonPR)
			n["criteria_set_digest"] = dig("another plan criterion")
			k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", doc{"op": dag.OpUpdateNode, "node": n})
		}},
		{name: "the acceptance was superseded", reason: WaitEdge("ab"), mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE dag_acceptances SET state = 'superseded'")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.reportNode("rp", "A", acceptOpts{})
			res, err := k.accept("rp", "A", AcceptInput{})
			if err != nil {
				t.Fatal(err)
			}
			if st := k.status("rp", "ab"); !st.Satisfied || st.AcceptanceID != res.AcceptanceID {
				t.Fatalf("the edge right after the acceptance = %+v", st)
			}
			a := accepted{Acceptance: Acceptance{RelationshipID: res.RelationshipID}}
			if c.mutate != nil {
				c.mutate(k, a)
			}
			st := k.status("rp", "ab")
			if c.reason == "" && !st.Satisfied {
				t.Fatalf("the edge = %+v, want it still satisfied", st)
			}
			if c.reason != "" && (st.Satisfied || st.Reason != c.reason) {
				t.Fatalf("the edge = %+v, want %s", st, c.reason)
			}
		})
	}
}
