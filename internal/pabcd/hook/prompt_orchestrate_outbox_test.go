// prompt_orchestrate_outbox_test.go is the CRW-1097 suite of the chat surface: a transition whose state
// was published while its ledger row was not written - the append failed, or the writer stopped right
// after the publication - is recorded by the next locked writer of the session, exactly once, and two
// transitions of one session record their rows in the order their states were published. The cases
// marked red reproduce on dev (6a5851f4f): the row is lost for good and the rows can be reordered.
package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptOutboxBlockLedger makes the transition ledger unwritable by putting a directory where the file
// goes; the returned function removes it again.
func promptOutboxBlockLedger(t *testing.T, cwd string) func() {
	t.Helper()
	path := filepath.Join(cwd, crwdir.DirName, state.LedgerFile)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

// promptOutboxEdges is the from>to of every ledger row, in order.
func promptOutboxEdges(t *testing.T, cwd string) []string {
	t.Helper()
	edges := []string{}
	for _, row := range promptOrchestrateLedger(t, cwd) {
		from, _ := row["from"].(string)
		to, _ := row["to"].(string)
		edges = append(edges, from+">"+to)
	}
	return edges
}

// Red on dev: an append that fails after the state was published answered nothing and lost the row for
// good. Now the answer says the transition was applied and its row is pending, and the next hook of the
// session (an ordinary prompt, whose Stop-budget stamp takes the session lock) records the row once.
func TestPromptOrchestrateFailedAppendIsRecordedByTheNextHook(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) { s.Phase = state.PhaseP; s.OrchestrationActive = true })
	unblock := promptOutboxBlockLedger(t, cwd)
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate A")
	if !strings.Contains(got, "[crw: AUDIT]") || !strings.Contains(got, "ledger row could not be written yet") {
		t.Errorf("the answer to an applied transition whose row failed: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseA {
		t.Fatalf("the transition was not applied: %+v", s)
	}
	unblock()
	promptOrchestrateAnswer(t, cwd, "s1", "t2", "thanks, carry on")
	promptOrchestrateAnswer(t, cwd, "s1", "t3", "and again")
	if edges := promptOutboxEdges(t, cwd); len(edges) != 1 || edges[0] != "P>A" {
		t.Errorf("the ledger after the next hooks: %v, want exactly [P>A]", edges)
	}
}

// A writer that stops right after the publication (a kill there) leaves the row pending; the next hook
// records it once and leaves no outbox behind.
func TestPromptOrchestrateStoppedAfterPublishIsRecordedByTheNextHook(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) { s.Phase = state.PhaseP; s.OrchestrationActive = true })
	seams := &promptDcloseSeams{afterOrchestratePublish: func() bool { return true }}
	promptSubmitHandleWith(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate A", TurnID: "t1", PabcdEnabled: true},
		"", promptSubmitHost(cwd), state.WithSessionLock, seams)
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseA {
		t.Fatalf("the transition was not published: %+v", s)
	}
	if edges := promptOutboxEdges(t, cwd); len(edges) != 0 {
		t.Fatalf("a stopped writer appended %v", edges)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, "s1"); len(pending) != 1 {
		t.Fatalf("pending events after the stop: %d, want 1", len(pending))
	}
	promptOrchestrateAnswer(t, cwd, "s1", "t2", "next prompt")
	promptOrchestrateAnswer(t, cwd, "s1", "t3", "and the one after")
	if edges := promptOutboxEdges(t, cwd); len(edges) != 1 || edges[0] != "P>A" {
		t.Errorf("the ledger after the next hooks: %v, want exactly [P>A]", edges)
	}
	if _, err := os.Stat(state.StatePath(cwd, "s1") + ".ledger-outbox"); !os.IsNotExist(err) {
		t.Errorf("the outbox outlived its last event: %v", err)
	}
}

// Red on dev: the row was appended after the session lock was released, so a second transition of the
// same session that took the lock in between recorded its row first, and the ledger told the two in the
// reverse of the order the state went through them. The second command runs from inside the lock seam,
// right after the first command's locked section returned.
func TestPromptOrchestrateRowsKeepTheStateOrder(t *testing.T) {
	cwd := t.TempDir()
	turn := "t1"
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
		s.StopBlockTurnID = &turn // the Stop-budget stamp is skipped, so the orchestrate write is the first locked call
	})
	fired := false
	lock := func(cwd, sessionID string, fn func() error) error {
		err := state.WithSessionLock(cwd, sessionID, fn)
		if !fired {
			fired = true
			promptOrchestrateAnswer(t, cwd, sessionID, "t2", "orchestrate reset")
		}
		return err
	}
	promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate A", TurnID: turn, PabcdEnabled: true},
		"", promptSubmitHost(cwd), lock)
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Fatalf("the two transitions did not both apply: %+v", s)
	}
	if edges := promptOutboxEdges(t, cwd); len(edges) != 2 || edges[0] != "P>A" || edges[1] != "A>IDLE" {
		t.Errorf("the ledger order: %v, want [P>A A>IDLE]", edges)
	}
}

// Red on dev (same shape as the first case): an unbound chat D-close is the free-pass C>D whose resting
// state is IDLE, and IDLE has no D edge, so a row lost there could never be written by a retry. The
// next hook records it once.
func TestPromptOrchestrateUnboundCloseRowIsRecordedByTheNextHook(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) { s.Phase = state.PhaseC; s.OrchestrationActive = true })
	unblock := promptOutboxBlockLedger(t, cwd)
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate D")
	if !strings.Contains(got, "ledger row could not be written yet") {
		t.Errorf("the answer to an applied close whose row failed: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Fatalf("the close was not applied: %+v", s)
	}
	unblock()
	promptOrchestrateAnswer(t, cwd, "s1", "t2", "orchestrate D")
	promptOrchestrateAnswer(t, cwd, "s1", "t3", "what now")
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "C" || rows[0]["to"] != "IDLE" || rows[0]["reason"] != "done" {
		t.Errorf("the ledger after the retry and the next hook: %+v, want exactly one C>IDLE done row", rows)
	}
}

// Red on dev: the bound all-done (markerless) close publishes IDLE with no recovery marker, so a close row
// whose append failed was lost for good - the answer warned, and a retry is refused IDLE->D. The row is now
// pending in the session's outbox and the next hook records it once, with its null closed work phase and
// the check epoch the close ran under.
func TestPromptDcloseAllDoneRowIsRecordedByTheNextHook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-all-done-outbox"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all done outbox"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-all-done-outbox")
	attest := promptDcloseAttest("", promptDcloseReceipt(t, cwd, "s1", "c-all-done-outbox"))
	// A ledger the close can read (its row guard does) but not append to.
	ledger := promptDclosePabcdLedgerPath(cwd)
	if err := os.WriteFile(ledger, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "[crw: DONE]") || !strings.Contains(answer, "the close was applied but its ledger row could not be written: ") {
		t.Errorf("the all-done answer: %q", answer)
	}
	if err := os.Chmod(ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	promptOrchestrateAnswer(t, cwd, "s1", "t2", "next prompt")
	promptOrchestrateAnswer(t, cwd, "s1", "t3", "and the one after")
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["reason"] != "done" || rows[0]["checkEpoch"] != "c-all-done-outbox" {
		t.Fatalf("the ledger after the next hooks: %+v, want exactly the all-done row", rows)
	}
	if v, present := rows[0]["closedWorkPhaseId"]; !present || v != nil {
		t.Errorf("closedWorkPhaseId: %v (present %v), want a present null", v, present)
	}
}

// An all-done close whose PABCD ledger cannot be searched for its close row still leaves a recoverable row: the answer warns,
// the event stays pending, and the next hook records exactly one close row once the ledger reads again.
func TestPromptDcloseAllDoneLedgerReadErrorKeepsARecoverableRow(t *testing.T) {
	cwd, attest := promptDcloseAllDoneRepo(t, "chat-all-done-readerr", "c-all-done-readerr")
	ledger := promptDclosePabcdLedgerPath(cwd)
	if err := os.MkdirAll(ledger, 0o755); err != nil {
		t.Fatal(err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "[crw: DONE]") || !strings.Contains(answer, "the close was applied but its ledger row could not be written: ") {
		t.Fatalf("the all-done answer: %q", answer)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, "s1"); len(pending) != 1 {
		t.Fatalf("pending events after the read error: %+v", pending)
	}
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	promptOrchestrateAnswer(t, cwd, "s1", "t2", "next prompt")
	promptOrchestrateAnswer(t, cwd, "s1", "t3", "and the one after")
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["reason"] != "done" || rows[0]["checkEpoch"] != "c-all-done-readerr" {
		t.Fatalf("the ledger after the next hooks: %+v, want exactly the all-done row", rows)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, "s1"); len(pending) != 0 {
		t.Fatalf("pending events after the recovery: %+v", pending)
	}
}

// A row left pending is recorded by the PostCompact hook too, which writes the session under its lock (1100 evaluation: it used to
// clear the injection cursor and leave the event, and a cleanup behind it, pending).
func TestPostCompactRecordsAPendingRow(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) { s.Phase = state.PhaseP; s.OrchestrationActive = true })
	seams := &promptDcloseSeams{afterOrchestratePublish: func() bool { return true }}
	promptSubmitHandleWith(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate A", TurnID: "t1", PabcdEnabled: true},
		"", promptSubmitHost(cwd), state.WithSessionLock, seams)
	if edges := promptOutboxEdges(t, cwd); len(edges) != 0 {
		t.Fatalf("a stopped writer appended %v", edges)
	}
	s := state.ReadState(cwd, "s1")
	if !sessionHookPostCompactEligible(s) {
		phase := state.PhaseA
		s.LastInjectedPhase = &phase
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
	}
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "s1"})
	if edges := promptOutboxEdges(t, cwd); len(edges) != 1 || edges[0] != "P>A" {
		t.Fatalf("the ledger after PostCompact: %v, want exactly [P>A]", edges)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, "s1"); len(pending) != 0 {
		t.Fatalf("pending after PostCompact: %+v", pending)
	}
}
