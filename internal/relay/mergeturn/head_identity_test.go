package mergeturn

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

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
