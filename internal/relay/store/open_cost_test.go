package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// settlementsBackfillMarker is the schema_meta row that says the assignment_settlements backfill
// is done. It is stored data, so the key is spelled out here: renaming it would make every store
// run the backfill again.
const settlementsBackfillMarker = "backfill:assignment_settlements"

// newOpenedStore creates an absent store the way a first command does and returns its path.
func newOpenedStore(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "relay.sqlite3")
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// rawDB is a plain connection to path, for what a test stages or reads beside the opener.
func rawDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// holdWriteLock takes SQLite's write lock on path from another connection and keeps it until the
// returned release runs. It checks that a second writer is turned away meanwhile, so a test that
// then sees an open succeed knows the open did not need that lock.
func holdWriteLock(t testing.TB, path string) (release func()) {
	t.Helper()
	ctx := context.Background()
	holder, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
		_ = holder.Close()
	}
	t.Cleanup(release)
	other, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	probe, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err = probe.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = probe.ExecContext(ctx, "BEGIN IMMEDIATE"); err == nil {
		_, _ = probe.ExecContext(ctx, "ROLLBACK")
		t.Fatal("the write lock is not held: a second writer got it")
	}
	return release
}

func settlementKeys(t testing.TB, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT relationship_id || '/' || thread_id || '/' || turn_id || '/' || terminal_status FROM assignment_settlements ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

func insertObservation(t testing.TB, db *sql.DB, thread, turn string, relationship any) {
	t.Helper()
	_, err := db.Exec("INSERT INTO observations (thread_id, turn_id, terminal_status, relationship_id, classification, event_id, observed_at) VALUES (?, ?, 'completed', ?, 'settled', NULL, '2026-10-01T00:00:00Z')", thread, turn, relationship)
	if err != nil {
		t.Fatal(err)
	}
}

func metaValue(t testing.TB, db *sql.DB, key string) (string, bool) {
	t.Helper()
	var value string
	err := db.QueryRow("SELECT value FROM schema_meta WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value, true
}

// A command that declares itself read-only opens the store without the write lock: another
// connection holding it does not stop the open, and the open does not wait for it. The store is a
// complete one (every index installed, WAL mode set), as every store the relay creates is; the
// open of an incomplete one still installs what is missing and takes the lock to do it
// (TestReadCommandOpenStillInstallsAMissingIndex).
func TestReadCommandOpenTakesNoWriteLock(t *testing.T) {
	for name, socket := range map[string]string{"unbound": "", "bound to a socket": "relay.sock"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "relay.sqlite3")
			s, err := Open(context.Background(), path, socket)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			release := holdWriteLock(t, path)
			defer release()

			ctx, cancel := context.WithTimeout(WithReadOnlyCommand(context.Background()), 3*time.Second)
			defer cancel()
			started := time.Now()
			s, err = Open(ctx, path, socket)
			if err != nil {
				t.Fatalf("a read-only command's open needed the write lock (gave up after %v): %v", time.Since(started), err)
			}
			defer s.Close()
			var rows int
			if err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_meta").Scan(&rows); err != nil || rows == 0 {
				t.Fatalf("the opened store does not read: %d rows: %v", rows, err)
			}
		})
	}
}

// The boundary of the guarantee above: a store that lacks an index is completed by the first open,
// a read-only command's included, exactly as before.
func TestReadCommandOpenStillInstallsAMissingIndex(t *testing.T) {
	path := newOpenedStore(t)
	db := rawDB(t, path)
	if _, err := db.Exec("DROP INDEX incident_routes_stage"); err != nil {
		t.Fatal(err)
	}
	s, err := Open(WithReadOnlyCommand(context.Background()), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	var name string
	if err = db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'incident_routes_stage'").Scan(&name); err != nil {
		t.Fatalf("the open did not install the missing index: %v", err)
	}
}

// The backfill is a one-time upgrade step: once the store records that it ran, an open does not run
// it again, however many opens follow and whichever form they take.
func TestSettlementsBackfillRunsOnce(t *testing.T) {
	ctx := context.Background()
	path := newOpenedStore(t)
	db := rawDB(t, path)
	// A store from before the marker: observations of settled assignments without their
	// settlements, and no marker row.
	insertObservation(t, db, "thread-a", "turn-1", "rel-a")
	insertObservation(t, db, "thread-b", "turn-2", "rel-b")
	insertObservation(t, db, "thread-c", "turn-3", nil)
	if _, err := db.Exec("DELETE FROM schema_meta WHERE key = ?", settlementsBackfillMarker); err != nil {
		t.Fatal(err)
	}

	open := func(form string, ctx context.Context) {
		t.Helper()
		s, err := Open(ctx, path, "")
		if err != nil {
			t.Fatalf("%s open: %v", form, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	open("first (writable)", ctx)
	want := []string{"rel-a/thread-a/turn-1/completed", "rel-b/thread-b/turn-2/completed"}
	if got := settlementKeys(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("the first open did not backfill the settlements of observations with a relationship: %v, want %v", got, want)
	}
	if value, ok := metaValue(t, db, settlementsBackfillMarker); !ok || value != "1" {
		t.Fatalf("the first open left no completion marker: %q, %v", value, ok)
	}

	// Later opens leave the settlements as they are: a row removed, an observation written
	// without one are not filled in by an open, in either form.
	if _, err := db.Exec("DELETE FROM assignment_settlements WHERE relationship_id = 'rel-a'"); err != nil {
		t.Fatal(err)
	}
	insertObservation(t, db, "thread-d", "turn-4", "rel-d")
	open("second (writable)", ctx)
	open("third (read-only command)", WithReadOnlyCommand(ctx))
	if got, want := settlementKeys(t, db), []string{"rel-b/thread-b/turn-2/completed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a later open ran the backfill again: %v, want %v", got, want)
	}
}

// A store written before the marker existed (here the frozen Python-made store, handed to Go) comes
// up with everything it had. A read-only command's open backfills the settlements as every open did
// and writes nothing else, because a read-only command leaves the store's rows as it found them;
// the first writable open adds the marker, and the opens after that take no write lock.
func TestEarlierVersionStoreUpgradesOnFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	fixture, err := testsupport.FrozenStore()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Fence(t, path, "go")
	db := rawDB(t, path)
	insertObservation(t, db, "thread-a", "turn-1", "rel-a")
	insertObservation(t, db, "thread-b", "turn-2", nil)
	if _, ok := metaValue(t, db, settlementsBackfillMarker); ok {
		t.Fatal("the earlier-version store already carries the marker")
	}
	metaBefore := testsupport.TableRows(t, db, "name = 'schema_meta'")["schema_meta"]
	tablesBefore := testsupport.TableRows(t, db, "name NOT IN ('schema_meta', 'assignment_settlements')")
	wantSettlements := []string{"rel-a/thread-a/turn-1/completed"}

	open := func(ctx context.Context) {
		t.Helper()
		s, err := Open(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		var integrity string
		if err = s.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity: %q: %v", integrity, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	open(WithReadOnlyCommand(context.Background()))
	if got := settlementKeys(t, db); !reflect.DeepEqual(got, wantSettlements) {
		t.Fatalf("the read-only open did not backfill: %v, want %v", got, wantSettlements)
	}
	if got := testsupport.TableRows(t, db, "name = 'schema_meta'")["schema_meta"]; !reflect.DeepEqual(got, metaBefore) {
		t.Fatalf("a read-only command changed schema_meta: %v, was %v", got, metaBefore)
	}

	open(context.Background())
	if value, ok := metaValue(t, db, settlementsBackfillMarker); !ok || value != "1" {
		t.Fatalf("the writable open left no marker: %q, %v", value, ok)
	}
	metaAfter := testsupport.TableRows(t, db, "name = 'schema_meta'")["schema_meta"]
	if len(metaAfter) != len(metaBefore)+1 {
		t.Fatalf("schema_meta rows: %d before, %d after; only the marker may be added", len(metaBefore), len(metaAfter))
	}
	for _, row := range metaBefore {
		if value, _ := metaValue(t, db, row["key"].(string)); value != row["value"] {
			t.Fatalf("schema_meta %v changed to %q", row, value)
		}
	}
	if got := settlementKeys(t, db); !reflect.DeepEqual(got, wantSettlements) {
		t.Fatalf("settlements after the writable open: %v, want %v", got, wantSettlements)
	}
	if !reflect.DeepEqual(testsupport.TableRows(t, db, "name NOT IN ('schema_meta', 'assignment_settlements')"), tablesBefore) {
		t.Fatal("the upgrade changed rows other than the settlements and the marker")
	}

	release := holdWriteLock(t, path)
	defer release()
	ctx, cancel := context.WithTimeout(WithReadOnlyCommand(context.Background()), 3*time.Second)
	defer cancel()
	s, err := Open(ctx, path, "")
	if err != nil {
		t.Fatalf("an upgraded store still needs the write lock to open for a read: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

// unmarkedStoreWithObservations returns a store with the backfill still to do: observations of settled
// assignments, their settlements missing, and no marker.
func unmarkedStoreWithObservations(t testing.TB) (string, *sql.DB) {
	t.Helper()
	path := newOpenedStore(t)
	db := rawDB(t, path)
	insertObservation(t, db, "thread-a", "turn-1", "rel-a")
	insertObservation(t, db, "thread-b", "turn-2", "rel-b")
	if _, err := db.Exec("DELETE FROM schema_meta WHERE key = ?", settlementsBackfillMarker); err != nil {
		t.Fatal(err)
	}
	return path, db
}

// The marker and the backfill are one transaction: when the marker cannot be written the settlements
// are not kept either, and the next open completes the upgrade.
func TestBackfillAndMarkerCommitTogether(t *testing.T) {
	path, db := unmarkedStoreWithObservations(t)
	if _, err := db.Exec("CREATE TRIGGER zz_no_marker BEFORE INSERT ON schema_meta WHEN NEW.key = '" + settlementsBackfillMarker + "' BEGIN SELECT RAISE(ABORT, 'marker refused'); END"); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, "")
	if err == nil {
		_ = s.Close()
		t.Fatal("the open succeeded though the marker could not be written")
	}
	if !strings.Contains(err.Error(), "marker refused") {
		t.Fatalf("unexpected failure: %v", err)
	}
	if got := settlementKeys(t, db); len(got) != 0 {
		t.Fatalf("the settlements stayed without their marker: %v", got)
	}
	if _, ok := metaValue(t, db, settlementsBackfillMarker); ok {
		t.Fatal("the marker was written")
	}
	if _, err = db.Exec("DROP TRIGGER zz_no_marker"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := settlementKeys(t, db); len(got) != 2 {
		t.Fatalf("the retry did not complete the backfill: %v", got)
	}
	if value, ok := metaValue(t, db, settlementsBackfillMarker); !ok || value != "1" {
		t.Fatalf("the retry left no marker: %q, %v", value, ok)
	}
}

// An opener that found the marker missing and then waited for the write lock finds, once it has the
// lock, that another opener completed the backfill, and does not run it again.
func TestBackfillIsNotRepeatedByAnOpenerThatWaitedForTheLock(t *testing.T) {
	path, db := unmarkedStoreWithObservations(t)
	other := rawDB(t, path)
	previous := backfillMarkerChecked
	backfillMarkerChecked = func() {
		backfillMarkerChecked = previous
		// The other opener finishes first: its backfill is done and marked.
		if _, err := other.Exec("INSERT INTO schema_meta VALUES (?, '1')", settlementsBackfillMarker); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { backfillMarkerChecked = previous })
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := settlementKeys(t, db); len(got) != 0 {
		t.Fatalf("the opener ran the backfill although the marker was already there: %v", got)
	}
}

// A writable open puts back a missing creation time and leaves the store_id and the marker alone.
// (The version row is part of what the fence's stamp check reads, so a store without it is refused
// before any seeding.)
func TestOpenSeedsOnlyWhatIsMissing(t *testing.T) {
	for name, ctx := range map[string]context.Context{"writable": context.Background(), "read-command": WithReadOnlyCommand(context.Background())} {
		t.Run(name, func(t *testing.T) {
			path := newOpenedStore(t)
			db := rawDB(t, path)
			storeID, _ := metaValue(t, db, "store_id")
			if _, err := db.Exec("DELETE FROM schema_meta WHERE key = 'store_created_at'"); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, path, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if created, _ := metaValue(t, db, "store_created_at"); created == "" {
				t.Fatal("store_created_at was not put back")
			}
			if got, _ := metaValue(t, db, "store_id"); got != storeID {
				t.Fatalf("store_id changed from %q to %q", storeID, got)
			}
			if value, _ := metaValue(t, db, settlementsBackfillMarker); value != "1" {
				t.Fatalf("marker = %q", value)
			}
		})
	}
}

// A store the relay creates carries the marker from the start: it has nothing to backfill, so its
// first reader never runs the backfill.
func TestCreatedStoreIsBornMarked(t *testing.T) {
	db := rawDB(t, newOpenedStore(t))
	if value, ok := metaValue(t, db, settlementsBackfillMarker); !ok || value != "1" {
		t.Fatalf("a created store carries the marker %q, %v; want \"1\"", value, ok)
	}
}

// countingQueryer records the statements a validation sends.
type countingQueryer struct {
	db      *sql.DB
	queries []string
}

func (c *countingQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.queries = append(c.queries, query)
	return c.db.QueryContext(ctx, query, args...)
}

// columnReads is how many statements asked SQLite for columns.
func (c *countingQueryer) columnReads() int {
	n := 0
	for _, query := range c.queries {
		if strings.Contains(strings.ToLower(query), "table_info") {
			n++
		}
	}
	return n
}

// stageSchema returns a database of its own with a column no other test of this process has added
// to a required table, so its schema digest is new.
func stageSchema(t testing.TB, unique string) *sql.DB {
	t.Helper()
	db := rawDB(t, newOpenedStore(t))
	if _, err := db.Exec("ALTER TABLE journal ADD COLUMN " + unique + " TEXT"); err != nil {
		t.Fatal(err)
	}
	return db
}

// Validating a schema reads each table's columns once per schema digest: a schema the process has
// found whole is not read again, in this database or in another, and a refusal is judged afresh.
func TestValidateOwnershipSchemaIsCachedPerSchemaDigest(t *testing.T) {
	ctx := context.Background()
	unique := fmt.Sprintf("zz_%d", time.Now().UnixNano())
	first := stageSchema(t, unique)
	cold := &countingQueryer{db: first}
	if err := ValidateOwnershipSchema(ctx, cold); err != nil {
		t.Fatal(err)
	}
	if n := cold.columnReads(); n != 1 {
		t.Fatalf("a schema never seen before took %d column reads, want one statement for all tables", n)
	}
	again := &countingQueryer{db: first}
	if err := ValidateOwnershipSchema(ctx, again); err != nil {
		t.Fatal(err)
	}
	if n := again.columnReads(); n != 0 {
		t.Fatalf("a second validation of the same schema read columns again (%d reads)", n)
	}
	second := stageSchema(t, unique)
	other := &countingQueryer{db: second}
	if err := ValidateOwnershipSchema(ctx, other); err != nil {
		t.Fatal(err)
	}
	if n := other.columnReads(); n != 0 {
		t.Fatalf("a database with the same schema was validated again (%d reads)", n)
	}

	// A schema that lacks a required column is judged on its own, whatever an earlier validation
	// of another schema concluded; its refusal is not kept.
	for _, statement := range []string{"DROP INDEX journal_managed_creation", "ALTER TABLE journal DROP COLUMN detail"} {
		if _, err := second.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for i := 0; i < 2; i++ {
		broken := &countingQueryer{db: second}
		err := ValidateOwnershipSchema(ctx, broken)
		if err == nil || !strings.Contains(err.Error(), "required column missing: journal.detail") {
			t.Fatalf("round %d: a table missing a required column was accepted: %v", i, err)
		}
		if broken.columnReads() == 0 {
			t.Fatalf("round %d: the refusal came from a remembered answer", i)
		}
	}
	if err := ValidateOwnershipSchema(ctx, &countingQueryer{db: first}); err != nil {
		t.Fatalf("the first schema stopped validating: %v", err)
	}
}

// A store this runtime created is whole at the first look: its schema is the one the build ships,
// so validating it reads the catalog and no column.
func TestValidateOwnershipSchemaAcceptsTheShippedSchemaWithoutReadingColumns(t *testing.T) {
	db := rawDB(t, newOpenedStore(t))
	counted := &countingQueryer{db: db}
	if err := ValidateOwnershipSchema(context.Background(), counted); err != nil {
		t.Fatal(err)
	}
	if n := counted.columnReads(); n != 0 {
		t.Fatalf("a store with the shipped schema took %d column reads", n)
	}
}

// shippedSchemaDigest is the digest of the tables the embedded DDL creates, and the schema it
// stands for is whole: the full per-table validation accepts it.
func TestShippedSchemaDigest(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ddl.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ddl, guards, err := SchemaStatements()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	for _, guard := range guards {
		if _, err = db.Exec(guard); err != nil {
			t.Fatal(err)
		}
	}
	required, err := requiredSchema()
	if err != nil {
		t.Fatal(err)
	}
	if err = validateByTableInfo(ctx, db, required); err != nil {
		t.Fatalf("the schema the DDL creates is not accepted by the per-table validation: %v", err)
	}
	digest, plain, err := catalogDigest(ctx, db, required)
	if err != nil || !plain {
		t.Fatalf("the schema the DDL creates is not plain: %v, %v", plain, err)
	}
	if digest != shippedSchemaDigest {
		t.Fatalf("the frozen tables changed: set shippedSchemaDigest (ownership.go) to %q", digest)
	}

	// The store the Python relay made holds the same tables, byte for byte.
	fixture, err := testsupport.FrozenStore()
	if err != nil {
		t.Fatal(err)
	}
	pythonPath := filepath.Join(t.TempDir(), "python.sqlite3")
	if err = os.WriteFile(pythonPath, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	python := rawDB(t, pythonPath)
	if got, plain, err := catalogDigest(ctx, python, required); err != nil || !plain || got != shippedSchemaDigest {
		t.Fatalf("the Python-made store's schema digest is %q (plain %v, %v), not the shipped one", got, plain, err)
	}
}

// The validation answers what the per-table validation answers for every schema, plain or not:
// the same acceptance and, for a refusal, the same words naming the same first defect.
func TestValidateOwnershipSchemaAnswersAsThePerTableValidationDoes(t *testing.T) {
	ctx := context.Background()
	required, err := requiredSchema()
	if err != nil {
		t.Fatal(err)
	}
	// earlier of two required tables, in the DDL's order
	position := map[string]int{}
	for i, table := range required {
		position[table.name] = i
	}
	earlier, later := "refusals", "route_incidents"
	if position[earlier] > position[later] {
		earlier, later = later, earlier
	}
	cases := []struct {
		name    string
		mutate  []string
		refused string
	}{
		{"whole", nil, ""},
		{"a column of two tables missing", []string{"DROP INDEX route_incidents_fault", "ALTER TABLE " + later + " DROP COLUMN recorded_seq", "DROP INDEX journal_managed_creation", "ALTER TABLE journal DROP COLUMN detail"}, "required column missing"},
		{"two tables missing", []string{"DROP TABLE " + later, "DROP TABLE " + earlier}, "required table missing: " + earlier},
		{"a table spelled in another case", []string{"ALTER TABLE journal RENAME TO journal_tmp", "ALTER TABLE journal_tmp RENAME TO Journal"}, ""},
		{"a view in place of a table", []string{"ALTER TABLE journal RENAME TO journal_rows", "CREATE VIEW journal AS SELECT * FROM journal_rows"}, ""},
		{"a temporary table hiding a required one", []string{"CREATE TEMP TABLE journal (at TEXT)"}, "required column missing: journal."},
		{"a temporary table of another case hiding a required one", []string{"CREATE TEMP TABLE JOURNAL (at TEXT)"}, "required column missing: journal."},
		{"a view without a required column", []string{"ALTER TABLE journal RENAME TO journal_rows", "CREATE VIEW journal AS SELECT at, kind, subject FROM journal_rows"}, "required column missing: journal."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := rawDB(t, newOpenedStore(t))
			for _, statement := range c.mutate {
				if _, err := db.Exec(statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			want := validateByTableInfo(ctx, db, required)
			got := ValidateOwnershipSchema(ctx, db)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("answer %v, the per-table validation answers %v", got, want)
			}
			if c.refused == "" && got != nil {
				t.Fatalf("a schema the per-table validation accepts was refused: %v", got)
			}
			if c.refused != "" && (got == nil || !strings.Contains(got.Error(), c.refused)) {
				t.Fatalf("want a refusal naming %q, got %v", c.refused, got)
			}
		})
	}
}
