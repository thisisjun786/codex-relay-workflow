package hook

// The CRW-1103 suite of the chat close: its row guards read the plan's own ledger once for all of its rows
// and the workspace ledger once, streamed; a damaged line still matches nothing and does not hide a row
// after it; the rows written are those of the whole-file reader.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// A bound chat close with a successor reads the plan's ledger once for its done and started rows and the
// workspace ledger once for its close row; the dev build read the plan's ledger once per row.
func TestPromptDcloseReadsEachLedgerOnce(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-stream-once"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "chat stream once"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{{ID: "t-1", Title: "work", Status: goalplan.TaskDone, Outcome: "tested"}}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "second", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	var history strings.Builder
	for i := range 200 {
		fmt.Fprintf(&history, `{"ts":"2026-01-01T00:00:00.000Z","slug":"%s","event":"task_done","detail":"t-%d"}`+"\n", slug, i)
	}
	promptDcloseWrite(t, cwd, filepath.Join(crwdir.DirName, "goalplans", slug, "ledger.jsonl"), history.String())
	promptDcloseSeedState(t, cwd, "s1", slug, "c-stream")
	attest := promptDcloseAttest("wp-1", promptDcloseReceipt(t, cwd, "s1", "c-stream"))
	counts := map[string]int{}
	seams := &promptDcloseSeams{openLedger: func(path string) (*os.File, error) {
		counts[filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path)]++
		return os.Open(path)
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", attest, seams)
	if panicked != nil || !strings.Contains(answer, "[crw: DONE]") {
		t.Fatalf("the close: %q %v", answer, panicked)
	}
	if counts[slug+"/ledger.jsonl"] != 1 || counts[".crw/ledger.jsonl"] != 1 {
		t.Fatalf("ledger reads: %v, want the plan's ledger once and the workspace ledger once", counts)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, "goalplans", slug, "ledger.jsonl"))
	if err != nil || !strings.Contains(string(raw), `"detail":"closed wp-1"`) || !strings.Contains(string(raw), `"detail":"started wp-2"`) {
		t.Fatalf("the goalplan rows: %s %v", raw, err)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.DcloseRecovery != nil {
		t.Fatalf("the resting state: %+v", s)
	}
}

// The chat reader skips a damaged line and still finds a row behind it, CRLF endings included.
func TestPromptDcloseStreamedGuardSkipsDamagedLines(t *testing.T) {
	cwd := t.TempDir()
	epoch := "c-1"
	ledger := "null\n{broken\n[1,2]\n\r\n" +
		`{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":"c-1","closedWorkPhaseId":"wp-1"}` + "\r\n"
	promptDcloseWrite(t, cwd, filepath.Join(crwdir.DirName, state.LedgerFile), ledger)
	if have, err := promptDcloseHasPabcdCloseRow(cwd, "s1", &epoch, "wp-1"); err != nil || !have {
		t.Fatalf("the row behind damaged lines: %v %v", have, err)
	}
	if have, err := promptDcloseHasPabcdCloseRow(cwd, "s1", &epoch, "wp-2"); err != nil || have {
		t.Fatalf("a row that is not there: %v %v", have, err)
	}
}
