package evidence

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/directives.golden.json pins the texts of the two directives under the CRW names. They are the oracle's texts with
// ".codexclaw" read as ".crw": the test derives them from the recording of the oracle's own gate and from the corpus fixture
// that records its three blocks, so the golden cannot drift from either.

type directiveGolden struct {
	Verifier   []string
	Escalation string
}

func loadDirectives(t *testing.T) directiveGolden {
	t.Helper()
	var g directiveGolden
	raw, err := os.ReadFile("testdata/directives.golden.json")
	must(t, err)
	must(t, json.Unmarshal(raw, &g))
	return g
}

func TestDirectivesAreTheOraclesTexts(t *testing.T) {
	_, oracle := loadUnrec(t)
	golden, rename := loadDirectives(t), strings.NewReplacer(".codexclaw", ".crw")
	if len(golden.Verifier) != MaxAttempts {
		t.Fatalf("%d verifier texts, want one per attempt (%d)", len(golden.Verifier), MaxAttempts)
	}
	for i, want := range golden.Verifier {
		if got := VerifierDirective(i + 1); got != want {
			t.Errorf("VerifierDirective(%d) = %q, want %q", i+1, got, want)
		}
		if o := oracle.Directives.Verifier[i]; o == nil || o.Decision != "block" || rename.Replace(o.Reason) != want {
			t.Errorf("the golden of attempt %d is not the oracle's block %+v", i+1, o)
		}
	}
	if oracle.Directives.Verifier[MaxAttempts] != nil {
		t.Errorf("the oracle's stop after the budget is released, not blocked: %+v", oracle.Directives.Verifier[MaxAttempts])
	}
	if got := EscalationDirective(); got != golden.Escalation || rename.Replace(oracle.Directives.Escalation) != golden.Escalation {
		t.Errorf("EscalationDirective() = %q, golden %q, oracle %q", got, golden.Escalation, oracle.Directives.Escalation)
	}
}

func TestVerifierDirectiveInTheCorpus(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/fixtures/cxc/hook__subagent-stop-verifying-evidence__three_attempts_then_release.json")
	must(t, err)
	var fx struct {
		Expect struct {
			Steps []struct {
				StdoutJSON struct{ Decision, Reason string } `json:"stdout_json"`
			}
		}
	}
	must(t, json.Unmarshal(raw, &fx))
	rename := strings.NewReplacer(".codexclaw", ".crw")
	for i := 1; i <= MaxAttempts; i++ {
		if step := fx.Expect.Steps[i-1].StdoutJSON; step.Decision != "block" || rename.Replace(step.Reason) != VerifierDirective(i) {
			t.Errorf("fixture block %d: %+v, want the directive %q", i, step, VerifierDirective(i))
		}
	}
}
