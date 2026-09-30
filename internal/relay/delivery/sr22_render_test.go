package delivery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// sr22Fixture is the store testdata/sr22_render.py left through the Python package (the fixture
// sr22.sqlite3) and the event and request it staged in it (sr22.json).
type sr22Fixture struct {
	EventID   string `json:"eventId"`
	RequestID string `json:"requestId"`
}

func sr22Store(t *testing.T) (sr22Fixture, *store.Store) {
	t.Helper()
	var staged sr22Fixture
	mustDo(t, json.Unmarshal(golden.Fixture(t, "sr22.json"), &staged))
	dbPath := filepath.Join(t.TempDir(), "relay.sqlite3")
	mustDo(t, os.WriteFile(dbPath, golden.Fixture(t, "sr22.sqlite3"), 0o600))
	// The fixture is a backup of Python's fenced store, alone in its directory: Go renders from
	// it after a takeover.
	testsupport.Rehome(t, dbPath)
	testsupport.HandOver(t, dbPath, "go")
	s, err := store.Open(context.Background(), dbPath, "")
	mustDo(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return staged, s
}

func Test24_SR_22_CompletionContextWholeMessageBytes(t *testing.T) {
	captured, s := sr22Store(t)
	d := NewService(s, NewFakeClock())

	for _, name := range []string{"project", "issue_only", "absent_relationship"} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.DB.Exec("DELETE FROM relationship_scope"); err != nil {
				t.Fatal(err)
			}
			if name == "project" {
				row, err := s.One(context.Background(), "SELECT relationship_id FROM deliveries WHERE event_id = ?", captured.EventID)
				mustDo(t, err)
				mustDo(t, storeseed.RecordRelationshipScope(context.Background(), s, row.Get("relationship_id").(string), "PRJ-1", "t"))
			}
			row, err := d.Get(context.Background(), captured.EventID)
			mustDo(t, err)
			if name == "absent_relationship" {
				row = make(Row, len(row))
				for key, value := range mustDeliveryRow(t, d, captured.EventID) {
					row[key] = value
				}
				row["relationship_id"] = "rel-0000000000000000"
			}
			receipt, err := d.Receipt(context.Background(), captured.EventID)
			mustDo(t, err)
			report, err := reportRows(context.Background(), s, captured.EventID)
			mustDo(t, err)
			got, err := composeWorkCompletion(context.Background(), s, row, receipt, captured.RequestID, report, 6000)
			mustDo(t, err)
			golden.Check(t, "message", []byte(got))
		})
	}
}

func mustDeliveryRow(t *testing.T, d *Service, event string) Row {
	t.Helper()
	row, err := d.Get(context.Background(), event)
	mustDo(t, err)
	return row
}
