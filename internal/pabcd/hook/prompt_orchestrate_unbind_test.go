package hook

import (
	"testing"

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
