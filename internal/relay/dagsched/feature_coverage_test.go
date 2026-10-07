package dagsched

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// dag-feature-coverage (CRW-839): the reading shows the packets, their acceptance and integration, and
// the packet that owns each criterion, and calls the feature complete only when every required criterion
// is covered by an integrated packet. A plan without packets answers as it always did.

// putPacketPlan writes a revision that declares a feature's criteria beside its changes.
func putPacketPlan(t *testing.T, f *fixture, plan string, parent int, request string, criteria []doc, changes ...doc) {
	t.Helper()
	cs := make([]any, len(changes))
	for i, c := range changes {
		cs[i] = c
	}
	d := doc{"schema": dag.SchemaRevision, "plan_id": plan, "project_key": "P-TEST", "request_id": request,
		"expected_parent_revision": parent, "author_task_id": "task-test", "changes": cs}
	if len(criteria) > 0 {
		fcs := make([]any, len(criteria))
		for i, c := range criteria {
			fcs[i] = c
		}
		d["feature_criteria"] = fcs
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := f.repo.Put(context.Background(), rev); err != nil {
		t.Fatalf("put: %v", err)
	}
}

func packetNodeDoc(id, issue, packet string, covers, owns []string) doc {
	n := nodeDoc(id, dag.NodeImplementation)
	n["issue_key"] = issue
	n["packet_id"] = packet
	cs := make([]any, len(covers))
	for i, s := range covers {
		cs[i] = s
	}
	n["covers"] = cs
	if len(owns) > 0 {
		os := make([]any, len(owns))
		for i, s := range owns {
			os[i] = s
		}
		n["owns"] = os
	}
	return doc{"op": dag.OpAddNode, "node": n}
}

func featureCriteriaDoc(issue string, criteria ...doc) doc {
	cs := make([]any, len(criteria))
	for i, c := range criteria {
		cs[i] = c
	}
	return doc{"issue_key": issue, "criteria": cs}
}

func criterionDoc(id string, required bool) doc { return doc{"id": id, "required": required} }

// packetExecution binds a node to the relationship that executes it.
func packetExecution(f *fixture, plan, node, relationship string) {
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,1,'m','initial',NULL)", plan, node, relationship)
}

// packetAcceptance records the active acceptance of a relationship's output.
func packetAcceptance(f *fixture, id, plan, node, relationship string) {
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, output_manifest_ref, evidence_digest, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, supersedes_acceptance_id, state) VALUES (?,?,?,'m',?,1,'ev','rev','c','verified','head-'||?,'owner/repo',7,NULL,NULL,'host','turn','{}','parent',0,'2026-10-02T00:00:00.000000+00:00',NULL,'active')", id, plan, node, relationship, relationship)
}

// packetLanded records a bundle that carried the relationship and landed.
func packetLanded(f *fixture, train, relationship, memberHead, landed string) {
	f.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES (?,?,?,?,?,?,?)", train, "tk", "owner/repo", "dev", "base", "leader", "t")
	f.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES (?,1,?,1,?,?)", train, "turn-"+train, relationship, memberHead)
	f.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES (?,1,'landed','parent',?,?)", train, `{"landedSha":"`+landed+`"}`, "t")
}

func coverageOf(t *testing.T, f *fixture, plan, issue string) FeatureCoverage {
	t.Helper()
	cov, err := f.sched.FeatureCoverage(context.Background(), plan, issue)
	if err != nil {
		t.Fatal(err)
	}
	return cov
}

// A feature with two packets reads incomplete while one of them is integrated, and complete once both are.
func TestFeatureCoverageNeedsEveryRequiredCriterionIntegrated(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1",
		[]doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true), criterionDoc("c2", true), criterionDoc("c3", false))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1", "c3"}, []string{"c1", "c3"}),
		packetNodeDoc("n2", "CRW-F", "p2", []string{"c2", "c3"}, nil))
	packetExecution(f, "plan", "n1", "rel-1")
	packetExecution(f, "plan", "n2", "rel-2")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-2", "plan", "n2", "rel-2")
	packetLanded(f, "train-1", "rel-1", "head-1", "landed-1")

	first := coverageOf(t, f, "plan", "CRW-F")
	if first.Complete {
		t.Fatalf("one of two packets integrated reads complete: %+v", first.Criteria)
	}
	if len(first.Packets) != 2 || first.Packets[0].PacketID != "p1" || first.Packets[0].Integration == nil || first.Packets[0].Integration.LandedSHA != "landed-1" {
		t.Fatalf("packets = %+v", first.Packets)
	}
	if first.Packets[1].Integration != nil {
		t.Fatalf("the second packet reads integrated: %+v", first.Packets[1])
	}
	// c3 is taken by both packets and owned by p1; the owner is named and the reading says so.
	byID := map[string]CoverageCriterion{}
	for _, c := range first.Criteria {
		byID[c.ID] = c
	}
	if got := byID["c3"]; got.Owner != "n1" || len(got.CoveredBy) != 2 {
		t.Fatalf("c3 = %+v, want n1 owning a criterion both packets take", got)
	}
	if got := byID["c2"]; got.Integrated || !got.Required {
		t.Fatalf("c2 = %+v, want a required criterion whose packet is not integrated", got)
	}

	packetLanded(f, "train-2", "rel-2", "head-2", "landed-2")
	second := coverageOf(t, f, "plan", "CRW-F")
	if !second.Complete {
		t.Fatalf("both packets integrated still reads incomplete: %+v", second.Criteria)
	}
}

// A plan without packets answers as it always did: the node is the feature's one packet and the criteria
// are the ones the relay registered for its relationship.
func TestFeatureCoverageSingleNodePlanAnswersAsToday(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", nil, addNode("solo", dag.NodeImplementation))
	packetExecution(f, "plan", "solo", "rel-solo")
	packetAcceptance(f, "acc-solo", "plan", "solo", "rel-solo")
	f.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, source_ref, set_digest, recorded_at) VALUES ('rel-solo','c1','one',1,NULL,'d','t')")

	before := coverageOf(t, f, "plan", "CRW-solo")
	if before.Complete {
		t.Fatalf("a feature whose output has not landed reads complete: %+v", before)
	}
	if len(before.Packets) != 1 || before.Packets[0].PacketID != "" || before.Packets[0].Acceptance == nil {
		t.Fatalf("packets = %+v", before.Packets)
	}
	if len(before.Criteria) != 1 || before.Criteria[0].ID != "c1" || !before.Criteria[0].Required || before.Criteria[0].Owner != "solo" {
		t.Fatalf("criteria = %+v", before.Criteria)
	}

	packetLanded(f, "train-solo", "rel-solo", "head-solo", "landed-solo")
	if after := coverageOf(t, f, "plan", "CRW-solo"); !after.Complete {
		t.Fatalf("the landed single node still reads incomplete: %+v", after.Criteria)
	}
}

// An issue the plan does not hold is the refusal unregistered_scope, as every read of a plan is.
func TestFeatureCoverageRefusesAnIssueThePlanDoesNotHold(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", nil, addNode("solo", dag.NodeImplementation))
	if _, err := f.sched.FeatureCoverage(context.Background(), "plan", "CRW-absent"); err == nil {
		t.Fatal("an issue with no node was answered")
	}
}
