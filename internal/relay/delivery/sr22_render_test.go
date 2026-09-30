package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

type sr22Capture struct {
	EventID   string            `json:"eventId"`
	RequestID string            `json:"requestId"`
	Messages  map[string]string `json:"messages"`
}

func captureSR22(t *testing.T) (sr22Capture, *store.Store) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "relay.sqlite3")
	// The fixture's artifact paths name the event: a fixed tree keeps them the same.
	tree := parityTree(t)
	home := t.TempDir()
	out := pyAnswer(t, "sr22", func() ([]byte, error) {
		repo := repoRoot(t)
		script, err := filepath.Abs("testdata/sr22_render.py")
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, dbPath, tree)
		cmd.Dir = filepath.Join(repo, "packages", "codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages", "codex-session-relay"))
		out, err := pythonCombined(cmd)
		if err != nil {
			return nil, fmt.Errorf("Python SR-22: %w", err)
		}
		return out, nil
	}, pyoracle.Substitute(tree, "<tree>"))
	var captured sr22Capture
	mustDo(t, json.Unmarshal(out, &captured))
	recordedStore(t, "sr22 store", dbPath)
	// The capture is a backup of Python's fenced store, alone in its directory: Go renders from
	// it after a takeover.
	testsupport.Rehome(t, dbPath)
	testsupport.HandOver(t, dbPath, "go")
	s, err := store.Open(context.Background(), dbPath, "")
	mustDo(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return captured, s
}

func Test24_SR_22_CompletionContextWholeMessageBytes(t *testing.T) {
	captured, s := captureSR22(t)
	d := NewService(s, NewFakeClock())

	for _, name := range []string{"project", "issue_only", "absent_relationship"} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.DB.Exec("DELETE FROM relationship_scope"); err != nil {
				t.Fatal(err)
			}
			if name == "project" {
				row, err := s.One(context.Background(), "SELECT relationship_id FROM deliveries WHERE event_id = ?", captured.EventID)
				mustDo(t, err)
				mustDo(t, s.RecordRelationshipScope(context.Background(), row.Get("relationship_id").(string), "PRJ-1", "t"))
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
			if got != captured.Messages[name] {
				t.Errorf("whole message differs from Python\n--- Go ---\n%s\n--- Python ---\n%s", got, captured.Messages[name])
			}
		})
	}
}

func mustDeliveryRow(t *testing.T, d *Service, event string) Row {
	t.Helper()
	row, err := d.Get(context.Background(), event)
	mustDo(t, err)
	return row
}
