package recall

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// CRW-1083 -- an index of an older schema asked for --verify says it needs a rebuild; a status read changes nothing.
func TestVerifiedStatusOfAnOlderSchemaIndexSaysRebuildRequired(t *testing.T) {
	db, path := indexTestDB(t)
	home := t.TempDir()
	writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestMessage(t, "retainedterm opening"))
	ingestForTest(t, home, db, 0)
	recallSQL(t, db, "ALTER TABLE files DROP COLUMN file_id; ALTER TABLE files DROP COLUMN checkpoint; UPDATE meta SET value='2' WHERE key='schema_version'")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, map[string]any, string) {
		var out, stderr bytes.Buffer
		code := Run(append([]string{"chat", "index", "--home", home, "--index-path", path, "--status", "--json"}, args...), &out, &stderr, time.Now())
		var report map[string]any
		_ = json.Unmarshal(out.Bytes(), &report)
		return code, report, stderr.String()
	}
	code, report, stderr := run("--verify")
	if code != 0 || report["freshness"] != "rebuild-required" || report["staleFiles"] != float64(0) {
		t.Fatalf("code=%d report=%v stderr=%s", code, report, stderr)
	}
	if code, report, _ := run(); code != 0 || report["freshness"] != nil {
		t.Errorf("plain status: %d %v", code, report)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"chat", "index", "--home", home, "--index-path", path, "--status", "--verify"}, &out, &errOut, time.Now()); code != 0 || !bytes.Contains(out.Bytes(), []byte("(rebuild-required)")) {
		t.Errorf("%d %q %q", code, out.String(), errOut.String())
	}
	check, err := openIndexReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if rows := indexRows(t, check, "SELECT value FROM meta WHERE key = 'schema_version'"); len(rows) != 1 || rows[0]["value"] != "2" || filesHasColumn(check, "file_id") {
		t.Errorf("a status read changed the index: %v", rows)
	}
	// The writer then rebuilds it, and the verified status is content-verified again.
	var w, werr bytes.Buffer
	if code := Run([]string{"chat", "index", "--home", home, "--index-path", path, "--verify", "--json"}, &w, &werr, time.Now()); code != 0 {
		t.Fatalf("%d %s", code, werr.String())
	}
	var report2 map[string]any
	_ = json.Unmarshal(w.Bytes(), &report2)
	if report2["freshness"] != "content-verified" {
		t.Errorf("%v", report2)
	}
}
