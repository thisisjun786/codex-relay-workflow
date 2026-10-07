package mergeturn

import (
	"fmt"
	"strings"
	"testing"
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
