package dagsched

import (
	"context"
	"encoding/json"
	"sort"
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
	// The plan fixes the criteria digest the release registered (coverageCriteriaDigest): the coverage
	// reading credits a packet only while the acceptance's effective criteria, the registration's and the
	// node's all agree (CRW-839 pre-merge d2).
	n["criteria_set_digest"] = coverageCriteriaDigest
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

// coverageCriteriaDigest is the criteria set every packet of these coverage tests is registered with and
// every acceptance stands on.
const coverageCriteriaDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// plainNodeDoc is a node with no packet identity whose plan fixes the same criteria digest.
func plainNodeDoc(id string) doc {
	n := nodeDoc(id, dag.NodeImplementation)
	n["criteria_set_digest"] = coverageCriteriaDigest
	return n
}

func featureCriteriaDoc(issue string, criteria ...doc) doc {
	cs := make([]any, len(criteria))
	for i, c := range criteria {
		cs[i] = c
	}
	return doc{"issue_key": issue, "criteria": cs}
}

func criterionDoc(id string, required bool) doc { return doc{"id": id, "required": required} }

// nodeSlice is the slice digest the plan holds for a node: the manifest of a current execution is built
// for exactly this.
func nodeSlice(f *fixture, plan, node string) string {
	for _, n := range f.snapshot(plan).Nodes {
		if n.NodeID == node {
			return n.SliceDigest
		}
	}
	f.t.Fatalf("no node %s in plan %s", node, plan)
	return ""
}

// packetExecution binds a node to the relationship that executes it, through a manifest built for the node
// version the plan holds (a node whose spec has moved since has no current execution).
func packetExecution(f *fixture, plan, node, relationship string) {
	digest := "manifest-" + relationship
	f.exec("INSERT INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES (?,?,?,'{}',0,'t')", digest, node, `{"node_slice_digest":"`+nodeSlice(f, plan, node)+`"}`)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,1,?,'initial',NULL)", plan, node, relationship, digest)
}

// liveRelationship records the live relationship a node's execution belongs to: the reading credits the
// execution of a relationship that is still active.
func liveRelationship(f *fixture, plan, node, relationship string) {
	issue := nodeIssue(f, plan, node)
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, parent_cwd, parent_cxc_session, child_task_id, child_host_id, child_cwd, child_cxc_session, execution_generation, artifact_roots, allowed_recipients, scope_ref, supersedes, superseded_by, created_at, updated_at) VALUES (?,?,'active','parent','host','/p','cxc',?,'host','/c','cxc',1,'[]','[]','scope',NULL,NULL,'t','t')", relationship, issue, "child-"+relationship)
}

func nodeIssue(f *fixture, plan, node string) string {
	for _, n := range f.snapshot(plan).Nodes {
		if n.NodeID == node {
			return n.IssueKey
		}
	}
	f.t.Fatalf("no node %s in plan %s", node, plan)
	return ""
}

// acceptanceHead is the head a packet's acceptance stands on, which its landing must match.
func acceptanceHead(relationship string) string { return "head-" + relationship }

// packetAcceptance records the active acceptance of a relationship's output.
func packetAcceptance(f *fixture, id, plan, node, relationship string) {
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, output_manifest_ref, evidence_digest, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, supersedes_acceptance_id, state) VALUES (?,?,?,'m',?,1,'ev','rev',?,'verified',?,'owner/repo',7,NULL,NULL,'host','turn','{}','parent',0,'2026-10-02T00:00:00.000000+00:00',NULL,'active')", id, plan, node, relationship, coverageCriteriaDigest, acceptanceHead(relationship))
}

// packetLanded records a bundle that carried the relationship and landed.
func packetLanded(f *fixture, train, relationship, memberHead, landed string) {
	f.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES (?,?,?,?,?,?,?)", train, "tk", "owner/repo", "dev", "base", "leader", "t")
	f.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES (?,1,?,1,?,?)", train, "turn-"+train, relationship, memberHead)
	// The landed event carries its OWN members mapping, which is what the coverage reader credits a member
	// by (CRW-839 d3): the mapping and not the train's member rows, so a member the bundle excluded is not read.
	detail := `{"landedSha":"` + landed + `","members":[{"seq":1,"relationshipId":"` + relationship + `","memberHead":"` + memberHead + `"}]}`
	f.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES (?,1,'landed','parent',?,?)", train, detail, "2026-10-02T00:02:00.000000+00:00")
}

// packetIntegrated records the integration evidence the scheduler's own predicate needs (CRW-839 d2): a
// positive ancestry observation of the packet's accepted head in a target - which is also what makes the
// target required - and the parent's merged mark on the same acceptance event, generation and revision.
func packetIntegrated(f *fixture, acceptance, relationship, repository, baseRef string) {
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, observed_at) VALUES (?,?,?,?,?, 'tip', 1, 'git merge-base --is-ancestor', 1, ?)",
		"obs-"+relationship+"-"+baseRef, acceptance, repository, baseRef, acceptanceHead(relationship), "2026-10-02T00:01:00.000000+00:00")
	f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', 'ev', 1, 'rev', 'merged', 'parent', '2026-10-02T00:01:00.000000+00:00')", relationship)
}

// packetRegistered records the criteria a packet's relationship registered, with the required flag the
// coverage reader compares against the feature's declaration (CRW-839 d4).
func packetRegistered(f *fixture, relationship string, required map[string]bool) {
	ids := make([]string, 0, len(required))
	for id := range required {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ok := required[id]
		n := 0
		if ok {
			n = 1
		}
		f.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, source_ref, set_digest, recorded_at) VALUES (?,?,?,?,NULL,?,'t')", relationship, id, id, n, coverageCriteriaDigest)
	}
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
	liveRelationship(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n2", "rel-2")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-2", "plan", "n2", "rel-2")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true, "c3": true})
	packetRegistered(f, "rel-2", map[string]bool{"c2": true, "c3": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	packetLanded(f, "train-1", "rel-1", acceptanceHead("rel-1"), "landed-1")

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

	packetLanded(f, "train-2", "rel-2", acceptanceHead("rel-2"), "landed-2")
	packetIntegrated(f, "acc-2", "rel-2", "owner/repo", "dev")
	second := coverageOf(t, f, "plan", "CRW-F")
	if !second.Complete {
		t.Fatalf("both packets integrated still reads incomplete: %+v", second.Criteria)
	}
}

// A plan without packets answers as it always did: the node is the feature's one packet and the criteria
// are the ones the relay registered for its relationship.
func TestFeatureCoverageSingleNodePlanAnswersAsToday(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", nil, doc{"op": dag.OpAddNode, "node": plainNodeDoc("solo")})
	packetExecution(f, "plan", "solo", "rel-solo")
	liveRelationship(f, "plan", "solo", "rel-solo")
	packetAcceptance(f, "acc-solo", "plan", "solo", "rel-solo")
	f.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, source_ref, set_digest, recorded_at) VALUES ('rel-solo','c1','one',1,NULL,?,'t')", coverageCriteriaDigest)

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

	packetLanded(f, "train-solo", "rel-solo", acceptanceHead("rel-solo"), "landed-solo")
	packetIntegrated(f, "acc-solo", "rel-solo", "owner/repo", "dev")
	if after := coverageOf(t, f, "plan", "CRW-solo"); !after.Complete {
		t.Fatalf("the landed single node still reads incomplete: %+v", after.Criteria)
	}
}

// An issue the plan does not hold is the refusal unregistered_scope, as every read of a plan is.
// A packet the plan has moved past credits nothing: the node's spec changed after it was released, so the
// execution that consumed the old spec is not the packet's current one.
func TestFeatureCoverageCreditsNothingForAnExecutionThePlanHasMovedPast(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	packetLanded(f, "train-1", "rel-1", acceptanceHead("rel-1"), "landed-1")
	if cov := coverageOf(t, f, "plan", "CRW-F"); !cov.Complete {
		t.Fatalf("the released packet reads incomplete: %+v", cov)
	}

	// The packet now covers another criterion, so the node gets a new version and the old execution is stale.
	putPacketPlan(t, f, "plan", 1, "r2",
		[]doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true), criterionDoc("c2", true))},
		doc{"op": dag.OpUpdateNode, "node": packetNodeDoc("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"})["node"]})
	after := coverageOf(t, f, "plan", "CRW-F")
	if after.Complete {
		t.Fatalf("a packet whose spec moved still reads complete: %+v", after)
	}
	if len(after.Packets) != 1 || after.Packets[0].RelationshipID != "" || after.Packets[0].Integration != nil {
		t.Fatalf("the moved packet still credits its old execution: %+v", after.Packets)
	}
}

// A landing of an earlier head does not credit a later acceptance of the same relationship.
func TestFeatureCoverageIgnoresALandingOfAnEarlierHead(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	// The bundle carried the head the node had BEFORE this acceptance.
	packetLanded(f, "train-old", "rel-1", "head-before", "landed-before")
	cov := coverageOf(t, f, "plan", "CRW-F")
	if cov.Complete {
		t.Fatalf("a landing of an earlier head completed the feature: %+v", cov.Packets)
	}
	if len(cov.Packets) != 1 || cov.Packets[0].Integration != nil {
		t.Fatalf("the earlier landing was credited: %+v", cov.Packets)
	}
}

func TestFeatureCoverageRefusesAnIssueThePlanDoesNotHold(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", nil, addNode("solo", dag.NodeImplementation))
	if _, err := f.sched.FeatureCoverage(context.Background(), "plan", "CRW-absent"); err == nil {
		t.Fatal("an issue with no node was answered")
	}
}
