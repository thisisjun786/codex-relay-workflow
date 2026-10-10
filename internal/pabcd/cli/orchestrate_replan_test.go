package cli

// The CRW-1100 suite: a re-plan P>A closes every open plan_audit round this session owns under an earlier
// epoch, and only those; the binding and the cleanup are one recorded piece of work, so a failure before the
// publication leaves everything as it was and a cleanup that fails after it stays pending and is finished,
// with no duplicate superseded row, by the next call of the session. The cases marked red reproduce on dev
// (6a5851f4f).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateReplanSeed writes the bound plan of the re-plan cases with five rounds: r1 and r2 this session's
// open plan_audit rounds of two earlier epochs, r3 another session's, r4 this session's final_gate round, r5
// this session's approved plan_audit round. The session is at P.
func orchestrateReplanSeed(t *testing.T, cwd, id string) (unit string) {
	t.Helper()
	unit = orchestrateTransitionSeedPlanUnit(t, cwd)
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "replan cleanup"})
	plan.Slug, plan.ActiveWorkPhaseID = id, new("wp1")
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	round := func(roundID string, purpose goalplan.ReviewPurpose, owner, epoch string, status goalplan.ReviewRoundStatus) goalplan.ReviewRoundState {
		r := goalplan.ReviewRoundState{
			RoundID: roundID, Purpose: purpose, PlanPath: unit, PlanSha256: strings.Repeat("a", 64),
			Status: status, Lane: goalplan.ReviewLane{LaunchID: roundID + "-launch"}, OpenedAt: "2026-08-28T00:00:00.000Z",
			OwnerSessionID: owner, WorkPhaseID: "wp1", PlanUnit: unit, PlanEpoch: epoch,
		}
		if status == goalplan.ReviewApproved {
			closed := "2026-08-28T01:00:00.000Z"
			r.Lane.Verdict, r.ClosedAt = goalplan.VerdictPass, &closed
		}
		return r
	}
	plan.ReviewRounds = []goalplan.ReviewRoundState{
		round("r1", goalplan.PurposePlanAudit, id, "e-old-1", goalplan.ReviewInFlight),
		round("r2", goalplan.PurposePlanAudit, id, "e-old-2", goalplan.ReviewInFlight),
		round("r3", goalplan.PurposePlanAudit, "another-session", "e-old-1", goalplan.ReviewInFlight),
		round("r4", goalplan.PurposeFinalGate, id, "e-old-1", goalplan.ReviewInFlight),
		round("r5", goalplan.PurposePlanAudit, id, "e-old-2", goalplan.ReviewApproved),
	}
	plan.ActivePlanAuditRoundID = new("r2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	if goalplan.ReadGoalplan(cwd, id) == nil {
		t.Fatal("the seeded plan does not read back")
	}
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P","slug":"`+id+`"}`)
	return unit
}

// orchestrateReplanAttest is the P>A attestation of the re-plan cases.
func orchestrateReplanAttest(unit string) string {
	return `{"from":"P","to":"A","did":"audited the plan","planUnit":"` + unit + `","workPhaseId":"wp1"}`
}

// orchestrateReplanStatuses is roundId=status of every round, in order.
func orchestrateReplanStatuses(t *testing.T, cwd, slug string) string {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		t.Fatal("the plan does not read")
	}
	parts := []string{}
	for _, r := range plan.ReviewRounds {
		parts = append(parts, r.RoundID+"="+string(r.Status))
	}
	return strings.Join(parts, " ")
}

// orchestrateReplanSuperseded counts the review_round_superseded rows of each round.
func orchestrateReplanSuperseded(t *testing.T, cwd, slug string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
		if row["event"] == string(goalplan.EventReviewRoundSuperseded) {
			id, _ := row["roundId"].(string)
			counts[id]++
		}
	}
	return counts
}

const orchestrateReplanAllClosed = "r1=inconclusive r2=inconclusive r3=in_flight r4=in_flight r5=approved"

// Red on dev: the re-plan closed the first stranded epoch's round only (r1), and r2 - this session's open
// round of another earlier epoch - stayed in flight, where a late sign-off could still approve a plan the
// session no longer runs. Now both close, with one superseded row each, and nothing else is touched.
func TestOrchestrateReplanClosesEveryObsoleteRound(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "replan-all"
	unit := orchestrateReplanSeed(t, cwd, id)
	if got := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", orchestrateReplanAttest(unit)); got.Code != 0 {
		t.Fatalf("P>A: %+v", got)
	}
	if got := orchestrateReplanStatuses(t, cwd, id); got != orchestrateReplanAllClosed {
		t.Fatalf("rounds after the re-plan: %s, want %s", got, orchestrateReplanAllClosed)
	}
	if counts := orchestrateReplanSuperseded(t, cwd, id); len(counts) != 2 || counts["r1"] != 1 || counts["r2"] != 1 {
		t.Fatalf("superseded rows: %v, want one each for r1 and r2", counts)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, id); len(pending) != 0 {
		t.Fatalf("the re-plan left %d pending event(s)", len(pending))
	}
}

// Red on dev: the cleanup ran before the state write, so a write that failed before the publication left the
// rounds closed and their rows written beside a session still at P. Now nothing is touched.
func TestOrchestrateReplanFailedStateWriteChangesNothing(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "replan-prepublish"
	unit := orchestrateReplanSeed(t, cwd, id)
	before := orchestrateReplanStatuses(t, cwd, id)
	seams := &orchestrateCommitSeams{writeState: orchestrateCommitFailedStateWrite}
	if _, err := orchestrateCommitTry(t, cwd, seams, "A", "--session", id, "--attest", orchestrateReplanAttest(unit)); err == nil {
		t.Fatal("a pre-publication failure was reported as success")
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseP {
		t.Fatalf("the failed write moved the session: %+v", s)
	}
	if got := orchestrateReplanStatuses(t, cwd, id); got != before {
		t.Fatalf("rounds after a failed write: %s, want %s", got, before)
	}
	if counts := orchestrateReplanSuperseded(t, cwd, id); len(counts) != 0 {
		t.Fatalf("superseded rows after a failed write: %v", counts)
	}
}

// hookCleanup is the cleanup the P>A event carries.
type hookCleanup = hook.PlanAuditCleanup

// A cleanup that fails after the publication is not dropped: the answer names it as pending, the binding
// stands, and the next call of the session finishes it with the same epoch and no duplicate row - also when
// the first attempt had already closed the rounds and only their rows were missing. The plan's ledger is
// made unwritable from inside the first attempt, so the drain that follows the publication cannot finish
// the cleanup either.
func TestOrchestrateReplanCleanupFailureIsFinishedByTheNextCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(cwd, sessionID string, c hookCleanup, plan *goalplan.Goalplan) error
	}{
		{"nothing-closed", func(string, string, hookCleanup, *goalplan.Goalplan) error { return syscall.EIO }},
		{"closed-without-rows", func(cwd, sessionID string, c hookCleanup, plan *goalplan.Goalplan) error {
			swept, _ := review.SupersedeRounds(plan, goalplan.PurposePlanAudit, sessionID, c.Epoch, c.Rounds, c.ClosedAt)
			if err := goalplan.WriteGoalplan(cwd, swept); err != nil {
				return err
			}
			return syscall.EIO
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, id := orchestrateTransitionRoot(t), "replan-pending-"+tc.name
			unit := orchestrateReplanSeed(t, cwd, id)
			ledger := filepath.Join(cwd, ".crw", "goalplans", id, goalplan.GoalplanLedgerFile)
			var epoch string
			seams := &orchestrateCommitSeams{cleanup: func(cwd, sessionID string, c hookCleanup, plan *goalplan.Goalplan) error {
				epoch = c.Epoch
				err := tc.stage(cwd, sessionID, c, plan)
				if mvErr := os.Rename(ledger, ledger+".aside"); mvErr != nil && !errors.Is(mvErr, os.ErrNotExist) {
					return mvErr
				}
				if mkErr := os.Mkdir(ledger, 0o700); mkErr != nil {
					return mkErr
				}
				return err
			}}
			got := orchestrateCommitRunOK(t, cwd, seams, "A", "--session", id, "--attest", orchestrateReplanAttest(unit))
			if got.Code != 0 || !strings.Contains(got.Output, "cleanup of this session's earlier plan_audit rounds is pending") {
				t.Fatalf("P>A with a failed cleanup: %+v", got)
			}
			after := state.ReadState(cwd, id)
			if after.Phase != state.PhaseA || after.PlanEpoch == nil || *after.PlanEpoch != epoch {
				t.Fatalf("the binding did not stand: %+v", after)
			}
			if err := os.Remove(ledger); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(ledger+".aside", ledger); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			// The next call of the session (refused: A>C is no edge) finishes the cleanup first.
			orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id)
			orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id)
			if got := orchestrateReplanStatuses(t, cwd, id); got != orchestrateReplanAllClosed {
				t.Fatalf("rounds after the next calls: %s, want %s", got, orchestrateReplanAllClosed)
			}
			if counts := orchestrateReplanSuperseded(t, cwd, id); len(counts) != 2 || counts["r1"] != 1 || counts["r2"] != 1 {
				t.Fatalf("superseded rows: %v, want one each for r1 and r2", counts)
			}
			if s := state.ReadState(cwd, id); s.PlanEpoch == nil || *s.PlanEpoch != epoch {
				t.Fatalf("the reconcile changed the epoch: %+v", s)
			}
			if pending, _, _ := state.PendingLedgerEvents(cwd, id); len(pending) != 0 {
				t.Fatalf("%d event(s) still pending", len(pending))
			}
			if edges := orchestrateOutboxEdges(t, cwd); strings.Join(edges, " ") != "P>A" {
				t.Fatalf("PABCD rows: %v, want exactly [P>A]", edges)
			}
		})
	}
}

// A late sign-off on a superseded round is refused and the session keeps the binding the re-plan made.
func TestOrchestrateReplanLateSignoffKeepsTheBinding(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "replan-late"
	unit := orchestrateReplanSeed(t, cwd, id)
	if got := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", orchestrateReplanAttest(unit)); got.Code != 0 {
		t.Fatalf("P>A: %+v", got)
	}
	bound := state.ReadState(cwd, id)
	plan := goalplan.ReadGoalplan(cwd, id)
	late := review.RecordVerdict(plan, review.VerdictInput{Purpose: goalplan.PurposePlanAudit, RoundID: "r2", LaunchID: "r2-launch", Verdict: goalplan.VerdictPass})
	if late.Kind == review.OK {
		t.Fatalf("a late sign-off on the superseded r2 was recorded: %+v", late)
	}
	after := state.ReadState(cwd, id)
	if after.PlanEpoch == nil || bound.PlanEpoch == nil || *after.PlanEpoch != *bound.PlanEpoch || after.PlanUnit == nil || *after.PlanUnit != unit {
		t.Fatalf("the binding moved: before %+v after %+v", bound, after)
	}
}

// Red on 3fceb240: a re-plan whose cleanup failed and whose transition row could not be written answered the row warning only, so the
// old plan_audit rounds stayed open with nothing said about it. Both are reported, each with its own reason.
func TestOrchestrateReplanReportsTheCleanupWhenTheRowFailsToo(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "replan-both"
	unit := orchestrateReplanSeed(t, cwd, id)
	orchestrateCommitLedgerDirectory(t, cwd)
	seams := &orchestrateCommitSeams{cleanup: func(string, string, hookCleanup, *goalplan.Goalplan) error { return syscall.EIO }}
	got := orchestrateCommitRunOK(t, cwd, seams, "A", "--session", id, "--attest", orchestrateReplanAttest(unit))
	if got.Code != 0 || !strings.Contains(got.Output, "ledger row for P -> A could not be written") {
		t.Fatalf("the row warning is missing: %+v", got)
	}
	if !strings.Contains(got.Output, "cleanup of this session's earlier plan_audit rounds is pending: "+syscall.EIO.Error()) {
		t.Fatalf("the cleanup warning is missing or lost its reason: %+v", got)
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseA {
		t.Fatalf("the re-plan did not stand: %+v", s)
	}
	// Both are finished by the next command once the ledger works.
	orchestrateOutboxUnblock(t, cwd)
	orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id)
	if got := orchestrateReplanStatuses(t, cwd, id); got != orchestrateReplanAllClosed {
		t.Fatalf("rounds after the next call: %s, want %s", got, orchestrateReplanAllClosed)
	}
	if counts := orchestrateReplanSuperseded(t, cwd, id); counts["r1"] != 1 || counts["r2"] != 1 || len(counts) != 2 {
		t.Fatalf("superseded rows: %v", counts)
	}
	if edges := orchestrateOutboxEdges(t, cwd); strings.Join(edges, " ") != "P>A" {
		t.Fatalf("PABCD rows: %v, want exactly [P>A]", edges)
	}
}
