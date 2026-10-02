package dagsched

import (
	"context"
	"math"
	"testing"
)

// D-05 stays undecided: the writer only records the evidence a ceiling above the standing cap rests on. A ceiling of 10 is clamped to 6 until the basis for that revision exists, a basis
// is evidence and so cannot be rewritten, and only the limit's declarer or the project's registered parent can record one.
func TestRecordCapBasis(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.declareLimit("project", "P-TEST", "runs", 10)
	basis := CapBasis{LimitID: "lim-project-runs", Revision: 1, WMinutes: 30, WSource: "measured: fork/join run", SMinutes: 5, SSource: "measured: parent turns", DecidedBy: "parent"}
	ctx := context.Background()
	if c := f.capacityNow(); c.Ceiling != 6 || c.Source != "clamped_no_basis" {
		t.Fatalf("before the basis: %+v", c)
	}
	if err := f.sched.RecordCapBasis(ctx, basis); err != nil {
		t.Fatal(err)
	}
	if c := f.capacityNow(); c.Ceiling != 10 || c.Basis != "recorded" {
		t.Fatalf("after the basis: %+v", c)
	}
	if err := f.sched.RecordCapBasis(ctx, basis); err != nil {
		t.Fatalf("the same basis again: %v", err)
	}
	other := basis
	other.WMinutes = 45
	if err := f.sched.RecordCapBasis(ctx, other); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a different basis for the same revision = %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*CapBasis)
		reason string
	}{
		"a revision the store does not hold": {func(b *CapBasis) { b.Revision = 9 }, "slot_unknown"},
		"another task":                       {func(b *CapBasis) { b.DecidedBy = "intruder" }, "scope_role_mismatch"},
		"no source for W":                    {func(b *CapBasis) { b.WSource = "" }, "malformed_receipt"},
		"zero minutes":                       {func(b *CapBasis) { b.SMinutes = 0 }, "malformed_receipt"},
		"infinite minutes":                   {func(b *CapBasis) { b.WMinutes = math.Inf(1) }, "malformed_receipt"},
		"not a number":                       {func(b *CapBasis) { b.WMinutes = math.NaN() }, "malformed_receipt"},
	} {
		t.Run(name, func(t *testing.T) {
			b := basis
			c.mutate(&b)
			if err := f.sched.RecordCapBasis(ctx, b); refusalReason(err) != c.reason {
				t.Fatalf("record = %v, want %s", err, c.reason)
			}
			if n := f.count("SELECT COUNT(*) FROM dag_cap_basis"); n != 1 {
				t.Fatalf("%d rows after a refusal", n)
			}
		})
	}
	f.declareLimit("project", "P-TEST", "tokens", 1000)
	tokens := basis
	tokens.LimitID = "lim-project-tokens"
	if err := f.sched.RecordCapBasis(ctx, tokens); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a basis for a tokens limit = %v", err)
	}
}
