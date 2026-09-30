package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	_ "modernc.org/sqlite"
)

type supervisorCapture struct {
	Captures []any                       `json:"captures"`
	Problems []string                    `json:"problems"`
	Tables   map[string][]map[string]any `json:"tables"`
}

func TestMain(m *testing.M) {
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := errors.Join(cleanup(), testsupport.RemoveCRW()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// supervisorFixture restores the tree one Python supervisor test left (the former
// testdata/capture.py): the store's snapshots it names, setup.sqlite3, event.sqlite3 or another,
// beside tree/, and, for a test whose replay reads it, Python's final store in tree/state, with the
// mirror its stamp implies.
func supervisorFixture(t *testing.T, id string) string {
	t.Helper()
	root, err := os.MkdirTemp("", "crw-supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	treeFixture(t, id, root)
	final := filepath.Join(root, "tree", "state", "relay.sqlite3")
	if _, err := os.Stat(final); err == nil {
		testsupport.Rehome(t, final)
	}
	return root
}

// removeStoreFiles deletes a store Python left and the files beside it that belong to it.
func removeStoreFiles(path string) error {
	for _, name := range []string{path, path + "-wal", path + "-shm", filepath.Join(filepath.Dir(path), "takeover.json")} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func readSupervisorCapture(t *testing.T, root string) supervisorCapture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result supervisorCapture
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Problems) > 0 {
		t.Fatalf("Python test failed: %v", result.Problems)
	}
	return result
}

// checkSupervisorValues compares the values a replay produced, as JSON decodes them, with the
// golden.
func checkSupervisorValues(t *testing.T, got []any, opts ...golden.Option) {
	t.Helper()
	golden.CheckJSON(t, goldenKey(t, "captures"), asJSON(t, got), opts...)
}

// checkSupervisorTables compares every populated row of every table but schema_meta and
// sqlite_sequence, fixture rows included, with the golden.
func checkSupervisorTables(t *testing.T, s *store.Store, opts ...golden.Option) {
	t.Helper()
	golden.CheckJSON(t, goldenKey(t, "tables"), asJSON(t, supervisorTables(t, s)), opts...)
}

func compareSupervisorValues(t *testing.T, got []any, python supervisorCapture) {
	t.Helper()
	var normalized []any
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &normalized); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < max(len(normalized), len(python.Captures)); i++ {
		var g, w any = "<missing>", "<missing>"
		if i < len(normalized) {
			g = normalized[i]
		}
		if i < len(python.Captures) {
			w = python.Captures[i]
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("assertion %d differs from Python: go=%s python=%s", i+1, jsonText(g), jsonText(w))
		}
	}
}
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// compareSupervisorTables compares every populated row in every table, including fixture rows.
func compareSupervisorTables(t *testing.T, s *store.Store, python supervisorCapture) {
	t.Helper()
	got := supervisorTables(t, s)
	for name, want := range python.Tables {
		var normalized []map[string]any
		if rows, ok := got[name]; ok {
			encoded, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &normalized); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(normalized, want) {
			t.Errorf("table %s differs from Python:\ngo: %s\npython: %s", name, jsonText(got[name]), jsonText(want))
		}
	}
	for name, rows := range got {
		if _, ok := python.Tables[name]; !ok && len(rows) > 0 {
			t.Errorf("unexpected Go table %s: %s", name, jsonText(rows))
		}
	}
}

// supervisorTables is every row of every table but schema_meta and sqlite_sequence, by table, in
// rowid order; a table without rows is absent.
func supervisorTables(t *testing.T, s *store.Store) map[string][]map[string]any {
	t.Helper()
	got := make(map[string][]map[string]any)
	names, err := s.DB.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	names.Close()
	for _, name := range tables {
		rows, err := s.DB.Query("SELECT * FROM " + name + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			fields := make([]any, len(columns))
			pointers := make([]any, len(fields))
			for i := range fields {
				pointers[i] = &fields[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			row := make(map[string]any, len(fields))
			for i, v := range fields {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				row[columns[i]] = v
			}
			got[name] = append(got[name], row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return got
}
func supervisorMirror(t *testing.T, id, snapshot string, body func(*Channel, *store.Store) []any) {
	t.Helper()
	root := supervisorFixture(t, id)
	dbpath := filepath.Join(root, "tree", "state", "relay.sqlite3")
	data, err := os.ReadFile(filepath.Join(root, snapshot+".sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	restoreSnapshot(t, dbpath, data, "go")
	s, err := store.Open(context.Background(), dbpath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &Channel{Store: s, Linkage: StoreLinkage{s}, Program: filepath.Join(repoRoot(t), ".venv/bin/codex-session-relay")}
	got := body(c, s)
	checkSupervisorValues(t, got, treeGolden(t, root)...)
	checkSupervisorTables(t, s, treeGolden(t, root)...)
}
func captureRelationID(t *testing.T, c *Channel) string {
	t.Helper()
	var id string
	if err := c.Store.DB.QueryRow("SELECT relationship_id FROM relationships LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
