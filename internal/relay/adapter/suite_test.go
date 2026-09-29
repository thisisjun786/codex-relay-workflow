package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var suiteDirectory string
var suiteBinary string
var suiteAlias string

// Build before TestMain changes HOME so the toolchain inherits the caller's
// caches and module environment, on both developer workstations and hosted CI.
// Every binary scenario uses this one clock-injected build; production defaults
// are unchanged because ordinary builds do not set the link-time clock seam.
func buildSuiteBinary(root string) error {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		return err
	}
	suiteBinary = filepath.Join(root, "crw")
	suiteAlias = filepath.Join(root, "codex-session-relay")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return err
	}
	command := exec.Command(goBinary, "build", "-buildvcs=false", "-ldflags=-X github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter.testClock=1700000000", "-o", suiteBinary, "./cmd/crw")
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("build shared crw: %w\n%s", err, output)
	}
	return os.Symlink(suiteBinary, suiteAlias)
}

type deliverySeed struct{ Event, Work string }

var seedOnce sync.Once
var seeded deliverySeed
var seedError error

// Python constructs the relational seed once, retaining its immutable artifact
// path. Each scenario gets independent SQLite copies, never a shared writer.
func copyDeliverySeed(t *testing.T, destination string) deliverySeed {
	t.Helper()
	seedOnce.Do(func() {
		root := filepath.Join(suiteDirectory, "oracle-seed")
		if err := os.Mkdir(root, 0700); err != nil {
			seedError = err
			return
		}
		repo, err := filepath.Abs("../../..")
		if err != nil {
			seedError = err
			return
		}
		command := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/settlement_capture.py"), root, "seed")
		command.Dir = repo
		output, err := command.CombinedOutput()
		if err != nil {
			seedError = fmt.Errorf("Python seed: %w\n%s", err, output)
			return
		}
		if err := json.Unmarshal(output, &seeded); err != nil {
			seedError = err
			return
		}
		seeded.Work = filepath.Join(root, "work")
	})
	if seedError != nil {
		t.Fatal(seedError)
	}
	data, err := os.ReadFile(filepath.Join(suiteDirectory, "oracle-seed", "python.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.sqlite3", "python.sqlite3"} {
		if err := os.WriteFile(filepath.Join(destination, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return seeded
}

// ownerNeutral applies the runtime-identity rule to the schema_meta rows of a whole-table dump
// of a store writer stamped.
func ownerNeutral(t *testing.T, writer testsupport.Runtime, tables any) {
	t.Helper()
	all, ok := tables.(map[string]any)
	if !ok {
		t.Fatalf("table dump is %T", tables)
	}
	rows, _ := all["schema_meta"].([]any)
	for _, row := range rows {
		pair, ok := row.([]any)
		if !ok || len(pair) != 2 {
			t.Fatalf("schema_meta row %v", row)
		}
		if key, ok := pair[0].(string); ok {
			pair[1] = testsupport.OwnerNeutral(t, writer, key, pair[1])
		}
	}
}

// preFenceFixture writes at dst a relay store as a pre-fence Python writes one, holding the rows
// of the stopped store at src: the frozen Python-produced empty store
// (contract/fixtures/sqlite-ddl), its own schema_meta identity kept, with every row of every src
// table but schema_meta copied in, in rowid order; schema_meta.socket_path is the socket given
// (canonical, as Python records the socket a store serves) or absent; then statements. It
// carries no ownership stamp: the caller Fences it for the runtime that uses it.
func preFenceFixture(t *testing.T, src, dst, socket string, statements ...string) {
	t.Helper()
	ctx := context.Background()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := os.ReadFile(filepath.Join(repo, "contract/fixtures/sqlite-ddl/python-store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dst, frozen, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, cleanup, err := ownership.CopySnapshot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	from, err := ownership.OpenExisting(ctx, snapshot, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer from.Close()
	to, err := ownership.OpenExisting(ctx, dst, "rw")
	if err != nil {
		t.Fatal(err)
	}
	defer to.Close()
	names := []string{}
	rows, err := from.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_meta' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	tx, err := to.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		rows, err := from.QueryContext(ctx, `SELECT * FROM "`+name+`" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		quoted := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = `"` + column + `"`
		}
		insert := `INSERT INTO "` + name + `" (` + strings.Join(quoted, ",") + `) VALUES (` + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + `)`
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, insert, values...); err != nil {
				t.Fatalf("%s: %v", name, errors.Join(err, tx.Rollback()))
			}
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM schema_meta WHERE key = 'socket_path'"); err != nil {
		t.Fatal(errors.Join(err, tx.Rollback()))
	}
	if socket != "" {
		canonical, err := store.CanonicalSocket(socket)
		if err != nil {
			t.Fatal(errors.Join(err, tx.Rollback()))
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('socket_path', ?)", canonical); err != nil {
			t.Fatal(errors.Join(err, tx.Rollback()))
		}
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, errors.Join(err, tx.Rollback()))
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = to.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
}
