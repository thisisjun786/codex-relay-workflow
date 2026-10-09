package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// CRW-838 (fix round 1): the list is read on until the targets are found, however many audited
// pull requests sit in the newest windows.
func TestCRW838PaginationPastTenThousandAuditedPullRequests(t *testing.T) {
	w := &crw838Window{}
	audited := map[string]bool{}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for n := 1; n <= 10001; n++ {
		w.entries = append(w.entries, auditPRListEntryOf(n, fmt.Sprintf("CRW-%d: change", n), base.Add(time.Duration(n)*time.Second).Format(time.RFC3339), fmt.Sprintf("m%d", n)))
		if n > 1 {
			audited[fmt.Sprintf("pr-%d", n)] = true
		}
	}
	w.install(t)
	pattern := regexp.MustCompile(auditPRDefaultPattern)
	entries, err := auditPRListPages(context.Background(), nil, base, pattern, audited, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	targets, _, err := auditPRSelect(entries, pattern, base, audited, nil, 1)
	if err != nil || len(targets) != 1 || targets[0].Number != 1 {
		t.Fatalf("the oldest unaudited pull request was not reached: targets=%v err=%v windows=%d entries=%d", targets, err, len(w.searches), len(entries))
	}
}

// A window gh keeps answering with the same entries cannot be followed: the listing says so by
// name instead of returning a short list as if it were whole.
func TestCRW838PaginationStallIsAnError(t *testing.T) {
	w := &crw838Window{}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for n := 1; n <= 201; n++ {
		w.entries = append(w.entries, auditPRListEntryOf(n, fmt.Sprintf("CRW-%d: change", n), base.Add(time.Hour).Format(time.RFC3339), fmt.Sprintf("m%d", n)))
	}
	w.install(t)
	audited := map[string]bool{}
	for n := 1; n <= 201; n++ {
		audited[fmt.Sprintf("pr-%d", n)] = true
	}
	_, err := auditPRListPages(context.Background(), nil, base, regexp.MustCompile(auditPRDefaultPattern), audited, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "pr_list_stalled") {
		t.Fatalf("a stalled listing must be a named error, got %v", err)
	}
}

// CRW-838 (fix round 1): the copy is made from the bytes the grade read and validated, not from
// a grade.json read again after the whole batch.
func TestCRW838BatchCopiesTheGradedBytes(t *testing.T) {
	state := t.TempDir()
	e, _, _ := auditTestEnv(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { now = now.Add(time.Second); return now }
	first := auditGoodBundle(t, auditModePR)
	second := auditGoodBundle(t, auditModePR)
	script := filepath.Join(t.TempDir(), "grader.sh")
	text := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" != '%s' ]; then printf '%%s' '%s' > '%s/grade.json'; fi\nprintf '%%s' '%s' > \"$1/grade.json\"\n", first, crw838JSONSecond, first, crw838JSONFirst)
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := auditSectionConfig(t, state, map[string]any{"grader": []string{script, "{bundle}"}, "workers": 1})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: first}, {Bundle: second}}); err != nil {
		t.Fatal(err)
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "" {
			t.Fatalf("an ok row carries an id: %+v", row)
		}
	}
	copyBytes, err := auditResultCopyRead(e, cfg, rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(copyBytes) != crw838JSONFirst {
		t.Fatalf("the copy of the first grade holds %s", copyBytes)
	}
}

// A grade file removed after the grade was read does not cost the row its copy.
func TestCRW838BatchKeepsTheCopyWhenTheFileIsGone(t *testing.T) {
	state := t.TempDir()
	e, _, _ := auditTestEnv(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { now = now.Add(time.Second); return now }
	first := auditGoodBundle(t, auditModePR)
	second := auditGoodBundle(t, auditModePR)
	script := filepath.Join(t.TempDir(), "grader.sh")
	text := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" != '%s' ]; then rm -f '%s/grade.json'; fi\nprintf '%%s' '%s' > \"$1/grade.json\"\n", first, first, crw838JSONFirst)
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := auditSectionConfig(t, state, map[string]any{"grader": []string{script, "{bundle}"}, "workers": 1})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: first}, {Bundle: second}}); err != nil {
		t.Fatal(err)
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Status != auditStatusOK || rows[0].ID == "" {
		t.Fatalf("an ok row must carry its copy: %+v", rows)
	}
	if _, err := auditResultCopyRead(e, cfg, rows[0]); err != nil {
		t.Fatal(err)
	}
}

// A new ok row whose result has nothing to copy is refused by name and not recorded.
func TestCRW838OKRowWithoutACopyIsRefused(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	cfg := auditSectionConfig(t, t.TempDir(), nil)
	result := AuditResult{Mode: auditModePR, Subject: "s", Head: "h", Status: auditStatusOK, Score: 7, GradedAt: "2026-10-10T00:00:00Z", Bundle: t.TempDir()}
	rows, err := auditRecord(e, cfg, []AuditResult{result})
	if err == nil || rows != 0 || !strings.Contains(err.Error(), "result_copy_unavailable") {
		t.Fatalf("an ok row with no result to copy must be refused by name, got rows=%d err=%v", rows, err)
	}
	ledger, readErr := auditReportLedger(e, cfg)
	if readErr != nil || len(ledger) != 0 {
		t.Fatalf("nothing is recorded: %v %v", ledger, readErr)
	}
}
