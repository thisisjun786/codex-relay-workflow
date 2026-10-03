package recall

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func recallDB(t *testing.T, path string) *RwDb {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	db, err := openDbReadWrite(path)
	if err != nil {
		t.Fatal("read-write open:", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func recallSQL(t *testing.T, d *RwDb, q string) {
	t.Helper()
	if err := d.Exec(q); err != nil {
		t.Fatal(q, err)
	}
}
func recallStmt(t *testing.T, d *RwDb, q string) *Stmt {
	t.Helper()
	s, err := d.Prepare(q)
	if err != nil {
		t.Fatal(q, err)
	}
	return s
}
func recallRow(t *testing.T, s *Stmt, params ...any) map[string]any {
	t.Helper()
	r, err := s.Get(params...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecallSQLiteOpens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "with space?#.sqlite")
	if d, err := openDbReadOnly(p); err == nil {
		_ = d.Close()
		t.Fatal("missing read-only open succeeded")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("read-only created a file", err)
	}
	d := recallDB(t, p)
	recallSQL(t, d, "CREATE TABLE t(x); INSERT INTO t VALUES ('kept')")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := openReadOnlyDb(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"INSERT INTO t VALUES ('bad')", "ALTER TABLE t ADD COLUMN y", "DROP TABLE t"} {
		if err := ro.Exec(q); err == nil || err.Error() != "attempt to write a readonly database" {
			t.Fatal(q, err)
		}
	}
	if recallRow(t, recallStmt(t, ro, "SELECT x FROM t"))["x"] != "kept" {
		t.Fatal("reader changed data")
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only changed database bytes", err)
	}
	for _, path := range []string{"", ":memory:"} {
		mem := recallDB(t, path)
		recallSQL(t, mem, "CREATE TABLE m(x)")
		r, err := openDbReadOnly(path)
		if err != nil {
			t.Fatal(path, err)
		}
		if err := r.Exec("CREATE TABLE m(x)"); err == nil {
			t.Fatal("read-only temporary DB wrote")
		}
		_ = r.Close()
	}
	// The oracle also lets URI mode=memory override readOnly; keep that quirk.
	uri, err := openDbReadOnly("file:recall-memory?mode=memory")
	if err != nil {
		t.Fatal(err)
	}
	if err := uri.Exec("CREATE TABLE uri_memory(x)"); err != nil {
		t.Fatal(err)
	}
	_ = uri.Close()
	if _, err := openDbReadWrite(":memory:\x00ignored"); err == nil || !strings.Contains(err.Error(), "without null bytes") {
		t.Fatal("NUL path accepted", err)
	}
}

func TestRecallSQLiteStatementsAndDefaults(t *testing.T) {
	d := recallDB(t, ":memory:")
	if recallRow(t, recallStmt(t, d, "PRAGMA foreign_keys"))["foreign_keys"] != float64(1) || recallRow(t, recallStmt(t, d, "PRAGMA busy_timeout"))["timeout"] != float64(0) {
		t.Fatal("SQLite defaults differ")
	}
	if _, err := d.Prepare(`SELECT "missing_column"`); err == nil {
		t.Fatal("DQS remained enabled")
	}
	if _, err := d.Prepare("SELECT missing FROM missing"); err == nil {
		t.Fatal("prepare was lazy")
	}
	recallSQL(t, d, "CREATE TABLE t(x UNIQUE); CREATE TABLE audit(x); CREATE TRIGGER log AFTER INSERT ON t BEGIN INSERT INTO audit VALUES(new.x); END;")
	s := recallStmt(t, d, "INSERT INTO t VALUES (?)")
	result, err := s.Run("first")
	if err != nil || result != (RunResult{1, 1}) {
		t.Fatal(result, err)
	}
	if _, err := s.Run("first"); err == nil || err.Error() != "UNIQUE constraint failed: t.x" {
		t.Fatal("UNIQUE error changed", err)
	}
	if _, err := s.Run("second"); err != nil {
		t.Fatal("failed statement not reusable", err)
	}
	recallSQL(t, d, "BEGIN")
	if _, err := s.Run("rollback"); err != nil {
		t.Fatal(err)
	}
	recallSQL(t, d, "ROLLBACK")
	rows, err := recallStmt(t, d, "SELECT * FROM audit").All()
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if recallRow(t, recallStmt(t, d, "SELECT 'a;b' AS x; DROP TABLE t"))["x"] != "a;b" {
		t.Fatal("prepare did not keep only first statement")
	}
	if r := recallRow(t, recallStmt(t, d, "SELECT 1 AS x, 2 AS x")); r["x"] != float64(2) {
		t.Fatal(r)
	}
	if r := recallRow(t, recallStmt(t, d, "SELECT 1 WHERE 0")); r != nil {
		t.Fatal("get no row is not nil", r)
	}
	first := recallStmt(t, d, "SELECT 1 AS x UNION ALL SELECT 9007199254740992")
	if recallRow(t, first)["x"] != float64(1) {
		t.Fatal("Get read beyond first row")
	}
	if _, err := first.All(); err == nil || err.Error() != "Value is too large to be represented as a JavaScript number: 9007199254740992" {
		t.Fatal(err)
	}
	if _, err := recallStmt(t, d, "SELECT 9007199254740992").Run(); err != nil {
		t.Fatal("Run decoded columns", err)
	}
	empty := recallStmt(t, d, "-- only a comment")
	if _, err := empty.Get(); err == nil || err.Error() != "statement has been finalized" {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(); err == nil || err.Error() != "statement has been finalized" {
		t.Fatal(err)
	}
}

func TestRecallSQLiteBindingsAndTypes(t *testing.T) {
	d := recallDB(t, ":memory:")
	s := recallStmt(t, d, "SELECT ? AS x, typeof(?) AS kind")
	for _, c := range []struct {
		value any
		want  any
		kind  string
	}{
		{nil, nil, "null"}, {"a\x00b", "a\x00b", "text"}, {[]byte{}, []byte{}, "blob"}, {[]byte{0, 255}, []byte{0, 255}, "blob"},
		{int(5), float64(5), "real"}, {float64(1.5), float64(1.5), "real"}, {big.NewInt(5), float64(5), "integer"},
	} {
		r := recallRow(t, s, c.value, c.value)
		if !reflect.DeepEqual(r["x"], c.want) || r["kind"] != c.kind {
			t.Errorf("%T: %#v; want %#v/%s", c.value, r, c.want, c.kind)
		}
	}
	if _, err := s.Get(true); err == nil || err.Error() != "Provided value cannot be bound to SQLite parameter 1." {
		t.Fatal(err)
	}
	if _, err := recallStmt(t, d, "SELECT 1").Get(2); err == nil || err.Error() != "column index out of range" {
		t.Fatal(err)
	}
	single := recallStmt(t, d, "SELECT ? AS x")
	_ = recallRow(t, single, "bound")
	if recallRow(t, single)["x"] != nil {
		t.Fatal("old binding retained")
	}
	if got := recallRow(t, recallStmt(t, d, "SELECT $x AS x, ? AS y"), map[string]any{"x": 2}, 3); !reflect.DeepEqual(got, map[string]any{"x": float64(2), "y": float64(3)}) {
		t.Fatal(got)
	}
	if got := recallRow(t, recallStmt(t, d, "SELECT ?1 AS x, ?2 AS y"), 2, 3); got["y"] != float64(3) {
		t.Fatal(got)
	}
	if _, err := recallStmt(t, d, "SELECT :x, @x").Get(map[string]any{"x": 2}); err == nil || !strings.Contains(err.Error(), "conflicting names") {
		t.Fatal(err)
	}
	recallSQL(t, d, "CREATE TABLE dates(x DATE); INSERT INTO dates VALUES ('2020-01-01')")
	if recallRow(t, recallStmt(t, d, "SELECT x FROM dates"))["x"] != "2020-01-01" {
		t.Fatal("DATE was converted")
	}
	r := recallRow(t, recallStmt(t, d, "SELECT CAST(x'F09080' AS TEXT) AS x, 9007199254740991 AS n"))
	if r["x"] != "\ufffd" || r["n"] != float64(9007199254740991) {
		t.Fatal(r)
	}
}

func TestRecallSQLiteMinimumIntegerOracleOverflow(t *testing.T) {
	d := recallDB(t, ":memory:")
	s := recallStmt(t, d, "SELECT ? AS x")
	if got := recallRow(t, s, big.NewInt(-1<<63))["x"]; got != float64(-1<<63) {
		t.Fatal(got)
	}
	if _, err := s.Get(big.NewInt(-1<<63 + 1)); err == nil {
		t.Fatal("neighboring unsafe integer must still fail")
	}
}

func TestRecallSQLiteNamedOrderAndCachedAmbiguity(t *testing.T) {
	d := recallDB(t, ":memory:")
	s := recallStmt(t, d, "SELECT $x AS x")
	for _, c := range []struct {
		args NamedParams
		want float64
	}{
		{NamedParams{{"x", 1}, {"$x", 2}}, 2}, {NamedParams{{"$x", 2}, {"x", 1}}, 1},
	} {
		if got := recallRow(t, s, c.args)["x"]; got != c.want {
			t.Fatal(got, c.want)
		}
	}
	if _, err := s.Get(map[string]any{"x": 1, "$x": 2}); err == nil {
		t.Fatal("unordered aliases silently chose a write value")
	}
	for _, q := range []string{"SELECT $$x AS b,$x AS a", "SELECT $x AS a,$$x AS b"} {
		for _, args := range []any{NamedParams{{"$x", 7}}, map[string]any{"$x": 7}} {
			r := recallRow(t, recallStmt(t, d, q), args)
			if r["a"] != float64(7) || r["b"] != nil {
				t.Fatal("exact name lost to bare alias", r)
			}
		}
	}
	for _, mode := range []string{"get", "all", "run"} {
		a := recallStmt(t, d, "SELECT :x AS x,@x AS y")
		call := func() error {
			switch mode {
			case "get":
				_, err := a.Get(NamedParams{{"x", 2}})
				return err
			case "all":
				_, err := a.All(NamedParams{{"x", 2}})
				return err
			default:
				_, err := a.Run(NamedParams{{"x", 2}})
				return err
			}
		}
		if err := call(); err == nil {
			t.Fatal("first ambiguity did not throw")
		}
		if err := call(); err != nil {
			t.Fatal("partial alias cache was lost", err)
		}
		if row := recallRow(t, a, NamedParams{{"x", 3}}); row["x"] != float64(3) || row["y"] != nil {
			t.Fatal(row)
		}
	}
	if _, err := recallStmt(t, d, "SELECT 1").Get(true); err == nil || err.Error() != "Provided value cannot be bound to SQLite parameter 1." {
		t.Fatal(err)
	}
}
