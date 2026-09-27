package faults

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test22_FLT_17_PreexistingStoreAcquiresFaultTables(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	first, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	rows, err := second.All(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'fault_%'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 18 {
		var names []string
		for _, r := range rows {
			names = append(names, text(r, "name"))
		}
		t.Fatalf("got %d fault tables: %s", len(rows), strings.Join(names, ","))
	}
	l := &Ledger{Store: second, Clock: &testClock{now: 100000}}
	o := testObservation("a", "report_omitted", Broken)
	record(t, l, ctx, o)
	if count(t, l, ctx, "fault_ledger") != 1 {
		t.Fatal("ledger unusable on reopened store")
	}
}
