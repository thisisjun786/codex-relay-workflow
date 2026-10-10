package hook

// CRW-1109 through the chat hook. Red on dev: a recognized command with an explicitly malformed --attest went
// ahead as a free transition (attestError was never read), and a command whose --attest text held a U+2028
// was read as chat. Both are refused now, with nothing written; a command without --attest is unchanged.

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestPromptOrchestrateMalformedAttestIsRefused(t *testing.T) {
	cwd := t.TempDir()
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P --attest {not json}")
	if !strings.Contains(got, "refused: the --attest of this command is malformed: attest JSON is not valid JSON") {
		t.Fatalf("answer: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Fatalf("the refused command moved the session: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refused command wrote rows: %+v", rows)
	}
	// Leaving --attest out is still the human free pass.
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t2", "orchestrate P"); !strings.Contains(got, "[crw: PLAN]") {
		t.Fatalf("the free pass: %q", got)
	}
}

func TestPromptOrchestrateSeparatorInACommandIsRefused(t *testing.T) {
	cwd := t.TempDir()
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P --attest {\"from\":\"IDLE\",\"to\":\"P\",\"did\":\"a b\"}")
	if !strings.Contains(got, "refused: the command's --attest text holds a carriage return, U+2028 or U+2029") {
		t.Fatalf("answer: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Fatalf("the refused command moved the session: %+v", s)
	}
}
