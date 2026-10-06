//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

var auditDocs = []string{
	`{"phase":"P","updatedAt":"2026-01-01T00:00:00.000Z"}`,
	`{"phase":"P"}`,
	`{"phase":"P","updatedAt":5}`,
	`{"phase":"NOPE","updatedAt":"2026-01-01T00:00:00.000Z"}`,
	`{"phase":5,"updatedAt":"2026-01-01T00:00:00.000Z"}`,
	`{"updatedAt":"2026-01-01T00:00:00.000Z"}`,
	`{"phase":"P","slug":"my-slug","updatedAt":"2025-05-05T05:05:05.000Z","flags":{"auditPassed":true}}`,
	`{`,
	``,
	` 
`,
	`{"phase":"P"} x`,
	"\uFEFF" + `{"phase":"P","updatedAt":"2026-01-01T00:00:00.000Z"}`,
	`{"phase":"P","updatedAt":"2026-01-01T00:00:00.000Z","extra":NaN}`,
	`{'phase':'P','updatedAt':'2026-01-01T00:00:00.000Z'}`,
	`{"phase":"P","updatedAt":"2026-01-01T00:00:00.000Z"}   `,
	`[]`,
	`null`,
	`5`,
}

func TestAuditProbePredicateMirrorsReader(t *testing.T) {
	for _, doc := range auditDocs {
		root := t.TempDir()
		path := filepath.Join(root, ".crw", "sessions", "s.json")
		if doc != "" {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			_ = os.WriteFile(path, []byte(doc), 0o644)
		}
		s, _ := state.ReadStateStrict(root, "s")
		t.Logf("doc=%q predicate=%v reader.updatedAt=%q", doc, readDefaultedUpdatedAt([]byte(doc)), s.UpdatedAt)
	}
}

func TestAuditProbeMaskSpans(t *testing.T) {
	for _, doc := range []string{
		`{"a":{"updatedAt":"2026-01-01T00:00:00.000Z"},"updatedAt":"2026-02-02T00:00:00.000Z"}`,
		`{"x":"a\"b","updatedAt":"2026-01-01T00:00:00.000Z"}`,
		`{ "updatedAt" : "2026-01-01T00:00:00.000Z" , "b":1}`,
		`{"updatedAt":"nope"}`,
		`{"updatedAt":"2026-01-01T00:00:00Z"}`,
		`{"updatedAt":"2026-01-01T00:00:00.000Z","updatedAt":"2026-03-03T00:00:00.000Z"}`,
		`{"updatedAt":"2026-01-01T00:00:00.000Z"} x`,
		`[1,2]`,
		`null`,
		`5`,
		`{`,
		``,
	} {
		t.Logf("doc=%q masked=%q", doc, maskTopLevelTimestamp(doc))
	}
}
