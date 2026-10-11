package swapgate_test

import (
	"context"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// declaringZone is a candidate that declares objects and names which of them are its additive zone, as
// `crw doctor declared-schema --json` answers.
func declaringZone(objects record.Object, zone []string) record.Object {
	listed := make([]any, len(zone))
	for i, key := range zone {
		listed[i] = key
	}
	return record.Set(declaring(objects), "zone", listed)
}

func keysOf(objects record.Object) []string {
	var out []string
	for _, field := range objects {
		out = append(out, field.Key)
	}
	return out
}

// The candidate names its own zone (CRW-1267). The same store and the same candidate give the same answer whichever
// build asks: a crw older than the candidate does not know the zone objects the candidate appends, and before the
// candidate named them it read their arrival as a plain EXTENDS that --backup-state-to does not release (r2 S10 L4b:
// `crw install rollback <newer dir>` run by the older current/bin/crw), while the candidate's own crw read EXTENDS_ZONE.
func TestTheCandidatesDeclaredZoneDecidesWhatIsTheZoneWhicheverBuildAsks(t *testing.T) {
	v1, zone := declaredParts(t)
	whole := join(v1, zone)
	later := object("table later_zone_step", "CREATE TABLE IF NOT EXISTS later_zone_step (id INTEGER PRIMARY KEY)",
		"trigger later_zone_step_no_delete", "CREATE TRIGGER IF NOT EXISTS later_zone_step_no_delete BEFORE DELETE ON later_zone_step BEGIN SELECT RAISE(ABORT, 'x'); END")
	newer := join(whole, later)
	newerZone := append(keysOf(zone), keysOf(later)...)

	cases := []struct {
		name      string
		held      record.Object
		candidate record.Object
		answer    string
		refuses   bool
	}{
		{"zone objects this build does not know, named by the candidate as its zone, arriving", whole, declaringZone(newer, newerZone), swapgate.ExtendsZone, true},
		{"the whole newer zone arriving on a store that predates the zone", v1, declaringZone(newer, newerZone), swapgate.ExtendsZone, true},
		{"the same objects from a candidate that does not name its zone", whole, declaring(newer), swapgate.Extends, true},
		{"the same objects from a candidate whose zone leaves one of them out", whole, declaringZone(newer, append(keysOf(zone), "table later_zone_step")), swapgate.Extends, true},
		{"a named zone does not carry an object the candidate does not name as zone", whole, declaringZone(join(newer, object("table brand_new", "CREATE TABLE brand_new (x)")), newerZone), swapgate.Extends, true},
		{"the newer zone leaving for a candidate that names this build's zone", newer, declaringZone(whole, keysOf(zone)), swapgate.Narrows, true},
		{"this build's zone leaving still reads NARROWS_ZONE", whole, declaringZone(v1, nil), swapgate.NarrowsZone, false},
		{"the same schema", newer, declaringZone(newer, newerZone), swapgate.Agrees, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := swapgate.SchemaCell(heldBy(tc.held), tc.candidate)
			if record.Get(cell, "answer") != tc.answer || refuses(t, cell) != tc.refuses {
				t.Fatalf("answer %v, refuses %v; want %s, %v\n%s", record.Get(cell, "answer"), refuses(t, cell), tc.answer, tc.refuses, golden.Canon(cell))
			}
		})
	}

	// the arrival the candidate names is released by the OPS-4.5 backup exactly as this build's zone is
	cells := map[string]record.Object{
		"daemon":      swapgate.Cell("STOPPED", true, "stopped", nil, false),
		"inFlight":    swapgate.Cell(swapgate.NoAttempts, true, "none", nil, swapgate.NoAttempts),
		"storeSchema": swapgate.SchemaCell(heldBy(whole), declaringZone(newer, newerZone)),
	}
	if !swapgate.AdditiveArrivalOnly(cells) {
		t.Fatalf("the named zone arriving alone is an additive arrival: %s", golden.Canon(cells["storeSchema"]))
	}
	if verdict := swapgate.DecideWithRelease(cells, &swapgate.Release{Backup: record.Object{{Key: "made", Value: true}}}); record.Get(verdict, "verdict") != swapgate.Allowed {
		t.Fatalf("released: %s", golden.Canon(verdict))
	}
}

// A zone the candidate names in a form that is not a list of object keys is not read as its zone: the cell does not
// answer, which keeps the installation exactly as a refusal does.
func TestAZoneTheCandidateNamesUnreadablyIsNotGuessed(t *testing.T) {
	v1, zone := declaredParts(t)
	for name, value := range map[string]any{
		"a string":            "table dag_plans",
		"a list with a value": []any{"table dag_plans", int64(1)},
		"an object":           record.Object{{Key: "table dag_plans", Value: true}},
	} {
		t.Run(name, func(t *testing.T) {
			cell := swapgate.SchemaCell(heldBy(v1), record.Set(declaring(join(v1, zone)), "zone", value))
			if record.Get(cell, "readable") != false || swapgate.Blocking("storeSchema", cell) != nil {
				t.Fatalf("%s", golden.Canon(cell))
			}
		})
	}
	// null is a candidate that names no zone, as one built before the zone was named: this build's zone decides
	cell := swapgate.SchemaCell(heldBy(v1), record.Set(declaring(join(v1, zone)), "zone", nil))
	if record.Get(cell, "answer") != swapgate.ExtendsZone {
		t.Fatalf("%s", golden.Canon(cell))
	}
}

// What this build declares names its zone: every object its zone statements create, sorted, and each of them among the
// objects it declares.
func TestTheDeclaredSchemaNamesItsZone(t *testing.T) {
	declared := swapgate.DeclaredSchema(context.Background())
	listed, ok := record.Get(declared, "zone").([]any)
	if !ok {
		t.Fatalf("no zone list: %s", golden.Canon(declared))
	}
	var got []string
	for _, v := range listed {
		got = append(got, v.(string))
	}
	want := keysOf(func() record.Object { _, zone := declaredParts(t); return zone }())
	sort.Strings(want)
	if !sort.StringsAreSorted(got) || len(got) != len(want) {
		t.Fatalf("zone %v, want %v", got, want)
	}
	objects := map[string]bool{}
	for _, key := range keysOf(golden.Obj(record.Get(declared, "objects"))) {
		objects[key] = true
	}
	for i := range want {
		if got[i] != want[i] || !objects[got[i]] {
			t.Fatalf("zone %v, want %v", got, want)
		}
	}
}

// A zone name the candidate gives never authorizes a departure, and counts for an arrival only when it names an object
// the candidate declares (CRW-1267 d1). A candidate that answers its own objects without a non-zone table the store
// holds and names that table as its zone reads NARROWS, the departure no runtime can read, refused with or without the
// OPS-4.5 backup, exactly as when it names no zone; objects leave as the zone's only by this build's own zone.
func TestACandidateZoneNameNeverReleasesADeparture(t *testing.T) {
	v1, zone := declaredParts(t)
	whole := join(v1, zone)
	legacy := object("table legacy_data", "CREATE TABLE legacy_data (id INTEGER PRIMARY KEY, body TEXT)")
	later := object("table later_zone_step", "CREATE TABLE IF NOT EXISTS later_zone_step (id INTEGER PRIMARY KEY)")

	cases := []struct {
		name      string
		held      record.Object
		candidate record.Object
		answer    string
	}{
		{"a non-zone table leaving, named by the candidate as its zone", join(whole, legacy), declaringZone(whole, append(keysOf(zone), "table legacy_data")), swapgate.Narrows},
		{"a non-zone table leaving, the candidate naming only it", join(whole, legacy), declaringZone(whole, []string{"table legacy_data"}), swapgate.Narrows},
		{"a newer zone object leaving, named by the candidate that does not declare it", join(whole, later), declaringZone(whole, append(keysOf(zone), "table later_zone_step")), swapgate.Narrows},
		{"a name the candidate does not declare carries no arrival", whole, declaringZone(join(whole, later), []string{"table later_zone_step_other"}), swapgate.Extends},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := swapgate.SchemaCell(heldBy(tc.held), tc.candidate)
			if record.Get(cell, "answer") != tc.answer || !refuses(t, cell) {
				t.Fatalf("answer %v, refuses %v; want %s, refused\n%s", record.Get(cell, "answer"), refuses(t, cell), tc.answer, golden.Canon(cell))
			}
			cells := map[string]record.Object{
				"daemon":      swapgate.Cell("STOPPED", true, "stopped", nil, false),
				"inFlight":    swapgate.Cell(swapgate.NoAttempts, true, "none", nil, swapgate.NoAttempts),
				"storeSchema": cell,
			}
			for _, release := range []*swapgate.Release{nil, {Backup: record.Object{{Key: "made", Value: true}}}} {
				if verdict := swapgate.DecideWithRelease(cells, release); record.Get(verdict, "verdict") == swapgate.Allowed {
					t.Fatalf("allowed with release %v: %s", release, golden.Canon(verdict))
				}
			}
		})
	}
}
