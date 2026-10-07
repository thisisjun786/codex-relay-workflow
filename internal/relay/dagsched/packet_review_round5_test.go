package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// criteriaBoundPacketExecution binds a node to the relationship that executes it, through a manifest that
// records the node version it consumed: its slice AND its criteria digest, as a real release does. The
// revalidation credit is judged against those two values (criteriaOnly), so the manifest must carry both.
func criteriaBoundPacketExecution(f *fixture, plan, node, relationship string) {
	var slice, criteria string
	for _, n := range f.snapshot(plan).Nodes {
		if n.NodeID == node {
			slice, criteria = n.SliceDigest, n.CriteriaSetDigest
		}
	}
	digest := "manifest-" + relationship
	body := `{"node_slice_digest":"` + slice + `","criteria_set_digest":"` + criteria + `"}`
	f.exec("INSERT INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES (?,?,?,'{}',0,'t')", digest, node, body)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,1,?,'initial',NULL)", plan, node, relationship, digest)
}

// coverageRevisedCriteriaDigest is the criteria digest a later criteria-only revision of the feature holds.
const coverageRevisedCriteriaDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// A criteria-only revision after a revalidation keeps the packet's credit: the output was re-judged under
// the new criteria and nothing else moved.
// A spec-only revision after that revalidation (here: the packet grows its covers) must end the credit,
// because the output was never judged for the new spec (CRW-839 generation 4 review, P1).
func TestRevalidatedCreditEndsWithASpecOnlyRevision(t *testing.T) {
	f := newFixture(t)
	criteria := []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true), criterionDoc("c2", false))}
	putPacketPlan(t, f, "plan", 0, "r1", criteria, packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	criteriaBoundPacketExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	f.exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES ('rel-1','plan','n1','CRW-F','p1',NULL,'t')")

	// r2: a criteria-only revision. The criteria digest moves, the spec does not; the output is revalidated.
	recriteria := packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"})["node"].(doc)
	recriteria["criteria_set_digest"] = coverageRevisedCriteriaDigest
	putPacketPlan(t, f, "plan", 1, "r2", criteria, doc{"op": dag.OpUpdateNode, "node": recriteria})
	f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv-1','acc-1',?,'ev','turn',1,'parent','t')", coverageRevisedCriteriaDigest)
	// the relationship's registered criteria are re-judged under the new digest too (a revalidation registers them again)
	f.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = 'rel-1'", coverageRevisedCriteriaDigest)
	if cov := coverageOf(t, f, "plan", "CRW-F"); !cov.Complete {
		t.Fatalf("a criteria-only revision dropped the revalidated packet: %+v", cov.Packets)
	}

	// r3: a spec-only revision after the revalidation. The packet now also covers c2.
	respec := packetNodeDoc("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1"})["node"].(doc)
	respec["criteria_set_digest"] = coverageRevisedCriteriaDigest
	putPacketPlan(t, f, "plan", 2, "r3", criteria, doc{"op": dag.OpUpdateNode, "node": respec})
	if cov := coverageOf(t, f, "plan", "CRW-F"); cov.Complete {
		t.Fatalf("a spec-only revision kept the revalidated credit: %+v", cov.Packets)
	}
}

// P2 (generation 4 review): a whole-repository hold is an overlap with every place a sibling packet takes,
// even when the sibling's path is disjoint from the holder's. Two packets, n2 owns pkg/a.go exclusively,
// then n1 declares the whole repository held: one owner each side of the place, so the declaration is refused.
func TestWholeRepositoryHoldOverlapsASiblingPacketPlace(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	twoPacketPlan(t, k.fixture, "rp")
	if _, err := k.sched.DeclareRegions(context.Background(), "rp", "n2", "parent",
		[]Region{{Repository: "owner/repo", Path: "pkg/a.go", Kind: "file", Change: "edit", Grade: GradeExclusive}}); err != nil {
		t.Fatalf("the owning packet's declaration was refused: %v", err)
	}
	if _, err := k.sched.DeclareRegions(context.Background(), "rp", "n1", "parent",
		[]Region{{Repository: "owner/repo", Path: "docs/unrelated.md", Kind: "file", Change: "edit", Exclusive: true}}); err == nil {
		t.Fatal("a whole-repository hold was accepted beside a sibling's owned file: the overlap has no single owner")
	}
}
