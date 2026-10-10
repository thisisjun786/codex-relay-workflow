package hook

// The plan-audit cleanup of a P>A event (CRW-1100) as the pre-merge evaluation of 3fceb240 judged it: a superseded row is owed to the
// rounds this cleanup closed and to no other, and a row or a plan write that is only visible is made durable before the cleanup is
// finished.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

const cleanupStamp = "2026-10-10T00:00:00.000Z"

// cleanupPlan writes a plan with the session's open plan_audit rounds r1 and r2 of an earlier epoch and returns it.
func cleanupPlan(t *testing.T, cwd, slug string) *goalplan.Goalplan {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "cleanup"})
	plan.Slug = slug
	round := func(id string) goalplan.ReviewRoundState {
		return goalplan.ReviewRoundState{
			RoundID: id, Purpose: goalplan.PurposePlanAudit, PlanPath: "u", PlanSha256: strings.Repeat("a", 64), Status: goalplan.ReviewInFlight,
			Lane: goalplan.ReviewLane{LaunchID: id + "-launch"}, OpenedAt: "2026-08-28T00:00:00.000Z", OwnerSessionID: "s1", PlanUnit: "u", PlanEpoch: "e-old",
		}
	}
	plan.ReviewRounds = []goalplan.ReviewRoundState{round("r1"), round("r2")}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	return goalplan.ReadGoalplan(cwd, slug)
}

func cleanupSupersededRows(t *testing.T, cwd, slug string) []string {
	t.Helper()
	rows, err := planAuditSupersededRows(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for id := range rows {
		out = append(out, id)
	}
	return out
}

// Red on 3fceb240: the rows owed were inferred from "inconclusive, no verdict", which an abort produces as well, so a round that was
// aborted on its own while the cleanup was pending got a superseded row it was never given. The cleanup's id tells them apart.
func TestSupersedePlanAuditRoundsGivesNoRowToARoundAbortedOnItsOwn(t *testing.T) {
	cwd := t.TempDir()
	plan := cleanupPlan(t, cwd, "demo")
	// r2 was aborted by the agent after the cleanup was queued and before it ran: closed, inconclusive, no verdict, another stamp.
	aborted := "2026-10-10T00:00:05.000Z"
	reviewer := "aborted: by the agent"
	plan.ReviewRounds[1].Status, plan.ReviewRounds[1].ClosedAt, plan.ReviewRounds[1].Lane.ReviewerSession = goalplan.ReviewInconclusive, &aborted, &reviewer
	c := PlanAuditCleanup{Kind: PlanAuditCleanupKind, ID: "pac-test", Slug: "demo", Epoch: "e-new", Rounds: []string{"r1", "r2"}, ClosedAt: cleanupStamp}
	if err := SupersedePlanAuditRounds(cwd, "s1", c, plan); err != nil {
		t.Fatal(err)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 1 || rows[0] != "r1" {
		t.Fatalf("superseded rows: %v, want r1 only", rows)
	}
	// Replayed: the same, and nothing twice.
	if err := SupersedePlanAuditRounds(cwd, "s1", c, goalplan.ReadGoalplan(cwd, "demo")); err != nil {
		t.Fatal(err)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 1 {
		t.Fatalf("superseded rows after the replay: %v", rows)
	}
}

// A round this cleanup closed, whose row could not be written, still gets it on the replay, though the plan already shows it closed.
func TestSupersedePlanAuditRoundsRecordsTheRowsOfARoundItClosedOnTheReplay(t *testing.T) {
	cwd := t.TempDir()
	plan := cleanupPlan(t, cwd, "demo")
	dir, err := goalplan.GoalplanDir(cwd, "demo")
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, goalplan.GoalplanLedgerFile)
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	c := PlanAuditCleanup{Kind: PlanAuditCleanupKind, ID: "pac-test", Slug: "demo", Epoch: "e-new", Rounds: []string{"r1", "r2"}, ClosedAt: cleanupStamp}
	if err := SupersedePlanAuditRounds(cwd, "s1", c, plan); err == nil {
		t.Fatal("a blocked ledger finished the cleanup")
	}
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if err := SupersedePlanAuditRounds(cwd, "s1", c, goalplan.ReadGoalplan(cwd, "demo")); err != nil {
		t.Fatal(err)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 2 {
		t.Fatalf("superseded rows: %v, want r1 and r2", rows)
	}
}

// Red on 3fceb240: a row that was visible but whose fsync failed was found on the replay and skipped without a sync, and a plan write
// whose directory sync failed was discarded at once, so the cleanup counted as finished and its event was retired over a storage
// failure nobody had been told about. The plan's ledger and directory are synced every time, and a failed sync keeps the cleanup pending.
func TestSupersedePlanAuditRoundsSyncsWhatItFindsBeforeItIsFinished(t *testing.T) {
	cwd := t.TempDir()
	plan := cleanupPlan(t, cwd, "demo")
	synced := 0
	fail := true
	real := planAuditSync
	t.Cleanup(func() { planAuditSync = real })
	planAuditSync = func(cwd, slug string) error {
		synced++
		if fail {
			return errors.New("injected sync failure")
		}
		return real(cwd, slug)
	}
	c := PlanAuditCleanup{Kind: PlanAuditCleanupKind, ID: "pac-test", Slug: "demo", Epoch: "e-new", Rounds: []string{"r1", "r2"}, ClosedAt: cleanupStamp}
	if err := SupersedePlanAuditRounds(cwd, "s1", c, plan); err == nil || !strings.Contains(err.Error(), "injected sync failure") {
		t.Fatalf("a failed sync finished the cleanup: %v", err)
	}
	// The replay finds both rounds closed and both rows in the ledger: nothing to write, and the sync still runs.
	fail = false
	if err := SupersedePlanAuditRounds(cwd, "s1", c, goalplan.ReadGoalplan(cwd, "demo")); err != nil {
		t.Fatal(err)
	}
	if synced != 2 {
		t.Fatalf("the sync ran %d times, want once per attempt", synced)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 2 {
		t.Fatalf("superseded rows: %v", rows)
	}
}

// Red on 63b65d5b: the rounds owed a row were told apart by the cleanup's millisecond stamp, so an abort that closed a round in the
// same millisecond (or under a clock that stepped back onto the stamp) carried the same closedAt and got a superseded row it was never
// given. The cleanup's own id, recorded on every round it closes, tells them apart.
func TestSupersedePlanAuditRoundsGivesNoRowToAnAbortWithTheCleanupsStamp(t *testing.T) {
	cwd := t.TempDir()
	plan := cleanupPlan(t, cwd, "demo")
	c := NewPlanAuditCleanup("demo", "e-new", []string{"r1", "r2"})
	stamp := c.ClosedAt
	reviewer := "aborted: independent manual abort"
	plan.ReviewRounds[1].Status, plan.ReviewRounds[1].ClosedAt, plan.ReviewRounds[1].Lane.ReviewerSession = goalplan.ReviewInconclusive, &stamp, &reviewer
	if err := SupersedePlanAuditRounds(cwd, "s1", c, plan); err != nil {
		t.Fatal(err)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 1 || rows[0] != "r1" {
		t.Fatalf("superseded rows: %v, want r1 only", rows)
	}
	if err := SupersedePlanAuditRounds(cwd, "s1", c, goalplan.ReadGoalplan(cwd, "demo")); err != nil {
		t.Fatal(err)
	}
	if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 1 || rows[0] != "r1" {
		t.Fatalf("superseded rows after the replay: %v, want r1 only", rows)
	}
}

// legacyCleanupPayloads are the cleanups the lane's earlier builds queued before the cleanup had an id: with the closing stamp
// (eb083d37f to 63b65d5bb) and without one (0664b63bb).
func legacyCleanupPayloads() map[string]PlanAuditCleanup {
	return map[string]PlanAuditCleanup{
		"stamp":    {Kind: PlanAuditCleanupKind, Slug: "demo", Epoch: "e-new", Rounds: []string{"r1", "r2"}, ClosedAt: cleanupStamp},
		"no-stamp": {Kind: PlanAuditCleanupKind, Slug: "demo", Epoch: "e-new", Rounds: []string{"r1", "r2"}},
	}
}

// Red on b7072ad9 (verify-r4 P1): a cleanup queued without an id closed its rounds with no supersededBy, its row append failed, and the
// retry, which closes nothing and has no id to match, owed no row and reported the cleanup finished, so the rounds' audit rows were lost.
func TestSupersedePlanAuditRoundsKeepsTheRowsOfALegacyCleanupAcrossAFailedAppend(t *testing.T) {
	for name, c := range legacyCleanupPayloads() {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			plan := cleanupPlan(t, cwd, "demo")
			dir, err := goalplan.GoalplanDir(cwd, "demo")
			if err != nil {
				t.Fatal(err)
			}
			ledger := filepath.Join(dir, goalplan.GoalplanLedgerFile)
			if err := os.Mkdir(ledger, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := SupersedePlanAuditRounds(cwd, "s1", c, plan); err == nil {
				t.Fatal("a blocked ledger finished the cleanup")
			}
			if err := os.Remove(ledger); err != nil {
				t.Fatal(err)
			}
			if err := SupersedePlanAuditRounds(cwd, "s1", c, goalplan.ReadGoalplan(cwd, "demo")); err != nil {
				t.Fatal(err)
			}
			if rows := cleanupSupersededRows(t, cwd, "demo"); len(rows) != 2 {
				t.Fatalf("superseded rows: %v, want r1 and r2", rows)
			}
		})
	}
}

// Red on b7072ad9 (verify-r4 P1): the same through the session's outbox: a P>A event queued with an id-less cleanup, a drain whose row
// append fails, and the next drain, which retired the event with no superseded row.
func TestDrainKeepsALegacyCleanupPendingUntilItsRowsAreRecorded(t *testing.T) {
	for name, c := range legacyCleanupPayloads() {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			plan := cleanupPlan(t, cwd, "demo")
			payload, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			pre := state.DefaultState("s1", "")
			pre.Phase = state.PhaseP
			if err := state.WriteState(cwd, pre); err != nil {
				t.Fatal(err)
			}
			pre = state.ReadState(cwd, "s1")
			post := pre
			post.Phase = state.PhaseA
			ev, err := state.NewLedgerEvent(cwd, pre, post, nil, payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.PrepareLedgerEvent(cwd, ev); err != nil {
				t.Fatal(err)
			}
			if err := state.WriteState(cwd, post); err != nil {
				t.Fatal(err)
			}
			dir, err := goalplan.GoalplanDir(cwd, plan.Slug)
			if err != nil {
				t.Fatal(err)
			}
			ledger := filepath.Join(dir, goalplan.GoalplanLedgerFile)
			if err := os.Mkdir(ledger, 0o700); err != nil {
				t.Fatal(err)
			}
			if report := DrainSessionLedger(cwd, "s1"); report.Err == nil || len(report.Pending) != 1 {
				t.Fatalf("a drain over a blocked plan ledger: %+v", report)
			}
			if err := os.Remove(ledger); err != nil {
				t.Fatal(err)
			}
			report := DrainSessionLedger(cwd, "s1")
			rows := cleanupSupersededRows(t, cwd, "demo")
			events, _, err := state.PendingLedgerEvents(cwd, "s1")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || len(events) != 0 || report.Err != nil {
				t.Fatalf("rows=%v pending=%d report=%+v, want r1 and r2 recorded and the event retired", rows, len(events), report)
			}
		})
	}
}
