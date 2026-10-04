package review

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

func TestLookupAndRejectedTransitions(t *testing.T) {
	p := reviewTestPlan()
	reviewTestEqual(t, LatestRound(p, goalplan.PurposePlanAudit), (*goalplan.ReviewRoundState)(nil))
	reviewTestEqual(t, RoundByLaunchID(p, goalplan.PurposePlanAudit, ""), (*goalplan.ReviewRoundState)(nil))
	reviewTestEqual(t, AbortRound(p, goalplan.PurposePlanAudit, "why"), ReviewRoundResult{Kind: NotFound, Reason: "no open plan_audit round"})
	reviewTestEqual(t, OpenRound(p, OpenRoundInput{PlanSha256: "hash"}), ReviewRoundResult{Kind: InvalidInput, Reason: "planPath is required"})
	reviewTestEqual(t, MarkInFlight(p, goalplan.PurposePlanAudit, "r1", "l"), ReviewRoundResult{Kind: NotFound, Reason: "no plan_audit round for launch l"})
	f := reviewTestFlight(t)
	reviewTestEqual(t, RoundByLaunchID(f.Plan, goalplan.PurposePlanAudit, f.Round.Lane.LaunchID).RoundID, f.Round.RoundID)
	reviewTestEqual(t, RoundByLaunchID(f.Plan, goalplan.PurposeFinalGate, f.Round.Lane.LaunchID), (*goalplan.ReviewRoundState)(nil))
	done := reviewTestVerdict(t, f, goalplan.VerdictPass)
	reviewTestEqual(t, EffectiveRound(done.Plan, goalplan.PurposePlanAudit), (*goalplan.ReviewRoundState)(nil))
	reviewTestEqual(t, LatestRound(done.Plan, goalplan.PurposePlanAudit).Status, goalplan.ReviewApproved)
	done.Plan.ReviewRounds = append(done.Plan.ReviewRounds, goalplan.ReviewRoundState{RoundID: "r2", Purpose: goalplan.PurposePlanAudit, Status: goalplan.ReviewPending, Lane: goalplan.ReviewLane{LaunchID: "new"}})
	got := MarkInFlight(done.Plan, goalplan.PurposePlanAudit, f.Round.RoundID, f.Round.Lane.LaunchID)
	reviewTestEqual(t, got, ReviewRoundResult{Kind: Stale, Reason: "round r1 was superseded before this verdict arrived"})
	reviewTestEqual(t, LatestRound(done.Plan, goalplan.PurposePlanAudit).RoundID, "r2")
}
func TestMetadataAndTimeSeams(t *testing.T) {
	p := reviewTestPlan()
	calls := 0
	in := OpenRoundInput{Purpose: goalplan.PurposePlanAudit, PlanPath: "p", PlanSha256: "s", Now: func() string { calls++; return "" }}
	r := reviewTestOK(t, OpenRound(p, in))
	reviewTestEqual(t, calls, 1)
	reviewTestEqual(t, r.Round.OpenedAt, "")
	reviewTestEqual(t, r.Round.Lane.LaunchID, "r1-")
	r = reviewTestOK(t, MarkLaunching(r.Plan, r.Round.Purpose, r.Round.RoundID, r.Round.Lane.LaunchID, reviewTestPtr("")))
	reviewTestEqual(t, r.Round.Lane.WorkspaceRoot, reviewTestPtr(""))
	r = reviewTestOK(t, MarkInFlight(r.Plan, r.Round.Purpose, r.Round.RoundID, r.Round.Lane.LaunchID))
	r.Plan.ReviewRounds[0].Lane.ArtifactSha256 = reviewTestPtr("old")
	r.Plan.ReviewRounds[0].Lane.ReviewerSession = reviewTestPtr("")
	done := reviewTestOK(t, RecordVerdict(r.Plan, VerdictInput{Purpose: r.Round.Purpose, RoundID: r.Round.RoundID, LaunchID: r.Round.Lane.LaunchID, Verdict: goalplan.VerdictPass, Now: func() string { calls++; return "" }}))
	reviewTestEqual(t, calls, 2)
	reviewTestEqual(t, done.Round.ClosedAt, reviewTestPtr(""))
	reviewTestEqual(t, done.Round.Lane.ArtifactSha256, reviewTestPtr("old"))
	reviewTestEqual(t, done.Round.Lane.ReviewerSession, reviewTestPtr(""))
	flying := reviewTestFlight(t)
	flying.Plan.ReviewRounds[0].Lane.ReviewerSession = reviewTestPtr("")
	aborted := reviewTestOK(t, AbortRound(flying.Plan, goalplan.PurposePlanAudit, "reason"))
	reviewTestEqual(t, aborted.Round.Lane.ReviewerSession, reviewTestPtr(""))
	same, closed := SupersedeStaleRounds(flying.Plan, goalplan.PurposePlanAudit, "s", "")
	if same != flying.Plan {
		t.Fatal("no-op plan copied")
	}
	reviewTestEqual(t, len(closed), 0)
}
func TestStoredIdentity(t *testing.T) {
	id := source.Identity{Kind: source.KindResolved, CommitSha: "c", Dirty: true, TreeHash: "h", CapturedAt: "t", SourceRoot: reviewTestPtr("root")}
	stored := StoredIdentity(id)
	reviewTestEqual(t, stored.Identity(), id)
	id.TreeHash = ""
	stored = StoredIdentity(id)
	reviewTestEqual(t, stored.TreeHash, (*string)(nil))
	reviewTestEqual(t, stored.Identity(), id)
}
func TestSignoffPositionAndASCIILabels(t *testing.T) {
	for _, s := range []string{"LAUNCH: id\nVERDıCT: PASS", "LAUNCH: id\nVERDICT: PASS\nextra", "VERDICT: PASS\nLAUNCH: id", "LAUNCH: id", "\n", "LAUNCH: id\nVERDICT: PASS."} {
		if got := ParseSignoff(s); got != nil {
			t.Fatalf("accepted %q: %#v", s, got)
		}
	}
	reviewTestEqual(t, ParseSignoff("quoted format\nLAUNCH: id\nVERDICT: PASS"), &ReviewSignoff{LaunchID: "id", Verdict: goalplan.VerdictPass})
}
