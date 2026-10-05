package review

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// CRW-573: the oracle mints "highest stored id + 1" in float64, so from 2^53 up the new id repeats a stored one, and it
// replaces every entry with that id when a round changes, which overwrites an approved round once the plan is written.
// The port refuses to mint such an id and replaces only the round that changed.

func reviewRoundIDEntry(id string, purpose goalplan.ReviewPurpose, status goalplan.ReviewRoundStatus, launch, sha string) goalplan.ReviewRoundState {
	return goalplan.ReviewRoundState{RoundID: id, Purpose: purpose, PlanPath: reviewTestPath, PlanSha256: sha, Status: status, Lane: goalplan.ReviewLane{LaunchID: launch}, OpenedAt: reviewTestTime}
}
func reviewRoundIDPlan(rounds ...goalplan.ReviewRoundState) *goalplan.Goalplan {
	p := reviewTestPlan()
	p.ReviewRounds = rounds
	return p
}

func TestReviewRoundIDRefused(t *testing.T) {
	const stored, limit = "is already stored", "not an exact integer below 2^53"
	for _, c := range []struct{ id, reason string }{
		{"r9007199254740992", stored}, // 2^53: the minted id equals it
		{"r100000000000000000000", stored},
		{"r9007199254740991", limit}, // 2^53-1: the next id would be 2^53
		{"r9007199254740993", limit}, // parses to 2^53
		{"r999999999999999999999", limit},
	} {
		t.Run(c.id, func(t *testing.T) {
			p := reviewRoundIDPlan(reviewRoundIDEntry(c.id, goalplan.PurposePlanAudit, goalplan.ReviewApproved, c.id+"-launch", "hash-a"))
			before := reviewOracleJSON(t, p)
			r := OpenRound(p, OpenRoundInput{Purpose: goalplan.PurposePlanAudit, PlanPath: reviewTestPath, PlanSha256: "hash-b", Now: reviewTestNow})
			if r.Kind != InvalidInput || r.Plan != nil || r.Round != nil || !strings.Contains(r.Reason, c.reason) {
				t.Fatalf("got %#v, want an invalid_input refusal containing %q", r, c.reason)
			}
			reviewTestEqual(t, reviewOracleJSON(t, p), before)
		})
	}
}

func TestReviewRoundIDBelowLimitUnchanged(t *testing.T) {
	for _, c := range []struct{ stored, next string }{
		{"r9007199254740990", "r9007199254740991"}, // the last id below 2^53
		{"r01", "r2"}, {"r+10tail", "r11"}, {"r41", "r42"},
	} {
		t.Run(c.stored, func(t *testing.T) {
			r := reviewTestOpen(t, reviewRoundIDPlan(reviewRoundIDEntry(c.stored, goalplan.PurposePlanAudit, goalplan.ReviewApproved, c.stored+"-launch", "hash-a")), "hash-b")
			reviewTestEqual(t, r.Round.RoundID, c.next)
			reviewTestEqual(t, r.Round.Lane.LaunchID, c.next+"-20260101000000")
			reviewTestEqual(t, len(r.Plan.ReviewRounds), 2)
		})
	}
}

// A pending round of the same document is refreshed without minting, so a damaged plan stays usable.
func TestReviewRoundIDPendingReuseAtLimit(t *testing.T) {
	id := "r9007199254740992"
	r := reviewTestOpen(t, reviewRoundIDPlan(reviewRoundIDEntry(id, goalplan.PurposePlanAudit, goalplan.ReviewPending, id+"-launch", "hash-a")), "hash-b")
	reviewTestEqual(t, r.Round.RoundID, id)
	reviewTestEqual(t, r.Round.PlanSha256, "hash-b")
	reviewTestEqual(t, len(r.Plan.ReviewRounds), 1)
}

func TestReviewRoundIDReplacesOnlyTheExactRound(t *testing.T) {
	pa, fg, e, ws := goalplan.PurposePlanAudit, goalplan.PurposeFinalGate, reviewRoundIDEntry, reviewTestPtr("workspace")
	for _, c := range []struct {
		name    string
		rounds  []goalplan.ReviewRoundState
		changed int
		run     func(*goalplan.Goalplan) ReviewRoundResult
	}{
		{"launching, same id and launch other purpose", []goalplan.ReviewRoundState{e("r1", fg, goalplan.ReviewApproved, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-b")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult { return MarkLaunching(p, pa, "r1", "r1-x", ws) }},
		{"launching, same id and purpose other launch", []goalplan.ReviewRoundState{e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-y", "hash-a")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult { return MarkLaunching(p, pa, "r1", "r1-y", ws) }},
		{"launching, other id same purpose and launch", []goalplan.ReviewRoundState{e("r2", pa, goalplan.ReviewPending, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-a")}, 0,
			func(p *goalplan.Goalplan) ReviewRoundResult { return MarkLaunching(p, pa, "r2", "r1-x", ws) }},
		{"launching, two pending rounds with one identity", []goalplan.ReviewRoundState{e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-b")}, 0,
			func(p *goalplan.Goalplan) ReviewRoundResult { return MarkLaunching(p, pa, "r1", "r1-x", ws) }},
		{"hash refresh, pending round with the identity of an approved one", []goalplan.ReviewRoundState{e("r1", pa, goalplan.ReviewApproved, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-b")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult {
				return OpenRound(p, OpenRoundInput{Purpose: pa, PlanPath: reviewTestPath, PlanSha256: "hash-c", Now: reviewTestNow})
			}},
		{"abort, in-flight round with the identity of an approved one", []goalplan.ReviewRoundState{e("r1", pa, goalplan.ReviewApproved, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewInFlight, "r1-x", "hash-b")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult { return AbortRound(p, pa, "stop") }},
		{"verdict", []goalplan.ReviewRoundState{e("r1", fg, goalplan.ReviewApproved, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewInFlight, "r1-x", "hash-b")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult {
				return RecordVerdict(p, VerdictInput{Purpose: pa, RoundID: "r1", LaunchID: "r1-x", Verdict: goalplan.VerdictFail, Now: reviewTestNow})
			}},
		{"abort", []goalplan.ReviewRoundState{e("r1", fg, goalplan.ReviewInFlight, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-b")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult { return AbortRound(p, pa, "stop") }},
		{"hash refresh of a pending round", []goalplan.ReviewRoundState{e("r1", fg, goalplan.ReviewPending, "r1-x", "hash-a"), e("r1", pa, goalplan.ReviewPending, "r1-x", "hash-a")}, 1,
			func(p *goalplan.Goalplan) ReviewRoundResult {
				return OpenRound(p, OpenRoundInput{Purpose: pa, PlanPath: reviewTestPath, PlanSha256: "hash-b", Now: reviewTestNow})
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := reviewRoundIDPlan(c.rounds...)
			got := reviewTestOK(t, c.run(p))
			reviewTestEqual(t, len(got.Plan.ReviewRounds), len(c.rounds))
			for i, want := range c.rounds {
				if i == c.changed {
					want = *got.Round
				}
				reviewTestEqual(t, got.Plan.ReviewRounds[i], want)
			}
		})
	}
}

// What the issue observed: the plan the transition returns, once written and read back, still holds the approved round.
func TestReviewRoundIDApprovedRoundSurvivesWrite(t *testing.T) {
	revive := func(p *goalplan.Goalplan) []goalplan.ReviewRoundState {
		cwd, _ := reviewTestWrite(t, p)
		revived := goalplan.ReadGoalplan(cwd, p.Slug)
		if revived == nil {
			t.Fatal("revival failed")
		}
		return revived.ReviewRounds
	}
	// The approved round has the pending round's id and launch id, and in the second case its purpose too.
	for _, purpose := range []goalplan.ReviewPurpose{goalplan.PurposeFinalGate, goalplan.PurposePlanAudit} {
		t.Run(string(purpose), func(t *testing.T) {
			approved := reviewRoundIDEntry("r1", purpose, goalplan.ReviewApproved, "r1-x", "hash-a")
			approved.Lane.Verdict = goalplan.VerdictPass
			p := reviewRoundIDPlan(approved, reviewRoundIDEntry("r1", goalplan.PurposePlanAudit, goalplan.ReviewPending, "r1-x", "hash-b"))
			got := reviewTestOpen(t, p, "hash-c")
			reviewTestEqual(t, revive(got.Plan)[0], revive(p)[0])
		})
	}
}
