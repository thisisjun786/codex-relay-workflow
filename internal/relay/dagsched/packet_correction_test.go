package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The correction findings of CRW-839 generation 4 (d2, d3, d6, d7): each test below fails without its fix
// and passes with it, on temporary stores only.

// d2: a bare positive ancestry observation is not integration. The scheduler's own predicate needs the
// merged mark on the same acceptance, so coverage must not credit an observation alone.
func TestCoverageDoesNotCreditAnObservationWithoutTheMergedMark(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	// the observation alone: no assignment_marks row
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, observed_at) VALUES ('obs-only',?, 'owner/repo','dev',?, 'tip', 1, 'git merge-base --is-ancestor', 1, 't')", "acc-1", acceptanceHead("rel-1"))

	cov := coverageOf(t, f, "plan", "CRW-F")
	if cov.Complete {
		t.Fatalf("an observation without the merged mark completed the feature: %+v", cov.Criteria)
	}
	if cov.Packets[0].Integration != nil {
		t.Fatalf("an observation without the merged mark was credited as integration: %+v", cov.Packets[0].Integration)
	}
}

// d3: train coverage reads the LANDED event's own members mapping, so a member the bundle excluded is not
// credited even though the train's member rows still name it.
func TestCoverageDoesNotCreditAMemberTheLandedBundleExcluded(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	// the train's member row names the relationship, but the landed event's mapping excludes it
	f.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES ('train-x','tk','owner/repo','dev','base','leader','t')")
	f.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES ('train-x',1,'turn-x',1,?,?)", "rel-1", acceptanceHead("rel-1"))
	f.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES ('train-x',1,'landed','parent',?,'t')",
		"{\"landedSha\":\"landed-x\",\"members\":[{\"seq\":1,\"relationshipId\":\"rel-other\",\"memberHead\":\"head-other\"}]}")

	cov := coverageOf(t, f, "plan", "CRW-F")
	if cov.Packets[0].Integration == nil {
		t.Fatalf("the direct observation was not credited: %+v", cov.Packets[0])
	}
	if cov.Packets[0].Integration.Method != "integration_observation" {
		t.Fatalf("the excluded member was credited from the bundle: %+v", cov.Packets[0].Integration)
	}
}

// d6: a packet whose relationship was archived after its merge keeps its acceptance and integration, so a
// completed feature does not read incomplete after ordinary cleanup.
func TestCoverageKeepsCreditForAPacketClosedAfterItsMerge(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	// the release recorded the packet this execution stands for
	f.exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES ('rel-1','plan','n1','CRW-F','p1',NULL,'t')")
	if cov := coverageOf(t, f, "plan", "CRW-F"); !cov.Complete {
		t.Fatalf("the landed packet reads incomplete: %+v", cov.Criteria)
	}

	// relationship-close-merged archives a settled merged relationship and keeps its acceptance.
	f.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = 'rel-1'")
	after := coverageOf(t, f, "plan", "CRW-F")
	if !after.Complete {
		t.Fatalf("cleanup erased the credit of a landed packet: %+v", after.Criteria)
	}
	if after.Packets[0].Integration == nil {
		t.Fatalf("the archived packet's integration was dropped: %+v", after.Packets[0])
	}
}

// d6, the other half: a relationship that was superseded or abandoned still counts for nothing.
func TestCoverageCreditsNothingForASupersededExecution(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	f.exec("UPDATE relationships SET status = 'archived', superseded_by = 'rel-2' WHERE relationship_id = 'rel-1'")
	if cov := coverageOf(t, f, "plan", "CRW-F"); cov.Complete {
		t.Fatalf("a superseded execution was credited: %+v", cov.Packets)
	}
}

// d7: two packets of one issue must not take an overlapping edit region without a declared owner.
func TestPacketRegionOwnerIsRequired(t *testing.T) {
	t.Parallel()
	t.Run("two packets, the same file, neither exclusive", func(t *testing.T) {
		k := newReleaseKit(t)
		twoPacketPlan(t, k.fixture, "rp")
		// n2's declaration replaces the disjoint one twoPacketPlan made, so both packets take the file.
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", "n2", "parent",
			[]Region{{Repository: "owner/repo", Path: "packet-one.go", Kind: "file", Change: "edit"}}); err == nil {
			t.Fatal("two packets of one issue took one file with no owner and the declaration was accepted")
		} else if !strings.Contains(err.Error(), "owner") {
			t.Fatalf("refusal = %v, want it to name the owner", err)
		}
	})
	t.Run("one packet owns the shared place, the other does not", func(t *testing.T) {
		k := newReleaseKit(t)
		twoPacketPlan(t, k.fixture, "rp")
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", "n1", "parent",
			[]Region{{Repository: "owner/repo", Path: "shared.go", Kind: "file", Change: "edit", Grade: GradeExclusive}}); err != nil {
			t.Fatalf("the owning packet's declaration was refused: %v", err)
		}
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", "n2", "parent",
			[]Region{{Repository: "owner/repo", Path: "shared.go", Kind: "file", Change: "edit"}}); err != nil {
			t.Fatalf("a declared owner did not settle the overlap: %v", err)
		}
	})
}

// d5 of the pre-merge evaluation: three packets may share one place as long as exactly one of them owns
// it. The judgement is over all the packets that take the place, not over each pair, so the second
// non-owner is not refused for overlapping the first non-owner.
func TestThreePacketsMayShareOneOwnedPlace(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	putPacketReleasePlan(t, k.fixture, "rp", 0, "rp-r1", []doc{declaredCriteriaDoc("CRW-F", "c1", "c2", "c3")},
		packetRelNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}),
		packetRelNode("n2", "CRW-F", "p2", []string{"c2"}, []string{"c2"}),
		packetRelNode("n3", "CRW-F", "p3", []string{"c3"}, []string{"c3"}))
	shared := func(node string, grade string) {
		t.Helper()
		r := Region{Repository: "owner/repo", Path: "shared.go", Kind: "file", Change: "edit", Grade: grade}
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", node, "parent", []Region{r}); err != nil {
			t.Fatalf("declare %s: %v", node, err)
		}
	}
	shared("n1", GradeExclusive)
	shared("n2", GradeIndependent)
	shared("n3", GradeIndependent)
}

// d3 of the pre-merge evaluation: the ownerless overlap a revision creates by turning two nodes that
// already declared a shared place into two packets of one issue must be refused by PLAN VALIDATION, not
// only later at release, so no invalid plan is committed.
func TestPacketRegionOwnerIsRequiredAtPlanWrite(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	// Two ordinary nodes of DIFFERENT issues, so the packet rule has nothing to say when they declare.
	plain := func(id, issue string) doc {
		n := relNode(id, dag.NodeImplementation)
		n["issue_key"] = issue
		return doc{"op": dag.OpAddNode, "node": n}
	}
	putPacketReleasePlan(t, k.fixture, "rp", 0, "rp-r1", nil, plain("n1", "CRW-F"), plain("n2", "CRW-G"))
	for _, node := range []string{"n1", "n2"} {
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", node, "parent",
			[]Region{{Repository: "owner/repo", Path: "shared.go", Kind: "file", Change: "edit"}}); err != nil {
			t.Fatalf("declare %s: %v", node, err)
		}
	}
	// Now one issue, two packets, and neither of them owns the shared place.
	if err := putPacketPlanErr(t, k.fixture, "rp", 1, "rp-r2", []doc{declaredCriteriaDoc("CRW-F", "c1", "c2")},
		doc{"op": dag.OpUpdateNode, "node": packetRelNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"})["node"]},
		doc{"op": dag.OpUpdateNode, "node": packetRelNode("n2", "CRW-F", "p2", []string{"c2"}, []string{"c2"})["node"]}); err == nil {
		t.Fatal("a plan that leaves two packets of one issue an ownerless shared place was committed")
	} else if !strings.Contains(err.Error(), "owner") {
		t.Fatalf("refusal = %v, want it to name the owner", err)
	}
}

// d4 of the pre-merge evaluation: the owner judgement is per PLACE. A broad declaration that covers
// several files touches as many places, and each is owned separately, so two packets that each own a
// different file under one tree are not two owners of one place.
func TestRegionOwnerIsJudgedPerPlace(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	putPacketReleasePlan(t, k.fixture, "rp", 0, "rp-r1", []doc{declaredCriteriaDoc("CRW-F", "c1", "c2", "c3")},
		packetRelNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}),
		packetRelNode("n2", "CRW-F", "p2", []string{"c2"}, []string{"c2"}),
		packetRelNode("n3", "CRW-F", "p3", []string{"c3"}, []string{"c3"}))
	declare := func(node string, r Region) {
		t.Helper()
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", node, "parent", []Region{r}); err != nil {
			t.Fatalf("declare %s: %v", node, err)
		}
	}
	declare("n2", Region{Repository: "owner/repo", Path: "pkg/a.go", Kind: "file", Change: "edit", Grade: GradeExclusive})
	declare("n3", Region{Repository: "owner/repo", Path: "pkg/b.go", Kind: "file", Change: "edit", Grade: GradeExclusive})
	// The broad declaration comes last, and the two files it covers have different owners: the tree is one
	// place, each file another, and neither file's owner is the other's.
	declare("n1", Region{Repository: "owner/repo", Path: "pkg", Kind: "tree", Change: "edit"})
}

// d2 of the pre-merge evaluation: a criteria-only revision moves the node's slice without moving the
// accepted output, and dag-accept revalidates the acceptance under the new criteria. The revalidated
// packet keeps its acceptance and integration in coverage instead of dropping out of the reading.
func TestCoverageKeepsARevalidatedPacket(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	// the release recorded the packet this execution stands for
	f.exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES ('rel-1','plan','n1','CRW-F','p1',NULL,'t')")
	if cov := coverageOf(t, f, "plan", "CRW-F"); !cov.Complete {
		t.Fatalf("the landed packet reads incomplete: %+v", cov.Criteria)
	}

	// A criteria-only revision: the node's SLICE moves (a new title is part of the node's spec), the
	// accepted output does not, and the acceptance is revalidated under the new criteria. The packet
	// identity and the covers are unchanged, which is what the revalidation fallback must require.
	updated := packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"})["node"].(doc)
	updated["title"] = "criteria-only revision"
	putPacketPlan(t, f, "plan", 1, "r2", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		doc{"op": dag.OpUpdateNode, "node": updated})
	f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv-1','acc-1',?,'ev','turn',1,'parent','t')", coverageCriteriaDigest)
	after := coverageOf(t, f, "plan", "CRW-F")
	if !after.Complete {
		t.Fatalf("a revalidated packet was dropped from coverage: %+v", after.Packets)
	}
	if after.Packets[0].RelationshipID != "rel-1" || after.Packets[0].Integration == nil {
		t.Fatalf("the revalidated packet lost its execution: %+v", after.Packets[0])
	}
}

// The other half of d2: a revalidation does not let a node whose PACKET IDENTITY moved keep crediting
// the execution of the packet it used to be.
func TestCoverageDoesNotCreditARevalidatedExecutionOfAnotherPacket(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true), criterionDoc("c2", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1", "c2"}, []string{"c1", "c2"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true, "c2": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	// the execution is recorded under packet p1
	f.exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES ('rel-1','plan','n1','CRW-F','p1',NULL,'t')")
	if cov := coverageOf(t, f, "plan", "CRW-F"); !cov.Complete {
		t.Fatalf("the landed packet reads incomplete: %+v", cov.Criteria)
	}

	// The node becomes another packet and the acceptance is revalidated, so the revalidation fallback
	// would credit p2 with p1's execution and landing. The packet identity check refuses that.
	putPacketPlan(t, f, "plan", 1, "r2", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true), criterionDoc("c2", true))},
		doc{"op": dag.OpUpdateNode, "node": packetNodeDoc("n1", "CRW-F", "p2", []string{"c1", "c2"}, []string{"c1", "c2"})["node"]})
	f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv-2','acc-1',?,'ev','turn',1,'parent','t')", coverageCriteriaDigest)
	after := coverageOf(t, f, "plan", "CRW-F")
	if after.Complete {
		t.Fatalf("a revalidated execution of another packet was credited: %+v", after.Packets)
	}
	if after.Packets[0].RelationshipID != "" || after.Packets[0].Integration != nil {
		t.Fatalf("the moved packet kept the old execution: %+v", after.Packets[0])
	}
}

// d3 of the second pre-merge round: a lifecycle change does not erase the credit of a landed packet, and
// an issue whose every node carries a lifecycle word is still the feature's packet set.
func TestCoverageKeepsCreditForALandedPausedOrArchivedNode(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1", []doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}))
	packetExecution(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	if cov := coverageOf(t, f, "plan", "CRW-F"); !cov.Complete {
		t.Fatalf("the landed packet reads incomplete: %+v", cov.Criteria)
	}
	// The node is archived: the plan keeps it, its slice and its landing, so the feature stays complete.
	putPacketPlan(t, f, "plan", 1, "r2", nil, doc{"op": dag.OpArchiveNode, "node_id": "n1"})
	after := coverageOf(t, f, "plan", "CRW-F")
	if !after.Complete {
		t.Fatalf("archiving a landed packet erased its credit: %+v", after.Criteria)
	}
	if len(after.Packets) != 1 || after.Packets[0].Integration == nil {
		t.Fatalf("the archived packet is gone from the reading: %+v", after.Packets)
	}
}

// d4 of the second pre-merge round: two different SYMBOLS of one file are different places, so a packet
// that touches both does not make their two owners owners of ONE place.
func TestRegionOwnerDistinguishesSymbols(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	putPacketReleasePlan(t, k.fixture, "rp", 0, "rp-r1", []doc{declaredCriteriaDoc("CRW-F", "c1", "c2", "c3")},
		packetRelNode("n1", "CRW-F", "p1", []string{"c1"}, []string{"c1"}),
		packetRelNode("n2", "CRW-F", "p2", []string{"c2"}, []string{"c2"}),
		packetRelNode("n3", "CRW-F", "p3", []string{"c3"}, []string{"c3"}))
	sym := func(node string, keys ...string) {
		t.Helper()
		regions := make([]Region, 0, len(keys))
		for _, key := range keys {
			regions = append(regions, Region{Repository: "owner/repo", Path: "pkg/a.go", Kind: "symbol", Key: key, Change: "edit"})
		}
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", node, "parent", regions); err != nil {
			t.Fatalf("declare %s: %v", node, err)
		}
	}
	owner := func(node, key string) {
		t.Helper()
		r := Region{Repository: "owner/repo", Path: "pkg/a.go", Kind: "symbol", Key: key, Change: "edit", Grade: GradeExclusive}
		if _, err := k.sched.DeclareRegions(context.Background(), "rp", node, "parent", []Region{r}); err != nil {
			t.Fatalf("declare %s: %v", node, err)
		}
	}
	// n2 owns Alpha and n3 owns Beta; n1 touches both without owning either. Each SYMBOL is a place with
	// exactly one owner, so nothing is refused. Read as one place (the file), the same three packets would
	// have two exclusive owners and be refused - which is the defect this pins.
	owner("n2", "Alpha")
	owner("n3", "Beta")
	sym("n1", "Alpha", "Beta")
}
