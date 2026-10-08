package premerge

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The nested members the schema names are enforced (CRW-952 review): a defect with no severity, a grader that names
// nothing, dispositions that are not an object, and a criterion with no verdict are refused as premerge_missing.
func TestDecodeRequiresTheNestedMembers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "a defect with no severity", mutate: func(r *Record) { r.Defects = []Defect{{ID: "d1", What: "x"}} }},
		{name: "a defect with a severity outside P0 to P3", mutate: func(r *Record) { r.Defects = []Defect{{ID: "d1", Severity: "P9"}} }},
		{name: "a grader that names no model", mutate: func(r *Record) { r.Grader.Model = "" }},
		{name: "a criterion with no verdict word", mutate: func(r *Record) { r.Criteria["c1"] = Criterion{Verdict: "maybe"} }},
		{name: "dispositions that name no one", mutate: func(r *Record) { r.Dispositions.By = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := baseRecord()
			c.mutate(&r)
			if got := verdictOf(t, mustRaw(t, r), Options{}); got != contract.RefusalPremergeMissing {
				t.Fatalf("want premerge_missing, got %q", got)
			}
		})
	}
}

// dispositions written as null is not an object, whatever the struct decodes it to.
func TestDecodeRefusesNullDispositions(t *testing.T) {
	var members map[string]any
	if err := json.Unmarshal(mustRaw(t, baseRecord()), &members); err != nil {
		t.Fatal(err)
	}
	members["dispositions"] = nil
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	if got := verdictOf(t, raw, Options{}); got != contract.RefusalPremergeMissing {
		t.Fatalf("want premerge_missing, got %q", got)
	}
}
