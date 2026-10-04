package swapgate_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// historyIndexKeys are the swap gate's keys ("index <name>") of the indexes CRW-301 added to the frozen v1 schema,
// as the golden delta of the contract lists them (contract/schema/relay-sqlite-history-indexes.json).
func historyIndexKeys(t *testing.T) []string {
	t.Helper()
	indexes, err := testsupport.HistoryIndexes()
	if err != nil || len(indexes) == 0 {
		t.Fatalf("the golden delta: %d indexes: %v", len(indexes), err)
	}
	keys := make([]string, len(indexes))
	for i, index := range indexes {
		keys[i] = "index " + index.Name
	}
	return keys
}

// without is objects minus the named keys.
func without(objects record.Object, keys []string) record.Object {
	drop := map[string]bool{}
	for _, key := range keys {
		drop[key] = true
	}
	var out record.Object
	for _, field := range objects {
		if !drop[field.Key] {
			out = append(out, field)
		}
	}
	return out
}

// CRW-301: the history indexes are objects of the frozen v1 schema script and not of the additive DAG zone (D-01),
// and each is an ordinary index (non-unique, no SQL function) on a table both readings declare, so the gate reads them as
// EXTENDS_INDEX arriving and NARROWS_INDEX leaving (pull request #397). An arrival is refused until the OPS-4.5 backup is
// taken and is released by it, which is the additive route and not the zone's; a departure is not refused.
func TestTheHistoryIndexesArriveAsAnIndexExtendsAndLeaveAsAnIndexNarrows(t *testing.T) {
	keys := historyIndexKeys(t)
	whole := golden.Obj(record.Get(swapgate.DeclaredSchema(context.Background()), "objects"))
	previous := without(whole, keys)
	if len(previous) != len(whole)-len(keys) {
		t.Fatalf("this build declares %d of the %d history indexes", len(whole)-len(previous), len(keys))
	}

	arrive := swapgate.SchemaCell(heldBy(previous), declaring(whole))
	if record.Get(arrive, "answer") != swapgate.ExtendsIndex || !refuses(t, arrive) {
		t.Fatalf("the indexes arriving: %s", golden.Canon(arrive))
	}
	for _, key := range keys {
		if !strings.Contains(golden.Canon(arrive), key) {
			t.Errorf("the arrival does not name %s", key)
		}
	}
	leave := swapgate.SchemaCell(heldBy(whole), declaring(previous))
	if record.Get(leave, "answer") != swapgate.NarrowsIndex || refuses(t, leave) {
		t.Fatalf("the indexes leaving: %s", golden.Canon(leave))
	}

	// With the daemon stopped and nothing in flight, a refused arrival is released by the backup, through the additive route
	// and not the zone's.
	cells := stoppedAndQuiet()
	cells["storeSchema"] = arrive
	if swapgate.ZoneArrivalOnly(cells) || !swapgate.AdditiveArrivalOnly(cells) {
		t.Fatalf("the indexes arriving are the additive arrival and not the zone's: zone %v, additive %v", swapgate.ZoneArrivalOnly(cells), swapgate.AdditiveArrivalOnly(cells))
	}
	if verdict := record.Get(swapgate.Decide(cells), "verdict"); verdict != swapgate.Blocked {
		t.Fatalf("without a backup the indexes arriving are not refused: %v", verdict)
	}
	release := &swapgate.Release{Backup: record.Object{{Key: "made", Value: true}}}
	if verdict := record.Get(swapgate.DecideWithRelease(cells, release), "verdict"); verdict != swapgate.Allowed {
		t.Fatalf("a backup does not carry the indexes: %v", verdict)
	}
}

// The same reading from real stores. A store of the version before CRW-301 that also lacks the zone meets a build that
// brings the zone and the indexes together: a plain EXTENDS, because the zone beside an index is neither class (#397), so
// the additive route does not carry it and a backup does not release it. Once this build has opened that store the two
// agree. A store that holds the zone and lacks the indexes reads as the indexes alone: EXTENDS_INDEX, released by the backup.
func TestTheHistoryIndexesAgainstRealStores(t *testing.T) {
	ctx := context.Background()
	keys := historyIndexKeys(t)
	declared := swapgate.DeclaredSchema(ctx)
	release := &swapgate.Release{Backup: record.Object{{Key: "made", Value: true}}}

	state := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(state, "relay.sqlite3")
	testsupport.CreatePreviousVersion(t, path, "", "go")
	together := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared)
	if record.Get(together, "answer") != swapgate.Extends || !refuses(t, together) || !strings.Contains(golden.Canon(together), "dag_plan_revisions") {
		t.Fatalf("a store without the zone and without the indexes, which reach it together: %s", golden.Canon(together))
	}
	for _, key := range keys {
		if !strings.Contains(golden.Canon(together), key) {
			t.Errorf("the arrival does not name %s", key)
		}
	}
	cells := stoppedAndQuiet()
	cells["storeSchema"] = together
	if swapgate.AdditiveArrivalOnly(cells) {
		t.Fatal("the zone arriving with the indexes is not an additive arrival")
	}
	if verdict := record.Get(swapgate.DecideWithRelease(cells, release), "verdict"); verdict != swapgate.Blocked {
		t.Fatalf("a backup carried the zone and the indexes together: %v", verdict)
	}

	s, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared); record.Get(got, "answer") != swapgate.Agrees {
		t.Fatalf("a store this build opened: %s", golden.Canon(got))
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, key := range keys {
		if _, err := raw.Exec("DROP INDEX " + strings.TrimPrefix(key, "index ")); err != nil {
			t.Fatal(err)
		}
	}
	alone := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared)
	if record.Get(alone, "answer") != swapgate.ExtendsIndex || !refuses(t, alone) || strings.Contains(golden.Canon(alone), "dag_plan") {
		t.Fatalf("a store that holds the zone and lacks the indexes: %s", golden.Canon(alone))
	}
	for _, key := range keys {
		if !strings.Contains(golden.Canon(alone), key) {
			t.Errorf("the arrival does not name %s", key)
		}
	}
	cells["storeSchema"] = alone
	if !swapgate.AdditiveArrivalOnly(cells) {
		t.Fatal("the indexes alone are an additive arrival")
	}
	if verdict := record.Get(swapgate.DecideWithRelease(cells, release), "verdict"); verdict != swapgate.Allowed {
		t.Fatalf("a backup does not carry the indexes alone: %v", verdict)
	}
}
