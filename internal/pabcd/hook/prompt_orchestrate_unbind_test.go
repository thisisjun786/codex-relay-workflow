package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-146: the chat `orchestrate unbind` is the pair of `crw pabcd loop unbind`: the same release of the session's goalplan slug,
// from rest only, with one audit row.

func TestPromptOrchestrateUnbindReleasesTheSlugFromRest(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) { s.Slug = "the-slug" })
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate unbind"), "[crw — unbound goalplan the-slug]"; got != want {
		t.Errorf("the answer\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); s.Slug != "" || s.Phase != state.PhaseIdle {
		t.Errorf("the state: %+v", s)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "IDLE" || rows[0]["to"] != "IDLE" || rows[0]["reason"] != "unbound" {
		t.Errorf("the ledger rows: %+v", rows)
	}
	// a repeat is a recognised no-op: nothing more is written
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t2", "orchestrate unbind"), "[crw — no goalplan is bound]"; got != want {
		t.Errorf("the repeat\n got %q\nwant %q", got, want)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Errorf("the repeat wrote a row: %+v", rows)
	}
}

func TestPromptOrchestrateUnbindIsRefusedInFlight(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Slug, s.Phase, s.OrchestrationActive = "the-slug", state.PhaseB, true
	})
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate unbind")
	if want := "[crw — refused: unbind: a PABCD cycle is in flight at phase B"; len(got) < len(want) || got[:len(want)] != want {
		t.Errorf("the refusal %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Slug != "the-slug" || s.Phase != state.PhaseB {
		t.Errorf("a refused unbind changed the state: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("a refused unbind wrote a row: %+v", rows)
	}
}

// CRW-146 verification fix round 1: the chat release writes the session state and one transition row and nothing else. It is not a
// phase move, so none of the bookkeeping a phase move does (the injection cursor, the injected turns, the Stop stagnation reset)
// applies, and the followups an earlier writer left pending in the outbox (a re-plan's plan-audit cleanup rewrites the goalplan) are
// not run by it or by the prompt entry that carries it.

func unbindChatSeed(t *testing.T, cwd string) {
	t.Helper()
	cleanupPlan(t, cwd, "demo")
	phase := state.PhaseB
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Slug = "demo"
		pointer := "wp-1"
		s.StopBlockWorkPhaseID = &pointer
		s.StopBlockPhase, s.StopBlockCount = &phase, 2
		s.InjectedTurns = []string{"old-turn"}
	})
}

func unbindChatPending(t *testing.T, cwd string) {
	t.Helper()
	cleanup := NewPlanAuditCleanup("demo", "e-new", []string{"r1"})
	followup, err := json.Marshal(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	s := state.ReadState(cwd, "s1")
	ev, err := state.NewLedgerEvent(cwd, s, s, nil, followup)
	if err != nil {
		t.Fatal(err)
	}
	ev.Published = true
	if err := state.PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
}

// unbindChatNormalised is the session state with the fields the prompt entry stamps for every prompt (the Stop turn guard and the
// write time) cleared: what is left is what the command itself decided.
func unbindChatNormalised(t *testing.T, s state.State) string {
	t.Helper()
	s.UpdatedAt, s.StopBlockTotal, s.StopBlockTurnID, s.StopBlockCapNotified = "", 0, nil, false
	b, err := state.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Red on ab12f038: the command went through the phase-transition field rewrite, which cleared the Stop stagnation counters and the
// injection cursor and appended the turn to the injected turns.
func TestVerifyUnbindChatPreservesUnrelatedState(t *testing.T) {
	cwd := t.TempDir()
	unbindChatSeed(t, cwd)
	before := state.ReadState(cwd, "s1")
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate unbind"); got != "[crw — unbound goalplan demo]" {
		t.Fatalf("the answer %q", got)
	}
	want := before
	want.Slug, want.StopBlockWorkPhaseID = "", nil
	if got, wantJSON := unbindChatNormalised(t, state.ReadState(cwd, "s1")), unbindChatNormalised(t, want); got != wantJSON {
		t.Fatalf("state\n got %s\nwant %s", got, wantJSON)
	}
}

// Red on ab12f038: the command's drains ran the pending plan-audit cleanup, which rewrote the goalplan and appended a
// review_round_superseded row to its ledger.
func TestVerifyUnbindDoesNotWriteGoalplanThroughPendingCleanup(t *testing.T) {
	cwd := t.TempDir()
	unbindChatSeed(t, cwd)
	unbindChatPending(t, cwd)
	dir, err := goalplan.GoalplanDir(cwd, "demo")
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	planBefore, ledgerBefore := read("goalplan.json"), read(goalplan.GoalplanLedgerFile)
	pending := func() int {
		entries, _ := os.ReadDir(state.StatePath(cwd, "s1") + ".ledger-outbox")
		return len(entries)
	}
	if pending() != 1 {
		t.Fatalf("the outbox before: %d", pending())
	}
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate unbind"); got != "[crw — unbound goalplan demo]" {
		t.Fatalf("the answer %q", got)
	}
	if !bytes.Equal(planBefore, read("goalplan.json")) {
		t.Fatal("the goalplan file changed")
	}
	if !bytes.Equal(ledgerBefore, read(goalplan.GoalplanLedgerFile)) {
		t.Fatalf("the goalplan ledger changed: %s", read(goalplan.GoalplanLedgerFile))
	}
	if pending() != 1 {
		t.Fatalf("the cleanup left the outbox: %d", pending())
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["reason"] != "unbound" {
		t.Fatalf("the transition rows: %+v", rows)
	}
}

// The prompt entry's own drain is the same for a prompt that carries a remember request: the release never runs a followup.
func TestVerifyUnbindDoesNotWriteGoalplanWhenThePromptAlsoAsksToRemember(t *testing.T) {
	cwd := t.TempDir()
	unbindChatSeed(t, cwd)
	unbindChatPending(t, cwd)
	dir, err := goalplan.GoalplanDir(cwd, "demo")
	if err != nil {
		t.Fatal(err)
	}
	planBefore, err := os.ReadFile(filepath.Join(dir, "goalplan.json"))
	if err != nil {
		t.Fatal(err)
	}
	promptOrchestrateAnswer(t, cwd, "s1", "t1", "remember this for later\norchestrate unbind")
	planAfter, err := os.ReadFile(filepath.Join(dir, "goalplan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(planBefore, planAfter) {
		t.Fatal("the goalplan file changed")
	}
}

// CRW-146 evaluation round (d1): the chat release answers "no goalplan is bound" only for a session state it could read. A damaged
// state file is not a session without a binding: the answer refuses, names the damaged state, and the file stays as it is, with or
// without a turn id and with the unbind also asking to remember (the CLI verb refuses the same file).
func TestPromptOrchestrateUnbindRefusesAnUnreadableSessionState(t *testing.T) {
	for _, tc := range []struct{ name, turn, prompt string }{
		{"with a turn id", "t1", "orchestrate unbind"},
		{"without a turn id", "", "orchestrate unbind"},
		{"with a remember request", "t1", "remember this for later\norchestrate unbind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			damaged := []byte(`{"phase":"IDLE","slug":"demo"`)
			path := state.StatePath(cwd, "s1")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, damaged, 0o600); err != nil {
				t.Fatal(err)
			}
			got := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: tc.prompt, TurnID: tc.turn, PabcdEnabled: true}, "", promptSubmitHost(cwd))
			want := "[crw — refused: unbind: the session state is unreadable; refusing to overwrite it. Nothing was written.]"
			if got != want {
				t.Errorf("the answer\n got %q\nwant %q", got, want)
			}
			if b, _ := os.ReadFile(path); !bytes.Equal(b, damaged) {
				t.Errorf("the damaged state was replaced: %q", b)
			}
			if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
				t.Errorf("a refused unbind wrote a row: %+v", rows)
			}
		})
	}
}

// A session without a state file is the clean rest: the repeat answer stays.
func TestPromptOrchestrateUnbindOfASessionWithoutStateIsTheNoopAnswer(t *testing.T) {
	cwd := t.TempDir()
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate unbind"), "[crw — no goalplan is bound]"; got != want {
		t.Errorf("the answer\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(state.StatePath(cwd, "s1")); !os.IsNotExist(err) {
		t.Errorf("a state file appeared: %v", err)
	}
}
