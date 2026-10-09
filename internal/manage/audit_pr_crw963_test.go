package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crw963World is a pull request list whose newest target (#12) can never be audited, because
// its patch cannot be read, and whose older target (#7) can. Every run goes through the real
// run function with --max 1.
type crw963World struct {
	t      *testing.T
	state  string
	cfg    *Config
	gh     *[][]string
	merge  string
	listed func(merge string) string
}

func newCRW963World(t *testing.T) *crw963World {
	t.Helper()
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	w := &crw963World{t: t, state: state, cfg: cfg, merge: "m12"}
	w.listed = func(merge string) string {
		return auditPRListJSON(t,
			auditPRMergeEntry(12, "CRW-12: always fails", "2026-10-05T00:00:00Z", merge),
			auditPRMergeEntry(7, "CRW-7: older and fine", "2026-10-03T00:00:00Z", "m7"))
	}
	auditPRFakeRelay(t,
		map[string]string{
			"CRW-12": auditPRAssignmentJSON(t, "rel-12", "child-12"),
			"CRW-7":  auditPRAssignmentJSON(t, "rel-7", "child-7"),
		},
		map[string]string{
			"child-12": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
			"child-7":  auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
		},
		map[string]string{
			"rel-12": auditPRCriteriaJSON(t, "c1", "it works"),
			"rel-7":  auditPRCriteriaJSON(t, "c1", "it works"),
		})
	auditPRFakeCheckout(t, "m7", map[string]string{"internal/a.go": "package a\n"})
	return w
}

// run installs the gh answer for the current head of #12 and runs `audit pr --max <max>`.
// #12 has no diff, so reading its patch fails; #7 has one.
func (w *crw963World) run(max int) (int, string) {
	w.t.Helper()
	patch := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	w.gh = auditPRFakeGh(w.t, w.listed(w.merge), map[int]string{7: patch})
	e, _, errOut := auditTestEnv(w.t)
	code := auditPRRunWith(context.Background(), e, w.cfg, max, false)
	return code, errOut.String()
}

func (w *crw963World) diffCalls(number string) int {
	n := 0
	for _, call := range *w.gh {
		if len(call) > 2 && call[0] == "pr" && call[1] == "diff" && call[2] == number {
			n++
		}
	}
	return n
}

func (w *crw963World) ledgerSubjects() []string {
	w.t.Helper()
	e, _, _ := auditTestEnv(w.t)
	rows, err := auditReportLedger(e, w.cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row.Subject)
	}
	return out
}

func (w *crw963World) failuresPath() string {
	return filepath.Join(w.state, "audit", "audit-pr-failures.jsonl")
}

// CRW-963 end condition 1: an always failing newest target does not hold the --max 1 place
// of an older pull request. The first run tries the newest and fails; the second run takes
// the older one that has never failed.
func TestCRW963AlwaysFailingNewestTargetDoesNotStarveAnOlderOne(t *testing.T) {
	w := newCRW963World(t)
	if code, errOut := w.run(1); code != 1 || !strings.Contains(errOut, "#12") {
		t.Fatalf("first run: exit %d %q, want 1 naming #12", code, errOut)
	}
	if got := w.ledgerSubjects(); len(got) != 0 {
		t.Fatalf("the failed target left ledger rows: %v", got)
	}
	if code, errOut := w.run(1); code != 0 {
		t.Fatalf("second run: exit %d %q, want 0 (the older pull request is audited)", code, errOut)
	}
	if got := w.ledgerSubjects(); len(got) != 1 || got[0] != "pr-7" {
		t.Fatalf("the ledger holds %v after two --max 1 runs, want pr-7", got)
	}
	if n := w.diffCalls("12"); n != 0 {
		t.Errorf("the second run tried #12 again (%d diff calls) before the older target", n)
	}
}

// CRW-963 end condition 2: three failures at the same head and the target is skipped, named
// in the report, and does not take a --max place.
func TestCRW963ThreeFailuresAtTheSameHeadSkipTheTarget(t *testing.T) {
	w := newCRW963World(t)
	// With --max 9 both targets are tried every run; #12 fails each time.
	for i := 1; i <= 3; i++ {
		code, _ := w.run(9)
		if code != 1 {
			t.Fatalf("run %d: exit %d, want 1 (the target failed)", i, code)
		}
	}
	if n := w.diffCalls("12"); n != 1 {
		t.Fatalf("the third run read #12's patch %d times, want once", n)
	}
	code, errOut := w.run(9)
	if code != 0 {
		t.Fatalf("fourth run: exit %d %q, want 0 (a skipped target is not a failure)", code, errOut)
	}
	if n := w.diffCalls("12"); n != 0 {
		t.Errorf("the fourth run tried #12 again: %d diff calls", n)
	}
	report, err := os.ReadFile(filepath.Join(w.state, "audit", auditReportFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "skipped: failed 3 times at m12") || !strings.Contains(string(report), "pr-12") {
		t.Errorf("the report does not name the skipped target:\n%s", report)
	}
}

// CRW-963 end condition 3: a new head makes the target a candidate again.
func TestCRW963ANewHeadMakesTheTargetACandidateAgain(t *testing.T) {
	w := newCRW963World(t)
	for i := 0; i < 3; i++ {
		w.run(9)
	}
	w.merge = "m12b"
	if code, _ := w.run(9); code != 1 {
		t.Fatalf("the run with a new head exited %d, want 1 (#12 is tried again and fails again)", code)
	}
	if n := w.diffCalls("12"); n != 1 {
		t.Errorf("a new head made #12 a candidate %d times, want once", n)
	}
	report, err := os.ReadFile(filepath.Join(w.state, "audit", auditReportFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), "skipped: failed 3 times") {
		t.Errorf("the report still lists #12 as skipped after its head changed:\n%s", report)
	}
}

// CRW-963 end condition 4: the failure record is append-only, one line per failure with the
// target, the head, the time and the reason, and a dry run writes none.
func TestCRW963TheFailureRecordIsAppendOnly(t *testing.T) {
	w := newCRW963World(t)
	w.run(9)
	first, err := os.ReadFile(w.failuresPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(first)), "\n")
	if len(lines) != 1 {
		t.Fatalf("the record has %d lines after one failure: %q", len(lines), first)
	}
	var row map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatal(err)
	}
	if row["target"] != "pr-12" || row["head"] != "m12" || row["at"] != "2023-11-14T22:13:20Z" || !strings.Contains(row["reason"], "12") {
		t.Errorf("the failure line is %v", row)
	}
	w.run(9)
	second, err := os.ReadFile(w.failuresPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(second), string(first)) || strings.Count(string(second), "\n") != 2 {
		t.Errorf("the second failure did not append to the first:\n%s", second)
	}
	// A dry run resolves and prints; it records nothing.
	patch := ""
	w.gh = auditPRFakeGh(t, w.listed(w.merge), map[int]string{7: patch})
	e, _, _ := auditTestEnv(t)
	auditPRRunWith(context.Background(), e, w.cfg, 9, true)
	third, _ := os.ReadFile(w.failuresPath())
	if string(third) != string(second) {
		t.Errorf("a dry run changed the failure record:\n%s", third)
	}
}
