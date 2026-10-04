package mergeturn

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-538: a parent holds one live turn per target. A request that names another pull request (or another
// relationship) than the one the live turn is bound to is refused and says which turn it is; the same pull
// request asked again keeps getting the same turn.

func livePullOptions(pr int64, relationship string) ClaimOptions {
	o := ClaimOptions{}
	if pr != 0 {
		o.PR = nullInt(pr)
	}
	if relationship != "" {
		o.Relationship = sql.NullString{String: relationship, Valid: true}
	}
	return o
}

func livePullClaim(w *fx, endpoint registry.Endpoint, project, head string, pr int64, relationship string) (map[string]any, error) {
	return w.m.Request(w.ctx, fxRepo, fxBase, project, endpoint.TaskID, endpoint.HostID, head, true, livePullOptions(pr, relationship))
}

func livePullDetail(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Detail
	}
	return fmt.Sprint(err)
}

func livePullCount(w *fx, query string, args ...any) int64 {
	w.t.Helper()
	var n int64
	if err := w.s.DB.QueryRowContext(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// The 2026-10-04 case: the parent holds the turn of pull request 500 and asks for one for pull request 501.
func TestLivePullAnotherPullRequestRequestIsRefusedNamingTheLiveTurn(t *testing.T) {
	w := newFx(t)
	turn := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, "rel-500"))["turnId"].(string)
	turns := livePullCount(w, "SELECT COUNT(*) FROM merge_turns")
	ledger := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")

	answer, err := livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501")
	if reasonOf(err) != "disposition_conflict" || answer != nil {
		t.Fatalf("the request for another pull request was not refused as disposition_conflict: %v %v", answer, err)
	}
	detail := livePullDetail(err)
	for _, want := range []string{turn, "pull request 500", "rel-500", "holding", "place 1 of 1", "pull request 501", "rel-501"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal does not say %q: %s", want, detail)
		}
	}
	if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turns"); got != turns {
		t.Errorf("a refused request wrote a turn: %d rows, was %d", got, turns)
	}
	if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger"); got != ledger {
		t.Errorf("a refused request wrote ledger rows: %d, was %d", got, ledger)
	}
	live := w.must(w.m.Turn(w.ctx, turn))
	if live["candidateHead"] != "head-500" || live["prNumber"] != int64(500) || live["state"] != Holding {
		t.Errorf("the live turn changed: %v", live)
	}
	key, _ := TargetKey(fxRepo, fxBase)
	contests, err := w.r.CoordinationConflicts(w.ctx, registry.DomainMergeTarget, key)
	if err != nil || len(contests) != 1 || contests[0].Get("reason") != "disposition_conflict" || contests[0].Get("incumbent") != turn {
		t.Errorf("the refusal was not kept as a contest of the target naming the live turn: %v %v", contests, err)
	}
}

// A claim that waits behind another project's turn has a place in the order too, and says it.
func TestLivePullRefusalNamesTheWaitingPlace(t *testing.T) {
	w := newFx(t)
	w.must(livePullClaim(w, beta, fxB, "head-b", 9, ""))
	waiting := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, ""))
	if waiting["state"] != Waiting {
		t.Fatalf("the claim behind another project's turn is %v", waiting["state"])
	}
	_, err := livePullClaim(w, alpha, fxA, "head-501", 501, "")
	detail := livePullDetail(err)
	if reasonOf(err) != "disposition_conflict" || !strings.Contains(detail, waiting["turnId"].(string)) || !strings.Contains(detail, "waiting") || !strings.Contains(detail, "place 2 of 2") {
		t.Fatalf("the waiting claim's refusal: %v", err)
	}
}

// What is another pull request: a recorded pull request or relationship that the request contradicts.
func TestLivePullWhatCountsAsAnotherPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name             string
		livePR           int64
		liveRelationship string
		askPR            int64
		askRelationship  string
		refused          bool
	}{
		{"another pull request", 500, "rel-500", 501, "rel-500", true},
		{"another pull request only", 500, "", 501, "", true},
		{"another relationship only", 0, "rel-500", 0, "rel-501", true},
		{"same pull request and relationship", 500, "rel-500", 500, "rel-500", false},
		{"same pull request only", 500, "rel-500", 500, "", false},
		{"same relationship only", 500, "rel-500", 0, "rel-500", false},
		{"nothing stated", 500, "rel-500", 0, "", false},
		{"the live claim records no pull request", 0, "rel-500", 501, "rel-500", false},
		{"the live claim records no relationship", 500, "", 500, "rel-other", false},
		{"the live claim records nothing", 0, "", 501, "rel-501", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFx(t)
			turn := w.must(livePullClaim(w, alpha, fxA, "head-500", tc.livePR, tc.liveRelationship))["turnId"].(string)
			answer, err := livePullClaim(w, alpha, fxA, "head-ask", tc.askPR, tc.askRelationship)
			if tc.refused {
				if reasonOf(err) != "disposition_conflict" {
					t.Fatalf("not refused: %v %v", answer, err)
				}
				return
			}
			if err != nil || answer["turnId"] != turn || answer["alreadyClaimed"] != true || answer["candidateHead"] != "head-500" {
				t.Fatalf("the repeated request is not the same turn: %v %v", answer, err)
			}
			if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turns"); got != 1 {
				t.Fatalf("%d turns", got)
			}
		})
	}
}

// Once the earlier turn is returned, the other pull request gets its own turn.
func TestLivePullOtherPullRequestIsGrantedAfterTheEarlierTurnIsReturned(t *testing.T) {
	w := newFx(t)
	first := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, "rel-500"))["turnId"].(string)
	if _, err := livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501"); reasonOf(err) != "disposition_conflict" {
		t.Fatalf("not refused while the first turn is live: %v", err)
	}
	w.must(w.m.Release(w.ctx, first, alpha.TaskID, "returned", "the next pull request goes first", ""))
	second := w.must(livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501"))
	if second["turnId"] == first || second["prNumber"] != int64(501) || second["relationshipId"] != "rel-501" || second["state"] != Holding || second["tenure"] != int64(2) {
		t.Fatalf("the second pull request's turn: %v", second)
	}
}
