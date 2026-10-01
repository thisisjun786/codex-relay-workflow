package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

var suiteDirectory string
var suiteBinary string
var suiteAlias string

// installSuiteBinary puts the crw every binary scenario runs, one clock-injected build, at
// <suite>/crw with its codex-session-relay alias beside it. Production defaults are unchanged
// because ordinary builds do not set the link-time clock seam.
func installSuiteBinary(root string) error {
	built, err := testsupport.BuildCRWPath("-buildvcs=false", "-ldflags=-X github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter.testClock=1700000000")
	if err != nil {
		return err
	}
	suiteBinary = filepath.Join(root, "crw")
	suiteAlias = filepath.Join(root, "codex-session-relay")
	if err := testsupport.CopyBinary(built, suiteBinary); err != nil {
		return err
	}
	return os.Symlink(suiteBinary, suiteAlias)
}

// deliverySeed is the relational seed Python's DeliveryTestCase built: a relationship, its
// final event queued for delivery, and the artifact that event carries. Revision is the event's
// revision hash; it and Event are digests over the artifact's path, which lies in this run's
// suite directory.
type deliverySeed struct{ Event, Revision, Work string }

// derived names the values of an answer that follow from the seed artifact's path: the event's
// revision and id, and the delivery request ids named after the id's first twelve characters.
func (seed deliverySeed) derived() []golden.Option {
	return []golden.Option{golden.Substitute(seed.Revision, "<seed-revision>"), golden.Substitute(seed.Event, "<seed-event>"), golden.Substitute("del-"+seed.Event[:12]+"-", "del-<seed-event-12>-")}
}

// seedTable is one populated table of a store, its rows in rowid order: an integer or a text
// is plain JSON, a real and a blob are tagged (sqliteArgument).
type seedTable struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// seedAnswer is what the Python seed left, the fixture delivery-seed.json: its event, the
// populated tables of its store but schema_meta, and the files of its tree, by path relative to
// the tree. The event id and the revision hash appear as <seed-event> and <seed-revision>, the
// suite directory the tree lay in as <suite>.
type seedAnswer struct {
	Event  string               `json:"event"`
	Tables map[string]seedTable `json:"tables"`
	Files  map[string]string    `json:"files"`
}

var seedFiles sync.Once

// Python constructed the relational seed once, retaining its immutable artifact path. Each
// scenario gets independent SQLite copies, never a shared writer: rebuilt from the seed's rows
// on the frozen Python-produced empty store, which is what preFenceFixture reads of them.
func copyDeliverySeed(t *testing.T, destination string) deliverySeed {
	t.Helper()
	root := filepath.Join(suiteDirectory, "oracle-seed")
	raw := bytes.ReplaceAll(golden.Fixture(t, "delivery-seed.json"), []byte("<suite>"), []byte(suiteDirectory))
	var answer seedAnswer
	if err := decodeNumbers(raw, &answer); err != nil {
		t.Fatal(err)
	}
	seedFiles.Do(func() {
		for name, content := range answer.Files {
			path := filepath.Join(root, filepath.FromSlash(name))
			if _, err := os.Stat(path); err == nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	seed := deliverySeed{Work: filepath.Join(root, "work")}
	var err error
	if seed.Revision, seed.Event, err = seedIdentity(answer.Tables, seed.Work); err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("<seed-event>"), []byte(seed.Event)), []byte("<seed-revision>"), []byte(seed.Revision))
	if err := decodeNumbers(raw, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Event != seed.Event {
		t.Fatalf("seed event %s, not %s", answer.Event, seed.Event)
	}
	path := filepath.Join(destination, "python.sqlite3")
	if err := writeSeedStore(path, answer.Tables); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "go.sqlite3"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return seed
}

// seedIdentity derives the seed event's revision hash and id from the one artifact under work
// and the event row's other fields, and checks them against the event row.
func seedIdentity(tables map[string]seedTable, work string) (string, string, error) {
	events := tables["events"]
	if len(events.Rows) != 1 {
		return "", "", fmt.Errorf("the seed has %d events", len(events.Rows))
	}
	row := map[string]any{}
	for i, column := range events.Columns {
		row[column] = events.Rows[0][i]
	}
	entries, _, err := BuildManifest([]string{filepath.Join(work, "out.txt")}, []string{work}, false)
	if err != nil {
		return "", "", err
	}
	revision, err := RevisionHash(entries)
	if err != nil {
		return "", "", err
	}
	number := func(key string) (int, error) {
		switch v := row[key].(type) {
		case json.Number:
			n, err := v.Int64()
			return int(n), err
		case int64:
			return int(v), nil
		}
		return 0, fmt.Errorf("seed event %s is %#v", key, row[key])
	}
	generation, err := number("execution_generation")
	if err != nil {
		return "", "", err
	}
	attempt, err := number("attempt")
	if err != nil {
		return "", "", err
	}
	relationship, _ := row["relationship_id"].(string)
	outcome, _ := row["outcome"].(string)
	turn, _ := row["turn_id"].(string)
	event, err := store.EventID(relationship, generation, revision, outcome, turn, &attempt)
	if err != nil {
		return "", "", err
	}
	for key, want := range map[string]string{"revision_hash": revision, "event_id": event} {
		if got := row[key]; got != want && got != "<seed-"+strings.TrimSuffix(strings.TrimSuffix(key, "_hash"), "_id")+">" {
			return "", "", fmt.Errorf("seed event %s is %v, Go derives %s", key, got, want)
		}
	}
	return revision, event, nil
}

// writeSeedStore writes at path the frozen Python-produced empty store holding tables' rows.
func writeSeedStore(path string, tables map[string]seedTable) error {
	ctx := context.Background()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		return err
	}
	frozen, err := os.ReadFile(filepath.Join(repo, "contract/fixtures/sqlite-ddl/python-store.sqlite3"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, frozen, 0o600); err != nil {
		return err
	}
	db, err := ownership.OpenExisting(ctx, path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		table := tables[name]
		columns := make([]string, len(table.Columns))
		for i, column := range table.Columns {
			columns[i] = quoted(column)
		}
		insert := `INSERT INTO ` + quoted(name) + ` (` + strings.Join(columns, ",") + `) VALUES (` + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + `)`
		for _, row := range table.Rows {
			values := make([]any, len(row))
			for i, value := range row {
				if values[i], err = sqliteArgument(value); err != nil {
					return errors.Join(err, tx.Rollback())
				}
			}
			if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
				return errors.Join(fmt.Errorf("%s: %w", name, err), tx.Rollback())
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
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
