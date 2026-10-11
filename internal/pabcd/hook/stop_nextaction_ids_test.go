package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// stopAttestWord is the shell word after `--attest ` in a block's command, read as a POSIX shell reads a single-quoted word
// (a quote inside it is written close, backslash, quote, reopen), so a test sees what the agent's shell passes to the CLI.
func stopAttestWord(t *testing.T, reason string) string {
	t.Helper()
	_, rest, ok := strings.Cut(reason, "--attest '")
	if !ok {
		t.Fatalf("no --attest word in %q", reason)
	}
	var word strings.Builder
	for i := 0; i < len(rest); i++ {
		if rest[i] != '\'' {
			word.WriteByte(rest[i])
			continue
		}
		if strings.HasPrefix(rest[i:], `'\''`) {
			word.WriteByte('\'')
			i += 3
			continue
		}
		return word.String()
	}
	t.Fatalf("unterminated --attest word in %q", reason)
	return ""
}

// CRW-1189 verification round 1 (P1): a work phase id the goalplan accepts (Unicode, spaces, colons, quotes) is given to the
// agent exactly: the attest the block shows decodes to the id, and the binding gate accepts it.
func TestStopContinuationCarriesAnyWorkPhaseIDTheGoalplanAccepts(t *testing.T) {
	ids := []string{"wp7", "검증단계", "phase 7", "phase:7", `it's "quoted"`, "back`tick", `back\slash`, "a<b>&c"}
	for _, id := range ids {
		for _, phase := range []state.Phase{state.PhaseP, state.PhaseB, state.PhaseC} {
			t.Run(id+"/"+string(phase), func(t *testing.T) {
				cwd, env := stopRig(t, "active")
				stopInFlight(t, cwd, phase, func(s *state.State) { s.Slug = "export" })
				stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
					p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase(id, "Exporter", goalplan.WorkPhaseInProgress)}
				})
				reason := stopBlockReason(t, stopRun(cwd, env))
				var att attest.Attestation
				if err := json.Unmarshal([]byte(stopAttestWord(t, reason)), &att); err != nil {
					t.Fatalf("the attest is not JSON: %v in %q", err, reason)
				}
				if att.WorkPhaseID != id {
					t.Errorf("workPhaseId=%q, want %q in %q", att.WorkPhaseID, id, reason)
				}
				if bind := attest.ValidateWorkPhaseBinding(&att, &id); !bind.OK {
					t.Errorf("the binding gate refuses the shown attest: %s", bind.Reason)
				}
				if strings.Contains(strings.SplitN(reason, "\n", 4)[2], "`"+"`") {
					t.Errorf("the command span is broken: %q", reason)
				}
			})
		}
	}
}

// The Windows form passes the JSON as a PowerShell single-quoted string, where a quote is doubled, not escaped as for a POSIX shell.
func TestStopContinuationQuotesAnIDForPowerShell(t *testing.T) {
	id := `it's "q"`
	got, ok := stopNextCommand(state.PhaseB, "win32", stopBinding{bound: true, workPhaseID: id})
	if !ok {
		t.Fatal("no command")
	}
	if strings.Contains(got, `'\''`) || !strings.Contains(got, `it''s`) {
		t.Errorf("a PowerShell string doubles the quote: %q", got)
	}
}

// A bound session whose goalplan cannot be read is still bound: the receipt the D close requires (CHECK-BINDING-01 keys on the
// slug alone) is still named and written first, no workPhaseId is shown (the binding gate lets a session through while no plan
// resolves), and the block says why and what to do.
func TestStopContinuationOfABoundSessionWithAnUnreadablePlanKeepsTheReceipt(t *testing.T) {
	for _, damage := range []string{"absent", "corrupt"} {
		t.Run(damage, func(t *testing.T) {
			cwd, env := stopRig(t, "active")
			stopInFlight(t, cwd, state.PhaseC, func(s *state.State) { s.Slug = "export" })
			if damage == "corrupt" {
				stopWritePlan(t, cwd, "export", nil)
				matches, _ := filepath.Glob(filepath.Join(cwd, ".crw", "goalplans", "export", "*.json"))
				if len(matches) == 0 {
					t.Fatal("no plan file to damage")
				}
				if err := os.WriteFile(matches[0], []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			reason := stopBlockReason(t, stopRun(cwd, env))
			if !strings.Contains(reason, `"testReceiptPath":".crw/evidence/rec-s1/test-receipt.json"`) {
				t.Errorf("the receipt path is missing in %q", reason)
			}
			if !strings.Contains(reason, "`CRW pabcd receipt test --session rec-s1 -- <the check command>`") {
				t.Errorf("the receipt command is missing in %q", reason)
			}
			if strings.Contains(reason, `,"workPhaseId":`) {
				t.Errorf("no id is known: %q", reason)
			}
			if !strings.Contains(reason, "goalplan export could not be read") || !strings.Contains(reason, "workPhaseId") {
				t.Errorf("the block does not say why no workPhaseId is shown and what to do: %q", reason)
			}
		})
	}
}
