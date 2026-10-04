package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

const reviewTestTime = "2026-01-01T00:00:00.000Z"
const reviewTestPath = "devlog/_plan/x/010.md"

func reviewTestNow() string          { return reviewTestTime }
func reviewTestPtr(s string) *string { return &s }
func reviewTestPlan() *goalplan.Goalplan {
	p := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "review round fixture", Now: reviewTestNow})
	p.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	return p
}
func reviewTestEqual[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
func reviewTestOK(t *testing.T, r ReviewRoundResult) ReviewRoundResult {
	t.Helper()
	if r.Kind != OK || r.Plan == nil || r.Round == nil {
		t.Fatalf("expected ok, got %#v", r)
	}
	return r
}
func reviewTestOpen(t *testing.T, p *goalplan.Goalplan, sha string) ReviewRoundResult {
	t.Helper()
	return reviewTestOK(t, OpenRound(p, OpenRoundInput{Purpose: goalplan.PurposePlanAudit, PlanPath: reviewTestPath, PlanSha256: sha, Now: reviewTestNow}))
}
func reviewTestFlight(t *testing.T) ReviewRoundResult {
	t.Helper()
	r := reviewTestOpen(t, reviewTestPlan(), strings.Repeat("a", 64))
	r = reviewTestOK(t, MarkLaunching(r.Plan, r.Round.Purpose, r.Round.RoundID, r.Round.Lane.LaunchID, reviewTestPtr("workspace")))
	return reviewTestOK(t, MarkInFlight(r.Plan, r.Round.Purpose, r.Round.RoundID, r.Round.Lane.LaunchID))
}
func reviewTestVerdict(t *testing.T, r ReviewRoundResult, v goalplan.Verdict) ReviewRoundResult {
	t.Helper()
	return reviewTestOK(t, RecordVerdict(r.Plan, VerdictInput{Purpose: r.Round.Purpose, RoundID: r.Round.RoundID, LaunchID: r.Round.Lane.LaunchID, Verdict: v, Now: reviewTestNow}))
}
func reviewTestWrite(t *testing.T, p *goalplan.Goalplan) (string, string) {
	t.Helper()
	cwd := t.TempDir()
	if err := goalplan.WriteGoalplan(cwd, p); err != nil {
		t.Fatal(err)
	}
	dir, err := goalplan.GoalplanDir(cwd, p.Slug)
	if err != nil {
		t.Fatal(err)
	}
	return cwd, filepath.Join(dir, goalplan.GoalplanFile)
}

// All 23 B-class tests of review-round.test.ts:42-305, in oracle order.
func TestBClass23(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{"R1", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			reviewTestEqual(t, r.Round.RoundID, "r1")
			reviewTestEqual(t, r.Round.Status, goalplan.ReviewPending)
			reviewTestEqual(t, r.Round.PlanSha256, a)
			reviewTestEqual(t, r.Round.Purpose, goalplan.PurposePlanAudit)
			reviewTestEqual(t, r.Plan.ActivePlanAuditRoundID, reviewTestPtr("r1"))
			reviewTestEqual(t, r.Plan.ActiveFinalGateRoundID, (*string)(nil))
			if r.Round.Lane.LaunchID == "" {
				t.Fatal("empty launch")
			}
		}},
		{"R1b", func(t *testing.T) {
			reviewTestEqual(t, OpenRound(reviewTestPlan(), OpenRoundInput{Purpose: goalplan.PurposePlanAudit, PlanPath: reviewTestPath}).Kind, InvalidInput)
		}},
		{"R2", func(t *testing.T) {
			first := reviewTestFlight(t)
			second := reviewTestOpen(t, first.Plan, b)
			reviewTestEqual(t, second.Round.RoundID, "r2")
			late := RecordVerdict(second.Plan, VerdictInput{Purpose: first.Round.Purpose, RoundID: first.Round.RoundID, LaunchID: first.Round.Lane.LaunchID, Verdict: goalplan.VerdictPass})
			reviewTestEqual(t, late.Kind, Stale)
			reviewTestEqual(t, second.Plan.ReviewRounds[0].Status, goalplan.ReviewInconclusive)
			reviewTestEqual(t, second.Plan.ReviewRounds[1].Status, goalplan.ReviewPending)
		}},
		{"R2b", func(t *testing.T) {
			r := reviewTestFlight(t)
			reviewTestEqual(t, RecordVerdict(r.Plan, VerdictInput{Purpose: r.Round.Purpose, RoundID: r.Round.RoundID, LaunchID: "bogus", Verdict: goalplan.VerdictPass}).Kind, Stale)
		}},
		{"R3", func(t *testing.T) {
			f := reviewTestFlight(t)
			first := reviewTestVerdict(t, f, goalplan.VerdictPass)
			again := RecordVerdict(first.Plan, VerdictInput{Purpose: f.Round.Purpose, RoundID: f.Round.RoundID, LaunchID: f.Round.Lane.LaunchID, Verdict: goalplan.VerdictFail})
			reviewTestEqual(t, again.Kind, CASFailed)
			reviewTestEqual(t, first.Plan.ReviewRounds[0].Status, goalplan.ReviewApproved)
			reviewTestEqual(t, first.Plan.ReviewRounds[0].Lane.Verdict, goalplan.VerdictPass)
		}},
		{"R3b", func(t *testing.T) {
			for _, v := range []goalplan.Verdict{goalplan.VerdictPass, goalplan.VerdictNearPass, goalplan.VerdictFail} {
				r := reviewTestVerdict(t, reviewTestFlight(t), v)
				want := goalplan.ReviewApproved
				if v == goalplan.VerdictFail {
					want = goalplan.ReviewChangesRequested
				}
				reviewTestEqual(t, r.Round.Status, want)
				if r.Round.ClosedAt == nil || *r.Round.ClosedAt == "" {
					t.Fatal("no closedAt")
				}
			}
		}},
		{"R4", func(t *testing.T) {
			r := reviewTestFlight(t)
			next := reviewTestOpen(t, r.Plan, b)
			reviewTestEqual(t, next.Plan.ReviewRounds[0].Status, goalplan.ReviewInconclusive)
			reviewTestEqual(t, next.Round.RoundID, "r2")
		}},
		{"R5", func(t *testing.T) {
			first := reviewTestOpen(t, reviewTestPlan(), a)
			launching := reviewTestOK(t, MarkLaunching(first.Plan, first.Round.Purpose, first.Round.RoundID, first.Round.Lane.LaunchID, reviewTestPtr("workspace")))
			next := reviewTestOpen(t, launching.Plan, b)
			reviewTestEqual(t, next.Plan.ReviewRounds[0].Status, goalplan.ReviewInconclusive)
			reviewTestEqual(t, next.Round.RoundID, "r2")
		}},
		{"R6", func(t *testing.T) {
			first := reviewTestOpen(t, reviewTestPlan(), a)
			again := reviewTestOpen(t, first.Plan, b)
			reviewTestEqual(t, again.Round.RoundID, "r1")
			reviewTestEqual(t, again.Round.PlanSha256, b)
			reviewTestEqual(t, len(again.Plan.ReviewRounds), 1)
		}},
		{"R6b", func(t *testing.T) {
			first := reviewTestOpen(t, reviewTestPlan(), a)
			other := reviewTestOK(t, OpenRound(first.Plan, OpenRoundInput{Purpose: goalplan.PurposePlanAudit, PlanPath: "devlog/_plan/x/020.md", PlanSha256: b, Now: reviewTestNow}))
			reviewTestEqual(t, other.Round.RoundID, "r2")
			reviewTestEqual(t, other.Plan.ReviewRounds[0].Status, goalplan.ReviewInconclusive)
		}},
		{"R7", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			got := MarkInFlight(r.Plan, r.Round.Purpose, r.Round.RoundID, r.Round.Lane.LaunchID)
			reviewTestEqual(t, got.Kind, CASFailed)
			reviewTestEqual(t, got.Actual, goalplan.ReviewPending)
		}},
		{"R8", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			r = reviewTestOK(t, MarkLaunching(r.Plan, r.Round.Purpose, r.Round.RoundID, r.Round.Lane.LaunchID, nil))
			got := RecordVerdict(r.Plan, VerdictInput{Purpose: r.Round.Purpose, RoundID: r.Round.RoundID, LaunchID: r.Round.Lane.LaunchID, Verdict: goalplan.VerdictPass})
			reviewTestEqual(t, got.Kind, CASFailed)
			reviewTestEqual(t, got.Actual, goalplan.ReviewLaunching)
		}},
		{"R9", func(t *testing.T) {
			audit := reviewTestFlight(t)
			gate := reviewTestOK(t, OpenRound(audit.Plan, OpenRoundInput{Purpose: goalplan.PurposeFinalGate, PlanPath: reviewTestPath, PlanSha256: a, Now: reviewTestNow}))
			reviewTestEqual(t, gate.Plan.ActivePlanAuditRoundID, reviewTestPtr(audit.Round.RoundID))
			reviewTestEqual(t, gate.Plan.ActiveFinalGateRoundID, reviewTestPtr(gate.Round.RoundID))
			if gate.Round.RoundID == audit.Round.RoundID {
				t.Fatal("purpose ids collided")
			}
			reviewTestEqual(t, gate.Plan.ReviewRounds[0].Status, goalplan.ReviewInFlight)
		}},
		{"R10", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			cwd, file := reviewTestWrite(t, r.Plan)
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err = json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			list := m["reviewRounds"].([]any)
			bad := map[string]any{}
			for k, v := range list[0].(map[string]any) {
				bad[k] = v
			}
			delete(bad, "purpose")
			bad["roundId"] = "r99"
			m["reviewRounds"] = append(list, bad)
			raw, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			back := goalplan.ReadGoalplan(cwd, r.Plan.Slug)
			if back == nil {
				t.Fatal("missing plan")
			}
			reviewTestEqual(t, len(back.ReviewRounds), 1)
			reviewTestEqual(t, back.ReviewRounds[0].RoundID, "r1")
		}},
		{"R11", func(t *testing.T) {
			f := reviewTestFlight(t)
			id := goalplan.SourceIdentity{Kind: source.KindResolved, CommitSha: "deadbee", CapturedAt: reviewTestTime}
			r := reviewTestOK(t, RecordVerdict(f.Plan, VerdictInput{Purpose: f.Round.Purpose, RoundID: f.Round.RoundID, LaunchID: f.Round.Lane.LaunchID, Verdict: goalplan.VerdictPass, ArtifactSha256: &b, ReviewerSession: reviewTestPtr("sess-1"), SourceIdentity: &id, Now: reviewTestNow}))
			reviewTestEqual(t, r.Round.Lane.SourceIdentity.CommitSha, "deadbee")
			reviewTestEqual(t, r.Round.Lane.ArtifactSha256, &b)
			reviewTestEqual(t, r.Round.Lane.ReviewerSession, reviewTestPtr("sess-1"))
		}},
		{"R12", func(t *testing.T) {
			f := reviewTestFlight(t)
			r := reviewTestVerdict(t, f, goalplan.VerdictPass)
			reviewTestEqual(t, StalenessOf(r.Plan, f.Round.RoundID, a), Fresh)
		}},
		{"R13", func(t *testing.T) {
			f := reviewTestFlight(t)
			r := reviewTestVerdict(t, f, goalplan.VerdictNearPass)
			reviewTestEqual(t, r.Round.Status, goalplan.ReviewApproved)
			reviewTestEqual(t, StalenessOf(r.Plan, f.Round.RoundID, b), StalePlan)
		}},
		{"R14", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			reviewTestEqual(t, StalenessOf(r.Plan, r.Round.RoundID, a), Open)
			reviewTestEqual(t, StalenessOf(r.Plan, "nope", a), Open)
		}},
		{"R14b", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			r.Plan.ActivePlanAuditRoundID = reviewTestPtr("r404")
			got := EffectiveRound(r.Plan, goalplan.PurposePlanAudit)
			if got == nil {
				t.Fatal("missing effective round")
			}
			reviewTestEqual(t, got.RoundID, "r1")
		}},
		{"R14c", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			second := *r.Round
			second.RoundID = "r2"
			second.Status = goalplan.ReviewInFlight
			r.Plan.ReviewRounds = append(r.Plan.ReviewRounds, second)
			r.Plan.ActivePlanAuditRoundID = reviewTestPtr("r404")
			got := EffectiveRound(r.Plan, goalplan.PurposePlanAudit)
			if got == nil {
				t.Fatal("missing effective round")
			}
			reviewTestEqual(t, got.RoundID, "r2")
			next := reviewTestOpen(t, r.Plan, b)
			reviewTestEqual(t, next.Round.RoundID, "r3")
			for _, prev := range next.Plan.ReviewRounds[:2] {
				reviewTestEqual(t, prev.Status, goalplan.ReviewInconclusive)
			}
		}},
		{"R14d", func(t *testing.T) {
			r := reviewTestOpen(t, reviewTestPlan(), a)
			second := *r.Round
			second.RoundID = "r2"
			second.Status = goalplan.ReviewInFlight
			r.Plan.ReviewRounds = append(r.Plan.ReviewRounds, second)
			got := EffectiveRound(r.Plan, goalplan.PurposePlanAudit)
			if got == nil {
				t.Fatal("missing effective round")
			}
			reviewTestEqual(t, got.RoundID, "r1")
		}},
		{"R15", func(t *testing.T) {
			f := reviewTestFlight(t)
			id := goalplan.SourceIdentity{Kind: source.KindResolved, CommitSha: "cafe123", Dirty: true, TreeHash: &b, CapturedAt: reviewTestTime}
			done := reviewTestOK(t, RecordVerdict(f.Plan, VerdictInput{Purpose: f.Round.Purpose, RoundID: f.Round.RoundID, LaunchID: f.Round.Lane.LaunchID, Verdict: goalplan.VerdictPass, SourceIdentity: &id, Now: reviewTestNow}))
			gate := reviewTestOK(t, OpenRound(done.Plan, OpenRoundInput{Purpose: goalplan.PurposeFinalGate, PlanPath: reviewTestPath, PlanSha256: a, Now: reviewTestNow}))
			cwd, _ := reviewTestWrite(t, gate.Plan)
			back := goalplan.ReadGoalplan(cwd, gate.Plan.Slug)
			if back == nil {
				t.Fatal("missing plan")
			}
			reviewTestEqual(t, len(back.ReviewRounds), 2)
			reviewTestEqual(t, back.ActiveFinalGateRoundID, reviewTestPtr(gate.Round.RoundID))
			reviewTestEqual(t, back.ActivePlanAuditRoundID, (*string)(nil))
			reviewTestEqual(t, back.ReviewRounds[0].Status, goalplan.ReviewApproved)
			reviewTestEqual(t, back.ReviewRounds[0].Lane.SourceIdentity.TreeHash, &b)
			reviewTestEqual(t, back.ReviewRounds[0].Lane.SourceIdentity.Dirty, true)
		}},
		{"R16", func(t *testing.T) {
			p := reviewTestPlan()
			cwd, _ := reviewTestWrite(t, p)
			back := goalplan.ReadGoalplan(cwd, p.Slug)
			if back == nil {
				t.Fatal("missing plan")
			}
			reviewTestEqual(t, back.ReviewRounds, ([]goalplan.ReviewRoundState)(nil))
			reviewTestEqual(t, back.ActivePlanAuditRoundID, (*string)(nil))
			reviewTestEqual(t, len(back.WorkPhases), 1)
		}},
	}
	reviewTestEqual(t, len(tests), 23)
	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}
