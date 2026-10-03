package delivery

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// historyShapes opens the two shapes a store of this version has: one this version created and one it upgraded from
// the store of the version before CRW-301 (SQLite takes the first of equally priced indexes in the order
// sqlite_master lists them, and an upgrade appends the new ones last).
func historyShapes(t *testing.T) map[string]*store.Store {
	t.Helper()
	ctx := context.Background()
	fresh, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.CreatePreviousVersion(t, path, "", "go")
	upgraded, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	return map[string]*store.Store{"fresh": fresh, "upgraded": upgraded}
}

func historyPlan(t *testing.T, s *store.Store, query string, args ...any) string {
	t.Helper()
	rows, err := all(context.Background(), s, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, r.S("detail"))
	}
	return strings.Join(lines, " | ")
}

// CRW-301: the daemon's read of the acknowledgements still to be verified reaches them through the partial index
// that holds only those, where it scanned every acknowledgement; and the omission context finds a generation's
// managed start through its dispatch request, where it scanned every managed start for each generation.
func TestHistoryIndexesServeThePendingAcksAndTheOmissionContext(t *testing.T) {
	t.Parallel()
	for shape, s := range historyShapes(t) {
		t.Run(shape, func(t *testing.T) {
			pending := historyPlan(t, s, pendingAcksSQL, 0.0, DeliveryUnconfirmed, Dispatched, InboxOnly, 8)
			if !strings.Contains(pending, "SCAN a USING INDEX acks_unverified") {
				t.Errorf("the pending-ack read does not go through acks_unverified:\n%s", pending)
			}
			omission := historyPlan(t, s, omissionContextSQL, omissionContextArgs("rel", "dispatch", "session", "turn")...)
			if !strings.Contains(omission, "SEARCH m USING INDEX managed_start_requests_dispatch (dispatch_request_id=?)") {
				t.Errorf("the omission context does not find the managed start through its dispatch request:\n%s", omission)
			}
		})
	}
}
