package store

import (
	"context"
	"slices"
	"testing"
)

// CRW-943: the per-recipient order conditions leave out a claimable fault notice to a recipient
// whose line a busy-backoff delivery holds, because such a notice cannot be claimed (I-216's notice
// part) and counting it as a row that goes first makes the whole supervisor channel yield with it.
// The existing truth table seeds report rows only, so it cannot see the exclusion at all; these
// cases hold the exclusion to its words on the two obligation kinds that matter.
//
// The held set is the caller's reading of the delivery path's own rule, so what the store is given
// here is the set, not the deliveries table: these are the store's own cases.

// orderProbe is one supervisor_messages row an order condition is asked about.
type orderProbe struct {
	id, kind, recipient, state string
}

func seedOrderProbes(t *testing.T, s *Store, probes []orderProbe) {
	t.Helper()
	ctx := context.Background()
	for _, p := range probes {
		_, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,?, 'rel','p','k','parent',?,'s','{}',?,'t','t')", p.id, "obl-"+p.id, p.kind, p.recipient, p.state)
		must(t, err)
	}
}

// TestSupervisorOrderConditions_leave_out_a_claimable_notice_to_a_held_recipient holds each of the
// three order conditions to its words: with the recipient named held, a claimable fault notice to it
// is out and every other row is unchanged; with nobody named held, the same rows come back as the
// plain condition selects them.
func TestSupervisorOrderConditions_leave_out_a_claimable_notice_to_a_held_recipient(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	seedOrderProbes(t, s, []orderProbe{
		{"notice-held", SupervisorNoticeObligationKind, "held", "queued"},
		{"notice-free", SupervisorNoticeObligationKind, "free", "queued"},
		{"report-held", "report", "held", "queued"},
		{"report-free", "report", "free", "queued"},
		{"notice-held-in-flight", SupervisorNoticeObligationKind, "held", "sending"},
	})
	// The in-flight notice carries a live lease, which is what makes it in flight rather than stranded.
	_, err := s.DB.ExecContext(context.Background(), "UPDATE supervisor_messages SET lease_until=? WHERE message_id='notice-held-in-flight'", sendableNow+100)
	must(t, err)

	conditions := []struct {
		name  string
		plain func() string
		held  func([]string) string
		// wantHeld is the set with 'held' named, sorted as selectedIDs returns it: a claimable notice
		// to it is out, and an in-flight one is not, because the yield takes away only what can be
		// claimed.
		wantHeld []string
	}{
		{"ahead", func() string { return SupervisorAheadSQL("") }, func(h []string) string { return SupervisorAheadExceptYieldingSQL("", h) },
			[]string{"notice-free", "notice-held-in-flight", "report-free", "report-held"}},
		{"ahead under the claim's lock", func() string { return SupervisorAheadInClaimSQL("") }, func(h []string) string { return SupervisorAheadInClaimExceptYieldingSQL("", h) },
			[]string{"notice-free", "notice-held-in-flight", "report-free", "report-held"}},
		{"attemptable", func() string { return SupervisorAttemptableSQL("") }, func(h []string) string { return SupervisorAttemptableExceptYieldingSQL("", h) },
			[]string{"notice-free", "report-free", "report-held"}},
	}
	for _, c := range conditions {
		t.Run(c.name, func(t *testing.T) {
			// Given: the condition with nobody named held, and the same with the held recipient named.
			plain := selectedIDs(t, s, "SELECT message_id FROM supervisor_messages WHERE "+c.plain(), sendableNow, sendableNow)
			withHeld := selectedIDs(t, s, "SELECT message_id FROM supervisor_messages WHERE "+c.held([]string{"held"}), SupervisorHeldNoticeArgs([]string{"held"}, sendableNow)...)
			// Then: the plain condition still counts the notice, and the held one leaves it out.
			if !slices.Contains(plain, "notice-held") {
				t.Fatalf("%s without a held reading does not count the claimable notice: %v", c.name, plain)
			}
			if !slices.Equal(withHeld, c.wantHeld) {
				t.Fatalf("%s with 'held' named selected %v, want %v", c.name, withHeld, c.wantHeld)
			}
		})
	}
}

// TestSupervisorHeldNoticeArgs_bind_now_then_the_recipients_then_now pins the order the three order
// conditions bind their arguments in, which is what lets a caller pass them one way for all three.
func TestSupervisorHeldNoticeArgs_bind_now_then_the_recipients_then_now(t *testing.T) {
	t.Parallel()
	got := SupervisorHeldNoticeArgs([]string{"a", "b"}, sendableNow)
	want := []any{sendableNow, "a", "b", sendableNow}
	if !slices.Equal(got, want) {
		t.Fatalf("held args %v, want %v", got, want)
	}
	if empty := SupervisorHeldNoticeArgs(nil, sendableNow); !slices.Equal(empty, []any{sendableNow, sendableNow}) {
		t.Fatalf("no held recipient binds %v, want now twice", empty)
	}
}
