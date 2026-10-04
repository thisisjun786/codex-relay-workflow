package recall

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ingestTestHome(t *testing.T) (string, *RwDb) {
	t.Helper()
	db, _ := indexTestDB(t)
	return buildIngestCodexHome(t), db
}
func ingestForTest(t *testing.T, home string, db *RwDb, days float64) IngestResult {
	t.Helper()
	r, err := ingest(home, db, days)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func ingestMainPath(t *testing.T, home string) string {
	t.Helper()
	files, err := ListRolloutFiles(home, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "-main.jsonl") {
			return f.Path
		}
	}
	t.Fatal("main fixture missing")
	return ""
}
func ingestMessage(t *testing.T, s string) string {
	return rolloutTestLine(t, map[string]any{"type": "response_item", "timestamp": "2026-01-01T00:00:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": s}}}})
}
func ingestWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}
func ingestFresh(t *testing.T, home string, db *RwDb, days float64, budget *FreshnessBudget) IndexFreshness {
	t.Helper()
	f, err := measureIndexFreshness(home, db, days, budget)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func ingestStamp(t *testing.T, db *RwDb) any {
	return recallRow(t, recallStmt(t, db, "SELECT value FROM meta WHERE key='last_ingest_at'"))["value"]
}

// Ports index.test.ts:31 and index-freshness.test.ts:179, including real pruning.
func TestIngestBuildNoopAndPrune(t *testing.T) {
	home, db := ingestTestHome(t)
	r := ingestForTest(t, home, db, 0)
	if r.Scanned != 4 || r.Ingested != 4 || r.Msgs != 12 {
		t.Fatal(r)
	}
	recallSQL(t, db, "UPDATE meta SET value='sentinel' WHERE key='last_ingest_at'")
	r = ingestForTest(t, home, db, 0)
	if r.Ingested+r.Appended+r.Pruned != 0 || ingestStamp(t, db) != "sentinel" {
		t.Fatal(r)
	}
	files, _ := ListRolloutFiles(home, 0)
	for _, f := range files {
		if strings.Contains(f.Path, "archived_sessions") {
			if err := os.Remove(f.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	if r = ingestForTest(t, home, db, 7); r.Pruned != 0 {
		t.Fatal(r)
	}
	if r = ingestForTest(t, home, db, 0); r.Pruned != 1 {
		t.Fatal(r)
	}
	if ingestStamp(t, db) == "sentinel" || len(indexRows(t, db, "SELECT path FROM files")) != 3 || len(indexRows(t, db, "SELECT id FROM msgs")) != 9 {
		t.Fatal("prune did not update records/stamp")
	}
	for _, table := range []string{"msgs_fts", "msgs_tri"} {
		if n := len(indexRows(t, db, "SELECT rowid FROM "+table+" WHERE "+table+" MATCH 'deployed'")); n != 3 {
			t.Fatalf("%s after prune: %d", table, n)
		}
	}
}

// Ports index.test.ts:140 and index-freshness.test.ts:197; offsets are bytes.
func TestIngestAppendAndPartialLine(t *testing.T) {
	home, db := ingestTestHome(t)
	ingestForTest(t, home, db, 0)
	recallSQL(t, db, "UPDATE meta SET value='sentinel' WHERE key='last_ingest_at'")
	p := ingestMainPath(t, home)
	b, _ := os.ReadFile(p)
	line := ingestMessage(t, "appended 한글 문장 quokka")
	partial := ingestMessage(t, "pending 한글")
	ingestWrite(t, p, append(b, []byte(line+partial[:len(partial)-2])...))
	r := ingestForTest(t, home, db, 0)
	if r.Appended != 1 || r.Ingested != 0 || r.Msgs != 1 || ingestStamp(t, db) == "sentinel" {
		t.Fatal(r)
	}
	row := recallRow(t, recallStmt(t, db, "SELECT bytes_ingested,last_ord FROM files WHERE path=?"), p)
	if row["bytes_ingested"] != float64(len(b)+len(line)) || row["last_ord"] != float64(4) {
		t.Fatal(row)
	}
	if n := len(indexRows(t, db, `SELECT rowid FROM msgs_tri WHERE msgs_tri MATCH '"quokka"'`)); n != 1 {
		t.Fatal(n)
	}
	b, _ = os.ReadFile(p)
	ingestWrite(t, p, append(b, []byte(partial[len(partial)-2:])...))
	if r = ingestForTest(t, home, db, 0); r.Msgs != 1 || r.Appended != 1 {
		t.Fatal(r)
	}
	if r = ingestForTest(t, home, db, 0); r.Ingested+r.Appended != 0 {
		t.Fatal(r)
	}
	if n := len(indexRows(t, db, "SELECT id FROM msgs")); n != 14 {
		t.Fatal(n)
	}
}

// Ports the storage portion of index.test.ts:202; search/scan stays separate.
func TestIngestToolCapUTF16(t *testing.T) {
	for _, input := range []string{strings.Repeat("y", TOOL_TEXT_CAP+500) + " needle", strings.Repeat("한", TOOL_TEXT_CAP+1), strings.Repeat("a", TOOL_TEXT_CAP-1) + "😀tail"} {
		t.Run(input[:1], func(t *testing.T) {
			db, _ := indexTestDB(t)
			home := t.TempDir()
			doc := rolloutTestLine(t, map[string]any{"type": "response_item", "timestamp": "t", "payload": map[string]any{"type": "function_call_output", "output": input}})
			writeRolloutTestFile(t, home, "sessions/2026/01/01/tool.jsonl", doc+ingestMessage(t, input))
			ingestForTest(t, home, db, 0)
			rows := indexRows(t, db, "SELECT match_field,text FROM msgs ORDER BY ord")
			want := strings.Repeat("y", TOOL_TEXT_CAP)
			if strings.HasPrefix(input, "한") {
				want = strings.Repeat("한", TOOL_TEXT_CAP)
			}
			if strings.HasPrefix(input, "a") {
				want = strings.Repeat("a", TOOL_TEXT_CAP-1) + "\ufffd"
			}
			if rows[0]["text"] != want || rows[1]["text"] != input {
				t.Fatal("UTF-16 cap or uncapped content differs")
			}
		})
	}
}

// Ports index.test.ts:246,441: normalized keys and legacy migration/backfill.
func TestIngestRepoKeysAndLegacyBackfill(t *testing.T) {
	home, db := ingestTestHome(t)
	ingestForTest(t, home, db, 0)
	rows := indexRows(t, db, "SELECT thread_id,repo_key FROM files ORDER BY thread_id")
	want := []map[string]any{{"thread_id": "archived", "repo_key": nil}, {"thread_id": "main", "repo_key": "github.com/example/alpha"}, {"thread_id": "old", "repo_key": "github.com/example/beta"}, {"thread_id": "sub", "repo_key": "github.com/example/alpha"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatal(rows)
	}
	before := indexRows(t, db, "SELECT id,text FROM msgs ORDER BY id")
	recallSQL(t, db, "DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key")
	ensureRepoKeyColumn(db)
	recallSQL(t, db, "UPDATE meta SET value='sentinel' WHERE key='last_ingest_at'")
	for range 2 {
		r := ingestForTest(t, home, db, 0)
		if r.Ingested+r.Appended+r.Pruned != 0 || !reflect.DeepEqual(before, indexRows(t, db, "SELECT id,text FROM msgs ORDER BY id")) || ingestStamp(t, db) != "sentinel" {
			t.Fatal("backfill reparsed messages")
		}
	}
	rows = indexRows(t, db, "SELECT thread_id,repo_key FROM files ORDER BY thread_id")
	want[2]["repo_key"] = nil // Unknown thread metadata is never guessed from a head.
	if !reflect.DeepEqual(rows, want) {
		t.Fatal(rows)
	}
}

// Ports index-freshness.test.ts:129,147,163,298, with read-only observations.
func TestIndexFreshnessPathsAndBudgets(t *testing.T) {
	for _, action := range []string{"none", "grown", "missing", "extra"} {
		t.Run(action, func(t *testing.T) {
			home, db := ingestTestHome(t)
			ingestForTest(t, home, db, 0)
			p := ingestMainPath(t, home)
			b, _ := os.ReadFile(p)
			switch action {
			case "grown":
				ingestWrite(t, p, append(b, []byte(ingestMessage(t, "freshness append"))...))
			case "missing":
				writeRolloutTestFile(t, home, "sessions/2026/01/01/new.jsonl", ingestMessage(t, "new"))
			case "extra":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
			before := indexRows(t, db, "SELECT key,value FROM meta ORDER BY key")
			f := ingestFresh(t, home, db, 0, nil)
			want := IndexFreshness{SourceFiles: 4, IndexedFiles: 4}
			switch action {
			case "grown":
				want.ChangedFiles = 1
			case "missing":
				want.SourceFiles = 5
				want.MissingFiles = 1
			case "extra":
				want.SourceFiles = 3
				want.ExtraFiles = 1
			}
			if action != "none" {
				want.StaleFiles = 1
			}
			if f != want {
				t.Fatalf("got %+v want %+v", f, want)
			}
			for _, budget := range []FreshnessBudget{{MaxStats: 0, MaxMs: 50}, {MaxStats: 512, MaxMs: 0}} {
				bounded := ingestFresh(t, home, db, 0, &budget)
				if !bounded.Truncated || bounded.ChangedFiles != 0 || bounded.StaleFiles > f.StaleFiles {
					t.Fatal(bounded)
				}
			}
			if !reflect.DeepEqual(before, indexRows(t, db, "SELECT key,value FROM meta ORDER BY key")) {
				t.Fatal("freshness wrote metadata")
			}
			if f := ingestFresh(t, home, db, 7, nil); f.ExtraFiles != 0 {
				t.Fatal(f)
			}
		})
	}
	if b := BannerFreshnessBudget(); b.MaxStats != 512 || b.MaxMs != 50 {
		t.Fatal(b)
	}
}

func TestIngestRewriteAndRollback(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "prune"}[prune], func(t *testing.T) {
			home, db := ingestTestHome(t)
			ingestForTest(t, home, db, 0)
			p := ingestMainPath(t, home)
			before := indexRows(t, db, "SELECT id,path,text FROM msgs ORDER BY id")
			files := indexRows(t, db, "SELECT path,size,bytes_ingested,last_ord FROM files ORDER BY path")
			if prune {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				recallSQL(t, db, "CREATE TRIGGER refuse BEFORE DELETE ON files BEGIN SELECT RAISE(ABORT,'fault'); END")
			} else {
				ingestWrite(t, p, []byte(ingestMessage(t, "replacement")))
				recallSQL(t, db, "CREATE TRIGGER refuse BEFORE INSERT ON files BEGIN SELECT RAISE(ABORT,'fault'); END")
			}
			if _, err := ingest(home, db, 0); err == nil || !strings.Contains(err.Error(), "fault") {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, indexRows(t, db, "SELECT id,path,text FROM msgs ORDER BY id")) || !reflect.DeepEqual(files, indexRows(t, db, "SELECT path,size,bytes_ingested,last_ord FROM files ORDER BY path")) {
				t.Fatal("rollback lost rows")
			}
			recallSQL(t, db, "DROP TRIGGER refuse")
			r := ingestForTest(t, home, db, 0)
			if prune {
				if r.Pruned != 1 {
					t.Fatal(r)
				}
			} else {
				if r.Ingested != 1 || r.Msgs != 1 {
					t.Fatal(r)
				}
				if n := len(indexRows(t, db, "SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH 'deployed'")); n != 3 {
					t.Fatal(n)
				}
			}
		})
	}
	for _, b := range [][]byte{nil, []byte("no newline"), []byte("한\npartial"), []byte("a\r\n")} {
		if got := completeLineBoundary(b); got != bytes.LastIndexByte(b, '\n')+1 {
			t.Fatal(got)
		}
	}
	db, path := indexTestDB(t)
	home := t.TempDir()
	writeRolloutTestFile(t, home, "sessions/2026/01/01/empty.jsonl", ingestMessage(t, "no final newline")[:10])
	if r := ingestForTest(t, home, db, 0); r.Msgs != 0 {
		t.Fatal(r)
	}
	ro, err := openIndexReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if f := ingestFresh(t, home, ro, 0, nil); f.StaleFiles != 0 {
		t.Fatal(f)
	}
	if _, err := readSlice(filepath.Join(home, "missing"), 0, 1); err == nil {
		t.Fatal("missing slice accepted")
	}
}
