package faults

import (
	"strings"
	"testing"
)

// A notice names an issue by a Linear key, a UUID, or a tracking ref owner/name#N. The ledger asks
// the network-free leaf internal/tracking/ref for the ref and adds its own bound on length.
func noticeIssueNegatives() []string {
	return []string{"o/r", "o/r#0", "o/r#07", "./r#1", "o/..#1", "TEAM-1234567890", "", "o/r#7 ", "o/r#7\n"}
}

func TestIsNoticeIssue(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"CRW-205", "TEAM-123", "123e4567-e89b-12d3-a456-426614174000", "o/r#7", "my.org/my_repo.go#1234", "thisisjun786/crw-work#1"} {
		if !IsNoticeIssue(s) {
			t.Errorf("IsNoticeIssue(%q) = false, want true", s)
		}
	}
	for _, s := range noticeIssueNegatives() {
		if IsNoticeIssue(s) {
			t.Errorf("IsNoticeIssue(%q) = true, want false", s)
		}
	}
	// A valid ref longer than the packet bound is refused: the packet stays bounded.
	long := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 60) + "#7"
	if len(long) <= 256 {
		t.Fatalf("fixture is %d bytes", len(long))
	}
	if IsNoticeIssue(long) {
		t.Errorf("IsNoticeIssue accepted a %d byte ref", len(long))
	}
}

func TestNoticeFactsIssueKeyTakesATrackingRef(t *testing.T) {
	t.Parallel()
	const key = "o/r#7"
	insert := func(t *testing.T, scope, externalRef string, relationship bool) map[string]any {
		t.Helper()
		l, ctx := testLedger(t)
		statements := []string{
			"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at, external_ref) VALUES ('abc123','crw','report_omitted','c','broken','{}','" + scope + "','P','open','t','t','t','" + externalRef + "')",
			"INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'pending','t','t')",
		}
		if relationship {
			statements = append([]string{"INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-1','" + key + "','active','pt','ph','ct','ch',1,'[]','[]','t','t')"}, statements...)
		}
		for _, statement := range statements {
			if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		facts, err := l.NoticeFacts(ctx, "n1")
		if err != nil {
			t.Fatal(err)
		}
		return facts
	}
	t.Run("scope", func(t *testing.T) {
		t.Parallel()
		facts := insert(t, `{"projectKey":"P","issueKey":"`+key+`"}`, "", false)
		if facts["issueKey"] != key || facts["anchor"] != "project:P" {
			t.Fatalf("issueKey = %v, anchor = %v", facts["issueKey"], facts["anchor"])
		}
	})
	t.Run("anchor", func(t *testing.T) {
		t.Parallel()
		facts := insert(t, `{"projectKey":"P","issueKey":"`+key+`"}`, "", true)
		if facts["issueKey"] != key || facts["anchor"] != "rel-1" {
			t.Fatalf("issueKey = %v, anchor = %v", facts["issueKey"], facts["anchor"])
		}
	})
	t.Run("external_ref", func(t *testing.T) {
		t.Parallel()
		facts := insert(t, `{"projectKey":"P"}`, key, false)
		if facts["externalRef"] != key {
			t.Fatalf("externalRef = %v", facts["externalRef"])
		}
	})
	for _, bad := range noticeIssueNegatives() {
		if strings.ContainsAny(bad, "'\n") || bad == "" {
			continue
		}
		t.Run("refused/"+bad[:min(len(bad), 20)], func(t *testing.T) {
			t.Parallel()
			facts := insert(t, `{"projectKey":"P","issueKey":"`+bad+`"}`, "", false)
			if facts["issueKey"] != nil {
				t.Fatalf("issueKey = %v, want nil", facts["issueKey"])
			}
		})
	}
	t.Run("refused/long", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 60) + "#7"
		facts := insert(t, `{"projectKey":"P","issueKey":"`+long+`"}`, "", false)
		if facts["issueKey"] != nil {
			t.Fatalf("issueKey = %v, want nil", facts["issueKey"])
		}
	})
}
