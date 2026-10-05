package mergeturn

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// livePullTicking gives every request its own instant, so the order the lane serves claims in is the order they were made.
func livePullTicking(w *fx) {
	var tick int64
	w.m.Now = func() string {
		tick++
		return registry.ISO(time.Unix(1_700_000_000+tick, 0))
	}
}

func livePullRefusedWith(t *testing.T, err error, wants ...string) {
	t.Helper()
	detail := livePullDetail(err)
	if reasonOf(err) != "disposition_conflict" {
		t.Fatalf("not refused as disposition_conflict: %v", err)
	}
	for _, want := range wants {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal does not say %q: %s", want, detail)
		}
	}
}

// The 2026-10-04 case: the parent holds the turn of pull request 500 and asks for one for pull request 501.
func TestLivePullAnotherPullRequestRequestIsRefusedNamingTheLiveTurn(t *testing.T) {
	w := newFx(t)
	turn := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, "rel-500"))["turnId"].(string)
	turns := livePullCount(w, "SELECT COUNT(*) FROM merge_turns")
	ledger := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")

	answer, err := livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501")
	if answer != nil {
		t.Fatalf("the request for another pull request was answered: %v", answer)
	}
	livePullRefusedWith(t, err, turn, "pull request 500", "rel-500", "head-500", "holding", "place 1 of 1", "pull request 501", "rel-501", "is not answered with the turn it already has")
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

// A claim that waits has a place in the order too, counted among the live claims in the order the lane serves them:
// the holder first, then the waiting claims by the time they were made.
func TestLivePullRefusalNamesThePlaceInTheOrder(t *testing.T) {
	w := newFx(t)
	livePullTicking(w)
	gamma := ep("task-gamma", "host-c", "/gamma")
	w.bindParent("PRJ-C", gamma)
	w.must(livePullClaim(w, beta, fxB, "head-b", 9, ""))
	first := w.must(livePullClaim(w, gamma, "PRJ-C", "head-c", 8, ""))
	mine := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, ""))
	if first["state"] != Waiting || mine["state"] != Waiting {
		t.Fatalf("the claims behind another project's turn are %v and %v", first["state"], mine["state"])
	}
	_, err := livePullClaim(w, alpha, fxA, "head-501", 501, "")
	livePullRefusedWith(t, err, mine["turnId"].(string), "waiting", "place 3 of 3")
	_, err = livePullClaim(w, gamma, "PRJ-C", "head-c2", 7, "")
	livePullRefusedWith(t, err, first["turnId"].(string), "waiting", "place 2 of 3")
}

// What is another pull request. A request that states no identity is the live turn's replay. One that states some is its
// replay only when every one of them is recorded on the turn and equal (CRW-587). An identity the turn does not record
// cannot be told from another pull request's, whatever commit the request names: two pull requests can point at one commit.
func TestLivePullWhatCountsAsAnotherPullRequest(t *testing.T) {
	const own = "head-500"
	for _, tc := range []struct {
		name             string
		livePR           int64
		liveRelationship string
		askPR            int64
		askRelationship  string
		askHead          string
		refused          bool
	}{
		{"another pull request and the same relationship", 500, "rel-500", 501, "rel-500", "", true},
		{"another pull request only", 500, "rel-500", 501, "", "", true},
		{"another relationship only", 0, "rel-500", 0, "rel-501", "", true},
		{"the same pull request and another relationship", 500, "rel-500", 500, "rel-501", "", true},
		{"another pull request at the turn's own head", 500, "rel-500", 501, "", own, true},
		{"a pull request against a claim recording only a relationship", 0, "rel-500", 501, "", "", true},
		{"a relationship against a claim recording only a pull request", 500, "", 0, "rel-500", "", true},
		{"identities against a claim recording none", 0, "", 501, "rel-501", "", true},
		{"the same pull request and relationship", 500, "rel-500", 500, "rel-500", "", false},
		{"the same pull request only", 500, "rel-500", 500, "", "", false},
		{"the same relationship only", 500, "rel-500", 0, "rel-500", "", false},
		{"nothing stated", 500, "rel-500", 0, "", "", false},
		{"nothing stated against a claim recording none", 0, "", 0, "", "", false},
		{"a relationship that agrees beside a pull request the claim does not record", 0, "rel-500", 501, "rel-500", "", true},
		{"a pull request that agrees beside a relationship the claim does not record", 500, "", 500, "rel-other", "", true},
		{"a pull request against a claim recording only a relationship, at its own head", 0, "rel-500", 500, "", own, true},
		{"a relationship against a claim recording only a pull request, at its own head", 500, "", 0, "rel-500", own, true},
		{"identities added to a claim recording none, at its own head", 0, "", 500, "rel-500", own, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFx(t)
			turn := w.must(livePullClaim(w, alpha, fxA, own, tc.livePR, tc.liveRelationship))["turnId"].(string)
			head := tc.askHead
			if head == "" {
				head = "head-ask"
			}
			answer, err := livePullClaim(w, alpha, fxA, head, tc.askPR, tc.askRelationship)
			if tc.refused {
				if reasonOf(err) != "disposition_conflict" || answer != nil {
					t.Fatalf("not refused: %v %v", answer, err)
				}
				return
			}
			if err != nil || answer["turnId"] != turn || answer["alreadyClaimed"] != true || answer["candidateHead"] != own {
				t.Fatalf("the repeated request is not the same turn: %v %v", answer, err)
			}
			if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turns"); got != 1 {
				t.Fatalf("%d turns", got)
			}
		})
	}
}

// A refusal says which kind it is: a request that contradicts the turn, or one the turn cannot confirm.
func TestLivePullRefusalSaysWhyTheRequestIsNotTheLiveTurns(t *testing.T) {
	w := newFx(t)
	turn := w.must(livePullClaim(w, alpha, fxA, "head-500", 0, ""))["turnId"].(string)
	_, err := livePullClaim(w, alpha, fxA, "head-501", 501, "")
	livePullRefusedWith(t, err, turn, "no pull request or relationship", "does not record the pull request this request names", "repeat the request with the arguments the claim was made with (its identity arguments: neither --pr nor --relationship)", "request a turn with other or additional identities, return it with merge-turn-release")
	if strings.Contains(livePullDetail(err), "is not answered with the turn it already has") {
		t.Errorf("a request the turn cannot confirm is not a request for another pull request: %s", livePullDetail(err))
	}

	w2 := newFx(t)
	w2.must(livePullClaim(w2, alpha, fxA, "head-500", 500, "rel-500"))
	_, err = livePullClaim(w2, alpha, fxA, "head-501", 501, "rel-501")
	livePullRefusedWith(t, err, "is not answered with the turn it already has", "repeat the request with the arguments the claim was made with (its identity arguments: --pr 500 --relationship 'rel-500')", "request a turn with other or additional identities, return it with merge-turn-release")
	if strings.Contains(livePullDetail(err), "does not record the") {
		t.Errorf("a contradiction is not an unconfirmed identity: %s", livePullDetail(err))
	}
}

// A turn whose merge began or whose outcome is unknown is still the parent's one live turn for the target.
func TestLivePullRefusalNamesAMergingAndAnUnknownTurn(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	if _, err := livePullCheck(w, turn, "head-500", livePullReader{target: w.target, pulls: pulls}); err != nil {
		t.Fatal(err)
	}
	_, err := livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501")
	livePullRefusedWith(t, err, turn, "merging", "place 1 of 1", "merge-turn-land")
	w.must(w.m.Unknown(w.ctx, turn, alpha.TaskID, "lost the connection"))
	_, err = livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501")
	livePullRefusedWith(t, err, turn, "unknown", "place 1 of 1", "merge-turn-resolve")
}

// Once the earlier turn is returned, the other pull request gets its own turn, and a closed turn is no longer counted.
func TestLivePullOtherPullRequestIsGrantedAfterTheEarlierTurnIsReturned(t *testing.T) {
	w := newFx(t)
	first := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, "rel-500"))["turnId"].(string)
	_, err := livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501")
	livePullRefusedWith(t, err, first, "holding")
	w.must(w.m.Release(w.ctx, first, alpha.TaskID, "returned", "the next pull request goes first", ""))
	second := w.must(livePullClaim(w, alpha, fxA, "head-501", 501, "rel-501"))
	if second["turnId"] == first || second["prNumber"] != int64(501) || second["relationshipId"] != "rel-501" || second["state"] != Holding || second["tenure"] != int64(2) {
		t.Fatalf("the second pull request's turn: %v", second)
	}
	_, err = livePullClaim(w, alpha, fxA, "head-502", 502, "rel-502")
	livePullRefusedWith(t, err, second["turnId"].(string), "pull request 501", "place 1 of 1")
}
