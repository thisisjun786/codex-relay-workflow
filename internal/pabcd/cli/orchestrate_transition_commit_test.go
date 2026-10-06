package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The commit order of the three write paths (CRW-811): the session state is published first and the
// transition-ledger row second, the oracle's own order. These cases are written to fail on the baseline
// (the row-first ordinary edge, the reset and override that return an error when only the row fails, and
// the work-phase gate that reads the goalplan without its write lock) and to pass once the order is fixed.

// orchestrateCommitTry drives the same pair the harness does, with the commit order's two seams; the
// exported entry point passes nil, so a test names exactly the dependency it stages.
func orchestrateCommitTry(t *testing.T, cwd string, seams *orchestrateCommitSeams, argv ...string) (CliResult, error) {
	t.Helper()
	parsed := ParseOrchestrateCliArgs(argv, cwd)
	read, err := RunOrchestrateRead(parsed, ReadEnv{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Result != nil {
		return *read.Result, nil
	}
	return orchestrateCommitRun(*parsed.Args, read.SessionID, seams)
}

func orchestrateCommitRunOK(t *testing.T, cwd string, seams *orchestrateCommitSeams, argv ...string) CliResult {
	t.Helper()
	got, err := orchestrateCommitTry(t, cwd, seams, argv...)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	return got
}

// orchestrateCommitLedgerDirectory makes the transition ledger unwritable by putting a directory where its
// file belongs, so every append fails and the publication's own success is observable on its own.
func orchestrateCommitLedgerDirectory(t *testing.T, cwd string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cwd, ".crw", "ledger.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// orchestrateCommitFailedStateWrite is the pre-publication failure seam: nothing reached the final path, so
// the error is not a state.PublishedError and no transition may be recorded.
func orchestrateCommitFailedStateWrite(cwd string, next state.State) error { return syscall.EIO }

// orchestrateCommitSyncFailedStateWrite publishes the state for real and then reports the post-rename
// directory sync failure, the shape state.WriteState produces when only the directory fsync fails.
func orchestrateCommitSyncFailedStateWrite(cwd string, next state.State) error {
	if err := state.WriteState(cwd, next); err != nil {
		return err
	}
	return &state.PublishedError{Err: syscall.EIO}
}

// orchestrateCommitPlan writes the bound goalplan of the rebind cases: wp1 in progress, wp2 pending.
func orchestrateCommitPlan(t *testing.T, cwd, slug string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "commit order"})
	plan.Slug, plan.ActiveWorkPhaseID = slug, new("wp1")
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// orchestrateCommitRebindLock is the ordering seam of the concurrent-rebind case: another writer publishes
// wp1 done and wp2 active after the gate's unlocked read and before this writer's lock callback runs. It is
// a seam, never a sleep.
func orchestrateCommitRebindLock(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		return goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome]{Kind: "unreadable", Reason: "goalplan '" + slug + "' does not exist"}, nil
	}
	plan.WorkPhases[0].Status, plan.WorkPhases[1].Status = goalplan.WorkPhaseDone, goalplan.WorkPhaseInProgress
	plan.ActiveWorkPhaseID = new("wp2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		return goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome]{}, err
	}
	return goalplan.WithGoalplanWriteLock(cwd, slug, fn, nil)
}

// TestOrchestrateCommitLedgerRowWarning is the reset and I>P override half: the state is published and the
// answer is a success carrying the ledger warning, where the baseline returns an error for a row that never
// mattered to the transition.
func TestOrchestrateCommitLedgerRowWarning(t *testing.T) {
	t.Run("interview-override", func(t *testing.T) {
		cwd, id := orchestrateTransitionRoot(t), "commit-ledger-override"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I"}`)
		orchestrateCommitLedgerDirectory(t, cwd)
		got := orchestrateCommitRunOK(t, cwd, nil, "P", "--session", id, "--attest",
			`{"from":"I","to":"P","did":"interview done","override":true}`)
		if got.Code != 0 || !strings.Contains(got.Output, "orchestrate P: I \u2192 P (agent override, session "+id+")") {
			t.Fatalf("override answer: %+v", got)
		}
		if !strings.Contains(got.Output, "orchestrate P: warning: ledger row for I -> P could not be written: ") {
			t.Fatalf("override warning: %q", got.Output)
		}
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseP {
			t.Fatalf("the override published no state: %+v", after)
		}
	})
	t.Run("reset", func(t *testing.T) {
		cwd, id := orchestrateTransitionRoot(t), "commit-ledger-reset"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"P","orchestrationActive":true}`)
		orchestrateCommitLedgerDirectory(t, cwd)
		got := orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
		if got.Code != 0 || !strings.Contains(got.Output, "orchestrate reset: current=P -> IDLE (session "+id+")") {
			t.Fatalf("reset answer: %+v", got)
		}
		if !strings.Contains(got.Output, "orchestrate reset: warning: ledger row for P -> IDLE could not be written: ") {
			t.Fatalf("reset warning: %q", got.Output)
		}
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseIdle {
			t.Fatalf("the reset published no state: %+v", after)
		}
	})
	t.Run("ordinary-edge", func(t *testing.T) {
		cwd, id := orchestrateTransitionRoot(t), "commit-ledger-ordinary"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
		orchestrateCommitLedgerDirectory(t, cwd)
		got := orchestrateCommitRunOK(t, cwd, nil, "P", "--session", id)
		if got.Code != 0 {
			t.Fatalf("ordinary answer: %+v", got)
		}
		if !strings.Contains(got.Output, "warning: ledger row for IDLE -> P could not be written: ") {
			t.Fatalf("ordinary warning: %q", got.Output)
		}
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseP {
			t.Fatalf("the ordinary edge published no state: %+v", after)
		}
	})
}

// TestOrchestrateCommitPrePublicationFailure is the other half of the order: a write that fails before the
// rename leaves the session where it was and writes no row, so no row describes a transition that did not
// happen, and the retry adds exactly one.
func TestOrchestrateCommitPrePublicationFailure(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "commit-pre-publication"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
	seams := &orchestrateCommitSeams{writeState: orchestrateCommitFailedStateWrite}
	got, err := orchestrateCommitTry(t, cwd, seams, "P", "--session", id)
	if err == nil || !errors.Is(err, syscall.EIO) {
		t.Fatalf("a pre-publication failure must be returned as an error: %+v %v", got, err)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseIdle {
		t.Fatalf("a failed publication moved the session: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("a failed publication wrote a ledger row: %+v", rows)
	}
	if retry := orchestrateCommitRunOK(t, cwd, nil, "P", "--session", id); retry.Code != 0 {
		t.Fatalf("retry: %+v", retry)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 1 || rows[0]["to"] != "P" {
		t.Fatalf("the retry must write exactly one row: %+v", rows)
	}
}

// TestOrchestrateCommitDirectorySyncWarning pins the published-but-unsynced case: the state is published,
// the row is written, and the answer is a success carrying the directory-sync warning.
func TestOrchestrateCommitDirectorySyncWarning(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "commit-sync-warning"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
	seams := &orchestrateCommitSeams{writeState: orchestrateCommitSyncFailedStateWrite}
	got := orchestrateCommitRunOK(t, cwd, seams, "P", "--session", id)
	if got.Code != 0 {
		t.Fatalf("sync-only failure: %+v", got)
	}
	if !strings.Contains(got.Output, "warning: session state was published but its directory could not be synced: ") ||
		!strings.Contains(got.Output, "input/output error") {
		t.Fatalf("sync warning: %q", got.Output)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseP {
		t.Fatalf("the published state is not there: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 1 || rows[0]["to"] != "P" {
		t.Fatalf("a published transition keeps its row: %+v", rows)
	}
}

// TestOrchestrateCommitRevalidatesTheBinding is the work-phase gate under the goalplan write lock: another
// writer publishes wp1 done and wp2 active between the unlocked read and the lock, so the stale workPhaseId
// must be refused and nothing written.
func TestOrchestrateCommitRevalidatesTheBinding(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "commit-rebind"
	orchestrateCommitPlan(t, cwd, id)
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
	seams := &orchestrateCommitSeams{lockGoalplan: orchestrateCommitRebindLock}
	got := orchestrateCommitRunOK(t, cwd, seams, "C", "--session", id, "--attest",
		`{"from":"B","to":"C","did":"built it","workPhaseId":"wp1"}`)
	if got.Code != 1 || !strings.Contains(got.Output, "LOOP-UNIT-CHAIN-01") {
		t.Fatalf("a rebound plan must refuse the stale binding: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseB {
		t.Fatalf("the refused transition moved the session: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refused transition wrote a ledger row: %+v", rows)
	}
}

// TestOrchestrateCommitBusyGoalplanRefuses is the busy half of the same gate: a held goalplan lock refuses
// the gated edge with its busy reason and publishes nothing.
func TestOrchestrateCommitBusyGoalplanRefuses(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "commit-busy-lock"
	orchestrateCommitPlan(t, cwd, id)
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
	lock := filepath.Join(cwd, ".crw", "goalplans", id, ".goalplan.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionPut(t, filepath.Join(lock, "owner.json"), "{\"pid\":4242}\n")
	got := orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id, "--attest",
		`{"from":"B","to":"C","did":"built it","workPhaseId":"wp1"}`)
	if got.Code != 1 || !strings.Contains(got.Output, "is busy") {
		t.Fatalf("a busy goalplan lock must refuse: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseB {
		t.Fatalf("the refused transition moved the session: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refused transition wrote a ledger row: %+v", rows)
	}
}

// TestOrchestrateCommitFailOpenWithoutPlan keeps the fail-open the gate had: an absent or unreadable
// goalplan never blocks a bound session's gated edge.
func TestOrchestrateCommitFailOpenWithoutPlan(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "commit-fail-open"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
	orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "goalplans", id, "goalplan.json"), "not json\n")
	got := orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id, "--attest",
		`{"from":"B","to":"C","did":"built it"}`)
	if got.Code != 0 {
		t.Fatalf("an unreadable goalplan must not block: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseC {
		t.Fatalf("the fail-open edge did not publish: %+v", after)
	}
}
