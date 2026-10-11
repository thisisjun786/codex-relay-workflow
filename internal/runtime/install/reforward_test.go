package install_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// laterZone is what a build newer than this one appends to the zone ledger: objects this build's zone statements do not
// create, so this build (the crw that runs the commands below) does not know them as the zone's.
var laterZone = record.Object{
	{Key: "table later_zone_step", Value: "CREATE TABLE IF NOT EXISTS later_zone_step (id INTEGER PRIMARY KEY)"},
	{Key: "index later_zone_step_id", Value: "CREATE INDEX IF NOT EXISTS later_zone_step_id ON later_zone_step (id)"},
}

// newerUnder is the declared schema of the runtime in newer (this build's schema, its zone and laterZone, naming all of
// that zone as its own, as `crw doctor declared-schema --json` answers), and this build's own answer for any other.
func newerUnder(newer string) func(context.Context, string) record.Object {
	return func(ctx context.Context, binary string) record.Object {
		declared := swapgate.DeclaredSchema(ctx)
		if !strings.HasPrefix(binary, newer+string(filepath.Separator)) {
			return declared
		}
		objects := append(append(record.Object{}, golden.Obj(record.Get(declared, "objects"))...), laterZone...)
		zone := append([]any{}, golden.List(record.Get(declared, "zone"))...)
		for _, field := range laterZone {
			zone = append(zone, field.Key)
		}
		return record.Set(record.Set(declared, "objects", objects), "zone", zone)
	}
}

// seededStore is the store the older runtime made and wrote: this build's schema with its zone, one relationship and one
// journal row (r2 S10 L1: relationships 1, journal 1).
func seededStore(t *testing.T, h *host) {
	t.Helper()
	path := filepath.Join(h.relayState, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	ctx := context.Background()
	s, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	relationship := store.Relationship{ID: "rel-1267", IssueKey: "CRW-1267", Status: store.StatusActive, ParentTaskID: "parent-1267", ChildTaskID: "child-1267", Generation: 1,
		ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t0", UpdatedAt: "t0"}
	generation := store.Generation{RelationshipID: "rel-1267", Number: 1, DispatchRequestID: "dispatch-1267", AnchorState: store.AnchorBound,
		DispatchTurnID: sql.NullString{String: "turn-1267", Valid: true}, OpenedAt: "t0", BoundAt: sql.NullString{String: "t0", Valid: true}}
	if err := storeseed.RecordRelationship(ctx, s, relationship, generation, "host-a", "host-a"); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.AppendJournal(ctx, s, store.JournalEntry{At: "t1", Kind: "probe", Subject: "rel-1267", Detail: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// storeCounts reads the relationships and the journal rows without opening the store for writing.
func storeCounts(t *testing.T, h *host) (relationships, journal int) {
	t.Helper()
	db, err := store.OpenInPlace(context.Background(), filepath.Join(h.relayState, "relay.sqlite3"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(context.Background(), "SELECT (SELECT count(*) FROM relationships), (SELECT count(*) FROM journal)").Scan(&relationships, &journal); err != nil {
		t.Fatal(err)
	}
	return relationships, journal
}

// A host that rolled back from a newer runtime returns to that same runtime with one command, `crw install rollback
// <newer dir> --backup-state-to DIR`, judged by the same criterion the update that first brought it in was (CRW-1267,
// r2 S10 L1 to L4b). The crw that runs every command here is older than the newer runtime: it does not know the zone
// objects the newer runtime appends, and the newer runtime names them as its zone itself. The store is not migrated by
// any of it (the zone arrives at the newer runtime's first write-open), and keeps its contents.
func TestARolledBackHostReturnsToTheSameNewerRuntimeWithOneCommand(t *testing.T) {
	// sequential: zoneInstalled hosts run the archive's binary and the state backup copies the state directory.
	h, _, second, old, next := zoneInstalled(t)
	seededStore(t, h)
	before := withoutSidecars(digestTree(t, h.relayState))
	o := h.options()
	o.CandidateSchema = newerUnder(next)

	// L2: the update to the newer runtime, the zone arriving, released by the backup
	o.StateBackup = backupOf(h, "b2")
	updated, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(updated, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone || at(updated, "swapGate", "stateBackup", "made") != true || h.pointerTarget(t) != next {
		t.Fatalf("update: exit %d, pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(at(updated, "swapGate")))
	}
	sameTree(t, before, withoutSidecars(digestTree(t, h.relayState)), "the state directory after the update")

	// L3: the rollback to the older runtime
	o.StateBackup = ""
	back, code := install.Rollback(context.Background(), o, "")
	if code != install.OK || h.pointerTarget(t) != old {
		t.Fatalf("rollback: exit %d, pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(back))
	}

	// L4: the same update again keeps the finished runtime directory and names the way back to it, run with the newer
	// runtime's own binary: the installed current/bin/crw is now the older runtime, which need not read the zone the
	// newer runtime names (CRW-1267 d2)
	again, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	repair, _ := at(again, "repair").(string)
	advised := filepath.Join(next, "bin", "crw")
	if code != install.Refused || at(again, "stagingDecision") != staging.Keep || !strings.Contains(repair, advised+" install rollback "+next+" --backup-state-to ") || h.pointerTarget(t) != old {
		t.Fatalf("update again: exit %d, pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(again))
	}
	if info, err := os.Stat(advised); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the advised binary %s is not an executable file: %v", advised, err)
	}

	// L4b: rollback <newer dir> without the backup names the route; with it, the host is on the newer runtime again
	refused, code := install.Rollback(context.Background(), o, next)
	if code != install.Refused || at(refused, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone ||
		!strings.Contains(golden.Canon(at(refused, "swapGate", "cells", "storeSchema")), "--backup-state-to") || h.pointerTarget(t) != old {
		t.Fatalf("rollback <dir> without the backup: exit %d\n%s", code, golden.Canon(refused))
	}
	o.StateBackup = backupOf(h, "b4b")
	forward, code := install.Rollback(context.Background(), o, next)
	if code != install.OK || at(forward, "swapGate", "cells", "storeSchema", "answer") != swapgate.ExtendsZone || at(forward, "swapGate", "stateBackup", "made") != true || h.pointerTarget(t) != next {
		t.Fatalf("rollback <dir>: exit %d, pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(forward))
	}
	if selected := golden.Canon(at(h.hostRecord(t), "selected")); !strings.Contains(selected, next) || strings.Contains(selected, old) {
		t.Fatalf("the record selects %s", selected)
	}

	sameTree(t, before, withoutSidecars(digestTree(t, h.relayState)), "the state directory after the round trip")
	if relationships, journal := storeCounts(t, h); relationships != 1 || journal != 1 {
		t.Fatalf("relationships %d, journal %d", relationships, journal)
	}
}
