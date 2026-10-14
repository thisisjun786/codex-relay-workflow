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

// reportRow is the one lead_report row of report, decoded.
func (s *reportUpStore) reportRow(report string) map[string]any {
	s.t.Helper()
	rows := s.rows("SELECT detail FROM journal WHERE kind = 'lead_report' AND subject = ?", report)
	if len(rows) != 1 {
		s.t.Fatalf("%d lead_report rows for %s, want one", len(rows), report)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(rows[0].Text("detail")), &detail); err != nil {
		s.t.Fatal(err)
	}
	return detail
}

// reportUp runs report-up and returns its exit code and its answer.
func (s *reportUpStore) reportUp(argv ...string) (int, map[string]any) {
	s.t.Helper()
	got := goCLI(s.t, append([]string{"--state", s.state, "report-up"}, argv...)...)
	if got.stdout == "" {
		return got.code, nil
	}
	return got.code, object(s.t, got.stdout)
}

// TestReportUpRestatedWhileStagingIsRefusedKeepsTheRowWithTheUnsentMessage (CRW-1361, Argon A2): a
// staged, unsent report restated under the same --id while the channel refuses to stage (the level
// above went away) keeps its first text in both the row and the message, and the refusal warns that
// nothing was restated. The row used to take the new text while the unsent message kept the first.
func TestReportUpRestatedWhileStagingIsRefusedKeepsTheRowWithTheUnsentMessage(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	first := s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-held-1", "--text", "the first text")
	if first["staged"] != true {
		t.Fatalf("the first report answered %v", first)
	}
	// The management seat above goes away: staging is refused with nobody above to tell.
	s.rows("UPDATE scope_bindings SET status = 'archived' WHERE task_id = '01mgmtA'")
	code, answer := s.reportUp("--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-held-1", "--text", "the second text")
	if code != 2 || answer["ok"] != false || answer["standing"] != true {
		t.Fatalf("the refused restatement: exit %d %v", code, answer)
	}
	if warning, _ := answer["warning"].(string); !strings.Contains(warning, "not restated") {
		t.Errorf("the refusal %v does not warn that the report was not restated", answer)
	}
	detail := s.reportRow("lrp-held-1")
	if detail["text"] != "the first text" || detail["messageId"] != first["messageId"] {
		t.Fatalf("the row was rewritten while its unsent message kept the first text: %v", detail)
	}
	packets := s.rows("SELECT packet FROM supervisor_messages")
	if len(packets) != 1 || !strings.Contains(packets[0].Text("packet"), "the first text") {
		t.Fatalf("supervisor messages %v, want the one unsent message with the first text", packets)
	}
	if n := len(s.rows("SELECT 1 FROM journal WHERE kind = 'lead_report_restated'")); n != 0 {
		t.Fatalf("%d lead_report_restated rows for a restatement that was not made", n)
	}
}

// TestReportUpRestatedAfterALateStagingAndSendKeepsTheSentRow (CRW-1361, Argon B1): a report first
// recorded with nobody above (no message), then staged by a replay once a seat is above, and sent, is
// never restated: whether it was sent is read from its message, not from the row's messageId.
func TestReportUpRestatedAfterALateStagingAndSendKeepsTheSentRow(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "01lead")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host")
	argv := []string{"--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-late-1", "--text", "the first text"}
	if code, answer := s.reportUp(argv...); code != 2 || answer["reason"] != "unregistered_scope" {
		t.Fatalf("with nobody above: exit %d %v", code, answer)
	}
	if detail := s.reportRow("lrp-late-1"); detail["messageId"] != nil {
		t.Errorf("the report with nobody above names a message: %v", detail)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "store", "--scope-kind", "store", "--task", "01store", "--host", "host")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	staged := s.must(append([]string{"report-up"}, argv...)...)
	if staged["staged"] != true || staged["replayed"] != true {
		t.Fatalf("the replay with a seat above answered %v", staged)
	}
	if detail := s.reportRow("lrp-late-1"); detail["messageId"] != staged["messageId"] || detail["recipient"] != "01store" {
		t.Errorf("the row %v does not name the message the replay staged (%v)", detail, staged["messageId"])
	}
	s.rows("UPDATE supervisor_messages SET state = 'dispatched'")
	third := s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-late-1", "--text", "a text after the send")
	if warning, _ := third["warning"].(string); !strings.Contains(warning, "already sent") {
		t.Errorf("the restatement after the send %v carries no warning that the sent text stands", third)
	}
	if detail := s.reportRow("lrp-late-1"); detail["text"] != "the first text" {
		t.Fatalf("the row of a sent report was rewritten: %v", detail)
	}
	if n := len(s.rows("SELECT 1 FROM journal WHERE kind = 'lead_report_restated'")); n != 0 {
		t.Fatalf("%d lead_report_restated rows for a sent report", n)
	}
}

// TestReportUpTheSameIDWithAnotherKindOrInitiativeIsRefused (CRW-1361, Argon B2): one --id is one
// report of one kind for one initiative. The message id is the kind, the initiative and the id, so a
// changed kind or initiative under a recorded --id would stage a second message beside the first; it is
// an input error, refused with both values named, and nothing is written - sent or not.
func TestReportUpTheSameIDWithAnotherKindOrInitiativeIsRefused(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-2", "--task", "01lead2", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-one-1", "--text", "the first text")
	journal := func() int { return len(s.rows("SELECT 1 FROM journal")) }
	for _, sent := range []bool{false, true} {
		if sent {
			s.rows("UPDATE supervisor_messages SET state = 'dispatched'")
		}
		before := journal()
		for _, change := range [][]string{
			{"--initiative", "INI-1", "--kind", "lead_blocked", "--text", "a changed text", "lead_report", "lead_blocked"},
			{"--initiative", "INI-1", "--kind", "lead_blocked", "--text", "the first text", "lead_report", "lead_blocked"},
			{"--initiative", "INI-2", "--kind", "lead_report", "--text", "a changed text", "INI-1", "INI-2"},
		} {
			code, answer := s.reportUp(append([]string{"--id", "lrp-one-1"}, change[:6]...)...)
			detail, _ := answer["detail"].(string)
			if code != 2 || answer["ok"] != false || answer["reason"] != "report_id_reused" || !strings.Contains(detail, change[6]) || !strings.Contains(detail, change[7]) {
				t.Fatalf("sent %v, %v: exit %d %v, want the input refused naming %s and %s", sent, change[:6], code, answer, change[6], change[7])
			}
			if n := len(s.rows("SELECT 1 FROM supervisor_messages")); n != 1 {
				t.Fatalf("sent %v, %v: %d supervisor messages, want the report's one", sent, change[:6], n)
			}
			if after := journal(); after != before {
				t.Fatalf("sent %v, %v: the refused report wrote %d journal rows", sent, change[:6], after-before)
			}
			if detail := s.reportRow("lrp-one-1"); detail["text"] != "the first text" || detail["kind"] != "lead_report" || detail["initiative"] != "INI-1" {
				t.Fatalf("sent %v, %v: the row changed: %v", sent, change[:6], detail)
			}
		}
	}
}

// TestReportUpRestatedWithAChangedSubjectOrEvidenceUpdatesTheRow (CRW-1361, Argon A4): the
// restatement is of the whole report - its subject, its evidence and its text - so a changed subject
// or evidence under the same text updates the row together with the unsent message.
func TestReportUpRestatedWithAChangedSubjectOrEvidenceUpdatesTheRow(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	report := func(subject, evidence string) {
		t.Helper()
		s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-whole-1", "--subject", subject, "--evidence", evidence, "--text", "the same text")
	}
	report("milestone one", "linear:CRW-1")
	report("milestone one, merged", "linear:CRW-1")
	if detail := s.reportRow("lrp-whole-1"); detail["subject"] != "milestone one, merged" {
		t.Errorf("a changed subject left the row stale: %v", detail)
	}
	report("milestone one, merged", "linear:CRW-2")
	detail := s.reportRow("lrp-whole-1")
	if evidence, _ := detail["evidence"].([]any); len(evidence) != 1 || evidence[0] != "linear:CRW-2" {
		t.Fatalf("a changed evidence left the row stale: %v", detail)
	}
	packets := s.rows("SELECT packet FROM supervisor_messages")
	if len(packets) != 1 || !strings.Contains(packets[0].Text("packet"), "milestone one, merged") || !strings.Contains(packets[0].Text("packet"), "linear:CRW-2") {
		t.Fatalf("supervisor messages %v, want the one restated message", packets)
	}
	if n := len(s.rows("SELECT 1 FROM journal WHERE kind = 'lead_report_restated'")); n != 2 {
		t.Fatalf("%d lead_report_restated rows, want each restatement recorded", n)
	}
}
