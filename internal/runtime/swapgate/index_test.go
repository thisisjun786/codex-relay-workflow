package swapgate_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-472: a difference that is only ordinary indexes on tables both readings declare has answers of its own, EXTENDS_INDEX
// (refused until the OPS-4.5 backup is taken, like the zone) and NARROWS_INDEX (not refused). "Ordinary" is non-unique and
// calls no SQL function, decided by SQLite on the statements and not by matching text.

// idxCatalog runs statements in a scratch in-memory database and returns its objects as a reading holds them: keyed by kind and
// name, with the statement SQLite keeps (so a quoted name, an IF NOT EXISTS or a qualifier arrives as the catalog spells it).
func idxCatalog(t *testing.T, statements ...string) record.Object {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	rows, err := db.Query(swapgate.SchemaObjectsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out record.Object
	for rows.Next() {
		var name string
		var statement sql.NullString
		if err := rows.Scan(&name, &statement); err != nil {
			t.Fatal(err)
		}
		var value any
		if statement.Valid {
			value = statement.String
		}
		out = append(out, record.Object{{Key: name, Value: value}}...)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

const (
	// idxTableA has a PRIMARY KEY and a UNIQUE constraint, so its database holds sqlite_autoindex entries that no reading carries
	// and that an index lookup by name must not count.
	idxTableA = "CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT, y TEXT, UNIQUE (x))"
	idxTableB = "CREATE TABLE b (k TEXT PRIMARY KEY, v TEXT)"
)

func TestAnIndexOnlyDifferenceHasAnswersOfItsOwn(t *testing.T) {
	with := func(extra ...string) record.Object {
		return idxCatalog(t, append([]string{idxTableA, idxTableB}, extra...)...)
	}
	cases := []struct {
		name      string
		held      record.Object
		candidate record.Object
		answer    string
		refuses   bool
	}{
		// the class
		{"an ordinary index arriving", with(), with("CREATE INDEX i ON a (y)"), swapgate.ExtendsIndex, true},
		{"a partial index arriving", with(), with("CREATE INDEX i ON a (y) WHERE x = 'z'"), swapgate.ExtendsIndex, true},
		{"an index arriving on a table that has an autoindex", with(), with("CREATE INDEX i ON b (v)"), swapgate.ExtendsIndex, true},
		{"two indexes arriving on two tables", with(), with("CREATE INDEX i ON a (y)", "CREATE INDEX j ON b (v)"), swapgate.ExtendsIndex, true},
		{"an ordinary index leaving", with("CREATE INDEX i ON a (y)"), with(), swapgate.NarrowsIndex, false},
		{"two ordinary indexes leaving", with("CREATE INDEX i ON a (y)", "CREATE INDEX j ON b (v)"), with(), swapgate.NarrowsIndex, false},
		{"an index arriving while another leaves", with("CREATE INDEX i ON a (y)"), with("CREATE INDEX j ON b (v)"), swapgate.ExtendsIndex, true},
		{"the same indexes", with("CREATE INDEX i ON a (y)"), with("CREATE INDEX i ON a (y)"), swapgate.Agrees, false},
		// everything else keeps the answer it had, with or without an ordinary index beside it
		{"a unique index arriving", with(), with("CREATE UNIQUE INDEX u ON a (y)"), swapgate.Extends, true},
		{"a unique index leaving", with("CREATE UNIQUE INDEX u ON a (y)"), with(), swapgate.Narrows, true},
		{"an ordinary and a unique index arriving", with(), with("CREATE INDEX i ON a (y)", "CREATE UNIQUE INDEX u ON b (v)"), swapgate.Extends, true},
		{"an ordinary index arriving while a unique one leaves", with("CREATE UNIQUE INDEX u ON a (y)"), with("CREATE INDEX i ON a (y)"), swapgate.Narrows, true},
		{"an index arriving with its new table", with(), with("CREATE TABLE c (z)", "CREATE INDEX i ON c (z)"), swapgate.Extends, true},
		{"a new table beside an ordinary index", with(), with("CREATE TABLE c (z)", "CREATE INDEX i ON a (y)"), swapgate.Extends, true},
		{"a new table alone", with(), with("CREATE TABLE c (z)"), swapgate.Extends, true},
		{"an ordinary index arriving while a table leaves", with("CREATE TABLE c (z)"), with("CREATE INDEX i ON a (y)"), swapgate.Narrows, true},
		{"a trigger beside an ordinary index", with(), with("CREATE INDEX i ON a (y)", "CREATE TRIGGER t AFTER INSERT ON a BEGIN SELECT 1; END"), swapgate.Extends, true},
		{"a view arriving", with(), with("CREATE VIEW w AS SELECT x FROM a"), swapgate.Extends, true},
		{"a column change alone", with(), idxCatalog(t, "CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT, y TEXT, z TEXT, UNIQUE (x))", idxTableB), swapgate.Differs, true},
		{"a column change with an index on the new column", with(), idxCatalog(t, "CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT, y TEXT, z TEXT, UNIQUE (x))", idxTableB, "CREATE INDEX i ON a (z)"), swapgate.Differs, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := swapgate.SchemaCell(heldBy(tc.held), declaring(tc.candidate))
			if record.Get(cell, "answer") != tc.answer || refuses(t, cell) != tc.refuses {
				t.Fatalf("answer %v, refuses %v; want %s, %v\n%s", record.Get(cell, "answer"), refuses(t, cell), tc.answer, tc.refuses, golden.Canon(cell))
			}
		})
	}

	// the arrival names what arrives, the way to take the backup, and says the difference is the indexes alone
	cell := swapgate.SchemaCell(heldBy(with()), declaring(with("CREATE INDEX i ON a (y)")))
	text := golden.Canon(cell)
	if !strings.Contains(text, "index i") || !strings.Contains(text, "--backup-state-to") || record.Get(golden.Obj(record.Get(cell, "evidence")), "indexOnly") != true {
		t.Fatalf("the arrival's cell: %s", text)
	}
	// the departure says why it is not refused
	text = golden.Canon(swapgate.SchemaCell(heldBy(with("CREATE INDEX i ON a (y)")), declaring(with())))
	if !strings.Contains(text, "index i") || !strings.Contains(text, "calls no SQL function") {
		t.Fatalf("the departure's cell: %s", text)
	}
}

// What "ordinary" means, on the text SQLite keeps in its catalog. A key that calls a SQL function, or a partial index whose
// WHERE does, can fail a build over existing rows, and a write an older runtime that does not know the index makes
// (json_extract over text that is not JSON), so it is not ordinary however non-unique it is.
func TestOnlyANonUniqueFunctionFreeIndexIsOrdinary(t *testing.T) {
	const table = "CREATE TABLE refusals (id INTEGER PRIMARY KEY, reason TEXT, verified TEXT, a TEXT, b INTEGER, code TEXT GENERATED ALWAYS AS (json_extract(reason, '$.code')) VIRTUAL)"
	for _, tc := range []struct {
		name, index string
		ordinary    bool
	}{
		{"a plain column", "CREATE INDEX i ON refusals (reason)", true},
		{"a descending and a collated key", "CREATE INDEX i ON refusals (a COLLATE NOCASE, b DESC)", true},
		{"arithmetic", "CREATE INDEX i ON refusals (b + 1)", true},
		{"a partial index comparing columns with literals", "CREATE INDEX i ON refusals (a) WHERE verified = 'unverified_turn' AND b > 3", true},
		{"IF NOT EXISTS", "CREATE INDEX IF NOT EXISTS i ON refusals (a)", true},
		{"a double-quoted name", `CREATE INDEX "i" ON refusals (a)`, true},
		{"a bracketed name", "CREATE INDEX [i] ON refusals (a)", true},
		{"a backtick name", "CREATE INDEX `i` ON refusals (a)", true},
		{"a schema-qualified name", "CREATE INDEX main.i ON refusals (a)", true},
		{"json_extract in the key", "CREATE INDEX i ON refusals (json_extract(reason, '$.code'))", false},
		{"json_valid in the WHERE", "CREATE INDEX i ON refusals (a) WHERE json_valid(reason)", false},
		{"->> in the key", "CREATE INDEX i ON refusals (reason ->> '$.x')", false},
		{"abs in the key", "CREATE INDEX i ON refusals (abs(b))", false},
		{"lower in the key", "CREATE INDEX i ON refusals (lower(a))", false},
		{"LIKE in the WHERE", "CREATE INDEX i ON refusals (a) WHERE a LIKE 'x%'", false},
		{"a virtual generated column that calls json_extract", "CREATE INDEX i ON refusals (code)", false},
		{"a unique index", "CREATE UNIQUE INDEX i ON refusals (a)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables, whole := idxCatalog(t, table), idxCatalog(t, table, tc.index)
			arrives, leaves := swapgate.SchemaCell(heldBy(tables), declaring(whole)), swapgate.SchemaCell(heldBy(whole), declaring(tables))
			wantArrival, wantDeparture := swapgate.Extends, swapgate.Narrows
			if tc.ordinary {
				wantArrival, wantDeparture = swapgate.ExtendsIndex, swapgate.NarrowsIndex
			}
			if record.Get(arrives, "answer") != wantArrival || !refuses(t, arrives) {
				t.Errorf("arriving: %s", golden.Canon(arrives))
			}
			if record.Get(leaves, "answer") != wantDeparture || refuses(t, leaves) == tc.ordinary {
				t.Errorf("leaving: %s", golden.Canon(leaves))
			}
		})
	}

	// statements no catalog would hold, which must not be executed or classified
	tables := idxCatalog(t, table)
	for name, statement := range map[string]any{
		"a statement that is not a CREATE INDEX":      "CREATE TABLE x (a)",
		"two statements":                              "CREATE INDEX i ON refusals (a); DROP TABLE refusals",
		"a semicolon inside a literal (conservative)": "CREATE INDEX i ON refusals (a) WHERE a = 'x;y'",
		"a NULL statement":                            nil,
		"a column the table does not have":            "CREATE INDEX i ON refusals (nope)",
		"a table neither side declares":               "CREATE INDEX i ON nowhere (a)",
		"text that is not SQL":                        "this is not sql",
	} {
		candidate := join(tables, record.Object{{Key: "index i", Value: statement}})
		if cell := swapgate.SchemaCell(heldBy(tables), declaring(candidate)); record.Get(cell, "answer") != swapgate.Extends || !refuses(t, cell) {
			t.Errorf("%s: %s", name, golden.Canon(cell))
		}
	}
}

// The zone and the indexes are told apart by what they are, and the zone is asked first: a difference that is only zone
// objects keeps its answers, and the zone beside an index is neither class (the issue keeps a new table mixed with an index
// arrival as it was).
func TestTheZoneBesideAnIndexIsNeitherClass(t *testing.T) {
	v1, zone := declaredParts(t)
	whole := join(v1, zone)
	index := object("index crw472_attempts_observed", "CREATE INDEX crw472_attempts_observed ON attempts (observed_at)")
	for _, tc := range []struct {
		name            string
		held, candidate record.Object
		answer          string
		refuses         bool
	}{
		{"an index on a frozen table arriving where the zone is held", whole, join(whole, index), swapgate.ExtendsIndex, true},
		{"the same index leaving where the zone is held", join(whole, index), whole, swapgate.NarrowsIndex, false},
		{"the zone arriving alone", v1, whole, swapgate.ExtendsZone, true},
		{"the zone leaving alone", whole, v1, swapgate.NarrowsZone, false},
		{"the zone and an index arriving together", v1, join(whole, index), swapgate.Extends, true},
		{"the zone and an index leaving together", join(whole, index), v1, swapgate.Narrows, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell := swapgate.SchemaCell(heldBy(tc.held), declaring(tc.candidate))
			if record.Get(cell, "answer") != tc.answer || refuses(t, cell) != tc.refuses {
				t.Fatalf("answer %v, refuses %v; want %s, %v", record.Get(cell, "answer"), refuses(t, cell), tc.answer, tc.refuses)
			}
		})
	}
}

// The release rule: the backup carries an index arrival as it carries the zone, and carries nothing else; the index
// departure needs no release because it is not refused.
func TestAReleaseCarriesAnIndexArrivalAndNothingElse(t *testing.T) {
	backup := record.Object{{Key: "made", Value: true}, {Key: "destination", Value: "/backup"}}
	release := &swapgate.Release{Backup: backup}
	arriving := func() map[string]record.Object {
		cells := stoppedAndQuiet()
		cells["storeSchema"] = swapgate.Cell(swapgate.ExtendsIndex, true, "indexes arrive", nil, nil)
		return cells
	}

	cells := arriving()
	if verdict := record.Get(swapgate.Decide(cells), "verdict"); verdict != swapgate.Blocked {
		t.Fatalf("without a release the arrival is blocked: %v", verdict)
	}
	gate := swapgate.DecideWithRelease(cells, release)
	if record.Get(gate, "verdict") != swapgate.Allowed || len(golden.List(record.Get(gate, "blockedBy"))) != 0 || golden.Canon(record.Get(gate, "stateBackup")) != golden.Canon(backup) {
		t.Fatalf("with a release the index arrival alone is allowed, and the gate carries the backup: %s", golden.Canon(gate))
	}
	if !swapgate.AdditiveArrivalOnly(cells) || swapgate.ZoneArrivalOnly(cells) {
		t.Fatal("an index arrival is an additive arrival and is not the zone")
	}
	if zone := stoppedAndQuiet(); !swapgate.AdditiveArrivalOnly(zone) || !swapgate.ZoneArrivalOnly(zone) {
		t.Fatal("the zone arrival is both")
	}

	for name, override := range map[string]map[string]record.Object{
		"a running daemon":             {"daemon": swapgate.Cell(scope.Running, true, "a daemon runs", nil, true)},
		"an open attempt":              {"inFlight": swapgate.Cell(int64(3), true, "3 attempts are open", nil, int64(3))},
		"a daemon that cannot be read": {"daemon": swapgate.Cell("access_error", false, "could not be read", nil, nil)},
		"attempts that cannot be read": {"inFlight": swapgate.Cell("unreadable", false, "could not be read", nil, nil)},
		"another difference":           {"storeSchema": swapgate.Cell(swapgate.Extends, true, "another object arrives too", nil, nil)},
		"a changed object":             {"storeSchema": swapgate.Cell(swapgate.Differs, true, "an object differs", nil, nil)},
		"a store that narrows":         {"storeSchema": swapgate.Cell(swapgate.Narrows, true, "an object is not declared", nil, nil)},
	} {
		cells := arriving()
		for cell, value := range override {
			cells[cell] = value
		}
		if swapgate.AdditiveArrivalOnly(cells) {
			t.Errorf("%s: the arrival is not alone", name)
		}
		if verdict := record.Get(swapgate.DecideWithRelease(cells, release), "verdict"); verdict == swapgate.Allowed {
			t.Errorf("%s: a release carried the swap", name)
		}
	}

	departure := stoppedAndQuiet()
	departure["storeSchema"] = swapgate.Cell(swapgate.NarrowsIndex, true, "indexes leave", nil, nil)
	if record.Get(swapgate.Decide(departure), "verdict") != swapgate.Allowed || swapgate.AdditiveArrivalOnly(departure) {
		t.Fatalf("the indexes leaving: %s", golden.Canon(swapgate.Decide(departure)))
	}
	departure["daemon"] = swapgate.Cell(scope.Running, true, "a daemon runs", nil, true)
	if record.Get(swapgate.Decide(departure), "verdict") != swapgate.Blocked {
		t.Fatal("a running daemon still refuses the departure")
	}
}

// The same answers from real stores: a store this build opened (the zone is in it), a candidate that declares one more
// ordinary index than the schema this build installs, and a store that holds indexes the candidate does not declare.
func TestAnIndexClassAgainstRealStores(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(state, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	s, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	declared := swapgate.DeclaredSchema(ctx)
	if got := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared); record.Get(got, "answer") != swapgate.Agrees {
		t.Fatalf("a store this build opened: %s", golden.Canon(got))
	}

	const name = "crw472_attempts_observed"
	arriving := declaring(join(golden.Obj(record.Get(declared, "objects")), object("index "+name, "CREATE INDEX "+name+" ON attempts (observed_at)")))
	cell := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), arriving)
	if record.Get(cell, "answer") != swapgate.ExtendsIndex || !refuses(t, cell) || !strings.Contains(golden.Canon(cell), "--backup-state-to") || !strings.Contains(golden.Canon(cell), "index "+name) {
		t.Fatalf("an ordinary index the store lacks: %s", golden.Canon(cell))
	}

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	exec := func(statement string) {
		t.Helper()
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec("CREATE INDEX crw472_extra ON attempts (observed_at)")
	cell = swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared)
	if record.Get(cell, "answer") != swapgate.NarrowsIndex || refuses(t, cell) || !strings.Contains(golden.Canon(cell), "index crw472_extra") {
		t.Fatalf("an ordinary index the candidate does not declare: %s", golden.Canon(cell))
	}
	// an index that calls a function is refused to leave, because an older runtime's own writes can fail on it
	exec("CREATE INDEX crw472_function ON refusals (json_extract(reason, '$.code'))")
	if cell = swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared); record.Get(cell, "answer") != swapgate.Narrows || !refuses(t, cell) {
		t.Fatalf("an index that calls a function: %s", golden.Canon(cell))
	}
	exec("DROP INDEX crw472_function")
	exec("CREATE UNIQUE INDEX crw472_unique ON attempts (request_id, observed_at)")
	if cell = swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared); record.Get(cell, "answer") != swapgate.Narrows || !refuses(t, cell) {
		t.Fatalf("a unique index the candidate does not declare: %s", golden.Canon(cell))
	}
}

var idxUnique = regexp.MustCompile(`(?i)^\s*create\s+unique\s+index`)

// The ratchet. Every non-unique index of the schema this build installs is ordinary, so one that is added later (and a
// future build that carries it) is told by this test and not by an install that refuses. The shipped schema holds one
// exception, journal_managed_creation, whose partial WHERE guards json_extract with json_valid inside a CASE: it is not
// classified ordinary (the function rule is not weakened to fit it) and the list below is exactly it.
func TestEveryNonUniqueIndexOfTheShippedSchemaIsOrdinaryButOne(t *testing.T) {
	v1, _ := declaredParts(t)
	all := golden.Obj(record.Get(swapgate.DeclaredSchema(context.Background()), "objects"))
	checked := 0
	var exceptions []string
	for _, field := range v1 {
		statement, _ := field.Value.(string)
		if !strings.HasPrefix(field.Key, "index ") || idxUnique.MatchString(statement) {
			continue
		}
		var rest record.Object
		for _, other := range all {
			if other.Key != field.Key {
				rest = append(rest, record.Object{other}...)
			}
		}
		switch answer := record.Get(swapgate.SchemaCell(heldBy(rest), declaring(all)), "answer"); answer {
		case swapgate.ExtendsIndex:
			checked++
		case swapgate.Extends:
			exceptions = append(exceptions, field.Key)
		default:
			t.Errorf("%s reads %v", field.Key, answer)
		}
	}
	if checked == 0 || strings.Join(exceptions, ",") != "index journal_managed_creation" {
		t.Fatalf("%d ordinary indexes; exceptions %v", checked, exceptions)
	}
}
