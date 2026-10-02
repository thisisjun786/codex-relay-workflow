package swapgate_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// declaredParts splits what this build declares into the frozen v1 schema and the additive DAG zone.
func declaredParts(t *testing.T) (v1, zone record.Object) {
	t.Helper()
	declared := swapgate.DeclaredSchema(context.Background())
	tables := 0
	for _, field := range golden.Obj(record.Get(declared, "objects")) {
		if strings.Contains(field.Key, " dag_") {
			zone = append(zone, field)
			if strings.HasPrefix(field.Key, "table dag_") {
				tables++
			}
			continue
		}
		v1 = append(v1, field)
	}
	// the zone's tables (every CREATE TABLE of its statements: twelve in CRW-183, more as the scheduler's phases append them), their
	// indexes and the triggers that keep the log append-only
	want := 0
	for _, statement := range store.DAGZoneStatements() {
		if strings.HasPrefix(statement, "CREATE TABLE") {
			want++
		}
	}
	if tables != want || want < 12 || len(zone) < want+7 {
		t.Fatalf("the declared schema holds %d zone objects, %d of them tables", len(zone), tables)
	}
	return v1, zone
}

func join(parts ...record.Object) record.Object {
	var out record.Object
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func object(pairs ...string) record.Object {
	var out record.Object
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, record.Object{{Key: pairs[i], Value: pairs[i+1]}}...)
	}
	return out
}

func heldBy(objects record.Object) record.Object {
	return record.Object{{Key: "readable", Value: true}, {Key: "present", Value: true}, {Key: "objects", Value: objects}, {Key: "dbPath", Value: "/d"}}
}

func declaring(objects record.Object) record.Object {
	return record.Object{{Key: "readable", Value: true}, {Key: "objects", Value: objects}, {Key: "schemaVersion", Value: store.SchemaVersion}, {Key: "detail", Value: nil}}
}

func refuses(t *testing.T, cell record.Object) bool {
	t.Helper()
	blocking := swapgate.Blocking("storeSchema", cell)
	if blocking == nil {
		t.Fatalf("the cell could not answer: %s", golden.Canon(cell))
	}
	return *blocking
}

// The additive DAG zone (D-01) arrives with a build that declares it and goes with a build that does not, and both are
// answers of their own: the arrival refuses until the OPS-4.5 route is taken (swapgate.DecideWithRelease), the removal is
// not refused, since an older runtime opens a store that holds the zone and ignores it. Only the zone: any other schema
// object involved, or any object defined differently, keeps the answers it always had.
func TestTheDAGZoneArrivalAndRemovalHaveTheirOwnAnswers(t *testing.T) {
	v1, zone := declaredParts(t)
	whole := join(v1, zone)

	cases := []struct {
		name      string
		held      record.Object
		candidate record.Object
		answer    string
		refuses   bool
	}{
		{"the zone arriving on a store that predates it", v1, whole, swapgate.ExtendsZone, true},
		{"the zone leaving, for a candidate that does not declare it", whole, v1, swapgate.NarrowsZone, false},
		{"the same schema", whole, whole, swapgate.Agrees, false},
		{"part of the zone present, the rest arriving", join(v1, zone[:3]), whole, swapgate.ExtendsZone, true},
		{"part of the zone leaving", whole, join(v1, zone[:3]), swapgate.NarrowsZone, false},
		{"the zone arriving together with another object", v1, join(whole, object("table brand_new", "CREATE TABLE brand_new (x)")), swapgate.Extends, true},
		{"the zone leaving together with another object", join(whole, object("table only_here", "CREATE TABLE only_here (x)")), v1, swapgate.Narrows, true},
		{"a non-zone object leaving beside a zone that stays", join(whole, object("table only_here", "CREATE TABLE only_here (x)")), whole, swapgate.Narrows, true},
		{"a non-zone object arriving beside a zone that stays", whole, join(whole, object("table brand_new", "CREATE TABLE brand_new (x)")), swapgate.Extends, true},
		{"the zone arriving while another object leaves", join(v1, object("table only_here", "CREATE TABLE only_here (x)")), whole, swapgate.Narrows, true},
		{"a dag_ object this build does not know, arriving", whole, join(whole, object("table dag_future", "CREATE TABLE dag_future (x)")), swapgate.Extends, true},
		{"a dag_ object this build does not know, leaving", join(whole, object("table dag_future", "CREATE TABLE dag_future (x)")), whole, swapgate.Narrows, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := swapgate.SchemaCell(heldBy(tc.held), declaring(tc.candidate))
			if record.Get(cell, "answer") != tc.answer || refuses(t, cell) != tc.refuses {
				t.Fatalf("answer %v, refuses %v; want %s, %v\n%s", record.Get(cell, "answer"), refuses(t, cell), tc.answer, tc.refuses, golden.Canon(cell))
			}
		})
	}

	// a zone object defined differently is DIFFERS whatever else holds, and a v1 object too
	altered := make(record.Object, len(whole))
	copy(altered, whole)
	for i, field := range altered {
		if field.Key == "table dag_plans" {
			altered[i].Value = "CREATE TABLE dag_plans (plan_id TEXT PRIMARY KEY)"
		}
	}
	for name, tc := range map[string][2]record.Object{
		"a zone table defined differently":                    {altered, whole},
		"the zone arriving beside a zone table that differs":  {join(v1, object("table dag_plans", "CREATE TABLE dag_plans (plan_id TEXT PRIMARY KEY)")), whole},
		"a v1 table defined differently beside the zone":      {join(object("table sessions", "CREATE TABLE sessions (x)"), zone), join(object("table sessions", "CREATE TABLE sessions (y)"), zone)},
		"a changed zone table beside a zone object that left": {altered[:len(altered)-1], whole[:len(whole)-1]},
	} {
		cell := swapgate.SchemaCell(heldBy(tc[0]), declaring(tc[1]))
		if answer := record.Get(cell, "answer"); answer != swapgate.Differs && answer != swapgate.Narrows && answer != swapgate.Extends || !refuses(t, cell) {
			t.Errorf("%s: %s", name, golden.Canon(cell))
		}
	}
	if cell := swapgate.SchemaCell(heldBy(altered), declaring(whole)); record.Get(cell, "answer") != swapgate.Differs {
		t.Errorf("a zone table defined differently is DIFFERS: %s", golden.Canon(cell))
	}
}

// The same answers from the real thing: a store that predates the zone, then the same store after this build opened it.
func TestTheDAGZoneAgainstRealStores(t *testing.T) {
	ctx := context.Background()
	declared := swapgate.DeclaredSchema(ctx)
	v1, _ := declaredParts(t)

	state := filepath.Join(t.TempDir(), "state")
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	extends := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared)
	if record.Get(extends, "answer") != swapgate.ExtendsZone || !strings.Contains(golden.Canon(extends), "dag_plan_revisions") || !strings.Contains(golden.Canon(extends), "--backup-state-to") {
		t.Fatalf("a store without the zone against a build with it, and the route named: %s", golden.Canon(extends))
	}
	if !refuses(t, extends) {
		t.Fatal("the arrival is refused until the route is taken")
	}

	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	held := swapgate.StoreSchema(ctx, state, "")
	if got := swapgate.SchemaCell(held, declared); record.Get(got, "answer") != swapgate.Agrees {
		t.Fatalf("a store this build opened: %s", golden.Canon(got))
	}
	narrows := swapgate.SchemaCell(held, declaring(v1))
	if record.Get(narrows, "answer") != swapgate.NarrowsZone || refuses(t, narrows) || !strings.Contains(golden.Canon(narrows), "dag_plan_revisions") {
		t.Fatalf("a build without the zone against a store that has it: %s", golden.Canon(narrows))
	}
}

func stoppedAndQuiet() map[string]record.Object {
	return map[string]record.Object{
		"daemon":      swapgate.Cell(scope.Stopped, true, "no daemon", nil, false),
		"inFlight":    swapgate.Cell(swapgate.NoAttempts, true, "no attempt is open", nil, swapgate.NoAttempts),
		"storeSchema": swapgate.Cell(swapgate.ExtendsZone, true, "the zone arrives", nil, nil),
	}
}

// The verdict is computed once, over the cells. The arrival alone is released by a backup; every other cell still
// refuses, so a release can never carry a swap past a running daemon, an open attempt or a reading that failed.
func TestAReleaseCarriesOnlyTheZoneArrivalAlone(t *testing.T) {
	backup := record.Object{{Key: "made", Value: true}, {Key: "destination", Value: "/backup"}}
	release := &swapgate.Release{Backup: backup}

	cells := stoppedAndQuiet()
	if verdict := record.Get(swapgate.Decide(cells), "verdict"); verdict != swapgate.Blocked {
		t.Fatalf("without a release the arrival is blocked: %v", verdict)
	}
	gate := swapgate.DecideWithRelease(cells, release)
	if record.Get(gate, "verdict") != swapgate.Allowed || len(golden.List(record.Get(gate, "blockedBy"))) != 0 || golden.Canon(record.Get(gate, "stateBackup")) != golden.Canon(backup) {
		t.Fatalf("with a release the arrival alone is allowed, and the gate carries the backup: %s", golden.Canon(gate))
	}
	if !swapgate.ZoneArrivalOnly(cells) {
		t.Fatal("the arrival alone")
	}

	refusing := map[string]map[string]record.Object{
		"a running daemon":             {"daemon": swapgate.Cell(scope.Running, true, "a daemon runs", nil, true)},
		"an open attempt":              {"inFlight": swapgate.Cell(int64(3), true, "3 attempts are open", nil, int64(3))},
		"a daemon that cannot be read": {"daemon": swapgate.Cell("access_error", false, "could not be read", nil, nil)},
		"attempts that cannot be read": {"inFlight": swapgate.Cell("unreadable", false, "could not be read", nil, nil)},
		"another difference":           {"storeSchema": swapgate.Cell(swapgate.Extends, true, "another object arrives too", nil, nil)},
		"a changed object":             {"storeSchema": swapgate.Cell(swapgate.Differs, true, "an object differs", nil, nil)},
		"a store that narrows":         {"storeSchema": swapgate.Cell(swapgate.Narrows, true, "an object is not declared", nil, nil)},
	}
	for name, override := range refusing {
		cells := stoppedAndQuiet()
		for cell, value := range override {
			cells[cell] = value
		}
		if swapgate.ZoneArrivalOnly(cells) {
			t.Errorf("%s: the arrival is not alone", name)
		}
		if verdict := record.Get(swapgate.DecideWithRelease(cells, release), "verdict"); verdict == swapgate.Allowed {
			t.Errorf("%s: a release carried the swap", name)
		}
	}

	// and the removal of the zone is not an arrival: it is allowed with no release at all
	narrows := stoppedAndQuiet()
	narrows["storeSchema"] = swapgate.Cell(swapgate.NarrowsZone, true, "the zone leaves", nil, nil)
	if record.Get(swapgate.Decide(narrows), "verdict") != swapgate.Allowed || swapgate.ZoneArrivalOnly(narrows) {
		t.Fatalf("the zone leaving: %s", golden.Canon(swapgate.Decide(narrows)))
	}
	narrows["daemon"] = swapgate.Cell(scope.Running, true, "a daemon runs", nil, true)
	if record.Get(swapgate.Decide(narrows), "verdict") != swapgate.Blocked {
		t.Fatal("a running daemon still refuses the removal of the zone")
	}
}
