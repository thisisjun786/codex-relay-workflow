package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-301: the lookups staging and selection make for each obligation they visit go through the indexes added for
// them, where each scanned a table that only grows. They are held on a store this version created and on one it
// upgraded from the version before (SQLite takes the first of equally priced indexes in the order sqlite_master
// lists them, and an upgrade appends the new ones last).
func TestHistoryIndexesServeTheObligationLookups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, shape := range []string{"fresh", "upgraded"} {
		t.Run(shape, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.sqlite3")
			if shape == "upgraded" {
				testsupport.CreatePreviousVersion(t, path, "", "go")
			}
			s, err := store.Open(ctx, path, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			plan := func(query string, args ...any) string {
				rows, err := s.All(ctx, "EXPLAIN QUERY PLAN "+query, args...)
				if err != nil {
					t.Fatal(err)
				}
				var lines []string
				for _, r := range rows {
					lines = append(lines, r.Text("detail"))
				}
				return strings.Join(lines, " | ")
			}
			verdicts := plan(syncVerdictsSQL, "rel", "event")
			if !strings.Contains(verdicts, "SEARCH sync_outbox USING INDEX sync_outbox_relationship_event (relationship_id=? AND event_id=?)") {
				t.Errorf("the verdict writes are not found by relationship and event:\n%s", verdicts)
			}
			message := plan(obligationMessageSQL, "obligation")
			if !strings.Contains(message, "SEARCH supervisor_messages USING INDEX supervisor_messages_obligation (obligation_id=?)") || strings.Contains(message, "TEMP B-TREE") {
				t.Errorf("the newest message of an obligation is not found by the obligation index without a sort:\n%s", message)
			}
		})
	}
}
