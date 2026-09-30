package faults

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Ownership of the comparison stores.
//
// Every relay store is fenced (docs/port/decisions.md 14 and 30): a runtime writes only a store
// it owns and refuses one the other runtime owns. So Go always runs on a Go-owned store and live
// Python on a Python-owned one, and a store is put into that state only through the shared
// fixtures in internal/testsupport:
//
//   - f1Twins: one empty store for each runtime, fenced for its owner (testsupport.Fence).
//   - pythonCopy: a copy of a Go-seeded store after a takeover to Python (Rehome, HandOver).
//   - seedPython: rows a test seeds into the Python store, written by Python's own writer.
//   - readStore: a read-only look at either store, which neither fence refuses.
//
// Only live Python reads a Python store, and Python is live only while its answers are recorded
// or checked (python_oracle_test.go). On replay the Python twin is neither made nor seeded, and
// what a test reads of it after Python ran is part of Python's recorded answer.

// f1Twins makes gd and pd the same empty store owned by each runtime: the frozen Python-produced
// empty store (contract/fixtures/sqlite-ddl), written by hand and then fenced for Go in gd and for
// Python in pd exactly as each runtime's absent-store initializer stamps a store. Their schema_meta
// rows differ only in the owner value, the one runtime difference testsupport.OwnerNeutral names;
// every other row, store_id and store_created_at included, is identical.
func f1Twins(t *testing.T, gd, pd string) {
	t.Helper()
	f1Twin(t, gd, "go")
	if pyoracle.Live() {
		f1Twin(t, pd, "python")
	}
}

// f1Twin is one twin of f1Twins: the frozen empty store in dir, fenced for owner.
func f1Twin(t *testing.T, dir, owner string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join(f1Root(), "contract", "fixtures", "sqlite-ddl", "python-store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "relay.sqlite3")
	if err = os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Fence(t, path, owner)
}

// pythonCopy gives pyDir a copy of the stopped Go-owned store in goDir as Python finds it after a
// takeover: the copy gets its own physical identity (testsupport.Rehome) and is then handed over
// to Python (testsupport.HandOver). Its schema_meta differs from Go's in owner, owner_epoch and
// takeover_id, so it serves comparisons of replies and fault rows; a comparison that includes
// schema_meta starts from f1Twins instead.
func pythonCopy(t *testing.T, goDir, pyDir string) {
	t.Helper()
	if !pyoracle.Live() {
		return
	}
	raw, err := os.ReadFile(filepath.Join(goDir, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(pyDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pyDir, "relay.sqlite3")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Rehome(t, path)
	testsupport.HandOver(t, path, "python")
}

// seedStatement is one statement a test seeds, with its arguments.
type seedStatement struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"`
}

// seedPython runs statements on the Python-owned store at path through the retained Python
// fence's own writer (codex_session_relay.store.Store), each in autocommit exactly as f1SeedBoth
// runs them on Go's store through store.Open.
func seedPython(t *testing.T, path string, statements ...seedStatement) {
	t.Helper()
	if len(statements) == 0 || !pyoracle.Live() {
		return
	}
	raw, err := json.Marshal(statements)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(f1Root(), "internal/relay/faults/testdata/seed_python.py"), path)
	cmd.Dir = f1Root()
	cmd.Stdin = strings.NewReader(string(raw))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed the Python store: %v\n%s", err, out)
	}
}

// seedSQL wraps plain statements for seedPython.
func seedSQL(statements ...string) []seedStatement {
	out := make([]seedStatement, len(statements))
	for i, statement := range statements {
		out[i] = seedStatement{SQL: statement}
	}
	return out
}

// copyRowsToPython makes the rows of the Python twin pd those of the stopped Go twin gd after a
// test seeded gd through Go's own ledger. It runs through seedPython, so Python's writer writes
// them: every table except schema_meta (the twins share it but the owner) is replaced with gd's
// rows under the same rowids, and sqlite_sequence last, as the inserts advance it.
func copyRowsToPython(t *testing.T, ctx context.Context, gd, pd string) {
	t.Helper()
	if !pyoracle.Live() {
		return
	}
	source := filepath.Join(gd, "relay.sqlite3")
	statements := []string{"ATTACH DATABASE '" + strings.ReplaceAll(source, "'", "''") + "' AS go_twin"}
	readStore(t, ctx, source, func(ctx context.Context, s *store.Store) error {
		tables, err := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name")
		if err != nil {
			return err
		}
		for _, table := range tables {
			name := text(table, "name")
			columns, err := s.All(ctx, `SELECT name FROM pragma_table_info(?)`, name)
			if err != nil {
				return err
			}
			names := []string{"rowid"}
			for _, column := range columns {
				names = append(names, `"`+text(column, "name")+`"`)
			}
			list := strings.Join(names, ",")
			statements = append(statements, `DELETE FROM main."`+name+`"`,
				`INSERT INTO main."`+name+`"(`+list+`) SELECT `+list+` FROM go_twin."`+name+`" ORDER BY rowid`)
		}
		return nil
	})
	statements = append(statements, "DELETE FROM main.sqlite_sequence",
		"INSERT INTO main.sqlite_sequence(rowid,name,seq) SELECT rowid,name,seq FROM go_twin.sqlite_sequence ORDER BY rowid",
		"DETACH DATABASE go_twin")
	seedPython(t, filepath.Join(pd, "relay.sqlite3"), seedSQL(statements...)...)
}

// readStore reads the store at path, whichever runtime owns it, through a read-only snapshot.
func readStore(t *testing.T, ctx context.Context, path string, read func(context.Context, *store.Store) error) {
	t.Helper()
	ro, err := store.OpenReadOnly(ctx, path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = ro.ReadSnapshot(ctx, read)
	if closeErr := ro.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// ownerNeutralRows applies the one runtime-identity rule to schema_meta rows, read as objects,
// of a store writer stamped.
func ownerNeutralRows(t *testing.T, writer testsupport.Runtime, rows any) any {
	t.Helper()
	list, _ := rows.([]any)
	for _, row := range list {
		if entry, ok := row.(map[string]any); ok {
			if key, ok := entry["key"].(string); ok {
				entry["value"] = testsupport.OwnerNeutral(t, writer, key, entry["value"])
			}
		}
	}
	return rows
}
