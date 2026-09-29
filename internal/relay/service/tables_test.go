package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Compare every table/column/value, including persisted JSON bytes. SQLite page
// layout, freelists and WAL checkpoints are not logical table contents.
// writer is the runtime that ran against home, so the one runtime-identity row, schema_meta's
// owner, must be its own.
func tables(t *testing.T, home string, writer testsupport.Runtime) string {
	t.Helper()
	path := filepath.Join(home, "state", "relay.sqlite3")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "absent"
	}
	cmd := exec.Command(filepath.Join(testRoot, ".venv/bin/python"), "-c", `import json, sqlite3, sys
from contextlib import closing
with closing(sqlite3.connect(sys.argv[1])) as db:
    tables = {}
    for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").fetchall():
        columns = [r[1] for r in db.execute('PRAGMA table_info("' + name + '")')]
        rows = db.execute('SELECT * FROM "' + name + '" ORDER BY rowid').fetchall()
        time_columns = {'next_eligible_at', 'next_retry_at', 'lease_until', 'last_send_at', 'window_start'}
        tables[name] = [[('EPOCH_TIME' if columns[i] in time_columns and isinstance(v, (int, float)) else v) for i, v in enumerate(row)] for row in rows]
    print(json.dumps(tables, ensure_ascii=True, indent=2))`, path)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("table snapshot: %v %s", err, raw)
	}
	var data map[string][][]any
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, row := range data["schema_meta"] {
		if row[0] == "store_id" {
			row[1] = "RANDOM_STORE_ID"
		}
		if key, ok := row[0].(string); ok {
			row[1] = testsupport.OwnerNeutral(t, writer, key, row[1])
		}
	}
	raw, err = json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return normalize(string(raw))
}
func Test29EmptyTickTableParity(t *testing.T) {
	home := t.TempDir()
	args := []string{"--socket", home + "/socket", "daemon", "--max-ticks", "1", "--allow-isolated-scope"}
	want := invoke(t, home, true, args...)
	before := tables(t, home, testsupport.Python)
	resetRuntime(t, home)
	got := invoke(t, home, false, args...)
	after := tables(t, home, testsupport.Go)
	compare(t, want, got)
	if before != after {
		t.Fatalf("tables\nPython %s\nGo %s", before, after)
	}
}
func Test29LaunchPolicyPersistence(t *testing.T) {
	home := t.TempDir()
	policy := filepath.Join(home, "execution.json")
	raw := `{"roles":{"parent":{"model":"test-model","reasoningEffort":"high"},"child":{"model":"test-model","reasoningEffort":"high"}}}`
	if err := os.WriteFile(policy, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{{"service", "declare", "--execution-policy", policy}, {"service", "status"}, {"service", "declare", "--forget-execution-policy"}}
	var want []capture
	var wantFiles []map[string]string
	for _, args := range steps {
		result := invoke(t, home, true, args...)
		if result.Code != 0 {
			t.Fatalf("policy setup refused: %+v", result)
		}
		want = append(want, result)
		wantFiles = append(wantFiles, files(t, home, testsupport.Python))
	}
	resetRuntime(t, home)
	for i, args := range steps {
		got := invoke(t, home, false, args...)
		compare(t, want[i], got)
		wf, _ := json.Marshal(wantFiles[i])
		gf, _ := json.Marshal(files(t, home, testsupport.Go))
		if string(wf) != string(gf) {
			t.Fatalf("%s\nPython %s\nGo %s", strings.Join(args, " "), wf, gf)
		}
	}
}
