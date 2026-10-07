package mergeturn

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-906 generation 2: the bundle half of the issue's decision 2. A node whose accepted result is being
// corrected is not merged on its old head. The bundle gate already refuses a member whose pull request
// head differs from the accepted head, and that guard holds only once the child has pushed: between
// opening (or recording) the correction generation and that first push the pull request still shows the
// accepted head H, so the train must refuse by the generation alone. These tests drive open, verify and
// land, each writing nothing.

// ucCorrection opens a correction generation of a relationship by hand, the shape generation-open leaves:
// the generations row and the relationship's live execution generation moved to it. The turn fixture
// seeds an acceptance without a relationships row, so the row is written here too.
func (w *tr) ucCorrection(relationship string) {
	w.t.Helper()
	allowed := fmt.Sprintf("[%q]", trLeader)
	w.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES (?, 'ISS-1', 'active', ?, 'host-a', 'child-1', 'host-c', 2, '[]', ?, '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')",
		relationship, trLeader, allowed)
	w.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at)"+
		" VALUES (?, 2, ?, 'bound', 'turn-dispatch-2', 'needs_changes_revision', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')",
		relationship, "correction-"+relationship)
}

// TestTrainRefusesAMemberUnderCorrection: open, verify and land each refuse a member whose accepted
// result has an open correction generation, name it, and write nothing.
func TestTrainRefusesAMemberUnderCorrection(t *testing.T) {
	t.Run("open refuses the member and writes no train", func(t *testing.T) {
		w := newTr(t)
		leader, members := w.threeMembers()
		w.ucCorrection("rel-task-m2")
		_, err := w.open(leader, trLeader, "base-0", members...)
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member under correction: %v", err)
		}
		for _, want := range []string{"under correction", "generation 2", "generation 1"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal does not name %q: %v", want, err)
			}
		}
		if n := w.trainEvents(); n != 0 {
			t.Fatalf("a refused open wrote %d event(s)", n)
		}
	})
	t.Run("verify refuses after the correction opened and writes no verified event", func(t *testing.T) {
		w := newTr(t)
		train := w.openedTrain()
		w.pr(900, "head-bundle", TrainLaneLabel)
		w.forge.runs["run-1"] = runFor("head-bundle")
		w.ucCorrection("rel-task-m3")
		if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member under correction at verify: %v", err)
		}
		if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
			t.Fatalf("a refused verify wrote %d verified event(s)", n)
		}
	})
	t.Run("land refuses and lands no turn", func(t *testing.T) {
		w := newTr(t)
		train := w.verifiedTrain()
		w.ucCorrection("rel-task-m2")
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member under correction at land: %v", err)
		}
		if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
			t.Fatalf("a refused land wrote %d landed event(s)", n)
		}
		if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
			t.Fatalf("a refused land landed %d turn(s)", n)
		}
	})
}

// TestTrainCarriesTheMemberAgainAfterTheCorrectionCloses: the gate is about the correction being open,
// not about the head. A withdrawn generation puts the relationship back on the generation the acceptance
// stands on, and the member rides again on the same head.
func TestTrainCarriesTheMemberAgainAfterTheCorrectionCloses(t *testing.T) {
	w := newTr(t)
	leader, members := w.threeMembers()
	w.ucCorrection("rel-task-m2")
	if _, err := w.open(leader, trLeader, "base-0", members...); err == nil {
		t.Fatal("a member under correction was carried")
	}
	// the withdrawal puts the relationship back on the generation its acceptance stands on
	w.exec("UPDATE relationships SET execution_generation = 1 WHERE relationship_id = 'rel-task-m2'")
	answer, err := w.open(leader, trLeader, "base-0", members...)
	if err != nil {
		t.Fatalf("the bundle after the correction was withdrawn: %v", err)
	}
	if answer["train"] == nil {
		t.Fatalf("the bundle after the correction was withdrawn = %v, want a train", answer)
	}
}

// ucLaneReport is the work report the lane's head comparison reads: the assignment the turn names has a
// report naming the head, so the check has something to compare the restated head with.
func (w *fx) ucLaneReport(relationship, head string) {
	w.t.Helper()
	w.exec("INSERT INTO work_reports (event_id, submission_no, relationship_id, execution_generation, revision_hash, repository, head_sha, cxc_status, cxc_reason, contract_version, summary, next_action, recorded_at)"+
		" VALUES (?, 1, ?, 1, ?, ?, ?, 'DONE', 'proved', 'v1', 'done', 'merge', '2023-11-14T22:13:20.000000+00:00')",
		"ev-report-"+relationship, relationship, "rev-"+relationship, fxRepo, head)
}

// ucLaneRelationship writes the relationship the turn names at the generation given, and (when the
// generation is 2) the correction generation opened over the accepted result. The fx fixture seeds no
// relationship row, so one is written for the turn the test holds.
func (w *fx) ucLaneRelationship(relationship string, generation int64) {
	w.t.Helper()
	w.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES (?, 'ISS-1', 'active', ?, 'host-a', 'child-1', 'host-c', ?, '[]', ?, '2023-11-14T22:13:20.000000+00:00', '2023-11-14T22:13:20.000000+00:00')",
		relationship, alpha.TaskID, generation, fmt.Sprintf("[%q]", alpha.TaskID))
	if generation > 1 {
		w.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at)"+
			" VALUES (?, ?, ?, 'bound', 'turn-dispatch-2', 'needs_changes_revision', '2023-11-14T22:13:20.000000+00:00', '2023-11-14T22:13:20.000000+00:00')",
			relationship, generation, "correction-"+relationship)
		// the acceptance the correction is opened over: the gate compares the live generation with the
		// generation this acceptance stands on
		w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
			" VALUES (?, 'plan-x', 'node-1', ?, ?, 1, ?, ?, 'crit-1', 'verified', 'head-a', ?, 1, 'bound', 'turn-1', '{}', ?, 0, '2023-11-14T22:13:20.000000+00:00', 'active')",
			"acc-"+relationship, "manifest-"+relationship, relationship, "ev-"+relationship, "rev-"+relationship, fxRepo, alpha.TaskID)
	}
}

// ucLaneForge is the acceptance's forge identity, written with the acceptance: the mapping from a pull
// request back to the relationship whose result it carries.
func (w *fx) ucLaneForge(relationship string, pr int64) {
	w.t.Helper()
	w.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?, ?, ?)", "acc-"+relationship, fxRepo, pr)
}

// CRW-906 generation 2: the lane's CLI lets a claim carry a forge pull request instead of a
// relationship, and that route must meet the same gate. The acceptance's forge identity is what maps
// the turn's pull request back to the relationship whose correction is open, so a PR-only turn is
// refused on the head the correction is repairing rather than exempted by the field it omits.
func TestTheLaneGateResolvesTheRelationshipFromThePullRequest(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	w.ucLaneForge("rel-lane", 7)
	turn := store.MergeTurnsRow{TurnID: "mtn-pr-only", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", PRNumber: sql.NullInt64{Int64: 7, Valid: true}, State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a PR-only turn whose accepted result is under correction was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "under correction") || !strings.Contains(refusal.Detail, "rel-lane") {
		t.Fatalf("the refusal does not name the relationship and the reason: %s", refusal.Detail)
	}
	// a turn whose pull request has no accepted result, and whose head no acceptance stands on, is left
	// to the lane's own rules
	other := turn
	other.PRNumber = sql.NullInt64{Int64: 99, Valid: true}
	other.CandidateHead = "head-unaccepted"
	if refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), other); err != nil || refusal != nil {
		t.Fatalf("a turn whose pull request has no accepted result = %+v %v, want no refusal", refusal, err)
	}
}

// CRW-906 generation 2: --relationship and --pr are both optional on merge-turn-request, so a claim can
// carry neither and hold only the head. The accepted head of a node under correction is then the one
// identity left, and the acceptance that recorded it is what the head belongs to: the turn is refused on
// the head the correction is repairing rather than exempted by the two selectors it omits.
func TestTheLaneGateResolvesTheRelationshipFromTheHeadItHolds(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	turn := store.MergeTurnsRow{TurnID: "mtn-bare", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a turn naming neither selector, holding an accepted head under correction, was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "under correction") || !strings.Contains(refusal.Detail, "rel-lane") {
		t.Fatalf("the refusal does not name the relationship and the reason: %s", refusal.Detail)
	}
	// a head no active acceptance of that repository stands on is left to the lane's own rules
	other := turn
	other.CandidateHead = "head-unaccepted"
	if refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), other); err != nil || refusal != nil {
		t.Fatalf("a turn whose head no acceptance stands on = %+v %v, want no refusal", refusal, err)
	}
}

// ucHeldOn is a held, acknowledged turn on a head whose claim records a relationship.
func (w *fx) ucHeldOn(relationship, head string) string {
	w.t.Helper()
	options := ClaimOptions{Relationship: sql.NullString{String: relationship, Valid: true}}
	turn := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, head, true, options))["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	return turn
}

// CRW-906 generation 2, the lane's own half: a merge turn can have been granted before the correction was
// opened, so the check that moves it to merging and the land that records the merge read the correction
// state themselves, inside the transaction that writes. Both refuse disposition_conflict naming the open
// generation, the check writes no current row and the turn stays holding, and the land records no landing
// and leaves the turn merging.
func TestTheLaneRefusesATurnWhoseAcceptedResultIsUnderCorrection(t *testing.T) {
	t.Run("the check refuses and the turn stays holding", func(t *testing.T) {
		w := newFx(t)
		turn := w.ucHeldOn("rel-lane", "head-a")
		w.ucLaneReport("rel-lane", "head-a")
		w.ucLaneRelationship("rel-lane", 2)
		_, err := w.check(turn, "head-a", "base-0", "")
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("check under correction: %v", err)
		}
		for _, want := range []string{"under correction", "generation 2", "generation 1"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal does not name %q: %v", want, err)
			}
		}
		if got := w.must(w.m.Turn(w.ctx, turn))["state"]; got != "holding" {
			t.Fatalf("the turn is %v after the refused check, want holding", got)
		}
		if n := w.ucCount("SELECT count(*) FROM merge_turn_checks WHERE result = 'current'"); n != 0 {
			t.Fatalf("the refused check wrote %d current row(s)", n)
		}
	})
	t.Run("the land refuses and records no landing", func(t *testing.T) {
		w := newFx(t)
		turn := w.ucHeldOn("rel-lane", "head-a")
		w.ucLaneReport("rel-lane", "head-a")
		w.must(w.check(turn, "head-a", "base-0", ""))
		w.merged("merge-1")
		w.ucLaneRelationship("rel-lane", 2)
		_, err := w.land(turn, "merge-1", "", "")
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("land under correction: %v", err)
		}
		if !strings.Contains(err.Error(), "under correction") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
		if got := w.must(w.m.Turn(w.ctx, turn))["state"]; got != "merging" {
			t.Fatalf("the turn is %v after the refused land, want merging", got)
		}
		if n := w.ucCount("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
			t.Fatalf("the refused land landed %d turn(s)", n)
		}
	})
	t.Run("a turn whose relationship is not under correction is unchanged", func(t *testing.T) {
		w := newFx(t)
		turn := w.ucHeldOn("rel-lane", "head-a")
		// the relationship stands on the generation its acceptance stands on: no correction is open
		w.ucLaneReport("rel-lane", "head-a")
		w.ucLaneRelationship("rel-lane", 1)
		if _, err := w.check(turn, "head-a", "base-0", ""); err != nil {
			t.Fatalf("a turn with no correction open was refused: %v", err)
		}
	})
}

// CRW-906 generation 2: a recorded base refresh moves the head the acceptance stands on without changing
// the accepted head, so a lane turn that names neither selector and holds the refreshed head must still
// resolve to the relationship whose correction is open. The head it holds is the accepted head or the head
// of a refresh of it, and either belongs to the same acceptance.
func TestTheLaneGateResolvesARefreshedHead(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	// the acceptance stands on head-a; a base refresh moved it to head-refreshed
	refresh := "dbr-" + strings.Repeat("a", 60)
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, ?, 1, ?, 1, 'ev-refresh', 'rev-refresh', 'head-refreshed', ?, ?, 'base-0', '{}', '[]', ?, 0, '2023-11-14T22:13:20.000000+00:00')",
		refresh, "acc-rel-lane", "rel-lane", fxRepo, fxBase, alpha.TaskID)
	turn := store.MergeTurnsRow{TurnID: "mtn-refreshed", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-refreshed", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a turn holding the refreshed head of an accepted result under correction was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "under correction") || !strings.Contains(refusal.Detail, "rel-lane") {
		t.Fatalf("the refusal does not name the relationship and the reason: %s", refusal.Detail)
	}
}

// CRW-906 generation 2: a member whose turn left the lane is excluded from the landing, but its code is
// CRW-906 generation 2, round 6 d2: a recorded base refresh outlives the acceptance it was recorded for,
// so a turn held for a refreshed head must still resolve once dag-accept --supersedes replaced the
// acceptance: otherwise the obsolete result merges after the correction was accepted over.
func TestTheLaneGateRefusesAReplacedHeadThatWasOnceRefreshed(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	w.ucLaneForge("rel-lane", 7)
	// the acceptance stood on head-a, a base refresh moved it to head-refreshed, and then the whole
	// acceptance was replaced by the corrected one
	refresh := "dbr-" + strings.Repeat("b", 60)
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, ?, 1, ?, 1, 'ev-refresh', 'rev-refresh', 'head-refreshed', ?, ?, 'base-0', '{}', '[]', ?, 0, '2023-11-14T22:13:20.000000+00:00')",
		refresh, "acc-rel-lane", "rel-lane", fxRepo, fxBase, alpha.TaskID)
	w.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE relationship_id = 'rel-lane'")
	turn := store.MergeTurnsRow{TurnID: "mtn-refreshed-replaced", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-refreshed", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a turn holding a refreshed head whose acceptance was replaced was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-lane") {
		t.Fatalf("the refusal does not name the relationship: %s", refusal.Detail)
	}
}

// CRW-906 generation 2, round 6 d3: an acceptance written before the forge rule keeps whatever target it
// was accepted against (a local checkout included) while dag_acceptance_forge holds the owner/name a merge
// turn is requested against. The lane gate must match the forge identity too, not the stored target.
func TestTheLaneGateMatchesATurnByItsForgeIdentityNotTheStoredTarget(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-local", 2)
	w.ucLaneForge("rel-local", 7)
	w.exec("UPDATE dag_acceptances SET repository = '/synthetic/checkout' WHERE relationship_id = 'rel-local'")
	// a turn of the real forge repository, naming neither selector, already merging on the accepted head
	turn := store.MergeTurnsRow{TurnID: "mtn-legacy", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Merging}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a legacy acceptance's forge turn was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-local") {
		t.Fatalf("the refusal does not name the relationship: %s", refusal.Detail)
	}
}

// CRW-906 generation 2: a member whose turn left the lane is excluded from the landing, but its code is
// still in the merge commit the bundle lands (every member head is checked as an ancestor of it), so the
// bundle must not record a landing of a tree that still carries a result the plan is repairing. The
// CRW-897 carve-out keeps holding for what it is about: a revoked acceptance or a moved head of a member
// that left does not refuse the rest of the bundle.
func TestTrainLandRefusesAnExcludedMemberUnderCorrection(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 2: %v", err)
	}
	memberTurn := rows[0].Get("turn_id").(string)
	// the member's parent withdraws its waiting turn, and the correction over its accepted result opens
	if _, err := w.m.Withdraw(w.ctx, memberTurn, "task-m2"); err != nil {
		t.Fatalf("withdrawing the member's turn: %v", err)
	}
	w.ucCorrection("rel-task-m2")
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	_, err = w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a land of a bundle still carrying an excluded member under correction: %v", err)
	}
	for _, want := range []string{"under correction", "generation 2", "generation 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("a refused land landed %d turn(s)", n)
	}
}

// CRW-906 generation 2, d2: acceptance uniqueness is per node and output, not per repository and pull
// request or per repository and head, so two accepted nodes can name the same commit. The gate must ask
// every matching relationship, not the newest one: a turn is refused when any of them is correcting the
// very head it holds.
func TestTheLaneGateAsksEveryMatchingAcceptance(t *testing.T) {
	w := newFx(t)
	// rel-a-live stands on its live generation and sorts first, so a lookup that took one arbitrary
	// match would take it; rel-z-correcting has a correction open over the same head
	w.ucLaneRelationship("rel-a-live", 1)
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-rel-a-live', 'plan-x', 'node-live', 'manifest-live', 'rel-a-live', 1, 'ev-live', 'rev-live', 'crit-1', 'verified', 'head-a', ?, 1, 'bound', 'turn-1', '{}', ?, 0, '2023-11-14T22:13:19.000000+00:00', 'active')",
		fxRepo, alpha.TaskID)
	w.ucLaneRelationship("rel-z-correcting", 2)
	turn := store.MergeTurnsRow{TurnID: "mtn-two", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a head two nodes accept, one of them under correction, was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-z-correcting") {
		t.Fatalf("the refusal does not name the correcting relationship: %s", refusal.Detail)
	}
}

// CRW-906 generation 2, d3: the excluded member's hold must not clear just because the correction was
// CRW-906 generation 2, d3: the recheck inside the write transaction asks about the head the bundle
// CRW-906 generation 2, d1: naming a relationship must not suppress the other identities. Acceptance
// CRW-906 generation 2: a bundle member is refused when the head it carries is the result another
// CRW-906 generation 2, round 6 d1: the landing transaction must apply the same identity-based gate the
// open and verify use. A correction of another relationship that accepts the same head, opened after the
// bundle was verified, is refused at land: otherwise the landing records the exact result being repaired.
func TestTrainLandRefusesAMemberWhoseHeadAnotherRelationshipIsRepairing(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	w.ucOtherCorrection("rel-other", "head-m2")
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a land whose member's head another relationship is repairing: %v", err)
	}
	if !strings.Contains(err.Error(), "rel-other") {
		t.Fatalf("the refusal does not name the repairing relationship: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("a refused land landed %d turn(s)", n)
	}
}

// CRW-906 generation 2: a bundle member is refused when the head it carries is the result another
// relationship is repairing, even though the member's own relationship is live. Acceptance uniqueness is
// per node and output, so two accepted nodes can name the same commit, and the bundle must apply the same
// rule the single lane does.
func TestTrainRefusesAMemberWhoseHeadAnotherRelationshipIsRepairing(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.ucOtherCorrection("rel-other", "head-m2")
	_, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a member whose head another relationship is repairing: %v", err)
	}
	if !strings.Contains(err.Error(), "rel-other") {
		t.Fatalf("the refusal does not name the repairing relationship: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}
}

// ucOtherCorrection writes another relationship's active acceptance on a head with a correction
// generation open over it: the shape the bundle gate must see through a member's own live relationship.
func (w *tr) ucOtherCorrection(relationship, head string) {
	w.t.Helper()
	allowed := fmt.Sprintf("[%q]", trLeader)
	w.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES (?, 'ISS-1', 'active', ?, 'host-a', 'child-x', 'host-c', 2, '[]', ?, '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')",
		relationship, trLeader, allowed)
	w.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at)"+
		" VALUES (?, 2, ?, 'bound', 'turn-dispatch-x', 'needs_changes_revision', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')",
		relationship, "correction-"+relationship)
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES (?, 'plan-x', 'node-x', 'manifest-x', ?, 1, 'ev-x', 'rev-x', 'crit-x', 'verified', ?, ?, 1, 'bound', 'turn-x', '{}', ?, 0, '2026-10-01T00:00:00Z', 'active')",
		"acc-"+relationship, relationship, head, trRepo, trLeader)
	w.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?, 'owner/repo', 1)", "acc-"+relationship)
}

// CRW-906 generation 2: an acceptance recorded before the forge rule keeps whatever target it was
// accepted against — a local checkout included — while dag_acceptance_forge holds the owner/name a merge
// turn is actually requested against. The gate matches the forge identity, not the stored target.
func TestTheLaneGateMatchesTheForgeIdentityNotTheStoredTarget(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-local", 2)
	w.ucLaneForge("rel-local", 7)
	w.exec("UPDATE dag_acceptances SET repository = '/synthetic/checkout' WHERE relationship_id = 'rel-local'")
	for _, turn := range []store.MergeTurnsRow{
		{TurnID: "mtn-by-pr", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
			HolderTaskID: alpha.TaskID, CandidateHead: "head-a", PRNumber: sql.NullInt64{Int64: 7, Valid: true}, State: Holding},
		{TurnID: "mtn-by-head", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
			HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding},
	} {
		refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
		if err != nil {
			t.Fatal(err)
		}
		if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
			t.Fatalf("%s: a turn matched by the forge identity rather than the stored target was not refused: %+v", turn.TurnID, refusal)
		}
		if !strings.Contains(refusal.Detail, "rel-local") {
			t.Fatalf("%s: the refusal does not name the relationship: %s", turn.TurnID, refusal.Detail)
		}
	}
}

// CRW-906 generation 2: after dag-accept --supersedes the head an acceptance recorded belongs to no
// active acceptance any more, so a turn held for it would otherwise merge exactly the result the
// correction replaced.
func TestTheLaneGateRefusesAHeadAnAcceptanceRecordedAndThenReplaced(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	w.ucLaneForge("rel-lane", 7)
	// the acceptance that recorded head-a was replaced: a later one stands on the corrected head now
	w.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE relationship_id = 'rel-lane'")
	turn := store.MergeTurnsRow{TurnID: "mtn-replaced", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a turn holding a replaced head was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-lane") || !strings.Contains(refusal.Detail, "replaced") {
		t.Fatalf("the refusal does not name the relationship and what happened: %s", refusal.Detail)
	}
}

// CRW-906 generation 2, d1: naming a relationship must not suppress the other identities. Acceptance
// uniqueness is per node and output, so two nodes can accept the same commit: a turn that names the
// relationship which is NOT being corrected is still carrying a head another relationship is repairing,
// and the gate must read that one too rather than trusting the name it was given.
func TestTheLaneGateAsksTheOtherAcceptancesOfANamedRelationship(t *testing.T) {
	w := newFx(t)
	// rel-named is the relationship the turn names, and it is not under correction
	w.ucLaneRelationship("rel-named", 1)
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-rel-named', 'plan-x', 'node-named', 'manifest-named', 'rel-named', 1, 'ev-named', 'rev-named', 'crit-1', 'verified', 'head-shared', ?, 1, 'bound', 'turn-1', '{}', ?, 0, '2023-11-14T22:13:19.000000+00:00', 'active')",
		fxRepo, alpha.TaskID)
	// rel-correcting accepts the same head and has a correction open over it
	w.ucLaneRelationship("rel-correcting", 2)
	w.exec("UPDATE dag_acceptances SET head_sha = 'head-shared' WHERE relationship_id = 'rel-correcting'")
	turn := store.MergeTurnsRow{TurnID: "mtn-named", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, RelationshipID: sql.NullString{String: "rel-named", Valid: true}, CandidateHead: "head-shared", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a turn naming a live relationship but holding a head another one is repairing was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-correcting") {
		t.Fatalf("the refusal does not name the correcting relationship: %s", refusal.Detail)
	}
}

// CRW-906 generation 2, d3: the recheck inside the write transaction asks about the head the bundle
// carries, not only whether a correction is open. A correction opened AND accepted over inside the gap
// makes the live and stand generations equal again, so a guard that only asked UnderCorrection would let
// the obsolete bundle through with a verified event for a head the plan no longer accepts.
func TestTrainVerifyRefusesAMemberWhoseAcceptedResultMovedOn(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	// the member's accepted result is corrected and accepted over while the run and the chain are being
	// proved: the live and stand generations are equal again by the time the write transaction runs, so a
	// guard that only asked whether a correction is open would let the obsolete bundle through. The
	// mutation runs inside the proof, which is the gap the recheck exists for.
	proof := &ucProver{trProof: w.proof, during: func() {
		w.exec("UPDATE relationships SET execution_generation = 2 WHERE relationship_id = 'rel-task-m2'")
		w.exec("UPDATE dag_acceptances SET execution_generation = 2, head_sha = 'head-m2-corrected' WHERE relationship_id = 'rel-task-m2' AND state = 'active'")
	}}
	_, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a verify whose member's accepted result moved on: %v", err)
	}
	if !strings.Contains(err.Error(), "head-m2-corrected") {
		t.Fatalf("the refusal does not name the stand the acceptance moved to: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}
}

// CRW-906 generation 2, d3: the excluded member's hold must not clear just because the correction was
// accepted over. Exclusion changes the accounting, not the bundle tree: the verified tree still carries the
// member head it was verified on, so once the acceptance stands on a corrected head the bundle no longer
// holds what the plan accepts for that member and must be rebuilt and verified.
func TestTrainLandRefusesAnExcludedMemberWhoseAcceptanceMovedOn(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 2: %v", err)
	}
	memberTurn := rows[0].Get("turn_id").(string)
	if _, err := w.m.Withdraw(w.ctx, memberTurn, "task-m2"); err != nil {
		t.Fatalf("withdrawing the member's turn: %v", err)
	}
	// dag-accept --supersedes moved the acceptance onto the corrected result: the relationship and the
	// acceptance stand on the same generation again, so no correction is open, and the head it stands on
	// is the corrected one the verified bundle does not carry.
	w.exec("UPDATE relationships SET execution_generation = 2 WHERE relationship_id = 'rel-task-m2'")
	w.exec("UPDATE dag_acceptances SET execution_generation = 2, head_sha = 'head-m2-corrected' WHERE relationship_id = 'rel-task-m2' AND state = 'active'")
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	_, err = w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a land of a bundle whose excluded member's acceptance moved on: %v", err)
	}
	if !strings.Contains(err.Error(), "head-m2-corrected") || !strings.Contains(err.Error(), "rebuild and verify") {
		t.Fatalf("the refusal does not name the stand and the way on: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("a refused land landed %d turn(s)", n)
	}
}

// ucProver is the checkout stand-in with a seam: it runs during() inside the chain proof, which is the
// gap between the reads a verify makes and the transaction it writes in. That is where a member's
// accepted result can move without either read seeing it, and what the transaction recheck exists for.
type ucProver struct {
	*trProof
	during func()
}

func (p *ucProver) Chain(ctx context.Context, checkout, head, base string, members []TrainMemberExpectation) (TrainChain, error) {
	chain, err := p.trProof.Chain(ctx, checkout, head, base, members)
	if err == nil && p.during != nil {
		p.during()
	}
	return chain, err
}

// ucCount is the number of rows a query answers, for the writes a refusal must not leave behind.
func (w *fx) ucCount(query string, args ...any) int64 {
	w.t.Helper()
	var n int64
	if err := w.s.DB.QueryRowContext(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// CRW-906 final round, GLM P1: a head is one commit however it is spelled. merge-turn-request stores the
// caller's head verbatim, so a head-only turn holding the accepted head in upper case must still resolve
// to the relationship whose correction is open; comparing raw text would let it pass the lane gate.
func TestTheLaneGateMatchesAHeadRegardlessOfItsSpelling(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	turn := store.MergeTurnsRow{TurnID: "mtn-spelled", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "  HEAD-A ", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a head-only turn holding the accepted head in another spelling was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-lane") {
		t.Fatalf("the refusal does not name the relationship: %s", refusal.Detail)
	}
}

// CRW-906 final round, evaluation 6f79654d D1: a head an accepted node replaced is not mergeable through another
// accepted node that still stands on it. The replaced-head check must run whenever the turn holds a head, not
// only when no relationship matched the head.
func TestTheLaneGateRefusesAReplacedHeadThatAnotherNodeStillAccepts(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-a", 2)
	w.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE relationship_id = 'rel-a'")
	w.ucLaneRelationship("rel-b", 1)
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-rel-b', 'plan-x', 'node-b', 'manifest-b', 'rel-b', 1, 'ev-b', 'rev-b', 'crit-1', 'verified', 'head-a', ?, 1, 'bound', 'turn-1', '{}', ?, 0, '2023-11-14T22:13:19.000000+00:00', 'active')",
		fxRepo, alpha.TaskID)
	turn := store.MergeTurnsRow{TurnID: "mtn-replaced-elsewhere", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a head another node accepts, after this node's correction replaced it, was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.Detail, "rel-a") {
		t.Fatalf("the refusal does not name the replaced relationship: %s", refusal.Detail)
	}
}

// CRW-906 final round, delta review ad054763: a head the active acceptance of this relationship reaches only
// through its own base refresh is that relationship's current result, so a superseded acceptance's refresh
// row for the same head must not refuse a turn holding it.
func TestTheLaneGateKeepsAHeadThatTheActiveAcceptanceReachesByARefresh(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-c", 2)
	refresh := "dbr-" + strings.Repeat("c", 60)
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, 'acc-rel-c', 1, 'rel-c', 1, 'ev-r1', 'rev-r1', 'head-refreshed', ?, ?, 'base-0', '{}', '[]', ?, 0, '2023-11-14T22:13:20.000000+00:00')",
		refresh, fxRepo, fxBase, alpha.TaskID)
	w.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE relationship_id = 'rel-c'")
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-rel-c2', 'plan-x', 'node-c', 'manifest-c2', 'rel-c', 2, 'ev-c2', 'rev-c2', 'crit-1', 'verified', 'head-b', ?, 1, 'bound', 'turn-1', '{}', ?, 0, '2023-11-14T22:13:21.000000+00:00', 'active')",
		fxRepo, alpha.TaskID)
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, 'acc-rel-c2', 1, 'rel-c', 2, 'ev-r2', 'rev-r2', 'head-refreshed', ?, ?, 'base-0', '{}', '[]', ?, 0, '2023-11-14T22:13:22.000000+00:00')",
		"dbr-"+strings.Repeat("d", 60), fxRepo, fxBase, alpha.TaskID)
	turn := store.MergeTurnsRow{TurnID: "mtn-refresh-current", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-refreshed", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal != nil {
		t.Fatalf("a head the active acceptance reaches by its own refresh was refused: %s", refusal.Detail)
	}
}

// CRW-906 evaluation 7f7b39ca D1: a member that left the lane with no acceptance of its own (revoked) still carries
// its head in the bundle's tree, so another node's open correction over that head refuses the landing. The
// carve-out for a revoked acceptance does not cover another node's correction.
func TestTrainLandRefusesAnExcludedMemberWhoseHeadAnotherNodeIsRepairing(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 2: %v", err)
	}
	memberTurn := rows[0].Get("turn_id").(string)
	if _, err := w.m.Withdraw(w.ctx, memberTurn, "task-m2"); err != nil {
		t.Fatalf("withdrawing the member's turn: %v", err)
	}
	w.exec("UPDATE dag_acceptances SET state = 'revoked' WHERE relationship_id = 'rel-task-m2'")
	w.ucOtherCorrection("rel-other", "head-m2")
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	_, err = w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a land whose excluded member's head another node is repairing: %v", err)
	}
	if !strings.Contains(err.Error(), "rel-other") {
		t.Fatalf("the refusal does not name the repairing relationship: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
}
