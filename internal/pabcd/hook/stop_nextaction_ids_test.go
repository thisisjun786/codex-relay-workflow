package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// stopCliDecode is the decoding the CLI gives an --attest word (cli.decodeCliAttest: pyjson.Loads with the same options, then
// attest.Coerce, which trims every text field), so the tests below see the attestation the binding gate sees, not a plain JSON read.
func stopCliDecode(t *testing.T, word string) *attest.Attestation {
	t.Helper()
	v, err := pyjson.Loads(word, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Map: true})
	if err != nil {
		t.Fatalf("the attest is not JSON: %v in %q", err, word)
	}
	att := attest.Coerce(v)
	if att == nil {
		t.Fatalf("the CLI reads no attestation from %q", word)
	}
	return att
}

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

// CRW-1189 verification round 1 (P1): a work phase id the goalplan accepts (Unicode, inner spaces, colons, quotes) is given to the
// agent exactly: the attest the block shows, decoded as the CLI decodes it (CRW-1189 round 2, d3: attest.Coerce included), carries
// the id, and the binding gate accepts it.
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
				att := stopCliDecode(t, stopAttestWord(t, reason))
				if att.WorkPhaseID != id {
					t.Errorf("workPhaseId=%q, want %q in %q", att.WorkPhaseID, id, reason)
				}
				if bind := attest.ValidateWorkPhaseBinding(att, &id); !bind.OK {
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

// CRW-1189 round 2 (d3): the CLI trims every text field of an attest (attest.Coerce), so an id the goalplan stores with leading or
// trailing white space can never reach the binding gate from an attest. The block does not show a workPhaseId it knows to be
// refused; it says why and what to change, and the decoded attest is not offered with an id that differs from the plan's.
func TestStopContinuationOfAnIDWithPaddingSaysItCannotBeAttested(t *testing.T) {
	for _, id := range []string{" phase 7 ", "phase7\n", "\tphase7", "\u00a0phase7\u00a0"} {
		for _, phase := range []state.Phase{state.PhaseB, state.PhaseC} {
			t.Run(id+"/"+string(phase), func(t *testing.T) {
				cwd, env := stopRig(t, "active")
				stopInFlight(t, cwd, phase, func(s *state.State) { s.Slug = "export" })
				stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
					p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase(id, "Exporter", goalplan.WorkPhaseInProgress)}
				})
				reason := stopBlockReason(t, stopRun(cwd, env))
				att := stopCliDecode(t, stopAttestWord(t, reason))
				if att.WorkPhaseID != "" {
					t.Errorf("a refused id is not shown: workPhaseId=%q in %q", att.WorkPhaseID, reason)
				}
				if !strings.Contains(reason, "leading or trailing white space") || !strings.Contains(reason, "goalplan export") {
					t.Errorf("the block does not say the id cannot be attested and where to change it: %q", reason)
				}
				if phase == state.PhaseC && !strings.Contains(reason, `"testReceiptPath":".crw/evidence/rec-s1/test-receipt.json"`) {
					t.Errorf("the receipt path stays: %q", reason)
				}
			})
		}
	}
}
