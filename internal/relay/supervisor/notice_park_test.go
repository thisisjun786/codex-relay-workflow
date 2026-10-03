package supervisor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestParkNotice_parks_only_a_notice_nothing_of_which_has_gone moved here from the faults package with
// the parker it tests; its cases and assertions are as they were. A ledger over a fresh store stands in
// for faults' testLedger, and the two accessors read a column as faults' text and integer do.
func TestParkNotice_parks_only_a_notice_nothing_of_which_has_gone(t *testing.T) {
	cases := []struct {
		name, state string
		assignments []string
		attempt     string
		parked      bool
	}{
		{"queued", "queued", nil, "", true},
		{"deferred busy", "deferred_busy", nil, "", true},
		{"withheld before the send", "withheld_pre_send", nil, "", true},
		{"queued after an attempt that sent nothing and was retry safe", "queued", nil, "'no',1", true},
		{"queued after an attempt that sent", "queued", nil, "'yes',1", false},
		{"queued after an attempt whose retry was not shown safe", "queued", nil, "'no',0", false},
		{"already held", "queued", []string{"hold_reason='held'"}, "", false},
		{"sending", "sending", nil, "", false},
		{"dispatched", "dispatched", nil, "", false},
		{"held uncertain", "held_uncertain", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given: a fault notice staged as a supervisor message in one state.
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			channel := NoticeChannel{Ledger: &faults.Ledger{Store: s, Clock: &delivery.FakeClock{T: 100000}}}
			seedNoticeMessage(t, ctx, s, "n1", "supervisor", tc.state, "2023-11-14T22:13:20.000000+00:00", append([]string{"obligation_kind='fault_notification'"}, tc.assignments...)...)
			if tc.attempt != "" {
				if _, err := s.Q(ctx).ExecContext(ctx, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, observed_at) VALUES ('req-1','n1',1,'m','withheld_pre_send',"+tc.attempt+",'{}','t','t')"); err != nil {
					t.Fatal(err)
				}
			}
			// When: it is parked.
			if err := channel.Park(ctx, "n1", "why"); err != nil {
				t.Fatal(err)
			}
			// Then: it is parked, and journaled once, only when it was unsent, unheld and nothing of it had gone.
			r, err := s.One(ctx, "SELECT hold_reason FROM supervisor_messages WHERE message_id='n1'")
			if err != nil {
				t.Fatal(err)
			}
			journal, err := s.One(ctx, "SELECT COUNT(*) AS n FROM journal WHERE kind='supervisor_notice_parked'")
			if err != nil {
				t.Fatal(err)
			}
			if parked := r.Text("hold_reason") == store.SupervisorHoldSuperseded; parked != tc.parked || (parkInteger(journal, "n") == 1) != tc.parked {
				t.Fatalf("hold %q, journal rows %d, want parked=%v", r.Text("hold_reason"), parkInteger(journal, "n"), tc.parked)
			}
		})
	}
}

func parkInteger(r store.Row, name string) int64 {
	switch v := r.Get(name).(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}
