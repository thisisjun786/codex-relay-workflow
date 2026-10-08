package premerge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The table below ports the branches of premerge_gate.py (the oracle, CRW-952 inputs) one by one. Each case
// names the branch it covers; the expected result is PASS (""), or the refusal name the branch produces.

const (
	testHead = "29db17e49d8222278a2e4aae43712c64df259b83"
	testDev  = "a850a3fcce3e36d6ef0bb74589375b8432a44207"
	testDig  = "13095caa0f1bf5b57cbee6a5b8e74a1c00eb48e7e0dc762889b0f79c27b557b9"
)

func baseRecord() Record {
	return Record{
		Schema: RecordSchema, Issue: "CRW-952", Node: "crw-952", Head: testHead, Dev: testDev, CriteriaDigest: testDig,
		Grader: Grader{Model: "gpt-6.1-sol", Effort: "xhigh", PromptDigest: "sha256:0000"}, GradedAt: "2026-10-08T12:00:00Z",
		Criteria: map[string]Criterion{"c1": {Verdict: "PASS", Evidence: "covered by the table test"}},
		Defects:  []Defect{}, Score: floatPtr(9), Summary: "clean run",
		Dispositions: Dispositions{By: "parent"},
	}
}

func floatPtr(f float64) *float64 { return &f }

func mustRaw(t *testing.T, r Record) []byte {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// verdictOf runs the record through Decode and Judge the way dag-accept does and returns the refusal name, or "" for PASS.
func verdictOf(t *testing.T, raw []byte, opts Options) contract.RefusalReason {
	t.Helper()
	rec, err := Decode(raw)
	if err == nil {
		err = Judge(rec, opts)
	}
	if err == nil {
		return ""
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a refusal must be a *Refusal, got %T: %v", err, err)
	}
	return refusal.Reason
}

func TestOracleBranches(t *testing.T) {
	const pass = contract.RefusalReason("")
	cases := []struct {
		name   string
		mutate func(*Record)
		opts   Options
		want   contract.RefusalReason
	}{
		{name: "clean record passes", want: pass},
		{name: "no criteria is an incomplete record", mutate: func(r *Record) { r.Criteria = nil }, want: contract.RefusalPremergeMissing},
		{name: "no defects list is an incomplete record", mutate: func(r *Record) { r.Defects = nil }, want: contract.RefusalPremergeMissing},
		{name: "no score is an incomplete record", mutate: func(r *Record) { r.Score = nil }, want: contract.RefusalPremergeMissing},
		{name: "head that is not a full sha is an incomplete record", mutate: func(r *Record) { r.Head = "29db17e4" }, want: contract.RefusalPremergeMissing},
		{name: "moved head without afterEvaluation", opts: Options{AfterEvaluation: true}, want: contract.RefusalPremergeAfterEvaluationMissing},
		{name: "moved head with a short afterEvaluation", opts: Options{AfterEvaluation: true},
			mutate: func(r *Record) { r.Dispositions.AfterEvaluation = "small fix" }, want: contract.RefusalPremergeAfterEvaluationMissing},
		{name: "moved head with afterEvaluation of 20 characters", opts: Options{AfterEvaluation: true},
			mutate: func(r *Record) { r.Dispositions.AfterEvaluation = "twenty chars of text!!" }, want: pass},
		{name: "PARTIAL criterion without a disposition", mutate: func(r *Record) {
			r.Criteria["c1"] = Criterion{Verdict: "PARTIAL", Evidence: "x"}
		}, want: contract.RefusalPremergeUndisposed},
		{name: "criterion without a verdict is refused by the schema before the branch (the oracle blocks it as well)", mutate: func(r *Record) {
			r.Criteria["c1"] = Criterion{Evidence: "x"}
		}, want: contract.RefusalPremergeMissing},
		{name: "P1 defect without a disposition", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P1", What: "x"}}
		}, want: contract.RefusalPremergeUndisposed},
		{name: "P3 defect marked minor_separable needs no disposition", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P3", Impact: "minor_separable", What: "x"}}
		}, want: pass},
		{name: "P3 defect without an impact needs no disposition", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P3", What: "x"}}
		}, want: pass},
		{name: "P3 defect with a regression impact needs a disposition", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P3", Impact: "regression_introduced", What: "x"}}
		}, want: contract.RefusalPremergeUndisposed},
		{name: "blocking item for a finding nobody needed blocks", mutate: func(r *Record) {
			r.Dispositions.Items = []Item{{Ref: "c9", Class: "blocking", Note: "parent decision"}}
		}, want: contract.RefusalPremergeBlocked},
		{name: "blocking item for a needed finding blocks", mutate: func(r *Record) {
			r.Criteria["c1"] = Criterion{Verdict: "FAIL"}
			r.Dispositions.Items = []Item{{Ref: "c1", Class: "blocking", Note: "fix it"}}
		}, want: contract.RefusalPremergeBlocked},
		{name: "separable criterion is never separable", mutate: func(r *Record) {
			r.Criteria["c1"] = Criterion{Verdict: "PARTIAL"}
			r.Dispositions.Items = []Item{{Ref: "c1", Class: "separable", FollowUp: "CRW-900", Note: "an independent reason of enough length here"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable P0 is forbidden", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P0", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900", Note: "an independent reason of enough length here"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable introduced defect is forbidden", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", Introduced: true, What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900", Note: "an independent reason of enough length here"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable regression_introduced defect is forbidden", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", Impact: "regression_introduced", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900", Note: "an independent reason of enough length here"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable in_promise defect is forbidden", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", InPromise: true, What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900", Note: "an independent reason of enough length here"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable with a follow-up that is not an issue key", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "later", Note: "an independent reason of enough length here"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable without a note", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable whose note is only an edit region", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900", Note: "outside the edit region"}}
		}, want: contract.RefusalPremergeSeparableForbidden},
		{name: "separable with an edit region and a real reason passes", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-900",
				Note: "Outside the declared region: the reader is a separate path that this diff never reaches and its own tests cover it fully."}}
		}, want: pass},
		{name: "separable outside the issue passes", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "separable", FollowUp: "CRW-901",
				Note: "independent: the stale reader is a separate path the diff does not reach, and it has its own tests"}}
		}, want: pass},
		{name: "not_applicable with a short note", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P1", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "not_applicable", Note: "no"}}
		}, want: contract.RefusalPremergeDispositionInvalid},
		{name: "already_resolved with code evidence passes", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P1", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "already_resolved", Note: "daemon.go:245 already halts on the damage path"}}
		}, want: pass},
		{name: "unknown class is invalid", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P1", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "maybe", Note: "x"}}
		}, want: contract.RefusalPremergeDispositionInvalid},
		{name: "an item for a finding nobody needed is not read", mutate: func(r *Record) {
			r.Dispositions.Items = []Item{{Ref: "d9", Class: "maybe", Note: "x"}}
		}, want: pass},
		{name: "carried P0 is never carried", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P0", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "carried P1 is fixed, not carried", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P1", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "carried FAIL criterion is fixed, not carried", mutate: func(r *Record) {
			r.Criteria["c1"] = Criterion{Verdict: "FAIL"}
			r.Dispositions.Items = []Item{{Ref: "c1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "carried PARTIAL criterion passes", mutate: func(r *Record) {
			r.Criteria["c1"] = Criterion{Verdict: "PARTIAL"}
			r.Dispositions.Items = []Item{{Ref: "c1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: pass},
		{name: "carried regression_introduced without newCodeOnly is forbidden", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", Impact: "regression_introduced", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "carried regression_introduced in code this change added passes", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", Impact: "regression_introduced", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", NewCodeOnly: "the new stress test in premerge_test.go",
				Note: "carried with a reason of enough length"}}
		}, want: pass},
		{name: "carried regression P1 with newCodeOnly is still forbidden", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P1", Impact: "regression_introduced", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", NewCodeOnly: "the new stress test in premerge_test.go",
				Note: "carried with a reason of enough length"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "carried with a bad follow-up key", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-", Note: "carried with a reason of enough length"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "carried with a short note", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", Note: "short"}}
		}, want: contract.RefusalPremergeCarriedForbidden},
		{name: "a later item for the same ref replaces an earlier one", mutate: func(r *Record) {
			r.Defects = []Defect{{ID: "d1", Severity: "P2", What: "x"}}
			r.Dispositions.Items = []Item{{Ref: "d1", Class: "blocking", Note: "first"},
				{Ref: "d1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: pass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := baseRecord()
			if c.mutate != nil {
				c.mutate(&r)
			}
			if got := verdictOf(t, mustRaw(t, r), c.opts); got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestRecordMembersAndEncoding(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want contract.RefusalReason
	}{
		{"not JSON", "{", contract.RefusalPremergeMissing},
		{"another schema", strings.Replace(string(mustRaw(t, baseRecord())), "premerge-record/1", "premerge-record/2", 1), contract.RefusalPremergeMissing},
		{"lone high surrogate in a string", strings.Replace(string(mustRaw(t, baseRecord())), "clean run", "bad \\ud800 text", 1), contract.RefusalPremergeMissing},
		{"lone low surrogate in a string", strings.Replace(string(mustRaw(t, baseRecord())), "clean run", "bad \\udc00 text", 1), contract.RefusalPremergeMissing},
		{"invalid UTF-8 bytes", strings.Replace(string(mustRaw(t, baseRecord())), "clean run", "bad \xff text", 1), contract.RefusalPremergeMissing},
		{"a paired surrogate is valid text", strings.Replace(string(mustRaw(t, baseRecord())), "clean run", "pair \\ud83d\\ude00", 1), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdictOf(t, []byte(c.raw), Options{}); got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestDigestIsCanonical(t *testing.T) {
	a := []byte(`{"schema":"premerge-record/1","issue":"CRW-952","score":9}`)
	b := []byte("{\n  \"score\": 9,\n  \"issue\": \"CRW-952\",\n  \"schema\": \"premerge-record/1\"\n}")
	da, err := Digest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Digest(b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("the same record with other key order and spacing must digest alike: %s != %s", da, db)
	}
	changed := []byte(`{"schema":"premerge-record/1","issue":"CRW-952","score":9.0}`)
	dc, err := Digest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if dc == da {
		t.Fatal("a changed number must change the digest (numbers are kept as written)")
	}
}
