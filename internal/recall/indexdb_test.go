package recall

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func indexTestDB(t *testing.T) (*RwDb, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CRW_HOME", root)
	path, err := indexPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}
func indexOracle(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/indexdb/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err = json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	return want
}
func indexRows(t *testing.T, db *RwDb, q string) []map[string]any {
	t.Helper()
	rows, err := recallStmt(t, db, q).All()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestIndexPath(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CRW_HOME": "relative crw", "HOME": "fallback"}, filepath.Join("relative crw", "recall", "index.sqlite")},
		{map[string]string{"CRW_HOME": "  root  ", "HOME": "fallback"}, filepath.Join("  root  ", "recall", "index.sqlite")},
		{map[string]string{"CRW_HOME": "\uFEFF \t", "HOME": "fallback"}, filepath.Join("fallback", ".crw", "recall", "index.sqlite")},
		{map[string]string{"HOME": ""}, filepath.Join(".crw", "recall", "index.sqlite")},
	} {
		got, err := indexPath(func(k string) (string, bool) { v, ok := c.env[k]; return v, ok })
		if err != nil || got != c.want {
			t.Errorf("%v: %q %v, want %q", c.env, got, err, c.want)
		}
	}
}
func TestIndexOracle(t *testing.T) {
	db, path := indexTestDB(t)
	want := indexOracle(t)
	got := map[string]any{"version": IndexSchemaVersion}
	got["schema"] = indexRows(t, db, "SELECT type,name,sql FROM sqlite_master WHERE name IN ('meta','files','msgs','idx_msgs_path','idx_msgs_ts','idx_files_repo_key','msgs_fts','msgs_tri','msgs_ai','msgs_ad','recall_hit_counts') ORDER BY name")
	fresh, err := indexStatus(db, "fixture-index")
	if err != nil {
		t.Fatal(err)
	}
	got["fresh"] = fresh
	got["pragmas"] = []any{recallRow(t, recallStmt(t, db, "PRAGMA journal_mode"))["journal_mode"], recallRow(t, recallStmt(t, db, "PRAGMA busy_timeout"))["timeout"]}
	if runtime.GOOS != "windows" {
		modes := []int{}
		for _, p := range []string{filepath.Dir(path), path, path + "-wal", path + "-shm"} {
			s, e := os.Stat(p)
			if e != nil {
				t.Fatal(e)
			}
			modes = append(modes, int(s.Mode().Perm()))
		}
		got["modes"] = modes
	} else {
		delete(want, "modes")
	}
	got["refs"] = []string{hitCountRef("abc", "x.jsonl"), hitCountRef("", "x.jsonl"), hitCountRef("", "x.jsonl")}
	got["empty"] = []any{}
	if len(readHitCounts(db, nil)) != 0 {
		t.Fatal("empty refs read history")
	}
	at := "2026-01-02T03:04:05.000Z"
	if err = bumpHitCounts(db, []string{"thread:a", "thread:b", "thread:a"}, at); err != nil {
		t.Fatal(err)
	}
	got["counts"] = readHitCounts(db, []string{"thread:a", "thread:b", "missing"})
	got["stamp"] = recallRow(t, recallStmt(t, db, "SELECT last_hit_at FROM recall_hit_counts WHERE ref='thread:a'"))["last_hit_at"]
	recallSQL(t, db, "INSERT INTO files(path,mtime_ms,size,source,date) VALUES('seed',1,2,'main','2026-01-02'); INSERT INTO msgs(id,path,ord,ts,role,match_field,synthetic,text) VALUES(1,'seed',0,'ts','user','content',0,'quokka 한글 트라이그램'); INSERT INTO meta VALUES('last_ingest_at','stamp');")
	status, err := indexStatus(db, "fixture-index")
	if err != nil {
		t.Fatal(err)
	}
	got["status"] = status
	fts := func() []int {
		return []int{len(indexRows(t, db, "SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH 'quokka'")), len(indexRows(t, db, `SELECT rowid FROM msgs_tri WHERE msgs_tri MATCH '"라이그"'`))}
	}
	got["inserted"] = fts()
	recallSQL(t, db, "DELETE FROM msgs WHERE id=1")
	got["deleted"] = fts()
	for _, table := range []string{"msgs_fts", "msgs_tri"} {
		recallSQL(t, db, "INSERT INTO "+table+"("+table+",rank) VALUES('integrity-check',1)")
	}
	recallSQL(t, db, "DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key")
	got["legacy"] = filesHasColumn(db, "repo_key")
	ensureRepoKeyColumn(db)
	got["migrated"] = filesHasColumn(db, "repo_key")
	got["rowSurvives"] = indexRows(t, db, "SELECT path,repo_key FROM files")
	recallSQL(t, db, "DROP TABLE recall_hit_counts")
	got["absent"] = readHitCounts(db, []string{"thread:a"})
	err = bumpHitCounts(db, []string{"thread:a"}, at)
	if err == nil {
		t.Fatal("missing writer table succeeded")
	}
	got["missingWrite"] = err.Error()
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got["recreated"] = readHitCounts(db, []string{"thread:a"})
	recallSQL(t, db, "INSERT INTO msgs(id,path,ord,ts,role,match_field,synthetic,text) VALUES(1,'seed',0,'ts','user','content',0,'quokka'); UPDATE msgs SET text='gemsbok' WHERE id=1;")
	got["updateDesync"] = []int{len(indexRows(t, db, "SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH 'quokka'")), len(indexRows(t, db, "SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH 'gemsbok'"))}
	ro, err := openIndexReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	err = ro.Exec("INSERT INTO meta VALUES('bad','bad')")
	if err == nil {
		t.Fatal("read-only wrote")
	}
	got["readonlyWrite"] = err.Error()
	_ = ro.Close()
	root := filepath.Dir(filepath.Dir(path))
	_, err = openIndexReadOnly(filepath.Join(root, "missing", "index.sqlite"))
	if err == nil {
		t.Fatal("missing read-only succeeded")
	}
	got["missingOpen"] = strings.ReplaceAll(err.Error(), root, "$R")
	for _, k := range []string{"inputs", "blobInputs", "coercions", "defects"} {
		delete(want, k)
	}
	if actual := canon(t, got); !reflect.DeepEqual(actual, want) {
		a, _ := json.MarshalIndent(actual, "", "  ")
		b, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("got %s\noracle %s", a, b)
	}
}
func TestIndexReadOnlyDoesNotMigrate(t *testing.T) {
	db, path := indexTestDB(t)
	recallSQL(t, db, "DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key; DROP TABLE recall_hit_counts")
	_ = db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := openIndexReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if filesHasColumn(ro, "repo_key") {
		t.Fatal("reader migrated")
	}
	ensureRepoKeyColumn(ro)
	if filesHasColumn(ro, "repo_key") {
		t.Fatal("read-only helper migrated")
	}
	if len(readHitCounts(ro, []string{"a"})) != 0 {
		t.Fatal("absent table did not degrade")
	}
	_ = ro.Close()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reader modified database", err)
	}
	db, err = openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ensureRepoKeyColumn(db)
	if !filesHasColumn(db, "repo_key") {
		t.Fatal("writer did not migrate")
	}
	if r := recallRow(t, recallStmt(t, db, "SELECT name FROM sqlite_master WHERE name='idx_files_repo_key'")); r == nil {
		t.Fatal("missing repo index")
	}
}
func TestIndexVersionResetPreservesHistory(t *testing.T) {
	db, path := indexTestDB(t)
	if err := bumpHitCounts(db, []string{"thread:kept"}, "stamp"); err != nil {
		t.Fatal(err)
	}
	recallSQL(t, db, "INSERT INTO files(path,mtime_ms,size,source,date) VALUES('old',1,1,'main','date'); UPDATE meta SET value='1' WHERE key='schema_version'")
	_ = db.Close()
	db, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := readHitCounts(db, []string{"thread:kept"}); got["thread:kept"] != 1 {
		t.Fatal("reset lost injection state", got)
	}
	status, err := indexStatus(db, path)
	if err != nil || status.Files != 0 || status.Msgs != 0 {
		t.Fatal("derived cache not rebuilt", status, err)
	}
	if v := recallRow(t, recallStmt(t, db, "SELECT value FROM meta WHERE key='schema_version'"))["value"]; v != IndexSchemaVersion {
		t.Fatal(v)
	}
	defect := indexOracle(t)["defects"].([]any)[0].(map[string]any)
	if len(defect["oracle"].(map[string]any)) != 0 || defect["classification"] != "intentionally-changed" {
		t.Fatal("missing recorded reset exception")
	}
}
func TestIndexFailedResetRollsBack(t *testing.T) {
	db, path := indexTestDB(t)
	recallSQL(t, db, "INSERT INTO files(path,mtime_ms,size,source,date) VALUES('kept',1,1,'main','date'); INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES('kept',0,'ts','user','content',0,'quokka'); DROP TABLE meta; CREATE VIEW meta AS SELECT 'schema_version' AS key, '1' AS value;")
	_ = db.Close()
	failed, err := openIndex(path)
	if err == nil {
		if failed != nil {
			_ = failed.Close()
		}
		t.Fatal("malformed version table accepted")
	}
	defect := indexOracle(t)["defects"].([]any)[1].(map[string]any)
	want := defect["port"].(map[string]any)
	if err.Error() != want["error"] {
		t.Fatalf("error %v, oracle %v", err, want["error"])
	}
	db, err = openDbReadWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names := []string{}
	for _, r := range indexRows(t, db, "SELECT name FROM sqlite_master WHERE name IN ('files','msgs','meta','recall_hit_counts') ORDER BY name") {
		names = append(names, r["name"].(string))
	}
	if !reflect.DeepEqual(canon(t, names), want["tables"]) {
		t.Fatal("failed reset lost tables", names)
	}
	if r := recallRow(t, recallStmt(t, db, "SELECT path FROM files")); r["path"] != "kept" {
		t.Fatal("failed reset lost file", r)
	}
	if len(indexRows(t, db, "SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH 'quokka'")) != 1 {
		t.Fatal("failed reset lost FTS content")
	}
	for _, table := range []string{"msgs_fts", "msgs_tri"} {
		recallSQL(t, db, "INSERT INTO "+table+"("+table+",rank) VALUES('integrity-check',1)")
	}
}
func TestIndexReadFailures(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	path := filepath.Join(root, "absent", "index.sqlite")
	if _, err := openIndexReadOnly(path); err == nil {
		t.Fatal("opened absent index")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("reader created directory", err)
	}
	corrupt := filepath.Join(root, "corrupt.sqlite")
	if err := os.WriteFile(corrupt, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if db, err := openIndex(corrupt); err == nil {
		_ = db.Close()
		t.Fatal("writer accepted corrupt database")
	}
	db := recallDB(t, filepath.Join(root, "closed.sqlite"))
	_ = db.Close()
	if filesHasColumn(db, "anything") {
		t.Fatal("closed column probe succeeded")
	}
	ensureRepoKeyColumn(db)
	if _, err := indexStatus(db, path); err == nil {
		t.Fatal("closed status succeeded")
	}
}

func TestIndexMigrationPreservesRows(t *testing.T) {
	db, path := indexTestDB(t)
	recallSQL(t, db, "INSERT INTO files(path,mtime_ms,size,source,date) VALUES('kept',1,1,'main','date'); INSERT INTO msgs(id,path,ord,ts,role,match_field,synthetic,text) VALUES(17,'kept',0,'ts','user','content',0,'quokka'); DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key;")
	if err := bumpHitCounts(db, []string{"thread:kept"}, "stamp"); err != nil {
		t.Fatal(err)
	}
	before := indexRows(t, db, "SELECT id,path,text FROM msgs")
	_ = db.Close()
	db, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !filesHasColumn(db, "repo_key") {
		t.Fatal("column not migrated")
	}
	if got := indexRows(t, db, "SELECT id,path,text FROM msgs"); !reflect.DeepEqual(got, before) {
		t.Fatal("migration moved messages", got, before)
	}
	if row := recallRow(t, recallStmt(t, db, "SELECT repo_key FROM files WHERE path='kept'")); row["repo_key"] != nil {
		t.Fatal("migration invented repo key", row)
	}
	if got := readHitCounts(db, []string{"thread:kept"}); got["thread:kept"] != 1 {
		t.Fatal("migration erased history", got)
	}
}
