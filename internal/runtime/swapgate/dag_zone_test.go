package swapgate_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The DAG zone is declared by this build, so the gate treats it like every other schema change (OPS-4.5): the first
// install of this build onto a store that predates the zone EXTENDS it, and a candidate that predates the zone
// NARROWS a store this build has opened. Both are refusals and both name the zone's objects.
func TestTheDAGZoneIsDeclaredAndTheGateRefusesItsArrivalAndItsRemoval(t *testing.T) {
	ctx := context.Background()
	declared := swapgate.DeclaredSchema(ctx)
	var withoutZone record.Object
	zone, tables := 0, 0
	for _, field := range golden.Obj(record.Get(declared, "objects")) {
		if strings.Contains(field.Key, " dag_") {
			zone++
			if strings.HasPrefix(field.Key, "table dag_") {
				tables++
			}
			continue
		}
		withoutZone = append(withoutZone, field)
	}
	// twelve tables, their indexes and the triggers that keep the log append-only
	if tables != 12 || zone < 12+7 {
		t.Fatalf("the declared schema holds %d zone objects, %d of them tables", zone, tables)
	}

	// a store that predates the zone, against this build
	state := filepath.Join(t.TempDir(), "state")
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	extends := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared)
	if record.Get(extends, "answer") != swapgate.Extends || !strings.Contains(scopeText(extends), "dag_plan_revisions") {
		t.Fatalf("a store without the zone against a build with it: %s", golden.Canon(extends))
	}
	if blocking := swapgate.Blocking("storeSchema", extends); blocking == nil || !*blocking {
		t.Fatal("the gate does not refuse the arrival of the zone")
	}

	// the same store after this build has opened it
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
	candidate := record.Object{{Key: "readable", Value: true}, {Key: "objects", Value: withoutZone}, {Key: "schemaVersion", Value: store.SchemaVersion}, {Key: "detail", Value: nil}}
	narrows := swapgate.SchemaCell(held, candidate)
	if record.Get(narrows, "answer") != swapgate.Narrows || !strings.Contains(scopeText(narrows), "dag_plan_revisions") {
		t.Fatalf("a build without the zone against a store that has it: %s", golden.Canon(narrows))
	}
}
