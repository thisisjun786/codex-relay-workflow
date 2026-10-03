package contracttest

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// The dump is the text the recorder's Node dumper prints (record.go sqliteDumper): tables ordered by
// type then name, rows in rowid order only for a plain table, an object per row in column order,
// a BLOB as an object keyed by byte index, REAL and integers as JSON.stringify spells them, text with
// only the quote, the backslash and control characters escaped, and a DATETIME column's text kept.
func TestCXCSQLite_dump_is_the_node_dump(t *testing.T) {
	r := cxcTestReplayer(t)
	path := filepath.Join(t.TempDir(), "x.sqlite")
	err := r.SeedSQLite(nil, map[string][]string{path: {
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, ratio REAL, raw BLOB, at DATETIME, n)",
		"INSERT INTO t VALUES (1, 'q\"<b>&' || char(8232) || 'e\\', 1.5, X'000102030405060708090a0b', '2026-01-01T00:00:00Z', NULL)",
		"INSERT INTO t VALUES (2, 'tab' || char(9) || 'ctl' || char(1) || char(10), 100.0, X'', NULL, 'untyped')",
		"INSERT INTO t VALUES (3, 'é漢', 1e21, X'ff', 'not a date', -1e-7)",
		"CREATE TABLE e (x)",
		"CREATE TABLE big (v INTEGER)",
		"INSERT INTO big VALUES (9007199254740993)",
		"CREATE INDEX i_t_name ON t(name)",
		"CREATE VIEW v AS SELECT id FROM t",
		"CREATE TRIGGER t_ai AFTER INSERT ON t BEGIN SELECT 1; END",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.DumpSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := jsValue(math.Copysign(0, -1)); text != "0" { // JSON.stringify prints -0 as 0
		t.Errorf("negative zero = %s, want 0", text)
	}
	want := `{"tables":[` +
		`{"name":"i_t_name","sql":"CREATE INDEX i_t_name ON t(name)","rows":null},` +
		`{"name":"big","sql":"CREATE TABLE big (v INTEGER)","rows":null},` +
		`{"name":"e","sql":"CREATE TABLE e (x)","rows":[]},` +
		`{"name":"t","sql":"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, ratio REAL, raw BLOB, at DATETIME, n)","rows":[` +
		`{"id":1,"name":"q\"<b>&` + "\u2028" + `e\\","ratio":1.5,"raw":{"0":0,"1":1,"2":2,"3":3,"4":4,"5":5,"6":6,"7":7,"8":8,"9":9,"10":10,"11":11},"at":"2026-01-01T00:00:00Z","n":null},` +
		`{"id":2,"name":"tab\tctl\u0001\n","ratio":100,"raw":{},"at":null,"n":"untyped"},` +
		`{"id":3,"name":"é漢","ratio":1e+21,"raw":{"0":255},"at":"not a date","n":-1e-7}]},` +
		`{"name":"t_ai","sql":"CREATE TRIGGER t_ai AFTER INSERT ON t BEGIN SELECT 1; END","rows":null},` +
		`{"name":"v","sql":"CREATE VIEW v AS SELECT id FROM t","rows":null}]}`
	if got != want {
		t.Errorf("dump differs:\n got %s\nwant %s", got, want)
	}
}

// A reader that closes last would delete the -wal and -shm files the tree walk has already listed.
func TestCXCSQLite_dump_leaves_the_wal_sidecars(t *testing.T) {
	r := cxcTestReplayer(t)
	path := filepath.Join(t.TempDir(), "w.sqlite")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE t (a)", "INSERT INTO t VALUES (7)"} {
		if _, err := writer.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + sidecar); err != nil {
			t.Fatalf("the writer left no %s: %v", sidecar, err)
		}
	}
	// Dump a copy no writer holds: only a read-only reader leaves the sidecars where they were.
	copied := filepath.Join(t.TempDir(), "w.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err == nil {
			err = os.WriteFile(copied+suffix, data, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.DumpSQLite(copied)
	if err != nil || !strings.Contains(got, `"rows":[{"a":7}]`) {
		t.Fatalf("dump = %q, %v: want the committed row", got, err)
	}
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(copied + sidecar); err != nil {
			t.Errorf("the dump removed %s: %v", sidecar, err)
		}
	}
}

// Seeding makes the file with the mode umask 022 gives, whatever the test process's umask is.
func TestCXCSQLite_seed_mode_ignores_the_process_umask(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "s.sqlite")
	if err := cxcTestReplayer(t).SeedSQLite(nil, map[string][]string{path: {"CREATE TABLE t (a)"}}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, %v, want 0644", info, err)
	}
	if err := cxcTestReplayer(t).SeedSQLite(nil, map[string][]string{path: {"INSERT INTO nowhere VALUES (1)"}}); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("a failing statement: error = %v, want one naming the statement", err)
	}
}

// A claim on a fixture with a given database runs through the whole replay: the database is seeded,
// the build runs, and the tree entry (form, mode, rows) is compared with the recording.
func TestCXCReplay_sqlite_claims(t *testing.T) {
	seeded := givenDoc(map[string]any{"sqlite": map[string][]string{"codex/x.sqlite": {"CREATE TABLE t (a)", "INSERT INTO t VALUES (1)"}}})
	entry := func(rows string) string {
		return `{"codex/x.sqlite":{"type":"file","mode":"0644","form":"sqlite","json":{"tables":[{"name":"t","sql":"CREATE TABLE t (a)","rows":` + rows + `}]}}}`
	}
	runCXCRows(t, []cxcRow{
		{name: "an identical claim on a seeded database passes", given: seeded, observe: `["codex"]`, tree: entry(`[{"a":1}]`)},
		{name: "a changed row is a difference at the entry's content", given: seeded, observe: `["codex"]`, tree: entry(`[{"a":2}]`), err: "tree/codex/x.sqlite/content"},
		{name: "a database the oracle left out of the expectation is a difference", given: seeded, observe: `["codex"]`, err: "tree/codex/x.sqlite/form"},
	})
}

// Replaying a seeded given through a whole case (no step touches the databases) must give back the
// entry the oracle recorded, for every database the oracle left as seeded: that is what a port
// claiming the fixture is compared with. The two fixtures whose recording is not the seeded database
// are listed with the reason; their entries are still checked to be dumped as sqlite.
func TestCXCSQLite_seeded_givens_reproduce_the_recordings(t *testing.T) {
	root, _ := Root()
	_, fixtures, err := loadCXCFixtures(root)
	if err != nil {
		t.Fatal(err)
	}
	r := cxcTestReplayer(t)
	notAsSeeded := map[string]string{ // fixture id -> why the recorded database is not the seeded one
		"cli__memory__requeue_apply_on_seeded_db": "the step requeues jobs, so the oracle updated the seeded rows",
		"cli__chat__search_refresh_builds_index":  "aliases are numbered by first appearance across the run's output, which a stepless replay lacks (<UUID_2> before <UUID_1>)",
	}
	compared := 0
	for id, fix := range fixtures {
		if len(fix.Given.SQLite) == 0 {
			continue
		}
		t.Run(id, func(t *testing.T) {
			scenario, err := r.scenario(id, fix, cxcClaim{})
			if err != nil {
				t.Fatal(err)
			}
			scenario.Steps, scenario.Observe = nil, nil
			for path := range scenario.Given.SQLite {
				scenario.Observe = append(scenario.Observe, strings.SplitN(path, "/", 2)[0])
			}
			got, err := cxccorpus.RunScenario(r, cxccorpus.RunOptions{Scratch: t.TempDir(), HomeVar: "CRW_HOME", Rules: r.rules}, scenario)
			if err != nil {
				t.Fatal(err)
			}
			want, err := mapStrings(fix.Expect, r.rename)
			if err == nil { // as check does, so both sides are compact documents
				got, err = mapStrings(got, func(s string) string { return s })
			}
			if err != nil {
				t.Fatal(err)
			}
			wantFlat, gotFlat := flatten(want), flatten(got)
			for path := range scenario.Given.SQLite {
				prefix := "tree/" + path + "/"
				for key, text := range wantFlat {
					if !strings.HasPrefix(key, prefix) {
						continue
					}
					if gotFlat[key] != text && notAsSeeded[id] == "" {
						t.Errorf("%s: seeded and dumped %.200q, the oracle recorded %.200q", key, gotFlat[key], text)
					}
				}
				if gotFlat[prefix+"form"] != "sqlite" {
					t.Errorf("%s is not dumped as sqlite: form %q", path, gotFlat[prefix+"form"])
				}
				compared++
			}
		})
	}
	if compared != 41 {
		t.Errorf("compared %d seeded databases, want the 41 of the 37 fixtures with a given database", compared)
	}
}

// A recording is rebuilt into a database (tables, base rows, the shadow tables an FTS5 table owns,
// then indexes, views and triggers); the dump of it, through the corpus normaliser as the engine does,
// must be the recording again. This covers every one of the 43 recorded sqlite entries, the recall
// index (FTS5 shadow tables, BLOBs, triggers) and the WAL one included, which no given seeds. The
// recordings are already normalised (an FTS shadow table's integer key holds "<MS>"), so such a key
// is rebuilt as a distinct number the normaliser turns back into "<MS>": the test checks the dump and
// its normalisation, not the oracle's ids.
func TestCXCSQLite_recordings_round_trip(t *testing.T) {
	root, _ := Root()
	ids, fixtures, err := loadCXCFixtures(root)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := cxccorpus.LoadRules(root)
	if err != nil {
		t.Fatal(err)
	}
	r, entries := cxcTestReplayer(t), 0
	for _, id := range ids {
		for _, path := range slices.Sorted(maps.Keys(fixtures[id].Expect.Tree)) {
			entry := fixtures[id].Expect.Tree[path]
			if entry.Form != "sqlite" {
				continue
			}
			entries++
			t.Run(id+"/"+path, func(t *testing.T) {
				db := filepath.Join(t.TempDir(), "rebuilt.sqlite")
				if err := rebuildSQLite(db, entry.JSON, strings.HasSuffix(path, "idx.sqlite")); err != nil {
					t.Fatal(err)
				}
				dump, err := r.DumpSQLite(db)
				if err != nil {
					t.Fatal(err)
				}
				var want bytes.Buffer
				if err := json.Compact(&want, entry.JSON); err != nil {
					t.Fatal(err)
				}
				if got := rules.NewSession(nil).Text(dump); got != want.String() {
					t.Errorf("round trip differs (%d bytes, want %d)", len(got), want.Len())
				}
			})
		}
	}
	if entries != 43 {
		t.Errorf("%d sqlite entries recorded, want 43", entries)
	}
}

func rebuildSQLite(path string, recorded json.RawMessage, wal bool) error {
	var dump struct {
		Tables []struct {
			Name, SQL string
			Rows      []map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(recorded, &dump); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	run := func(statement string, args ...any) error {
		if _, err := db.Exec(statement, args...); err != nil {
			return fmt.Errorf("%w in %.80q", err, statement)
		}
		return nil
	}
	if wal {
		if err := run("PRAGMA journal_mode=WAL"); err != nil {
			return err
		}
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	starts := func(sqlText, prefix string) bool { return strings.HasPrefix(strings.ToUpper(sqlText), prefix) }
	for _, o := range dump.Tables { // an FTS5 table creates its shadow tables itself
		if starts(o.SQL, "CREATE VIRTUAL TABLE") {
			if err := run(o.SQL); err != nil {
				return err
			}
		}
	}
	shadow := map[string]bool{}
	names, err := db.Query("SELECT name FROM pragma_table_list WHERE type = 'shadow'")
	if err != nil {
		return err
	}
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			return err
		}
		shadow[name] = true
	}
	if err := names.Close(); err != nil {
		return err
	}
	nextMS := int64(1600000000000) // the epoch milliseconds the normaliser turns into "<MS>"
	insert := func(name string, rows []map[string]json.RawMessage) error {
		for _, row := range rows {
			var cols, marks []string
			var args []any
			for _, col := range slices.Sorted(maps.Keys(row)) {
				var v any
				switch raw := row[col]; {
				case string(raw) == "null":
				case raw[0] == '{': // a Uint8Array: an object keyed by byte index
					var bytesByIndex map[string]byte
					if err := json.Unmarshal(raw, &bytesByIndex); err != nil {
						return err
					}
					blob := make([]byte, len(bytesByIndex))
					for i := range blob {
						blob[i] = bytesByIndex[strconv.Itoa(i)]
					}
					v = blob
				case raw[0] == '"':
					var s string
					if err := json.Unmarshal(raw, &s); err != nil {
						return err
					}
					if s == "<MS>" && col == "id" {
						v, nextMS = nextMS, nextMS+1
					} else {
						v = s
					}
				default:
					n, err := strconv.ParseInt(string(raw), 10, 64)
					if err != nil {
						return err
					}
					v = n
				}
				cols, marks, args = append(cols, quote(col)), append(marks, "?"), append(args, v)
			}
			if err := run("INSERT INTO "+quote(name)+" ("+strings.Join(cols, ",")+") VALUES ("+strings.Join(marks, ",")+")", args...); err != nil {
				return err
			}
		}
		return nil
	}
	for _, o := range dump.Tables {
		if starts(o.SQL, "CREATE TABLE") && !shadow[o.Name] {
			if err := errors.Join(run(o.SQL), insert(o.Name, o.Rows)); err != nil {
				return err
			}
		}
	}
	for _, o := range dump.Tables {
		if shadow[o.Name] {
			if err := errors.Join(run("DELETE FROM "+quote(o.Name)), insert(o.Name, o.Rows)); err != nil {
				return err
			}
		}
	}
	for _, kind := range []string{"CREATE INDEX", "CREATE UNIQUE INDEX", "CREATE VIEW", "CREATE TRIGGER"} {
		for _, o := range dump.Tables {
			if starts(o.SQL, kind) {
				if err := run(o.SQL); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
