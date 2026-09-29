package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	_ "modernc.org/sqlite"
)

type supervisorCapture struct {
	Captures []any                       `json:"captures"`
	Problems []string                    `json:"problems"`
	Tables   map[string][]map[string]any `json:"tables"`
}

var (
	supervisorCaptures   sync.Map
	supervisorBinaryOnce sync.Once
	supervisorBinaryPath string
	supervisorBinaryDir  string
	supervisorBinaryErr  error
)

func TestMain(m *testing.M) {
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	supervisorCaptures.Range(func(_, value any) bool { _ = os.RemoveAll(value.(string)); return true })
	if supervisorBinaryDir != "" {
		if err := os.RemoveAll(supervisorBinaryDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

// pythonSupervisorCapture executes each Python test at most once per test binary.
func pythonSupervisorCapture(t *testing.T, id string) (string, supervisorCapture) {
	t.Helper()
	if v, ok := supervisorCaptures.Load(id); ok {
		return v.(string), readSupervisorCapture(t, v.(string))
	}
	root, err := os.MkdirTemp("", "crw-supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/capture.py")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, root, id)
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src")+":"+filepath.Join(repo, "packages/codex-session-relay"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python capture %s: %v\n%s", id, err, output)
	}
	supervisorCaptures.Store(id, root)
	return root, readSupervisorCapture(t, root)
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
func supervisorMirror(t *testing.T, id, snapshot string, body func(*Channel, *store.Store) []any) {
	t.Helper()
	root, python := pythonSupervisorCapture(t, id)
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
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	c := &Channel{Store: s, Linkage: StoreLinkage{s}, Program: filepath.Join(repo, ".venv/bin/codex-session-relay")}
	got := body(c, s)
	compareSupervisorValues(t, got, python)
	compareSupervisorTables(t, s, python)
}
func captureRelationID(t *testing.T, c *Channel) string {
	t.Helper()
	var id string
	if err := c.Store.DB.QueryRow("SELECT relationship_id FROM relationships LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
