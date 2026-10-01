package supervisor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
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

// ordFixture restores the tree one Python OnRequestSupervisor scenario left (the former
// testdata/onrequest_ord_capture.py): the staged.sqlite3 snapshot holding the one message the
// scenario staged.
func ordFixture(t *testing.T, mode string) string {
	t.Helper()
	root := t.TempDir()
	treeFixture(t, mode, root)
	return root
}

func replayORDChannel(t *testing.T, mode string) {
	t.Helper()
	root := ordFixture(t, mode)
	dbPath := filepath.Join(root, "tree", "state", "relay.sqlite3")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "staged.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	restoreSnapshot(t, dbPath, data, "go")
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
	previous := TokenSource
	TokenSource = bytes.NewReader(make([]byte, 32))
	defer func() { TokenSource = previous }()

	var id string
	if err := s.DB.QueryRow("SELECT message_id FROM supervisor_messages").Scan(&id); err != nil {
		t.Fatal(err)
	}
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
	got.Tables = supervisorTables(t, s)
	rows := got.Tables["supervisor_messages"]
	got.Row = rows[len(rows)-1]
	normalizeORDRows(got.Tables)
	normalizeORDRow(got.Row)
	golden.CheckJSON(t, "capture", asJSON(t, got), treeGolden(t, root)...)
}

type ordSendHost struct {
	sendHost
	turnID    string
	startedAt float64
}

func (h *ordSendHost) ListTurnIDs(string, int) ([]any, error) { return []any{h.turnID}, nil }
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

func Test24_ORD_8_HandoffOnRequestSupervisorPushWholeRowsAndReply(t *testing.T) {
	replayORDChannel(t, "push")
}

func Test24_ORD_9_HandoffFoldedPushWholeRowsAndReplies(t *testing.T) {
	replayORDChannel(t, "folded")
}
