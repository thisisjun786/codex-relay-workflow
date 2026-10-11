package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-146 verification fix round 1: the release of a slug writes the session state and one transition row and nothing else, so the
// outbox followups an earlier writer left pending (a P>A re-plan's plan-audit cleanup rewrites the goalplan and appends to its
// ledger) are not run by it; they stay pending for a writer that is allowed to run them.

// loopUnbindPendingCleanup leaves, on the session at rest, a published plan-audit cleanup in the outbox and the open plan_audit
// round it would close in the bound goalplan.
func loopUnbindPendingCleanup(t *testing.T, cwd, slug string) {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		t.Fatal("no goalplan")
	}
	plan.ReviewRounds = []goalplan.ReviewRoundState{{
		RoundID: "r1", Purpose: goalplan.PurposePlanAudit, PlanPath: "u", PlanSha256: strings.Repeat("a", 64), Status: goalplan.ReviewInFlight,
		Lane: goalplan.ReviewLane{LaunchID: "r1-launch"}, OpenedAt: "2026-08-28T00:00:00.000Z", OwnerSessionID: loopUnbindSession, PlanUnit: "u", PlanEpoch: "e-old",
	}}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	cleanup := hook.NewPlanAuditCleanup(slug, "e-new", []string{"r1"})
	followup, err := json.Marshal(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	s := state.ReadState(cwd, loopUnbindSession)
	ev, err := state.NewLedgerEvent(cwd, s, s, nil, followup)
	if err != nil {
		t.Fatal(err)
	}
	ev.Published = true
	if err := state.PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
}

func loopUnbindOutbox(t *testing.T, cwd string) []string {
	t.Helper()
	entries, err := os.ReadDir(state.StatePath(cwd, loopUnbindSession) + ".ledger-outbox")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Red on ab12f038: the verb drained the whole outbox, so a pending plan-audit cleanup rewrote the goalplan and appended a
// review_round_superseded row to its ledger while the verb answered that neither was changed.
func TestVerifyLoopUnbindPreservesGoalplanWithPendingCleanup(t *testing.T) {
	cwd, slug := loopUnbindBound(t)
	if got := orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", loopUnbindSession); got.Code != 0 {
		t.Fatalf("reset: %d %q", got.Code, got.Output)
	}
	loopUnbindPendingCleanup(t, cwd, slug)
	// Unrelated session fields the verb must carry over untouched.
	s := state.ReadState(cwd, loopUnbindSession)
	phase := state.PhaseB
	s.StopBlockCount, s.StopBlockPhase = 2, &phase
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	s = state.ReadState(cwd, loopUnbindSession)
	before := loopUnbindRead(t, cwd, slug)
	outboxBefore := loopUnbindOutbox(t, cwd)
	if len(outboxBefore) != 1 {
		t.Fatalf("the outbox before: %v", outboxBefore)
	}

	got := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if got.Code != 0 || !strings.Contains(got.Output, slug) {
		t.Fatalf("unbind: %d %q", got.Code, got.Output)
	}
	after := loopUnbindRead(t, cwd, slug)
	if !bytes.Equal(before.plan, after.plan) {
		t.Fatalf("the goalplan file changed:\nbefore %s\nafter  %s", before.plan, after.plan)
	}
	if !bytes.Equal(before.planLedger, after.planLedger) {
		t.Fatalf("the goalplan ledger changed:\nbefore %s\nafter  %s", before.planLedger, after.planLedger)
	}
	if rows := loopUnbindRows(t, after.ledger[len(before.ledger):]); len(rows) != 1 || rows[0]["reason"] != "unbound" {
		t.Fatalf("the transition rows: %+v", rows)
	}
	// The followup is still owed, and the state differs from the one before by the slug and its pointer alone.
	if outbox := loopUnbindOutbox(t, cwd); len(outbox) != len(outboxBefore) {
		t.Fatalf("the pending cleanup left the outbox: %v", outbox)
	}
	gotState := state.ReadState(cwd, loopUnbindSession)
	want := s
	want.Slug, want.StopBlockWorkPhaseID = "", nil
	gotState.UpdatedAt, want.UpdatedAt = "", ""
	if gotJSON, wantJSON := loopUnbindEncode(t, gotState), loopUnbindEncode(t, want); gotJSON != wantJSON {
		t.Fatalf("state\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// A stored checkEpoch on a session at IDLE with no D-close marker is not read back: state.ReadStateStrict keeps the epoch only for
// phase C or an IDLE that holds a marker, as every writer of the session sees it. Such a file is the normalised rest, so the verb
// releases it (reset has nothing to clear there either, so refusing would leave no way out), and the rewrite carries the same
// normalisation every other writer applies.
func TestVerifyLoopUnbindTreatsAStoredIdleCheckEpochAsRest(t *testing.T) {
	cwd, slug := loopUnbindBound(t)
	if got := orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", loopUnbindSession); got.Code != 0 {
		t.Fatalf("reset: %d %q", got.Code, got.Output)
	}
	path := state.StatePath(cwd, loopUnbindSession)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	obj["checkEpoch"] = "c-pending"
	raw, err = json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before := loopUnbindRead(t, cwd, slug)
	got := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if got.Code != 0 || !strings.Contains(got.Output, slug) {
		t.Fatalf("unbind: %d %q", got.Code, got.Output)
	}
	after := loopUnbindRead(t, cwd, slug)
	if !bytes.Equal(before.plan, after.plan) || !bytes.Equal(before.planLedger, after.planLedger) {
		t.Fatal("the goalplan or its ledger changed")
	}
	if s := state.ReadState(cwd, loopUnbindSession); s.Slug != "" || s.Phase != state.PhaseIdle || s.CheckEpoch != nil {
		t.Fatalf("the state: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "ledger.jsonl")); err != nil {
		t.Fatal(err)
	}
}
