package review

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
)

func TestNormalizeGrade(t *testing.T) {
	for _, c := range []struct {
		raw      string
		grade    Grade
		security bool
		ok       bool
	}{
		{"P0", P0, false, true}, {" p1 ", P1, false, true}, {"Critical", P0, false, true}, {"blocker", P0, false, true},
		{"high", P1, false, true}, {"medium", P2, false, true}, {"low", P3, false, true}, {"nit", P3, false, true},
		{"High security", P1, true, true}, {"security: critical", P0, true, true}, {"low, high", P1, false, true},
		{"security", "", true, false}, {"", "", false, false}, {"bogus", "", false, false}, {"P4", "", false, false}, {"highest", "", false, false},
	} {
		if g, sec, ok := NormalizeGrade(c.raw); g != c.grade || sec != c.security || ok != c.ok {
			t.Errorf("NormalizeGrade(%q) = %q, %v, %v; want %q, %v, %v", c.raw, g, sec, ok, c.grade, c.security, c.ok)
		}
	}
}

// The rows are the decision table in the package documentation.
func TestDecide(t *testing.T) {
	keep, drop := Decision{Keep: true}, func(r DropReason) Decision { return Decision{Reason: r} }
	for _, c := range []struct {
		v       Verdict
		support int
		severe  bool
		want    Decision
	}{
		{VerdictConfirmed, 1, false, keep}, {VerdictConfirmed, 3, true, keep}, {VerdictRejected, 3, true, drop(ReasonRejected)},
		{VerdictUncertain, 2, false, keep}, {VerdictUncertain, 1, true, keep}, {VerdictUncertain, 1, false, drop(ReasonBelowThreshold)},
		{VerdictUncertain, 0, false, drop(ReasonBelowThreshold)}, {VerdictUnverified, 1, false, keep}, {"", 1, false, keep}, {"bogus", 1, false, keep},
	} {
		if got := Decide(c.v, c.support, c.severe); got != c.want {
			t.Errorf("Decide(%q, %d, %v) = %+v, want %+v", c.v, c.support, c.severe, got, c.want)
		}
	}
}

func TestNoiseFilter(t *testing.T) {
	f, err := NewNoiseFilter("^gen/")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"go.sum": true, "sub/go.sum": true, "web/package-lock.json": true, "vendor/a/b.go": true,
		"node_modules/x/y.js": true, "web/app.min.js": true, "api/x.pb.go": true, "gen/a.go": true,
		"main.go": false, "internal/vendorish/a.go": false, "docs/go.sum.md": false} {
		if got := f.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
	if _, err := NewNoiseFilter("[bad"); err == nil {
		t.Error("an invalid pattern must be refused")
	}
}

type fakeHead map[string]int

func (h fakeHead) Lines(path string) (int, error) {
	if path == "broken.go" {
		return 0, errors.New("disk error")
	}
	if n, ok := h[path]; ok {
		return n, nil
	}
	return 0, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

func finding(mut func(*Finding)) Finding {
	f := Finding{File: "a.go", Line: 3, Title: "Nil map write", Explanation: "m is nil here", Severity: "high", Grade: P1,
		Perspective: "correctness", Reviewers: []int{0, 2}, Support: 2, Verdict: VerdictConfirmed}
	if mut != nil {
		mut(&f)
	}
	return f
}

func TestApply(t *testing.T) {
	rules := Rules{
		Changed: map[string]bool{"a.go": true, "b.go": true, "go.sum": true, "gone.go": true, "broken.go": true},
		Head:    fakeHead{"a.go": 10, "b.go": 5, "go.sum": 3},
	}
	for _, c := range []struct {
		name   string
		in     Finding
		reason DropReason // empty: kept
		file   string     // kept: the stored path
		want   Verdict    // kept: the stored verdict
	}{
		{"kept as reported", finding(nil), "", "a.go", VerdictConfirmed},
		{"path is cleaned", finding(func(f *Finding) { f.File = "./a.go" }), "", "a.go", VerdictConfirmed},
		{"last line of the file", finding(func(f *Finding) { f.Line, f.EndLine = 8, 10 }), "", "a.go", VerdictConfirmed},
		{"no verdict is unverified", finding(func(f *Finding) { f.Verdict = "" }), "", "a.go", VerdictUnverified},
		{"unrecognised verdict is unverified", finding(func(f *Finding) { f.Verdict = "bogus" }), "", "a.go", VerdictUnverified},
		{"explanation is not tested for non-finding", finding(func(f *Finding) { f.Explanation = "no issues in tests, but" }), "", "a.go", VerdictConfirmed},
		{"non-finding title", finding(func(f *Finding) { f.Title = "LOOKS GOOD" }), ReasonNonFinding, "", ""},
		{"non-finding explanation without a title", finding(func(f *Finding) { f.Title, f.Explanation = "", "No issues found" }), ReasonNonFinding, "", ""},
		{"nothing to report", finding(func(f *Finding) { f.Title, f.Explanation = "", "" }), ReasonNonFinding, "", ""},
		{"noise file beats unknown grade", finding(func(f *Finding) { f.File, f.Grade = "go.sum", "" }), ReasonNoiseFile, "", ""},
		{"empty path", finding(func(f *Finding) { f.File = "" }), ReasonInvalidLocation, "", ""},
		{"absolute path", finding(func(f *Finding) { f.File = "/etc/passwd" }), ReasonInvalidLocation, "", ""},
		{"escaping path", finding(func(f *Finding) { f.File = "../a.go" }), ReasonInvalidLocation, "", ""},
		{"line zero", finding(func(f *Finding) { f.Line = 0 }), ReasonInvalidLocation, "", ""},
		{"range ends before it starts", finding(func(f *Finding) { f.EndLine = 2 }), ReasonInvalidLocation, "", ""},
		{"file not in the diff", finding(func(f *Finding) { f.File = "./other.go" }), ReasonNotInDiff, "", ""},
		{"file deleted at head", finding(func(f *Finding) { f.File = "gone.go" }), ReasonMissingAtHead, "", ""},
		{"line beyond head", finding(func(f *Finding) { f.Line = 11 }), ReasonLineBeyondHead, "", ""},
		{"range end beyond head", finding(func(f *Finding) { f.EndLine = 11 }), ReasonLineBeyondHead, "", ""},
		{"bare security is not kept", finding(func(f *Finding) { f.Grade, f.Security = "", true }), ReasonUnknownGrade, "", ""},
		{"rejected", finding(func(f *Finding) { f.Verdict = VerdictRejected }), ReasonRejected, "", ""},
		{"uncertain, one reviewer, P2", finding(func(f *Finding) { f.Verdict, f.Support, f.Grade = VerdictUncertain, 1, P2 }), ReasonBelowThreshold, "", ""},
		{"uncertain, one reviewer, P3 security", finding(func(f *Finding) { f.Verdict, f.Support, f.Grade, f.Security = VerdictUncertain, 1, P3, true }), "", "a.go", VerdictUncertain},
		{"unverified, one reviewer, P3 is kept", finding(func(f *Finding) { f.Verdict, f.Support, f.Grade = VerdictUnverified, 1, P3 }), "", "a.go", VerdictUnverified},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := rules.Apply([]Finding{c.in})
			switch {
			case err != nil:
				t.Fatal(err)
			case c.reason == "" && (len(got.Findings) != 1 || got.Findings[0].File != c.file || got.Findings[0].Verdict != c.want):
				t.Fatalf("want one kept finding %s/%s, got %+v", c.file, c.want, got)
			case c.reason != "" && (len(got.Dropped) != 1 || got.Dropped[0].Reason != c.reason || got.Dropped[0].Finding.File != c.in.File):
				t.Fatalf("want one drop %s of the finding as received, got %+v", c.reason, got)
			}
		})
	}
	if _, err := rules.Apply([]Finding{finding(func(f *Finding) { f.File = "broken.go" })}); err == nil {
		t.Error("a head reader error other than not-exist must be returned")
	}
	got, _ := rules.Apply([]Finding{finding(nil), finding(func(f *Finding) { f.File = "x.go" }), finding(func(f *Finding) { f.File = "b.go" })})
	if len(got.Findings) != 2 || !slices.Equal([]string{got.Findings[0].File, got.Findings[1].File}, []string{"a.go", "b.go"}) || len(got.Dropped) != 1 {
		t.Errorf("input order must be kept, got %+v", got)
	}
	if empty, _ := rules.Apply(nil); empty.Findings == nil || empty.Dropped == nil {
		t.Error("Result slices must be non-nil")
	}
}
