package mergeturn

// CRW-897's red-first cases for the merge train's four confirmed gaps: a member pull request that
// moved after verify is refused by verify and land, and land excludes a member whose turn left the
// lane; the order check finds a member through its active acceptance; TrainHalve terminates with
// joined members on one side; and a second open by the same leader is refused by the in-transaction
// membership check. Every case uses the package's temporary store, forge stand-in and git stand-ins,
// never the live relay.

import (
	"strings"
	"testing"
	"time"
)

// TestTrainVerifyRefusesAMovedMember: a member pull request that moved after the train opened makes
// verify refuse with nothing written (CRW-897, answer 1).
func TestTrainVerifyRefusesAMovedMember(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	// member 102's pull request moved after the train opened
	w.pr(102, "head-m2-moved")
	_, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a member that moved after the train opened: %v", err)
	}
	if detail := err.Error(); !strings.Contains(detail, "102") || !strings.Contains(detail, "head-m2-moved") || !strings.Contains(detail, "head-m2") {
		t.Fatalf("the refusal does not name the pull request and both heads: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}
}

// TestTrainLandRefusesAMovedMember: the same move seen at land, whose turn is still live, refuses
// with nothing written (CRW-897, answer 1).
func TestTrainLandRefusesAMovedMember(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	w.pr(102, "head-m2-moved")
	_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a member that moved before land: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("a refused land landed %d turn(s)", n)
	}
}

// TestTrainLandExcludesAMemberWhoseTurnLeftTheLane: when a member's turn is returned or withdrawn
// after the train opened, land records the rest and names the excluded member in the landed event
// with its reason; the excluded member's moved head is not a reason to refuse the bundle
// (CRW-897, answer 1).
func TestTrainLandExcludesAMemberWhoseTurnLeftTheLane(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 2: %v", err)
	}
	memberTurn := rows[0].Get("turn_id").(string)
	// the member's parent withdraws its waiting turn, and its pull request moves afterwards
	if _, err := w.m.Withdraw(w.ctx, memberTurn, "task-m2"); err != nil {
		t.Fatalf("withdrawing the member's turn: %v", err)
	}
	w.pr(102, "head-m2-moved")
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	answer, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err != nil {
		t.Fatalf("land with an excluded member: %v", err)
	}
	if answer["state"] != "landed" {
		t.Fatalf("state after land = %v, want landed", answer["state"])
	}
	// the two surviving members landed, the excluded one did not
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 2 {
		t.Fatalf("landed turns = %d, want 2", n)
	}
	if state := w.turn(memberTurn).State; state != "withdrawn" {
		t.Fatalf("the excluded member's turn = %s, want withdrawn", state)
	}
	// the landed event names the excluded member with its reason
	detail := w.eventDetail(train, "landed")
	excluded, _ := detail["excluded"].([]any)
	if len(excluded) != 1 {
		t.Fatalf("the landed event's excluded list = %v, want one member", detail["excluded"])
	}
	entry, _ := excluded[0].(map[string]any)
	if entry["prNumber"] != float64(102) || entry["turnId"] != memberTurn || entry["state"] != "withdrawn" {
		t.Fatalf("the excluded entry = %v", entry)
	}
	if reason, _ := entry["closeReason"].(string); !strings.Contains(reason, "withdrawn") {
		t.Fatalf("the excluded entry names no reason: %v", entry)
	}
	// the mapping of the members that landed holds the two survivors
	members, _ := detail["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("the landed mapping holds %d members, want 2", len(members))
	}
}

// TestTrainLandRefusesAMovedMemberWhoseTurnIsLive: a member that moved while its turn is still
// waiting is refused, not excluded (CRW-897, answer 1).
func TestTrainLandRefusesAMovedMemberWhoseTurnIsLive(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	w.pr(103, "head-m3-moved")
	_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a moved member whose turn is live: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("a refused land landed %d turn(s)", n)
	}
}

// TestTrainOrderRefusalFindsAMemberThroughItsAcceptance: the review's order case. The leader is
// independent (no plan node), an edge A->B joins two other members, and B's acceptance was
// base-refreshed so its stand head is no longer dag_acceptances.head_sha. The reversed bundle (B
// before A) must be refused (CRW-897, answer 2).
func TestTrainOrderRefusalFindsAMemberThroughItsAcceptance(t *testing.T) {
	w := newTr(t)
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
	w.pr(102, "head-m2")
	w.waiting("PRJ-M3", "task-m3", "head-m3", 103)
	w.pr(103, "head-m3")
	// plan P: node A (PR 102) -> node B (PR 103), and the leader (101) is on no plan node
	w.exec("INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES ('P','PRJ-LEADER','task-leader','2026-10-01T00:00:00Z')")
	w.exec("INSERT INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, author_task_id, recorded_at) VALUES ('P',1,0,'req-1','d-1','{}','sd-1','task-leader','2026-10-01T00:00:00Z')")
	w.exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, criteria_set_digest) VALUES ('P','A',1,NULL,'sd-A','A','implementation','c-A')")
	w.exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, criteria_set_digest) VALUES ('P','B',1,NULL,'sd-B','B','implementation','c-B')")
	w.exec("INSERT INTO dag_edges (plan_id, edge_id, introduced_rev, retired_rev, from_node_id, to_node_id, kind) VALUES ('P','e1',1,NULL,'A','B','artifact_verified')")
	// the members' acceptances are replaced with plan acceptances (the ones open reads)
	w.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id IN ('acc-rel-task-m2','acc-rel-task-m3')")
	w.exec("DELETE FROM dag_acceptances WHERE acceptance_id IN ('acc-rel-task-m2','acc-rel-task-m3')")
	w.accept("acc-A", "P", "A", "head-m2", 102)
	w.exec("UPDATE dag_acceptances SET relationship_id = 'rel-task-m2' WHERE acceptance_id = 'acc-A'")
	w.accept("acc-B", "P", "B", "head-m3", 103)
	w.exec("UPDATE dag_acceptances SET relationship_id = 'rel-task-m3' WHERE acceptance_id = 'acc-B'")
	// B's acceptance was base-refreshed: the stand head moves and the member's turn and pull request
	// follow it, while dag_acceptances.head_sha still holds the original head
	w.refreshStand("acc-B", "rel-task-m3", "head-m3-refreshed")
	w.exec("UPDATE merge_turns SET candidate_head = 'head-m3-refreshed' WHERE relationship_id = 'rel-task-m3'")
	w.forge.pulls[103] = TrainPullRequest{Number: 103, State: "open", BaseRef: trBase, HeadSHA: "head-m3-refreshed"}
	// the reversed order (B before its predecessor A) is refused, which the head lookup missed. It
	// writes nothing, so the accepted order can still open below.
	if _, err := w.open(leader, trLeader, "base-0", 101, 103, 102); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a reversed order with a base-refreshed member: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events"); n != 0 {
		t.Fatalf("a refused open wrote %d event(s)", n)
	}
	// the right order is accepted, so the refusal was the order and not something else
	if _, err := w.open(leader, trLeader, "base-0", 101, 102, 103); err != nil {
		t.Fatalf("the plan order with a base-refreshed member was refused: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events"); n != 1 {
		t.Fatalf("the events written = %d, want only the accepted open's", n)
	}
}

// TestTrainHalveTerminatesAndKeepsJoinedMembers: the review's TrainHalve case (a join whose
// predecessors land in different halves), a chain, and an all-joined bundle. Each call must return
// inside a deadline, and every pair joined by a plan edge must share a side (CRW-897, answer 3).
func TestTrainHalveTerminatesAndKeepsJoinedMembers(t *testing.T) {
	nodes := []TrainMemberNode{{PRNumber: 101, NodeID: "A"}, {PRNumber: 102, NodeID: "X"}, {PRNumber: 103, NodeID: "B"}, {PRNumber: 104, NodeID: "C"}}
	// the review's case: order [A, X, B, C] with edges A->C and B->C. The old repair loop never
	// terminated on it.
	join := []TrainPlanEdge{{FromNodeID: "A", ToNodeID: "C"}, {FromNodeID: "B", ToNodeID: "C"}}
	first, second := halveWithin(t, []int64{101, 102, 103, 104}, nodes, join)
	sides := map[int64]int{}
	for _, pr := range first {
		sides[pr] = 0
	}
	for _, pr := range second {
		sides[pr] = 1
	}
	if sides[101] != sides[104] || sides[103] != sides[104] {
		t.Fatalf("the join split a dependency: %v / %v", first, second)
	}
	if len(first)+len(second) != 4 {
		t.Fatalf("the halves hold %d members, want 4", len(first)+len(second))
	}

	// a chain A->B->C: the whole chain is one group and cannot be split, so all four go first when
	// the chain holds three of the four members only if it fits; here the chain is 101,102,103 and 104
	// is alone, so the first half takes a whole group and both halves are non-empty
	chain := []TrainPlanEdge{{FromNodeID: "A", ToNodeID: "X"}, {FromNodeID: "X", ToNodeID: "B"}}
	first, second = halveWithin(t, []int64{101, 102, 103, 104}, nodes, chain)
	if len(first) == 0 || len(second) == 0 {
		t.Fatalf("the chain halves = %v / %v, want both non-empty", first, second)
	}
	order := map[int64]int{}
	for i, pr := range []int64{101, 102, 103} {
		order[pr] = i
	}
	_ = order
	if sideOf(t, first, second, 101) != sideOf(t, first, second, 102) || sideOf(t, first, second, 102) != sideOf(t, first, second, 103) {
		t.Fatalf("the chain was split: %v / %v", first, second)
	}

	// every member in one group: it cannot be split, so all of it goes first and the second is empty
	all := []TrainPlanEdge{{FromNodeID: "A", ToNodeID: "X"}, {FromNodeID: "X", ToNodeID: "B"}, {FromNodeID: "B", ToNodeID: "C"}}
	first, second = halveWithin(t, []int64{101, 102, 103, 104}, nodes, all)
	if len(first) != 4 || len(second) != 0 {
		t.Fatalf("an all-joined bundle halved into %v / %v, want all first", first, second)
	}

	// a bundle with no edges at all still halves by order
	first, second = halveWithin(t, []int64{101, 102, 103, 104}, nodes, nil)
	if len(first)+len(second) != 4 {
		t.Fatalf("an edge-free bundle halved into %v / %v", first, second)
	}
}

// halveWithin runs TrainHalve with a deadline, so a loop fails rather than hangs.
func halveWithin(t *testing.T, order []int64, nodes []TrainMemberNode, edges []TrainPlanEdge) (first, second []int64) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, second = TrainHalve(order, nodes, edges)
	}()
	select {
	case <-done:
		return first, second
	case <-time.After(5 * time.Second):
		t.Fatalf("TrainHalve did not return within the deadline for order %v", order)
		return nil, nil
	}
}

// sideOf is the half a member stands in.
func sideOf(t *testing.T, first, second []int64, pr int64) int {
	t.Helper()
	for _, p := range first {
		if p == pr {
			return 0
		}
	}
	for _, p := range second {
		if p == pr {
			return 1
		}
	}
	t.Fatalf("pull request %d is in neither half (%v / %v)", pr, first, second)
	return -1
}

// TestTrainOpenRefusesASecondLiveTrainByTheSameLeader: the in-transaction membership check. The
// early check is skipped by the test seam, so only the check inside the recording transaction can
// refuse (CRW-897, answer 4).
func TestTrainOpenRefusesASecondLiveTrainByTheSameLeader(t *testing.T) {
	w := newTr(t)
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
	w.pr(102, "head-m2")
	if _, err := w.open(leader, trLeader, "base-0", 101, 102); err != nil {
		t.Fatalf("the first open: %v", err)
	}
	// the early check is what normally catches a second open; skip it so the in-transaction check
	// is the one that must refuse
	trainSkipEarlyMembership = true
	t.Cleanup(func() { trainSkipEarlyMembership = false })
	if _, err := w.open(leader, trLeader, "base-0", 101, 102); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a second open by the same leader: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_trains"); n != 1 {
		t.Fatalf("trains = %d, want only the first", n)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events"); n != 1 {
		t.Fatalf("events = %d, want only the first open's", n)
	}
}

// TestTrainOpenRefusesAMemberOfALiveTrainInsideTheTransaction: the same in-transaction check for a
// member turn that joined another live train after the early check (CRW-897, answer 4).
func TestTrainOpenRefusesAMemberOfALiveTrainInsideTheTransaction(t *testing.T) {
	w := newTr(t)
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
	w.pr(102, "head-m2")
	rows, _ := w.s.All(w.ctx, "SELECT turn_id FROM merge_turns WHERE relationship_id = 'rel-task-m2'")
	memberTurn := rows[0].Get("turn_id").(string)
	// another live train already carries the member's turn
	w.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES ('aaa-other','tgt','owner/repo','dev','base-0','task-other','2026-10-01T00:00:00Z')")
	w.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES ('aaa-other',1,?,102,'rel-task-m2','head-m2')", memberTurn)
	w.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES ('aaa-other',1,'opened','task-other','{}','2026-10-01T00:00:00Z')")
	trainSkipEarlyMembership = true
	t.Cleanup(func() { trainSkipEarlyMembership = false })
	if _, err := w.open(leader, trLeader, "base-0", 101, 102); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a member of another live train at open: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_trains"); n != 1 {
		t.Fatalf("trains = %d, want only the pre-existing one", n)
	}
}
