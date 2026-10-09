package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CRW-838 (post-evaluation): two grades of one subject and head that start in the same second
// are both recorded, each under an id and a copy of its own. The id stays the first sixteen hex
// characters of sha256(target, head, graded_at); the graded_at of the later row moves on to the
// next free second instead of the second grade being refused.
func TestCRW838SameSecondRegradeWithAnotherResultIsRecorded(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	e := crw838Env(t, &now)
	t.Setenv("AUDIT_FAKE", "json")
	t.Setenv("AUDIT_JSON", crw838JSONFirst)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFakeGrader(t)})
	bundle := auditGoodBundle(t, auditModePR)
	crw838Grade(t, e, cfg, bundle)
	t.Setenv("AUDIT_JSON", crw838JSONSecond)
	crw838Grade(t, e, cfg, bundle)
	rows := crw838Rows(t, state)
	if len(rows) != 2 {
		t.Fatalf("the ledger has %d rows, want both grades", len(rows))
	}
	crw838CheckCopies(t, state, rows, []string{crw838JSONFirst, crw838JSONSecond})
}

func crw838CheckCopies(t *testing.T, state string, rows []map[string]json.RawMessage, want []string) {
	t.Helper()
	ids := map[string]bool{}
	for i, row := range rows {
		id := crw838String(t, row, "id")
		if ids[id] {
			t.Errorf("row %d shares the id %s", i, id)
		}
		ids[id] = true
		data, err := os.ReadFile(filepath.Join(state, "audit", "results", id+".json"))
		if err != nil || string(data) != want[i] {
			t.Errorf("row %d copy = %q (%v), want %q", i, data, err, want[i])
		}
		wantID := auditRowID("s", "h", crw838String(t, row, "graded_at"))
		if id != wantID {
			t.Errorf("row %d id %s is not the id of its own graded_at (%s)", i, id, wantID)
		}
	}
}

// A batch of bundles that name the same subject and head, graded in one second with the default
// workers, records every one of them, with different results and with the same result.
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
			if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: first}, {Bundle: second}}); err != nil {
				t.Fatalf("AuditGrade: %v", err)
			}
			rows := crw838Rows(t, state)
			if len(rows) != 2 {
				t.Fatalf("the ledger has %d rows, want 2", len(rows))
			}
			crw838CheckCopies(t, state, rows, []string{crw838JSONFirst, other})
		})
	}
}
