package delivery

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type sr22Capture struct {
	EventID   string            `json:"eventId"`
	RequestID string            `json:"requestId"`
	Messages  map[string]string `json:"messages"`
}

func captureSR22(t *testing.T) (sr22Capture, *store.Store) {
	t.Helper()
	repo := repoRoot(t)
	root := t.TempDir()
	dbPath := filepath.Join(root, "relay.sqlite3")
	script, err := filepath.Abs("testdata/sr22_render.py")
	mustDo(t, err)
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, dbPath)
	cmd.Dir = filepath.Join(repo, "packages", "codex-session-relay")
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages", "codex-session-relay"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python SR-22: %v\n%s", err, out)
	}
	var captured sr22Capture
	mustDo(t, json.Unmarshal(out, &captured))
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
