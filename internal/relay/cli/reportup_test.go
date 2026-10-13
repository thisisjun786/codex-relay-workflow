package cli_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1333 (design CRW-1314 §2): `crw relay report-up --initiative KEY --kind ...` is a lead's report
// upward: the report row (a lead_report journal row) first, then the direct message on the supervisor
// channel to the management seat above the initiative, both in one transaction.

type reportUpStore struct {
	t     *testing.T
	state string
	path  string
}

func newReportUpStore(t *testing.T) *reportUpStore {
	t.Helper()
	state := ownerOnlyState(t, tempHome(t), "report-up")
	path := filepath.Join(state, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	return &reportUpStore{t: t, state: state, path: path}
}

func (s *reportUpStore) must(argv ...string) map[string]any {
	s.t.Helper()
	got := goCLI(s.t, append([]string{"--state", s.state}, argv...)...)
	if got.code != 0 {
		s.t.Fatalf("%v: exit %d %s", argv, got.code, got.stdout)
	}
	return object(s.t, got.stdout)
}

func (s *reportUpStore) rows(query string, args ...any) []store.Row {
	s.t.Helper()
	db, err := store.Open(context.Background(), s.path, "")
	if err != nil {
		s.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.All(context.Background(), query, args...)
	if err != nil {
		s.t.Fatal(err)
	}
	return rows
}

// TestReportUpStagesTheLeadsDecisionRequestToItsManagement: a lead's decision request reaches the
// management seat that registered it, once per report.
func TestReportUpStagesTheLeadsDecisionRequestToItsManagement(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	argv := []string{"report-up", "--initiative", "INI-1", "--kind", "lead_decision_request", "--subject", "approve the release-gate exception",
		"--text", "the gate refuses a docs-only change", "--evidence", "linear:CRW-1"}
	answer := s.must(argv...)
	if answer["ok"] != true || answer["staged"] != true || answer["replayed"] != false || answer["recipient"] != "01mgmtA" ||
		answer["recipientScopeKind"] != "management" || answer["sender"] != "01lead" || answer["kind"] != "lead_decision_request" {
		t.Fatalf("report-up answered %v", answer)
	}
	report, _ := answer["reportId"].(string)
	if len(report) != len("lrp_")+24 || report[:4] != "lrp_" {
		t.Fatalf("report id %q", report)
	}
	journal := s.rows("SELECT subject, detail FROM journal WHERE kind = 'lead_report'")
	if len(journal) != 1 || journal[0].Text("subject") != report {
		t.Fatalf("lead_report rows %v", journal)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(journal[0].Text("detail")), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["reportedBy"] != "01lead" || detail["initiative"] != "INI-1" || detail["text"] != "the gate refuses a docs-only change" || detail["messageId"] != answer["messageId"] {
		t.Fatalf("lead_report detail %v", detail)
	}
	messages := s.rows("SELECT obligation_kind, obligation_id, relationship_id, sender_task_id, recipient_task_id, state FROM supervisor_messages")
	if len(messages) != 1 || messages[0].Text("obligation_kind") != "lead_decision_request" || messages[0].Text("obligation_id") != report ||
		messages[0].Text("relationship_id") != "initiative:INI-1" || messages[0].Text("recipient_task_id") != "01mgmtA" || messages[0].Text("state") != "queued" {
		t.Fatalf("supervisor messages %v", messages)
	}
	again := s.must(argv...)
	if again["replayed"] != true || again["staged"] != false || again["reportId"] != report || again["messageId"] != answer["messageId"] {
		t.Fatalf("the replay answered %v", again)
	}
	if n := len(s.rows("SELECT 1 FROM journal WHERE kind = 'lead_report'")); n != 1 {
		t.Fatalf("%d lead_report rows after the replay, want one", n)
	}
}

// TestReportUpWithNobodyAboveKeepsTheReport: with no management seat above and no store seat the
// report is still on record and the refusal says so; nothing is staged.
func TestReportUpWithNobodyAboveKeepsTheReport(t *testing.T) {
	s := newReportUpStore(t)
	t.Setenv("CODEX_THREAD_ID", "01lead")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host")
	got := goCLI(t, "--state", s.state, "report-up", "--initiative", "INI-1", "--kind", "lead_report", "--text", "the delivery scope is complete")
	if got.code != 2 {
		t.Fatalf("exit %d %s, want the refusal", got.code, got.stdout)
	}
	answer := object(t, got.stdout)
	if answer["ok"] != false || answer["reason"] != "unregistered_scope" || answer["standing"] != true || answer["reportId"] == nil {
		t.Fatalf("answer %v", answer)
	}
	if n := len(s.rows("SELECT 1 FROM journal WHERE kind = 'lead_report' AND subject = ?", answer["reportId"])); n != 1 {
		t.Fatalf("%d lead_report rows, want the report kept", n)
	}
	if n := len(s.rows("SELECT 1 FROM supervisor_messages")); n != 0 {
		t.Fatalf("%d supervisor messages staged with nobody to tell", n)
	}
	// The kinds are the lead's three; anything else is a parser refusal.
	if got := goCLI(t, "--state", s.state, "report-up", "--initiative", "INI-1", "--kind", "blocked", "--text", "x"); got.code != 2 || got.stdout != "" {
		t.Fatalf("--kind blocked: exit %d %q", got.code, got.stdout)
	}
}
