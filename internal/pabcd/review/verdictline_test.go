package review

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// CRW-1116: the reviewer skill and the relay renderer both write `GO-WITH-FIXES (blockers=N)`; the sign-off parser reads it.
func TestParseSignoffReadsTheBlockerCount(t *testing.T) {
	ok := []struct {
		verdict  string
		want     goalplan.Verdict
		blockers int
		findings []string
	}{
		{"PASS", goalplan.VerdictPass, 0, nil},
		{"FAIL", goalplan.VerdictFail, 0, nil},
		{"NEAR-PASS", goalplan.VerdictNearPass, 0, nil},
		{"GO-WITH-FIXES", goalplan.VerdictNearPass, 0, nil},
		{"go-with-fixes", goalplan.VerdictNearPass, 0, nil},
		{"GO-WITH-FIXES (blockers=1)", goalplan.VerdictNearPass, 1, nil},
		{"GO-WITH-FIXES (blockers=12)", goalplan.VerdictNearPass, 12, nil},
		{"GO-WITH-FIXES (blockers=9999)", goalplan.VerdictNearPass, 9999, nil},
		{"go-with-fixes   ( Blockers = 3 )", goalplan.VerdictNearPass, 3, nil},
		{"NEAR-PASS (blockers=2)", goalplan.VerdictNearPass, 2, nil},
		{"GO-WITH-FIXES (blockers=2; findings=c1,r2)", goalplan.VerdictNearPass, 2, []string{"c1", "r2"}},
		{"GO-WITH-FIXES (blockers=1; findings=src/a.go:12)", goalplan.VerdictNearPass, 1, []string{"src/a.go:12"}},
	}
	for _, c := range ok {
		got := ParseSignoff("reviewed\nLAUNCH: r1-20260101000000\nVERDICT: " + c.verdict)
		if got == nil {
			t.Fatalf("%q was not parsed", c.verdict)
		}
		if got.LaunchID != "r1-20260101000000" || got.Verdict != c.want || got.Blockers != c.blockers || strings.Join(got.Findings, "|") != strings.Join(c.findings, "|") {
			t.Fatalf("%q: %+v", c.verdict, got)
		}
	}
	for _, bad := range []string{
		"GO-WITH-FIXES (blockers=0)", "GO-WITH-FIXES (blockers=-1)", "GO-WITH-FIXES (blockers=+2)", "GO-WITH-FIXES (blockers=)",
		"GO-WITH-FIXES (blockers)", "GO-WITH-FIXES (blockers=x)", "GO-WITH-FIXES (blockers=1.5)", "GO-WITH-FIXES (blockers=10000)",
		"GO-WITH-FIXES (blockers=99999999999999999999)", "GO-WITH-FIXES ()", "GO-WITH-FIXES (blockers=2", "GO-WITH-FIXES blockers=2",
		"GO-WITH-FIXES (blockers=2) trailing", "GO-WITH-FIXES (blockers=2)(blockers=3)", "GO-WITH-FIXES (count=2)",
		"GO-WITH-FIXES (blockers=2; findings=)", "GO-WITH-FIXES (blockers=2; findings=a,,b)", "GO-WITH-FIXES (blockers=2; other=a)",
		"GO-WITH-FIXES (blockers=2; findings=a b)", "PASS (blockers=1)", "FAIL (blockers=1)", "PASS (blockers=0)", "(blockers=2)",
		"GO-WITH-FIXES (bloc\u212Aers=2)", "GO-WITH-FIXES (blockers=١)",
	} {
		if got := ParseSignoff("LAUNCH: id\nVERDICT: " + bad); got != nil {
			t.Fatalf("%q was parsed: %+v", bad, got)
		}
	}
}

func TestVerdictLineRenderingAndParsingAgree(t *testing.T) {
	for _, n := range []int{1, 2, 9999} {
		line := "VERDICT: GO-WITH-FIXES" + BlockerSuffix(n, nil)
		got := ParseSignoff("LAUNCH: id\n" + line)
		if got == nil || got.Blockers != n || got.Verdict != goalplan.VerdictNearPass {
			t.Fatalf("%q: %+v", line, got)
		}
	}
	if got := BlockerSuffix(2, []string{"c1", "r2"}); got != " (blockers=2; findings=c1,r2)" {
		t.Fatal(got)
	}
	if MaxBlockers != 9999 {
		t.Fatal(MaxBlockers)
	}
}

func TestRecordVerdictKeepsTheBlockerCountAndFindings(t *testing.T) {
	f := reviewTestFlight(t)
	done := reviewTestOK(t, RecordVerdict(f.Plan, VerdictInput{Purpose: f.Round.Purpose, RoundID: f.Round.RoundID, LaunchID: f.Round.Lane.LaunchID,
		Verdict: goalplan.VerdictNearPass, Blockers: 2, Findings: []string{"c1", "r2"}}))
	reviewTestEqual(t, done.Round.Status, goalplan.ReviewApproved)
	reviewTestEqual(t, done.Round.Lane.Verdict, goalplan.VerdictNearPass)
	reviewTestEqual(t, done.Round.Lane.Blockers, 2)
	reviewTestEqual(t, done.Round.Lane.Findings, []string{"c1", "r2"})
}
