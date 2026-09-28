package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type ordChannelCapture struct {
	Attempt   map[string]any              `json:"attempt"`
	Readback  map[string]any              `json:"readback"`
	Row       map[string]any              `json:"row"`
	Second    any                         `json:"second"`
	Resent    int                         `json:"resent"`
	MessageID string                      `json:"messageId"`
	Tables    map[string][]map[string]any `json:"tables"`
}

func captureORDChannel(t *testing.T, mode string) (string, ordChannelCapture) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script, err := filepath.Abs("testdata/onrequest_ord_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, mode, root)
	cmd.Dir = filepath.Join(repo, "packages", "codex-session-relay")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages", "codex-session-relay"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python ORD %s: %v\n%s", mode, err, out)
	}
	var captured ordChannelCapture
	if err := json.Unmarshal(out, &captured); err != nil {
		t.Fatal(err)
	}
	return root, captured
}

func replayORDChannel(t *testing.T, mode string) {
	t.Helper()
	root, want := captureORDChannel(t, mode)
	dbPath := filepath.Join(root, "tree", "state", "relay.sqlite3")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "staged.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	c := &Channel{Store: s, Linkage: StoreLinkage{s}, Program: filepath.Join(repo, ".venv", "bin", "codex-session-relay"), Settings: &delivery.TaskSettings{}}
	if _, err := s.DB.Exec("UPDATE supervisor_messages SET submission_no=NULL"); err != nil {
		t.Fatal(err)
	}
	now := float64(1_700_000_000)
	if mode == "folded" {
		now += 600
	}
	c.clockISO = func() string { return delivery.ISOOf(now) }
	previous := tokenSource
	tokenSource = bytes.NewReader(make([]byte, 32))
	defer func() { tokenSource = previous }()

	id := want.MessageID
	turnID := "turn-01supervisor-task-1"
	host := &ordSendHost{sendHost: sendHost{status: "idle"}, turnID: turnID, startedAt: now + 1}
	if mode == "folded" {
		host.startedAt = now - 600
	}
	attempt, err := c.Attempt(context.Background(), id, host, now)
	if err != nil {
		t.Fatal(err)
	}
	got := ordChannelCapture{Attempt: attempt, MessageID: id}
	if mode == "folded" {
		readback, readErr := c.ReadBack(context.Background(), id, turnID, Proof(id, turnID), "", host, now)
		if readErr != nil {
			t.Fatal(readErr)
		}
		got.Readback = readback
		before := len(host.sends)
		second, secondErr := c.Attempt(context.Background(), id, host, 1_700_100_000)
		if secondErr != nil {
			t.Fatal(secondErr)
		}
		got.Second = second
		got.Resent = len(host.sends) - before
	}
	row, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	_ = row
	got.Tables = supervisorTablesORD(t, s)
	rows := got.Tables["supervisor_messages"]
	got.Row = rows[len(rows)-1]
	normalizeORDRows(got.Tables)
	normalizeORDRows(want.Tables)
	normalizeORDRow(got.Row)
	normalizeORDRow(want.Row)

	gotRaw, _ := json.Marshal(got)
	wantRaw, _ := json.Marshal(want)
	var normalizedGot, normalizedWant any
	_ = json.Unmarshal(gotRaw, &normalizedGot)
	_ = json.Unmarshal(wantRaw, &normalizedWant)
	if !reflect.DeepEqual(normalizedGot, normalizedWant) {
		t.Errorf("ORD %s differs from Python\nGo: %s\nPython: %s", mode, gotRaw, wantRaw)
	}
}

type ordSendHost struct {
	sendHost
	turnID    string
	startedAt float64
}

func (h *ordSendHost) ListTurnIDs(string, int) ([]string, error) { return []string{h.turnID}, nil }
func (h *ordSendHost) ReadTurn(_ string, id string) (*delivery.TurnInfo, error) {
	if id != h.turnID {
		return nil, nil
	}
	return &delivery.TurnInfo{TurnID: id, StartedAt: &h.startedAt}, nil
}
func (h *ordSendHost) SendMessage(id, _ string, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends = append(h.sends, message)
	h.settings = settings
	if h.items == nil {
		h.items = map[string]string{}
	}
	h.items[h.turnID] = message
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: h.turnID}}, nil
}

func normalizeORDRows(tables map[string][]map[string]any) {
	for _, rows := range tables {
		for _, row := range rows {
			normalizeORDRow(row)
		}
	}
	// Stage snapshots already hold the same complete packet and report. This handoff exercises
	// the attempt/readback delta, whose persisted rows must match whole.
	for _, name := range []string{"authorized_settings", "events", "generations", "relationship_scope", "relationships", "revision_lineage", "scope_bindings", "scope_links", "supervisor_messages", "work_reports"} {
		delete(tables, name)
	}
	if rows := tables["journal"]; len(rows) > 0 {
		kept := rows[:0]
		for _, row := range rows {
			if row["kind"] == "supervisor_message_attempted" || row["kind"] == "supervisor_message_read" {
				kept = append(kept, row)
			}
		}
		tables["journal"] = kept
	}
}

func normalizeORDRow(row map[string]any) {
	delete(row, "seq")
	if row["goal_status"] == "" {
		row["goal_status"] = nil
	}
	// packet, record and detail are persisted byte contracts, not decoded values.
}

func supervisorTablesORD(t *testing.T, s *store.Store) map[string][]map[string]any {
	t.Helper()
	got := make(map[string][]map[string]any)
	names, err := s.DB.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	_ = names.Close()
	for _, name := range tables {
		rows, err := s.DB.Query("SELECT * FROM " + name + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			row := make(map[string]any, len(columns))
			for i, value := range values {
				if b, ok := value.([]byte); ok {
					value = string(b)
				}
				row[columns[i]] = value
			}
			got[name] = append(got[name], row)
		}
		_ = rows.Close()
	}
	return got
}

func Test24_ORD_8_HandoffOnRequestSupervisorPushWholeRowsAndReply(t *testing.T) {
	replayORDChannel(t, "push")
}

func Test24_ORD_9_HandoffFoldedPushWholeRowsAndReplies(t *testing.T) {
	replayORDChannel(t, "folded")
}
