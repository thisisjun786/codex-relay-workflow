package cli_test

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// TestReportUpAReuseThatLandsAfterThePrecheckIsRefusedUnderTheWriteLock (CRW-1390, Argon V5 P3 of 146cbab5): report-up checks a
// recorded --id before it stages and again inside the staging's transaction. A second call that records the same --id with another kind
// between the two is found by the second check: this call is refused report_id_reused, its message is not staged (the staging and the
// row are one transaction) and the second call's report and message stand as they were written.
func TestReportUpAReuseThatLandsAfterThePrecheckIsRefusedUnderTheWriteLock(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	landed := false
	restore := cli.SetReportUpAfterPrecheck(func() {
		if landed {
			return
		}
		landed = true
		// the other call: the same --id as a lead_blocked report, recorded and staged in full
		if answer := s.must("report-up", "--initiative", "INI-1", "--kind", "lead_blocked", "--id", "lrp-race-1", "--text", "the gate is red"); answer["staged"] != true {
			t.Fatalf("the other call answered %v", answer)
		}
	})
	defer restore()
	code, answer := s.reportUp("--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-race-1", "--text", "milestone one is done")
	if !landed {
		t.Fatal("the other call never ran between the check and the staging")
	}
	detail, _ := answer["detail"].(string)
	if code != 2 || answer["ok"] != false || answer["reason"] != "report_id_reused" || !strings.Contains(detail, "lead_blocked") || !strings.Contains(detail, "lead_report") {
		t.Fatalf("exit %d %v, want report_id_reused naming the recorded lead_blocked and the given lead_report", code, answer)
	}
	if answer["recordedKind"] != "lead_blocked" || answer["kind"] != "lead_report" {
		t.Fatalf("the refusal names recorded kind %v and kind %v", answer["recordedKind"], answer["kind"])
	}
	messages := s.rows("SELECT obligation_kind FROM supervisor_messages")
	if len(messages) != 1 || messages[0].Text("obligation_kind") != "lead_blocked" {
		t.Fatalf("the supervisor messages are %v, want only the other call's lead_blocked", messages)
	}
	if row := s.reportRow("lrp-race-1"); row["kind"] != "lead_blocked" || row["text"] != "the gate is red" {
		t.Fatalf("the report row is %v, want the other call's", row)
	}
}
