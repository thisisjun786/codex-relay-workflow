package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// historyShapes opens the two shapes a store of this version has: one this version created, and one it upgraded
// from the store of the version before CRW-301. SQLite takes the first of equally priced indexes it meets in the
// order sqlite_master lists them, and an upgrade appends the new ones last, so a plan has to be right on both.
func historyShapes(t *testing.T) map[string]*Store {
	t.Helper()
	ctx := context.Background()
	fresh, err := Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.CreatePreviousVersion(t, path, "", "go")
	upgraded, err := Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	return map[string]*Store{"fresh": fresh, "upgraded": upgraded}
}

func planLines(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.DB.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, detail)
	}
	return strings.Join(lines, " | ")
}

// CRW-301: the hourly send-budget count reaches the attempts of the current window through attempts_sent_at and the
// supervisor's through supervisor_attempts_transport_started, where it scanned both tables whole. The statements
// that must not change plan stay where they were: the claim path's count of one relationship's sends keeps reaching
// the deliveries by relationship, as a single-column deliveries(recipient_task_id) would have taken it over
// (the planner, with no statistics, takes an equality for ten rows on any index), and a fault notice is still found
// through the unique index that covers it and not through the obligation index.
func TestHistoryIndexesServeTheSendBudgetAndLeaveOtherPlansAlone(t *testing.T) {
	t.Parallel()
	for shape, s := range historyShapes(t) {
		t.Run(shape, func(t *testing.T) {
			stamps := RelationshipSendsArgs(1_700_000_000)
			spent := planLines(t, s, "SELECT * FROM "+RelationshipSpentSQL+" spent", stamps...)
			for _, want := range []string{"SEARCH sba USING INDEX attempts_sent_at (sent_at>? AND sent_at<?)", "SEARCH sbs USING INDEX supervisor_attempts_transport_started (transport_started_at>? AND transport_started_at<?)"} {
				if !strings.Contains(spent, want) {
					t.Errorf("the hourly count does not read by %s:\n%s", want, spent)
				}
			}
			claim := planLines(t, s, "SELECT "+RelationshipSendsSQL("?", "?"), append(append([]any{"rel", "recipient"}, stamps[:2]...), append([]any{"rel", "recipient"}, stamps[2:]...)...)...)
			for _, want := range []string{"SEARCH sbd USING INDEX deliveries_relationship_created (relationship_id=?)", "SEARCH sba USING INDEX sqlite_autoindex_attempts_2 (event_id=?)"} {
				if !strings.Contains(claim, want) {
					t.Errorf("the claim path's count lost %s:\n%s", want, claim)
				}
			}
			if strings.Contains(claim, "attempts_sent_at") {
				t.Errorf("the claim path's count reads by the window index:\n%s", claim)
			}
			notice := planLines(t, s, "SELECT * FROM supervisor_messages WHERE obligation_kind='fault_notification' AND obligation_id=?", "notice")
			if !strings.Contains(notice, "USING INDEX supervisor_messages_one_notice") || strings.Contains(notice, "supervisor_messages_obligation") {
				t.Errorf("a fault notice is not found through its unique index:\n%s", notice)
			}
		})
	}
}

func historyIndexNamesIn(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	indexes, err := testsupport.HistoryIndexes()
	if err != nil {
		t.Fatal(err)
	}
	var present []string
	for _, index := range indexes {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", index.Name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			present = append(present, index.Name)
		}
	}
	return present
}

// CRW-301: the indexes are statements of the schema script, which every writable open runs, so the first open of a
// store of the previous version builds them whichever command made it, a read-only one included (it opens for
// writing first, the way openForRead tries). What does not build them is the read that opens nothing for writing.
func TestHistoryIndexesAreBuiltByAnyWritableOpenAndNotByAReadInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, open := range map[string]func(path string) error{
		"a command": func(path string) error {
			s, err := Open(ctx, path, "")
			if err == nil {
				err = s.Close()
			}
			return err
		},
		"a read-only command": func(path string) error {
			s, err := Open(WithReadOnlyCommand(ctx), path, "")
			if err == nil {
				err = s.Close()
			}
			return err
		},
	} {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.CreatePreviousVersion(t, path, "", "go")
		if present := historyIndexNamesIn(t, path); len(present) != 0 {
			t.Fatalf("the previous version's store already holds %v", present)
		}
		if err := open(path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if present := historyIndexNamesIn(t, path); len(present) != 6 {
			t.Errorf("after %s opened a store of the previous version it holds %v, want the six history indexes", name, present)
		}
	}
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.CreatePreviousVersion(t, path, "", "go")
	read, err := OpenInPlace(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	if present := historyIndexNamesIn(t, path); len(present) != 0 {
		t.Errorf("a read in place built %v", present)
	}
}
