package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	_ "modernc.org/sqlite"
)

// Most tests here call t.Parallel: each builds its own store and temporary tree, and the
// process-wide environment (HOME, the XDG directories, the relay roots) is isolated once, by
// TestMain, so no test sets it. A test that replaces a package-level seam (TokenSource, or the
// CLI and delivery clocks sosClockNow and sosDeliveryClock) stays serial and restores the seam
// when it ends; testing runs every serial test before any parallel one, so the two never overlap.
// Do not add t.Parallel to such a test, and do not call t.Setenv in a parallel one.
func TestMain(m *testing.M) {
	testsupport.Main(m)
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

// checkSupervisorValues compares the values a replay produced, as JSON decodes them, with the
// golden; no values is an empty list.
func checkSupervisorValues(t *testing.T, got []any, opts ...golden.Option) {
	t.Helper()
	if got == nil {
		got = []any{}
	}
	golden.CheckJSON(t, goldenKey(t, "captures"), asJSON(t, got), opts...)
}

// checkSupervisorTables compares every populated row of every table but schema_meta and
// sqlite_sequence, fixture rows included, with the golden.
func checkSupervisorTables(t *testing.T, s *store.Store, opts ...golden.Option) {
	t.Helper()
	golden.CheckJSON(t, goldenKey(t, "tables"), asJSON(t, supervisorTables(t, s)), opts...)
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// supervisorTables is every row of every table but schema_meta and sqlite_sequence, by table, in
// rowid order; a table without rows is absent.
func supervisorTables(t *testing.T, s *store.Store) map[string][]map[string]any {
	t.Helper()
	return testsupport.NonEmpty(testsupport.TableRows(t, s.DB, "name NOT IN ('schema_meta','sqlite_sequence')"))
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
