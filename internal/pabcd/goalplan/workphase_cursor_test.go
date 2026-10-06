package goalplan

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The oracle's answers for the work-phase cursor cases of work-phase-states.test.ts
// and goalplan-regression.test.ts:184-191, and the absentSuccessorDetail wordings,
// recorded once with testdata/workphase_cursor/record-oracle.mjs (CXC v0.2.40 under
// Node 24); no Node runs here. A case holds the plan the test handed the oracle and
// the compact answer it gave.
func TestWorkphaseCursorOracleParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/workphase_cursor/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Oracle          string
		EffectiveCursor []struct {
			Test     string
			Plan     Goalplan
			Expected *string
		}
		AbsentSuccessorDetail []struct {
			Reason   string
			Expected string
		}
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.Oracle != "CXC v0.2.40 3c1459ac" || len(corpus.EffectiveCursor) != 18 || len(corpus.AbsentSuccessorDetail) != 4 {
		t.Fatalf("recorded corpus shrank: oracle=%q cursor=%d absent=%d", corpus.Oracle, len(corpus.EffectiveCursor), len(corpus.AbsentSuccessorDetail))
	}
	for _, c := range corpus.EffectiveCursor {
		t.Run("cursor/"+c.Test, func(t *testing.T) {
			before, err := json.Marshal(&c.Plan)
			if err != nil {
				t.Fatal(err)
			}
			got := EffectiveActiveWorkPhaseID(&c.Plan)
			switch {
			case got == nil && c.Expected == nil:
			case got == nil || c.Expected == nil || *got != *c.Expected:
				t.Errorf("effectiveActiveWorkPhaseId = %v; want %v", workphaseCursorDisplay(got), workphaseCursorDisplay(c.Expected))
			}
			after, err := json.Marshal(&c.Plan)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("the cursor reader mutated its plan")
			}
		})
	}
	for _, c := range corpus.AbsentSuccessorDetail {
		t.Run("absent/"+c.Reason, func(t *testing.T) {
			if got := AbsentSuccessorDetail(c.Reason); got != c.Expected {
				t.Errorf("absentSuccessorDetail(%q) = %q; want %q", c.Reason, got, c.Expected)
			}
		})
	}
}

func workphaseCursorDisplay(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}
