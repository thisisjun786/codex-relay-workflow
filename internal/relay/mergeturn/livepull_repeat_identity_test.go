package mergeturn

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-587: a request is the repeat of the holder's live claim only when it names no identity, or every identity it names
// (the pull request, the relationship) is recorded on the claim and equal. The commit a request names is not an
// identity: two pull requests can point at one commit.

const repeatIdentityCommit = "commit-h"

func repeatIdentityPull(number int64, set bool) sql.NullInt64 {
	return sql.NullInt64{Int64: number, Valid: set}
}

func repeatIdentityRelationship(id string, set bool) sql.NullString {
	return sql.NullString{String: id, Valid: set}
}

func repeatIdentityClaim(w *fx, head string, options ClaimOptions) (map[string]any, error) {
	return w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, head, true, options)
}

// The 2026-10-05 reproduction: pull requests 500 and 501 of owner/repo point at one commit H, the parent claimed with the
// relationship of 500 and no --pr, and a request for pull request 501 at H got the turn of 500 back as alreadyClaimed.
func TestRepeatIdentityPullRequestsAtOneCommitDoNotShareATurn(t *testing.T) {
	w := newFx(t)
	turn := w.must(livePullClaim(w, alpha, fxA, repeatIdentityCommit, 0, "R500"))["turnId"].(string)
	turns := livePullCount(w, "SELECT COUNT(*) FROM merge_turns")
	ledger := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")

	answer, err := livePullClaim(w, alpha, fxA, repeatIdentityCommit, 501, "")
	if answer != nil {
		t.Fatalf("the request for pull request 501 was answered with the turn of the claim made for R500: %v", answer)
	}
	livePullRefusedWith(t, err, turn, "relationship 'R500'", "this request names pull request 501", "does not record the pull request this request names",
		"repeat the request with the arguments the claim was made with (its identity arguments: --relationship 'R500' and no --pr)", "return it with merge-turn-release")
	if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turns"); got != turns {
		t.Errorf("a refused request wrote a turn: %d rows, was %d", got, turns)
	}
	if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger"); got != ledger {
		t.Errorf("a refused request wrote ledger rows: %d, was %d", got, ledger)
	}
	live := w.must(w.m.Turn(w.ctx, turn))
	if live["candidateHead"] != repeatIdentityCommit || live["prNumber"] != nil || live["relationshipId"] != "R500" || live["state"] != Holding {
		t.Errorf("the live turn changed: %v", live)
	}
	key, _ := TargetKey(fxRepo, fxBase)
	contests, err := w.r.CoordinationConflicts(w.ctx, registry.DomainMergeTarget, key)
	if err != nil || len(contests) != 1 || contests[0].Get("reason") != "disposition_conflict" || contests[0].Get("incumbent") != turn {
		t.Errorf("the refusal was not kept as a contest of the target naming the live turn: %v %v", contests, err)
	}

	// the claim's own arguments are still its repeat
	again := w.must(livePullClaim(w, alpha, fxA, repeatIdentityCommit, 0, "R500"))
	if again["turnId"] != turn || again["alreadyClaimed"] != true {
		t.Fatalf("the claim's own arguments are not its repeat: %v", again)
	}
	// and once it is returned, pull request 501 gets a turn of its own at the same commit
	w.must(w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "pull request 501 goes next", ""))
	second := w.must(livePullClaim(w, alpha, fxA, repeatIdentityCommit, 501, ""))
	if second["turnId"] == turn || second["prNumber"] != int64(501) || second["state"] != Holding || second["candidateHead"] != repeatIdentityCommit {
		t.Fatalf("the turn of pull request 501: %v", second)
	}
}

// A claim made with no identity is the repeat of a request that names none, and of no request that names one, whatever
// commit the request states.
func TestRepeatIdentityBareClaimIsRepeatedOnlyWithoutIdentity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pr           int64
		relationship string
		head         string
	}{
		{"a pull request at the claim's commit", 501, "", repeatIdentityCommit},
		{"a pull request at another commit", 501, "", "commit-other"},
		{"a relationship at the claim's commit", 0, "R501", repeatIdentityCommit},
		{"both at the claim's commit", 501, "R501", repeatIdentityCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFx(t)
			turn := w.must(livePullClaim(w, alpha, fxA, repeatIdentityCommit, 0, ""))["turnId"].(string)
			answer, err := livePullClaim(w, alpha, fxA, tc.head, tc.pr, tc.relationship)
			if answer != nil {
				t.Fatalf("a bare claim was answered to a request naming an identity: %v", answer)
			}
			livePullRefusedWith(t, err, turn, "no pull request or relationship", "does not record the",
				"repeat the request with the arguments the claim was made with (its identity arguments: neither --pr nor --relationship)")
			again := w.must(livePullClaim(w, alpha, fxA, repeatIdentityCommit, 0, ""))
			if again["turnId"] != turn || again["alreadyClaimed"] != true || again["candidateHead"] != repeatIdentityCommit {
				t.Fatalf("the bare claim repeated without identity is not the same turn: %v", again)
			}
			if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turns"); got != 1 {
				t.Fatalf("%d turns", got)
			}
		})
	}
}

// A claim made with a pull request and no relationship says so in the refusal, so the caller repeats it with that.
func TestRepeatIdentityRefusalSaysWhatAPullRequestOnlyClaimWasMadeWith(t *testing.T) {
	w := newFx(t)
	turn := w.must(livePullClaim(w, alpha, fxA, repeatIdentityCommit, 500, ""))["turnId"].(string)
	_, err := livePullClaim(w, alpha, fxA, repeatIdentityCommit, 0, "R500")
	livePullRefusedWith(t, err, turn, "pull request 500", "does not record the relationship this request names",
		"repeat the request with the arguments the claim was made with (its identity arguments: --pr 500 and no --relationship)")
}

// Which values count as naming or recording an identity: a pull request when its field is set (whatever the number, 0
// included), a relationship when its field is set and not empty, and nothing for a value left in a field that is not
// set. A request that contradicts the turn says so, also when it names an identity the turn does not record besides.
func TestRepeatIdentityWhichValuesNameAnIdentity(t *testing.T) {
	const contradiction = "is not answered with the turn it already has"
	const unrecorded = "does not record the"
	for _, tc := range []struct {
		name   string
		live   ClaimOptions
		asked  ClaimOptions
		repeat bool
		says   string
	}{
		{"a pull request numbered 0 against a claim recording none", ClaimOptions{}, ClaimOptions{PR: repeatIdentityPull(0, true)}, false, "does not record the pull request this request names"},
		{"a pull request numbered 0 against a claim recording the same", ClaimOptions{PR: repeatIdentityPull(0, true)}, ClaimOptions{PR: repeatIdentityPull(0, true)}, true, ""},
		{"a pull request number left in a field that is not set", ClaimOptions{}, ClaimOptions{PR: repeatIdentityPull(501, false)}, true, ""},
		{"a pull request number left in a field that is not set beside a recorded one", ClaimOptions{PR: repeatIdentityPull(500, true)}, ClaimOptions{PR: repeatIdentityPull(501, false)}, true, ""},
		{"an empty relationship in a set field", ClaimOptions{}, ClaimOptions{Relationship: repeatIdentityRelationship("", true)}, true, ""},
		{"a relationship left in a field that is not set", ClaimOptions{}, ClaimOptions{Relationship: repeatIdentityRelationship("R500", false)}, true, ""},
		{"a claim holding an empty relationship records none", ClaimOptions{Relationship: repeatIdentityRelationship("", true)}, ClaimOptions{Relationship: repeatIdentityRelationship("R500", true)}, false, "does not record the relationship this request names"},
		{"neither identity recorded by the claim", ClaimOptions{}, ClaimOptions{PR: repeatIdentityPull(501, true), Relationship: repeatIdentityRelationship("R501", true)}, false, "does not record the pull request and the relationship this request names"},
		{"a pull request that contradicts beside a relationship the claim does not record", ClaimOptions{PR: repeatIdentityPull(500, true)}, ClaimOptions{PR: repeatIdentityPull(501, true), Relationship: repeatIdentityRelationship("R501", true)}, false, contradiction},
		{"a relationship that contradicts beside a pull request the claim does not record", ClaimOptions{Relationship: repeatIdentityRelationship("R500", true)}, ClaimOptions{PR: repeatIdentityPull(501, true), Relationship: repeatIdentityRelationship("R501", true)}, false, contradiction},
		{"both identities recorded and equal", ClaimOptions{PR: repeatIdentityPull(500, true), Relationship: repeatIdentityRelationship("R500", true)}, ClaimOptions{PR: repeatIdentityPull(500, true), Relationship: repeatIdentityRelationship("R500", true)}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFx(t)
			turn := w.must(repeatIdentityClaim(w, repeatIdentityCommit, tc.live))["turnId"].(string)
			answer, err := repeatIdentityClaim(w, repeatIdentityCommit, tc.asked)
			if tc.repeat {
				if err != nil || answer["turnId"] != turn || answer["alreadyClaimed"] != true {
					t.Fatalf("not the repeat of the live turn: %v %v", answer, err)
				}
				return
			}
			if answer != nil {
				t.Fatalf("answered with the live turn: %v", answer)
			}
			livePullRefusedWith(t, err, turn, tc.says)
			if tc.says == contradiction && strings.Contains(livePullDetail(err), unrecorded) {
				t.Errorf("a request that contradicts the turn is said to name what it does not record: %s", livePullDetail(err))
			}
			if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turns"); got != 1 {
				t.Fatalf("%d turns", got)
			}
		})
	}
}
