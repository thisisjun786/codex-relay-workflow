package swapgate_test

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// zoneNames indexes a declared zone by object name, the key SchemaCell compares with.
func zoneNames(zone record.Object) map[string]bool {
	out := map[string]bool{}
	for _, field := range zone {
		out[field.Key] = true
	}
	return out
}

// CRW-728: the two proof tables the DAG zone appends (dag_acceptance_refreshes, dag_verified_heads)
// are zone objects like every other appended statement: their arrival is EXTENDS_ZONE, released only
// by the OPS-4.5 route (--backup-state-to, which copies the whole state directory first), and their
// removal is NARROWS_ZONE, which an older runtime is not refused for. Nothing else about the gate
// changes, so this test derives the objects from the shipped statements rather than naming them by
// hand: an object this build does not declare would not be the zone's.
func TestTheProofTablesAreZoneObjects(t *testing.T) {
	v1, zone := declaredParts(t)
	whole := join(v1, zone)

	// the two tables this change appends, and their triggers, are declared objects of the zone
	for _, key := range []string{"table dag_acceptance_refreshes", "trigger dag_acceptance_refreshes_no_update", "trigger dag_acceptance_refreshes_no_delete", "table dag_verified_heads", "trigger dag_verified_heads_no_update", "trigger dag_verified_heads_no_delete"} {
		if _, ok := zoneNames(zone)[key]; !ok {
			t.Fatalf("the declared zone does not hold %s", key)
		}
	}
	// a store that predates them: the arrival is the zone's own answer and it refuses until the route is taken
	before := make(record.Object, 0, len(whole))
	for _, field := range whole {
		if !strings.Contains(field.Key, "dag_acceptance_refreshes") && !strings.Contains(field.Key, "dag_verified_heads") {
			before = append(before, field)
		}
	}
	arrival := swapgate.SchemaCell(heldBy(before), declaring(whole))
	if record.Get(arrival, "answer") != swapgate.ExtendsZone || !refuses(t, arrival) || !strings.Contains(golden.Canon(arrival), "--backup-state-to") {
		t.Fatalf("the two proof tables arriving: %s", golden.Canon(arrival))
	}
	// the removal of the same objects is the zone's own answer and is not refused
	removal := swapgate.SchemaCell(heldBy(whole), declaring(before))
	if record.Get(removal, "answer") != swapgate.NarrowsZone || refuses(t, removal) {
		t.Fatalf("the two proof tables leaving: %s", golden.Canon(removal))
	}
	// the route: the arrival alone is released by a made backup, and nothing else is
	cells := stoppedAndQuiet()
	cells["storeSchema"] = swapgate.Cell(swapgate.ExtendsZone, true, "the proof tables arrive", nil, nil)
	if verdict := record.Get(swapgate.Decide(cells), "verdict"); verdict != swapgate.Blocked {
		t.Fatalf("without a release the arrival is blocked: %v", verdict)
	}
	backup := record.Object{{Key: "made", Value: true}, {Key: "destination", Value: "/backup"}}
	gate := swapgate.DecideWithRelease(cells, &swapgate.Release{Backup: backup})
	if record.Get(gate, "verdict") != swapgate.Allowed || len(golden.List(record.Get(gate, "blockedBy"))) != 0 {
		t.Fatalf("with a release the arrival alone is allowed: %s", golden.Canon(gate))
	}
	if !swapgate.ZoneArrivalOnly(cells) {
		t.Fatal("the proof tables' arrival alone")
	}
}
