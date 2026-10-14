package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReportUpRestatedUnderTheSameIDUpdatesTheRowAndTheUnsentMessage (CRW-1361): report-up again with
// the same --id and a changed text restates the report. While the message is unsent the lead_report
// row and the message both say the new text (the row used to keep the first text while the message
// was rewritten); once the message may have gone, neither is changed and the answer says so.
func TestReportUpRestatedUnderTheSameIDUpdatesTheRowAndTheUnsentMessage(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	report := func(text string) map[string]any {
		t.Helper()
		return s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-milestone-1", "--text", text)
	}
	row := func() map[string]any {
		t.Helper()
		rows := s.rows("SELECT detail FROM journal WHERE kind = 'lead_report' AND subject = 'lrp-milestone-1'")
		if len(rows) != 1 {
			t.Fatalf("%d lead_report rows for the report, want one", len(rows))
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(rows[0].Text("detail")), &detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}
	packet := func() string {
		t.Helper()
		rows := s.rows("SELECT packet FROM supervisor_messages")
		if len(rows) != 1 {
			t.Fatalf("%d supervisor messages, want the report's one", len(rows))
		}
		return rows[0].Text("packet")
	}
	first := report("milestone one is done")
	if first["staged"] != true || row()["text"] != "milestone one is done" {
		t.Fatalf("the first report answered %v with row %v", first, row())
	}
	firstDigest := row()["bodyDigest"]

	// Restated while unsent: the message and the row both carry the new text.
	second := report("milestone one is done, and the docs are merged")
	if second["ok"] != true || second["messageId"] != first["messageId"] {
		t.Fatalf("the restatement answered %v", second)
	}
	if !strings.Contains(packet(), "the docs are merged") {
		t.Fatalf("the unsent message was not restated: %s", packet())
	}
	detail := row()
	if detail["text"] != "milestone one is done, and the docs are merged" || detail["bodyDigest"] == firstDigest {
		t.Fatalf("the lead_report row kept the first text after the unsent message was restated: %v", detail)
	}
	if n := len(s.rows("SELECT 1 FROM journal WHERE kind = 'lead_report_restated' AND subject = 'lrp-milestone-1'")); n != 1 {
		t.Fatalf("%d lead_report_restated rows, want the restatement recorded once", n)
	}

	// The message has gone: a third text changes neither the message nor the row, and the answer warns.
	s.rows("UPDATE supervisor_messages SET state = 'dispatched'")
	sent := packet()
	third := report("a third text after the send")
	if packet() != sent {
		t.Fatal("a sent message was rewritten")
	}
	if detail := row(); detail["text"] != "milestone one is done, and the docs are merged" {
		t.Fatalf("the row of a sent report was rewritten: %v", detail)
	}
	if warning, _ := third["warning"].(string); !strings.Contains(warning, "already") {
		t.Fatalf("the answer to a restatement after the send %v carries no warning that the sent text stands", third)
	}
}
