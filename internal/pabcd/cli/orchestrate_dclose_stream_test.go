package cli

// The CRW-1103 suite of the CLI close: the row guards read the plan's own ledger once for both of its
// questions and the workspace ledger once, streamed, holding no row; a damaged line after an early match
// still fails the close; the answers and the rows are those of the whole-file reader; a large history keeps
// the recovery marker through a close whose finalization is pending and its retry.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateDcloseStreamHistory gives both ledgers n unrelated rows: other sessions' close rows in the
// workspace ledger and other phases' rows in the plan's ledger, so the guards have a history to read.
func orchestrateDcloseStreamHistory(t *testing.T, cwd, slug string, n int) {
	t.Helper()
	var ws, gp strings.Builder
	for i := range n {
		fmt.Fprintf(&ws, `{"ts":"2026-01-01T00:00:00.000Z","sessionId":"other-%d","from":"C","to":"IDLE","reason":"done","checkEpoch":"c-%d","closedWorkPhaseId":"wp-x","evidence":"%s"}`+"\n", i, i, strings.Repeat("e", 120))
		fmt.Fprintf(&gp, `{"ts":"2026-01-01T00:00:00.000Z","slug":"%s","event":"task_done","detail":"t-%d %s"}`+"\n", slug, i, strings.Repeat("d", 120))
	}
	orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "ledger.jsonl"), ws.String())
	orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"), gp.String())
}

// orchestrateDcloseCountingOpen counts the opens of each path.
func orchestrateDcloseCountingOpen(counts map[string]int) func(string) (*os.File, error) {
	return func(path string) (*os.File, error) {
		counts[filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path)]++
		return os.Open(path)
	}
}

// A normal bound close with a successor answers both plan-ledger questions (workphase_done and
// workphase_started) with one read, and the workspace guard with one; the dev build read the plan's ledger
// twice (strace evidence in /state/crw/crw-lane-orchestrate-atomicity/evidence/CRW-1103/).
func TestOrchestrateDcloseReadsEachLedgerOnce(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "stream-once", "stream-once-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	orchestrateDcloseStreamHistory(t, cwd, slug, 200)
	counts := map[string]int{}
	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{openLedger: orchestrateDcloseCountingOpen(counts)})
	if err != nil || got.Code != 0 {
		t.Fatalf("close: %+v %v", got, err)
	}
	if counts[slug+"/ledger.jsonl"] != 1 || counts[".crw/ledger.jsonl"] != 1 {
		t.Fatalf("ledger reads: %v, want the plan's ledger once and the workspace ledger once", counts)
	}
	rows := orchestrateDcloseGoalplanRows(t, cwd, slug)
	if last := rows[len(rows)-2:]; last[0]["detail"] != "closed wp-1" || last[1]["detail"] != "started wp-2" {
		t.Fatalf("the goalplan rows the close appended: %+v", last)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows of this session = %d, want 1", n)
	}
}

// The CLI keeps checking every line after a match: a damaged line behind an early matching row still fails
// the guard, as the whole-file reader did (the hook's reader skips it; the two policies stay apart).
func TestOrchestrateDcloseDamagedLineAfterAMatchStillFails(t *testing.T) {
	cwd := t.TempDir()
	id, slug := "stream-damaged", "stream-damaged-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "damaged after match"})
	plan.Slug = slug
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch, phase := "c-1", "wp-1"
	orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "ledger.jsonl"),
		`{"sessionId":"`+id+`","from":"C","to":"IDLE","reason":"done","checkEpoch":"c-1","closedWorkPhaseId":"wp-1"}`+"\nnull\n")
	if _, err := orchestrateDcloseHasPabcdCloseRow(cwd, id, &epoch, &phase); err == nil {
		t.Error("a null line after the matching row was not refused")
	}
	orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"),
		`{"slug":"`+slug+`","event":"workphase_done","detail":"closed wp-1"}`+"\n{not json\n")
	if _, err := orchestrateDcloseHasGoalplanRow(cwd, slug, goalplan.EventWorkphaseDone, "closed wp-1"); err == nil {
		t.Error("a broken line after the matching row was not refused")
	}
}

// The streamed guard holds no row: scanning a ledger of 60,000 rows (about 11 MB) keeps the live heap far
// below the ledger's size. The pipe the read seam hands back lets the writer measure the heap while the
// reader is still inside the scan, with most of the ledger already consumed.
func TestOrchestrateDcloseGuardHoldsNoRows(t *testing.T) {
	cwd := t.TempDir()
	slug := "stream-memory"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "memory"})
	plan.Slug = slug
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	const rows = 60000
	line := []byte(fmt.Sprintf(`{"ts":"2026-01-01T00:00:00.000Z","slug":"%s","event":"task_done","detail":"%s"}`+"\n", slug, strings.Repeat("d", 150)))
	var during uint64
	open := func(string) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		go func() {
			defer w.Close()
			var before runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			for i := range rows {
				if _, err := w.Write(line); err != nil {
					return
				}
				if i == rows*9/10 {
					var now runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&now)
					if now.HeapAlloc > before.HeapAlloc {
						during = now.HeapAlloc - before.HeapAlloc
					}
				}
			}
		}()
		return r, nil
	}
	have, err := orchestrateDcloseHasGoalplanRows(cwd, slug, open, orchestrateDcloseGoalplanKey{goalplan.EventWorkphaseDone, "closed wp-9"})
	if err != nil || have[0] {
		t.Fatalf("scan: %v %v", have, err)
	}
	total := uint64(rows * len(line))
	if during > total/8 {
		t.Fatalf("the live heap grew by %d bytes while scanning a %d-byte ledger; the guard must hold no rows", during, total)
	}
}

// A close over a large history whose finalization lock is busy answers the pending text and keeps the
// recovery marker; the same D request finishes it with exactly one row of each kind.
func TestOrchestrateDcloseLargeHistoryKeepsTheMarkerThroughARetry(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "stream-large", "stream-large-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	orchestrateDcloseStreamHistory(t, cwd, slug, 20000)
	lockDir := orchestrateDcloseCancelLockDir(t, cwd, slug)
	var takeErr error
	seam := orchestrateDcloseSeam{goalplanLockSeam: orchestrateDcloseCancelLockSeam(true,
		func() { takeErr = os.Mkdir(lockDir, 0o700) }, func(int) {})}
	got, err := orchestrateDcloseRun(t, cwd, id, seam)
	if takeErr != nil || err != nil {
		t.Fatal(takeErr, err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "finalization is pending") {
		t.Fatalf("a close whose finalization lock is busy: %+v", got)
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseIdle || s.DcloseRecovery == nil {
		t.Fatalf("the marker did not survive: %+v", s)
	}
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
	cur := state.ReadState(cwd, id)
	again, err := orchestrateDcloseContext(t.Context(), cwd, id, "wp-1", cur, orchestrateDcloseAttest(id), true, orchestrateDcloseSeam{})
	if err != nil || again.Code != 0 || !strings.Contains(again.Output, "is complete") {
		t.Fatalf("the retry: %+v %v", again, err)
	}
	if s := state.ReadState(cwd, id); s.DcloseRecovery != nil {
		t.Fatalf("the retry left the marker: %+v", s)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows of this session = %d, want 1", n)
	}
	done, started := 0, 0
	for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
		switch row["detail"] {
		case "closed wp-1":
			done++
		case "started wp-2":
			started++
		}
	}
	if done != 1 || started != 1 {
		t.Fatalf("goalplan rows: done %d, started %d; want 1 and 1", done, started)
	}
}
