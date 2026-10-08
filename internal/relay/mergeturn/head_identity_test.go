package mergeturn

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// refreshIDFor is the id acceptance.Stands accepts for a base refresh row: the digest of its fields.
func refreshIDFor(acceptanceID, relationship string, generation int64, event, revision, head, repository, ref, tip string) string {
	return acceptance.RefreshDigest(acceptanceID, relationship, generation, event, revision, head, repository, ref, tip, "{}", "[]")
}

// CRW-906 generation 2, round 8: a base refresh keeps a superseded head on its acceptance only when the relay
// validly recorded it. A refresh row whose id is not the digest of its fields does not keep the head.
func TestAnInvalidBaseRefreshDoesNotKeepASupersededHead(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-c", 2)
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, 'acc-rel-c', 1, 'rel-c', 1, 'ev-r1', 'rev-r1', 'head-refreshed', ?, ?, 'base-0', '{}', '[]', ?, 0, '2023-11-14T22:13:20.000000+00:00')",
		"dbr-"+strings.Repeat("c", 60), fxRepo, fxBase, alpha.TaskID)
	w.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE relationship_id = 'rel-c'")
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-rel-c2', 'plan-x', 'node-c', 'manifest-c2', 'rel-c', 2, 'ev-c2', 'rev-c2', 'crit-1', 'verified', 'head-b', ?, 1, 'bound', 'turn-1', '{}', ?, 0, '2023-11-14T22:13:21.000000+00:00', 'active')",
		fxRepo, alpha.TaskID)
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, 'acc-rel-c2', 1, 'rel-c', 2, 'ev-r2', 'rev-r2', 'head-refreshed', ?, ?, 'base-0', '{}', '[]', ?, 0, '2023-11-14T22:13:22.000000+00:00')",
		"dbr-"+strings.Repeat("d", 60), fxRepo, fxBase, alpha.TaskID)
	turn := store.MergeTurnsRow{TurnID: "mtn-invalid-refresh", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-refreshed", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a superseded head kept by a refresh row whose id is not its digest was not refused: %+v", refusal)
	}
}

// CRW-906 generation 2, round 7: one head identity. Every spelling below names the same commit under
// SameCommit, so the lane gate must refuse a turn holding any of them when it holds the accepted head.
var headIdentitySpellings = []string{
	"HEAD-A", "head-a", " head-a", "\thead-a\n", "head-a\v", "\fhead-a\r",
	"\u00a0head-a", "head-a\u0085", "\u2003head-a", "head-a\u3000",
}

func TestTheLaneGateAgreesWithSameCommitForEverySpelling(t *testing.T) {
	for _, head := range headIdentitySpellings {
		t.Run(fmt.Sprintf("%q", head), func(t *testing.T) {
			w := newFx(t)
			w.ucLaneRelationship("rel-lane", 2)
			// The accepted head is stored in the spelling under test (a merge turn or an acceptance is stored
			// verbatim), and the turn holds the clean accepted head: SameCommit names them the same commit.
			w.exec("UPDATE dag_acceptances SET head_sha = ? WHERE acceptance_id = ?", head, "acc-rel-lane")
			turn := store.MergeTurnsRow{TurnID: "mtn-spelling", TargetKey: "tgt-x", Repository: fxRepo, BaseRef: fxBase, ProjectKey: fxA,
				HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
			refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
			if err != nil {
				t.Fatal(err)
			}
			if !SameCommit(head, "head-a") {
				t.Fatalf("SameCommit does not name the accepted head for %q", head)
			}
			if refusal == nil {
				t.Fatalf("the lane admitted a turn holding the accepted head stored as %q", head)
			}
		})
	}
}

// The head column of a SQL comparison must go through the one head definition, never a hand-written trim
// or lower: the next query that trims a head is caught here instead of by an evaluation.
func TestNoSQLTrimsOrLowersAHeadOrShaColumn(t *testing.T) {
	// the call must take a head or sha column itself, so a repository lower() on the same line is not a head
	headCall := regexp.MustCompile(`(?i)\b(trim|lower)\((\w+\.)?\w*(head|sha)`)
	for _, dir := range []string{".", "../dagsched"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for n, line := range strings.Split(string(body), "\n") {
				if headCall.MatchString(line) {
					t.Errorf("%s:%d applies trim or lower to a head or sha; compare with crw_same_commit instead: %s", file, n+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// CRW-906 generation 2, round 11: an acceptance can record a local checkout and a forge identity; a turn held under
// the local path is the same node, so the lane gate matches it by either name.
func TestTheLaneGateMatchesALocalPathTurnWhenTheAcceptanceHasAForgeIdentity(t *testing.T) {
	w := newFx(t)
	w.ucLaneRelationship("rel-lane", 2)
	w.exec("UPDATE dag_acceptances SET repository = '/synthetic/checkout' WHERE acceptance_id = 'acc-rel-lane'")
	w.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES ('acc-rel-lane', ?, 1)", fxRepo)
	turn := store.MergeTurnsRow{TurnID: "mtn-local-path", TargetKey: "tgt-x", Repository: "/synthetic/checkout", BaseRef: fxBase, ProjectKey: fxA,
		HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
	refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != contract.RefusalDispositionConflict {
		t.Fatalf("a turn held under the local path of an accepted head with a forge identity was not refused: %+v", refusal)
	}
}
