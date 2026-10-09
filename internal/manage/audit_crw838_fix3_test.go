package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRW-838 (decision of 10-10, second): the id of an ok row is the first sixteen hex characters
// of sha256(target, head, graded_at, sha256 of the graded result bytes). graded_at stays the time
// of the grading. Two grades of one target and head in one second are both recorded: other bytes
// get other ids and copies, the same bytes share one id and one copy.

func crw838CheckRows(t *testing.T, state string, rows []map[string]json.RawMessage, at string, want []string) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("the ledger has %d rows, want %d", len(rows), len(want))
	}
	for i, row := range rows {
		if got := crw838String(t, row, "graded_at"); got != at {
			t.Errorf("row %d graded_at = %q, want the real grading time %q", i, got, at)
		}
		id := crw838String(t, row, "id")
		if wantID := crw838WantID("s", "h", at, want[i]); id != wantID {
			t.Errorf("row %d id = %q, want %q", i, id, wantID)
		}
		data, err := os.ReadFile(filepath.Join(state, "audit", "results", id+".json"))
		if err != nil || string(data) != want[i] {
			t.Errorf("row %d copy = %q (%v), want %q", i, data, err, want[i])
		}
	}
}

func crw838CopyFiles(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "audit", "results"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if strings.Contains(entry.Name(), "tmp") {
			t.Errorf("a temporary file is left: %s", entry.Name())
		}
	}
	return names
}

func TestCRW838SameSecondRegradeWithAnotherResultIsRecorded(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	e := crw838Env(t, &now)
	t.Setenv("AUDIT_FAKE", "json")
	t.Setenv("AUDIT_JSON", crw838JSONFirst)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFakeGrader(t)})
	bundle := auditGoodBundle(t, auditModePR)
	first := crw838Grade(t, e, cfg, bundle)
	t.Setenv("AUDIT_JSON", crw838JSONSecond)
	second := crw838Grade(t, e, cfg, bundle)
	const at = "2026-10-10T01:00:00Z"
	for i, r := range []AuditResult{first, second} {
		if r.GradedAt != at {
			t.Errorf("result %d graded_at = %q, want %q", i, r.GradedAt, at)
		}
	}
	crw838CheckRows(t, state, crw838Rows(t, state), at, []string{crw838JSONFirst, crw838JSONSecond})
	if names := crw838CopyFiles(t, state); len(names) != 2 {
		t.Errorf("results holds %v, want two copies", names)
	}
	// The time-range filter sees the real time: nothing was graded at or after the next second.
	listing, err := AuditList(context.Background(), e, cfg, AuditListOptions{HasSince: true, Since: now.Add(time.Second)})
	if err != nil || len(listing.Results) != 0 {
		t.Errorf("rows since the next second = %d (%v), want none", len(listing.Results), err)
	}
	listing, err = AuditList(context.Background(), e, cfg, AuditListOptions{HasSince: true, Since: now})
	if err != nil || len(listing.Results) != 2 {
		t.Errorf("rows since the grading second = %d (%v), want both", len(listing.Results), err)
	}
}

func TestCRW838SameSecondRegradeWithTheSameResultSharesOneCopy(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 30, 0, 0, time.UTC)
	e := crw838Env(t, &now)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", crw838JSONFirst)})
	bundle := auditGoodBundle(t, auditModePR)
	crw838Grade(t, e, cfg, bundle)
	crw838Grade(t, e, cfg, bundle)
	rows := crw838Rows(t, state)
	crw838CheckRows(t, state, rows, "2026-10-10T01:30:00Z", []string{crw838JSONFirst, crw838JSONFirst})
	if crw838String(t, rows[0], "id") != crw838String(t, rows[1], "id") {
		t.Errorf("the same bytes got two ids")
	}
	if names := crw838CopyFiles(t, state); len(names) != 1 {
		t.Errorf("results holds %v, want one shared copy", names)
	}
}

// A batch of bundles that name the same subject and head, graded in one second with the default
// workers, records every one of them at the real time: other results get ids of their own, the
// same result shares one id and one copy.
func TestCRW838SameSecondBatchOfOneTargetIsRecorded(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same=%v", same), func(t *testing.T) {
			state := t.TempDir()
			now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
			e := crw838Env(t, &now)
			first := auditGoodBundle(t, auditModePR)
			second := auditGoodBundle(t, auditModePR)
			other := crw838JSONSecond
			if same {
				other = crw838JSONFirst
			}
			script := filepath.Join(t.TempDir(), "grader.sh")
			text := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = '%s' ]; then printf '%%s' '%s' > \"$1/grade.json\"; else printf '%%s' '%s' > \"$1/grade.json\"; fi\n", first, crw838JSONFirst, other)
			if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := auditSectionConfig(t, state, map[string]any{"grader": []string{script, "{bundle}"}, "workers": 3})
			results, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: first}, {Bundle: second}})
			if err != nil {
				t.Fatalf("AuditGrade: %v", err)
			}
			for i, r := range results {
				if r.GradedAt != "2026-10-10T02:00:00Z" {
					t.Errorf("result %d graded_at = %q, want the real time", i, r.GradedAt)
				}
			}
			rows := crw838Rows(t, state)
			crw838CheckRows(t, state, rows, "2026-10-10T02:00:00Z", []string{crw838JSONFirst, other})
			wantCopies := 2
			if same {
				wantCopies = 1
			}
			if names := crw838CopyFiles(t, state); len(names) != wantCopies {
				t.Errorf("results holds %v, want %d copies", names, wantCopies)
			}
		})
	}
}

// A file under the id with other bytes cannot come from the digest of these bytes; it is a real
// error, named, and the row is not appended.
func TestCRW838ACopyWithOtherBytesUnderTheIDIsRefused(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
	e := crw838Env(t, &now)
	bundle := auditGoodBundle(t, auditModePR)
	if err := os.WriteFile(filepath.Join(bundle, auditGradeFile), []byte(crw838JSONFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := auditSectionConfig(t, state, nil)
	result := AuditResult{Mode: auditModePR, Subject: "s", Head: "h", Issue: "CRW-1", Status: auditStatusOK, Score: 6, GradedAt: "2026-10-10T03:00:00Z", Bundle: bundle}
	path := filepath.Join(state, "audit", "results", crw838WantID("s", "h", result.GradedAt, crw838JSONFirst)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(crw838JSONSecond), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := auditRecord(e, cfg, []AuditResult{result})
	if err == nil || !strings.Contains(err.Error(), "result_copy_conflict") || rows != 0 {
		t.Fatalf("auditRecord = %d, %v, want result_copy_conflict and no row", rows, err)
	}
	if data, _ := os.ReadFile(path); string(data) != crw838JSONSecond {
		t.Errorf("the copy was replaced: %q", data)
	}
}
