package recall

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func recallText(s string) *string   { return &s }
func recallTime(n float64) *float64 { return &n }

func TestRecallThreadMeta(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state_2.sqlite")
	d := recallDB(t, p)
	recallSQL(t, d, `CREATE TABLE threads(id,title,cwd,git_branch,git_origin_url,updated_at_ms);
INSERT INTO threads VALUES ('b','B','/b','', '  origin  ',1.5),('a','A','/a',NULL,' ',2),('b','B2',4,2,NULL,NULL),(7,'skip','/skip',NULL,NULL,1);`)
	_ = d.Close()
	r := loadThreadMeta(p)
	want := map[string]ThreadMeta{"b": {Title: "B2"}, "a": {Title: "A", Cwd: "/a", UpdatedAtMs: recallTime(2)}}
	if r.Warning != "" || !reflect.DeepEqual(r.IDs, []string{"b", "a"}) || !reflect.DeepEqual(r.ByID, want) {
		t.Fatal(r)
	}
	d = recallDB(t, p)
	recallSQL(t, d, `DELETE FROM threads; INSERT INTO threads VALUES ('b','B','/b','', '  origin  ',1.5)`)
	_ = d.Close()
	r = loadThreadMeta(p)
	if !reflect.DeepEqual(r.ByID["b"], ThreadMeta{"B", "/b", recallText(""), recallText("  origin  "), recallTime(1.5)}) {
		t.Fatal(r)
	}
}

// An unsafe integer in git_origin_url is not a missing column: the reader reports it instead of
// silently dropping the origin (port: fixed, CRW-1123, :393). TestSweepThreadMetaRetriesOnlyAMissingLegacyColumn.
func TestRecallThreadMetaLegacyFallback(t *testing.T) {
	for _, c := range []struct {
		schema, insert string
		want           ThreadMeta
	}{
		{`CREATE TABLE threads(id,title,cwd,git_branch,updated_at_ms)`, `INSERT INTO threads VALUES ('x','Legacy','/old',NULL,5)`, ThreadMeta{Title: "Legacy", Cwd: "/old", UpdatedAtMs: recallTime(5)}},
	} {
		p := filepath.Join(t.TempDir(), "state.sqlite")
		d := recallDB(t, p)
		recallSQL(t, d, c.schema+";"+c.insert)
		_ = d.Close()
		r := loadThreadMeta(p)
		if r.Warning != "" || !reflect.DeepEqual(r.ByID["x"], c.want) {
			t.Fatal(r)
		}
	}
}

func TestRecallThreadMetaWarningsAndCase(t *testing.T) {
	for _, c := range []struct{ schema, want string }{
		{`CREATE TABLE other(x)`, "state db unreadable (no such table: threads)"},
		{`CREATE TABLE threads(id,title,cwd,git_branch,git_origin_url,updated_at_ms);INSERT INTO threads VALUES ('x','unsafe','/p',NULL,NULL,9007199254740992)`, "state db unreadable (Value is too large to be represented as a JavaScript number: 9007199254740992)"},
		{`CREATE TABLE threads(id,title,cwd,git_branch,git_origin_url,updated_at_ms);INSERT INTO threads VALUES ('x','unsafe origin','/p',NULL,9007199254740992,1)`, "state db unreadable (Value is too large to be represented as a JavaScript number: 9007199254740992)"},
	} {
		p := filepath.Join(t.TempDir(), "state.sqlite")
		d := recallDB(t, p)
		recallSQL(t, d, c.schema)
		_ = d.Close()
		r := loadThreadMeta(p)
		if r.ByID == nil || len(r.ByID) != 0 || r.Warning != c.want {
			t.Fatal(r)
		}
	}
	for _, c := range []struct{ path, want string }{
		{"", "state db not found (metadata enrichment off)"},
		{filepath.Join(t.TempDir(), "missing.sqlite"), "state db unreadable (unable to open database file)"},
	} {
		r := loadThreadMeta(c.path)
		if r.ByID == nil || len(r.ByID) != 0 || r.Warning != c.want {
			t.Fatal(r)
		}
	}
	p := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(p, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := loadThreadMeta(p)
	if r.Warning != "state db unreadable (file is not a database)" || len(r.ByID) != 0 {
		t.Fatal(r)
	}
}
