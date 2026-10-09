package managed

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// reconcileWrites is everything a managed start leaves behind that a stop which "writes nothing"
// must not change: the journal, the reservation rows and the marker files (the attempt markers
// among them), as one comparable string.
func reconcileWrites(t *testing.T, k *reconcileKit) string {
	t.Helper()
	var out []string
	for _, query := range []string{
		"SELECT seq||'|'||kind||'|'||COALESCE(subject,'')||'|'||COALESCE(detail,'') FROM journal ORDER BY seq",
		"SELECT request_id||'|'||state||'|'||revision||'|'||COALESCE(receipt_status,'') FROM managed_start_requests ORDER BY request_id",
	} {
		rows, err := k.start.Store.DB.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out = append(out, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	err := filepath.WalkDir(k.start.MarkerRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			out = append(out, "dir|"+path)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, "file|"+path+"|"+string(data))
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// CRW-935 item 1. A stop whose orphan re-read ended without a conclusion writes nothing - no
// attempt marker, no managed_start_observed row, no receipt change - and only answers with the
// unobservable reconciliation. The same request called again decides from the start.
func TestReconcileOrphanInconclusiveStopWritesNothing(t *testing.T) {
	t.Parallel()
	k, app := orphanKit(t, "name-timeout", "accept")
	k.host.sendReceipts = []map[string]any{orphanStandby("t-1")}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")

	k.host.failures["thread/read|t-1"] = errors.New("thread/read: connection reset by peer")
	before := reconcileWrites(t, k)
	for range 2 {
		got := k.run()
		k.expect(got, "incomplete", "creation_unknown", "unobservable")
		if detail := pyjson.Text(recon(got)["detail"]); !strings.Contains(detail, "could not be read again") {
			t.Fatalf("the stop's detail: %q", detail)
		}
		if after := reconcileWrites(t, k); after != before {
			t.Fatalf("an inconclusive stop wrote:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}
	if len(app.archiveCalls) != 0 || len(k.host.creates) != 1 {
		t.Fatalf("an inconclusive stop acted: archives=%v creates=%v", app.archiveCalls, k.host.creates)
	}

	// The repeat with a working read decides from the start: two archive rows and the next attempt.
	delete(k.host.failures, "thread/read|t-1")
	k.expect(k.run(), "admitted", "", "recreated")
	if len(app.archiveCalls) != 1 || orphanArchiveRows(t, k) != 2 || len(k.host.creates) != 2 {
		t.Fatalf("the repeat: archives=%v rows=%d creates=%v", app.archiveCalls, orphanArchiveRows(t, k), k.host.creates)
	}
}

// CRW-935 item 2. A child title over 500 bytes always gets the byte diagnostic, including one over
// 500 characters and one inside 500 characters whose bytes pass 500.
func TestParseRequestTitleOverFiveHundredBytesNamesTheByteBound(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, title string }{
		{"501 characters", strings.Repeat("a", 501)},
		{"inside 500 characters, over 500 bytes", strings.Repeat("é", 300)},
		{"over both, multibyte", strings.Repeat("é", 501)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			raw := requestWithTitle(t, c.title)
			_, err := ParseRequest(raw)
			if err == nil || !strings.Contains(err.Error(), "child.title must be at most 500 bytes") {
				t.Fatalf("title of %d bytes: %v", len(c.title), err)
			}
		})
	}
}
