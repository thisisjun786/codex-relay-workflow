package mergeturn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-906 generation 2, round 14: a repository is one name however it is spelled. The lane reads a local
// checkout through its git directory, so SameRepository resolves a path the way the lane does, and the
// lane gate and the bundle gates match a turn's repository by it under every spelling an acceptance has.

// TestSameRepositoryNamesOneCheckoutByItsLocalReading pins the one definition of a repository name.
func TestSameRepositoryNamesOneCheckoutByItsLocalReading(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	same := []string{
		checkout, checkout + "/", checkout + "/.", filepath.Join(checkout, "..", "checkout"),
		filepath.Join(checkout, ".git"), link, link + "/", filepath.Join(link, ".git"),
	}
	for _, spelling := range same {
		if !SameRepository(checkout, spelling) || !SameRepository(spelling, checkout) {
			t.Fatalf("%q and the checkout %q name different repositories", spelling, checkout)
		}
	}
	for _, spelling := range []string{other, filepath.Join(root, "missing")} {
		if SameRepository(checkout, spelling) {
			t.Fatalf("%q names the checkout %q", spelling, checkout)
		}
	}
	if !SameRepository(" Owner/Repo ", "owner/repo") {
		t.Fatal("two spellings of one forge slug do not name one repository")
	}
	if SameRepository(checkout, "owner/repo") || SameRepository("owner/repo", checkout) {
		t.Fatal("a path and a forge slug name one repository")
	}
	if SameRepository("", "") || SameRepository(checkout, "  ") {
		t.Fatal("an empty spelling names a repository")
	}
}

// TestTheCorrectionHoldsEverySpellingOfTheAcceptedRepository: a head under an open correction is held for a turn
// whose repository is any spelling of the accepted repository, and for no other repository.
func TestTheCorrectionHoldsEverySpellingOfTheAcceptedRepository(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, repository string
		held             bool
	}{
		{"the checkout", checkout, true},
		{"the checkout with a trailing slash", checkout + "/", true},
		{"the checkout through a dot segment", checkout + "/./", true},
		{"the checkout through a parent segment", filepath.Join(checkout, "..", "checkout"), true},
		{"the checkout's git directory", filepath.Join(checkout, ".git"), true},
		{"a symlink to the checkout", link, true},
		{"an unrelated checkout with the same head", other, false},
		{"the forge slug of a path-accepted node", fxRepo, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newFx(t)
			w.ucLaneRelationship("rel-lane", 2)
			w.exec("UPDATE dag_acceptances SET repository = ? WHERE acceptance_id = 'acc-rel-lane'", checkout)
			turn := store.MergeTurnsRow{TurnID: "mtn-spelling", TargetKey: "tgt-x", Repository: c.repository, BaseRef: fxBase, ProjectKey: fxA,
				HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
			refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
			if err != nil {
				t.Fatal(err)
			}
			if c.held != (refusal != nil) {
				t.Fatalf("turn repository %q: held = %v, want %v (refusal %+v)", c.repository, refusal != nil, c.held, refusal)
			}
			if refusal != nil && refusal.Reason != contract.RefusalDispositionConflict {
				t.Fatalf("the refusal is %q, want disposition_conflict", refusal.Reason)
			}
		})
	}
}

// TestTheCorrectionHoldsAForgeSlugSpelledInAnyCase: an acceptance that recorded a checkout and its forge identity
// is held for the forge slug under any case and padding, and the forge match names the same repository.
func TestTheCorrectionHoldsAForgeSlugSpelledInAnyCase(t *testing.T) {
	for _, spelling := range []string{fxRepo, strings.ToUpper(fxRepo), " " + fxRepo + "\t"} {
		w := newFx(t)
		w.ucLaneRelationship("rel-lane", 2)
		w.exec("UPDATE dag_acceptances SET repository = '/synthetic/checkout' WHERE acceptance_id = 'acc-rel-lane'")
		w.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES ('acc-rel-lane', ?, 1)", fxRepo)
		turn := store.MergeTurnsRow{TurnID: "mtn-forge", TargetKey: "tgt-x", Repository: spelling, BaseRef: fxBase, ProjectKey: fxA,
			HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
		refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
		if err != nil {
			t.Fatal(err)
		}
		if refusal == nil {
			t.Fatalf("the forge slug spelled %q was not held", spelling)
		}
	}
}

// TestTrainLandCarriesAnExcludedMemberWhoseStandMovedByABaseRefresh: a member whose turn was withdrawn holds the
// accepted head H; the member then refreshes its base (dag-base-refresh H', same acceptance, no correction). The
// bundle that carries H is still the accepted result, so landing it records the landing.
func TestTrainLandCarriesAnExcludedMemberWhoseStandMovedByABaseRefresh(t *testing.T) {
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
	acc, err := w.s.All(w.ctx, "SELECT acceptance_id, execution_generation, head_sha FROM dag_acceptances WHERE relationship_id = 'rel-task-m2' AND state = 'active'")
	if err != nil || len(acc) != 1 {
		t.Fatalf("the member's active acceptance: %v", err)
	}
	accID := acc[0].Get("acceptance_id").(string)
	gen := acc[0].Get("execution_generation").(int64)
	const refreshed = "head-m2-refreshed"
	refresh := refreshIDFor(accID, "rel-task-m2", gen, "ev-m2-refresh", "rev-m2-refresh", refreshed, trRepo, trBase, "base-0")
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, ?, 1, 'rel-task-m2', ?, 'ev-m2-refresh', 'rev-m2-refresh', ?, ?, ?, 'base-0', '{}', '[]', 'task-m2', 0, '2023-11-14T22:13:20.000000+00:00')",
		refresh, accID, gen, refreshed, trRepo, trBase)
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	answer, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err != nil {
		t.Fatalf("land with a member whose stand moved by a base refresh: %v", err)
	}
	if answer["state"] != "landed" {
		t.Fatalf("state after land = %v, want landed", answer["state"])
	}
}
