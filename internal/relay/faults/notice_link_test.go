package faults

import "testing"

// A notice names the fault's published issue by a link only when it is one of the trackers' issue
// URLs: Linear's, or GitHub's https://github.com/<owner>/<name>/issues/<N>.
func TestNoticeFactsExternalRefLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ref   string
		named bool
	}{
		{"CRW-99", true},
		{"https://linear.app/jun786/issue/CRW-99/some-title", true},
		{"https://github.com/o/r/issues/7", true},
		{"https://github.com/thisisjun786/crw-work/issues/1234", true},
		{"https://github.com/my.org/my_repo.go/issues/1", true},
		{"https://github.com/o/r/issues/0", false},
		{"https://github.com/o/r/issues/07", false},
		{"https://github.com/o/r/issues/", false},
		{"https://github.com/o/r/pull/7", false},
		{"https://github.com/o/r/issues/7/extra", false},
		{"https://github.com/o/r/issues/7?x=1", false},
		{"https://github.com/o/r/issues/7#issuecomment-1", false},
		{"https://github.com/o/issues/7", false},
		{"https://github.com/../r/issues/7", false},
		{"https://github.com/o/../issues/7", false},
		{"https://github.com/o/r/issues/1234567890", false},
		{"http://github.com/o/r/issues/7", false},
		{"https://example.com/o/r/issues/7", false},
		{"https://github.com.evil.example/o/r/issues/7", false},
		{"https://evil.example/https://github.com/o/r/issues/7", false},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			l, ctx := testLedger(t)
			for _, statement := range []string{
				"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at, external_ref) VALUES ('abc123','crw','report_omitted','c','broken','{}','{\"projectKey\": \"P\"}','P','open','t','t','t','" + tc.ref + "')",
				"INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'pending','t','t')",
			} {
				if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			facts, err := l.NoticeFacts(ctx, "n1")
			if err != nil {
				t.Fatal(err)
			}
			if facts["issuePublished"] != true {
				t.Fatalf("issuePublished = %v", facts["issuePublished"])
			}
			want := any(nil)
			if tc.named {
				want = tc.ref
			}
			if facts["externalRef"] != want {
				t.Fatalf("externalRef = %v, want %v", facts["externalRef"], want)
			}
		})
	}
}
