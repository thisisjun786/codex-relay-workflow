package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// These tests pin, over a seeded store, what each guard of the supervisor channel's "can be sent"
// condition decides, so that deleting any one of the overlapping guards is caught: the attempt's
// check before any host work, the claim's check under its write lock, and the daemon pass's head
// selection. The three answer the same question from three vantage points and each one decides
// something the others do not; the refusal text names which one fired.

const guardNow = 1_700_000_000

// guardHost counts the host work an attempt does before it can refuse.
type guardHost struct {
	*sendHost
	reads, sends int
}

func (h *guardHost) ReadThread(_ context.Context, id string) (delivery.ThreadFacts, error) {
	h.reads++
	return h.sendHost.ReadThread(context.Background(), id)
}
func (h *guardHost) SendMessage(_ context.Context, id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends++
	return h.sendHost.SendMessage(context.Background(), id, thread, message, settings)
}

func newGuardHost() *guardHost { return &guardHost{sendHost: &sendHost{status: "idle"}} }

// seedMessage inserts a message to the fixture recipient with the given state and staging time, then applies
// the SQL assignments (for example "hold_reason='held'") to it.
func seedMessage(t *testing.T, f *stageFixture, id, state, stagedAt string, assignments ...string) {
	t.Helper()
	sweepExec(t, f, fmt.Sprintf("INSERT INTO supervisor_messages(message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,staged_at,updated_at) VALUES ('%s','obl-%s','report','rel-1','PRJ-1','completion','notification','parent','supervisor','s-%s','{}','%s','%s','%s')", id, id, id, state, stagedAt, stagedAt))
	for _, a := range assignments {
		sweepExec(t, f, fmt.Sprintf("UPDATE supervisor_messages SET %s WHERE message_id='%s'", a, id))
	}
}

const olderThanFixture = "2023-11-14T22:13:10.000000+00:00"

func journalCount(t *testing.T, f *stageFixture) int {
	t.Helper()
	var n int
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM journal").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func attemptCount(t *testing.T, f *stageFixture, id string) int {
	t.Helper()
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return len(attempts)
}

func wantRefusal(t *testing.T, err error) Refusal {
	t.Helper()
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "not_claimable" {
		t.Fatalf("want a not_claimable refusal, got %v", err)
	}
	return refusal
}

func TestAttempt_an_older_claimable_message_refuses_before_any_host_work(t *testing.T) {
	t.Parallel()
	// Given: an older message to the same recipient that can be sent now, and the fixture's own message.
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	id := stagedID(t, f)
	seedMessage(t, f, "older-claimable", "queued", olderThanFixture)
	host := newGuardHost()
	before, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	journalBefore := journalCount(t, f)
	// When: the newer message is attempted.
	record, err := f.c.Attempt(f.ctx, id, host, guardNow)
	// Then: the attempt's own check refuses, naming the older message, before the host is asked anything.
	refusal := wantRefusal(t, err)
	if record != nil || !strings.Contains(refusal.Detail, "older-claimable") || !strings.Contains(refusal.Detail, "an ordered selection can have on its own") {
		t.Fatalf("refusal %+v record %v", refusal, record)
	}
	after, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if host.reads != 0 || host.sends != 0 || attemptCount(t, f, id) != 0 || journalCount(t, f) != journalBefore || after != before {
		t.Fatalf("host reads %d sends %d, attempts %d, journal %d -> %d, row %+v -> %+v", host.reads, host.sends, attemptCount(t, f, id), journalBefore, journalCount(t, f), before, after)
	}
}

func TestClaim_an_older_message_made_claimable_after_the_pre_check_is_refused_under_the_lock(t *testing.T) {
	t.Parallel()
	// Given: an older message that is held, so the attempt's check lets the newer one through, and a writer
	// that releases the hold between that check and the claim's read.
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	id := stagedID(t, f)
	seedMessage(t, f, "older-held", "queued", olderThanFixture, "hold_reason='held'")
	f.c.beforeClaimRead = func(tx context.Context) {
		if _, err := f.s.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL WHERE message_id='older-held'"); err != nil {
			t.Error(err)
		}
		f.c.beforeClaimRead = nil
	}
	host := newGuardHost()
	// When: the newer message is attempted.
	record, err := f.c.Attempt(f.ctx, id, host, guardNow)
	// Then: the claim refuses under its lock with its own wording, after the host work the pre-check lets through,
	// and nothing of the attempt is left behind.
	refusal := wantRefusal(t, err)
	if record != nil || !strings.Contains(refusal.Detail, "older-held") || strings.Contains(refusal.Detail, "ordered selection") {
		t.Fatalf("refusal %+v record %v", refusal, record)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if host.reads == 0 || host.sends != 0 || attemptCount(t, f, id) != 0 || row.State != "queued" || row.AttemptCount != 0 {
		t.Fatalf("host reads %d sends %d, attempts %d, row %+v", host.reads, host.sends, attemptCount(t, f, id), row)
	}
}

func TestClaim_a_hold_on_an_older_message_that_became_in_flight_does_not_block_under_the_lock(t *testing.T) {
	t.Parallel()
	// Characterization, not endorsement: under the claim's lock a hold excludes a message whatever its state,
	// while the attempt's check ignores a hold on a message in flight (store.SupervisorAheadInClaimSQL and
	// store.SupervisorAheadSQL). Which form the claim should use is a decision for the owner; this pins the
	// behavior the refactor preserved (docs/port/refactor-backlog.md).
	// Given: an older message that is not due (so the attempt's check lets the newer one through) and a writer
	// that moves it into flight with a hold between that check and the claim's read.
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	id := stagedID(t, f)
	seedMessage(t, f, "older-later", "queued", olderThanFixture, fmt.Sprintf("next_eligible_at=%d", guardNow+1000))
	f.c.beforeClaimRead = func(tx context.Context) {
		if _, err := f.s.Q(tx).ExecContext(tx, fmt.Sprintf("UPDATE supervisor_messages SET state='sending',hold_reason='parked',lease_owner='other',lease_until=%d WHERE message_id='older-later'", guardNow+300)); err != nil {
			t.Error(err)
		}
		f.c.beforeClaimRead = nil
	}
	host := newGuardHost()
	// When: the newer message is attempted.
	record, err := f.c.Attempt(f.ctx, id, host, guardNow)
	// Then: the claim does not count the held in-flight message as ahead, and the send goes out.
	if err != nil || record == nil || record["deliveryState"] != "dispatched" || host.sends != 1 {
		t.Fatalf("record %v err %v sends %d", record, err, host.sends)
	}
}

func TestAutoHeads_pages_one_head_per_recipient_whatever_kind_of_older_message_stands_in_front(t *testing.T) {
	t.Parallel()
	// Given: pairs of messages to separate recipients, each pair's older message of a different kind.
	f := fixture24(t)
	at := func(n int) string { return fmt.Sprintf("2023-11-14T22:13:%02d.000000+00:00", n) }
	type pair struct {
		recipient      string
		older, younger string
		olderState     string
		assignments    []string
		heads          []string // what the pass pages, by id
	}
	pairs := []pair{
		{"r-claimable", "a1", "a2", "queued", nil, []string{"a1"}},
		{"r-alone", "b1", "", "queued", nil, []string{"b1"}},
		{"r-stranded", "c1", "c2", "sending", []string{fmt.Sprintf("lease_until=%d", guardNow-10)}, []string{"c1"}},
		{"r-in-flight", "d1", "d2", "sending", []string{fmt.Sprintf("lease_until=%d", guardNow+100)}, []string{"d2"}},
		{"r-held", "e1", "e2", "queued", []string{"hold_reason='held'"}, []string{"e2"}},
		{"r-not-due", "f1", "f2", "queued", []string{fmt.Sprintf("next_eligible_at=%d", guardNow+100)}, []string{"f2"}},
		{"r-sent", "g1", "g2", "dispatched", nil, []string{"g2"}},
	}
	n := 0
	var want []string
	for _, p := range pairs {
		n++
		seedMessage(t, f, p.older, p.olderState, at(n), append([]string{"recipient_task_id='" + p.recipient + "'"}, p.assignments...)...)
		if p.younger != "" {
			n++
			seedMessage(t, f, p.younger, "queued", at(n), "recipient_task_id='"+p.recipient+"'")
		}
		want = append(want, p.heads...)
	}
	heads := func(limit int) string {
		t.Helper()
		rows, err := f.c.autoHeads(f.ctx, guardNow, limit, "", "")
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.MessageID)
		}
		return strings.Join(ids, ",")
	}
	// When: the pass selects its heads, over the whole backlog and over a page of three.
	// Then: each recipient contributes the oldest message that can be attempted, and a recipient's backlog
	// does not consume the page.
	if got, wantAll := heads(100), strings.Join(want, ","); got != wantAll {
		t.Fatalf("heads %s, want %s", got, wantAll)
	}
	if got, wantPage := heads(3), strings.Join(want[:3], ","); got != wantPage {
		t.Fatalf("page of three %s, want %s", got, wantPage)
	}
}

func TestEligibleSupervisorMessages_lists_what_can_be_claimed_in_staging_order(t *testing.T) {
	t.Parallel()
	// Given: messages that can be claimed now, and messages held, not yet due, sending or sent.
	f := fixture24(t)
	at := func(n int) string { return fmt.Sprintf("2023-11-14T22:13:%02d.000000+00:00", n) }
	seedMessage(t, f, "m1", "withheld_pre_send", at(1))
	seedMessage(t, f, "m2", "queued", at(2), "hold_reason='held'")
	seedMessage(t, f, "m3", "deferred_busy", at(3), fmt.Sprintf("next_eligible_at=%d", guardNow-5))
	seedMessage(t, f, "m4", "queued", at(4), fmt.Sprintf("next_eligible_at=%d", guardNow+5))
	seedMessage(t, f, "m5", "sending", at(5), fmt.Sprintf("lease_until=%d", guardNow+5))
	seedMessage(t, f, "m6", "dispatched", at(6))
	seedMessage(t, f, "m7", "queued", at(7), fmt.Sprintf("next_eligible_at=%d", guardNow))
	// When: the eligible messages are listed.
	rows, err := storeseed.EligibleSupervisorMessages(f.ctx, f.s, guardNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.MessageID)
	}
	// Then: only the claimable ones appear, oldest first, including one due exactly now.
	if strings.Join(got, ",") != "m1,m3,m7" {
		t.Fatalf("eligible %v", got)
	}
}

func TestHoldUnaddressed_holds_only_a_message_that_has_not_been_sent(t *testing.T) {
	t.Parallel()
	// Given: one message per state the channel records.
	f := fixture24(t)
	states := []string{"queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "dispatched", "read"}
	for i, state := range states {
		seedMessage(t, f, "s-"+state, state, fmt.Sprintf("2023-11-14T22:13:%02d.000000+00:00", i))
	}
	for _, state := range states {
		row, err := f.c.Get(f.ctx, "s-"+state)
		if err != nil {
			t.Fatal(err)
		}
		// When: the channel holds the message as unaddressed.
		if err := f.c.holdUnaddressed(f.ctx, row, Refusal{"unregistered_scope", "nobody above"}, guardNow); err != nil {
			t.Fatal(err)
		}
		// Then: the hold lands on a message in an unsent state and on no other.
		held, err := f.c.Get(f.ctx, "s-"+state)
		if err != nil {
			t.Fatal(err)
		}
		if want := store.SupervisorUnsent(state); held.HoldReason.Valid != want {
			t.Fatalf("state %s: hold %+v, want held=%v", state, held.HoldReason, want)
		}
	}
}

func TestSupervisorUnsentStatesAreTheStatesThatQueueADelivery(t *testing.T) {
	t.Parallel()
	// The supervisor_messages states and the delivery states share their spelling; the condition lists them once.
	for _, state := range []string{delivery.Queued, delivery.DeferredBusy, delivery.WithheldPreSend} {
		if !store.SupervisorUnsent(state) {
			t.Fatalf("%s should be unsent", state)
		}
	}
	for _, state := range []string{delivery.Sending, delivery.HeldUncertain, delivery.Dispatched, delivery.InboxOnly, delivery.Acknowledged, delivery.Superseded, "read", ""} {
		if store.SupervisorUnsent(state) {
			t.Fatalf("%q should not be unsent", state)
		}
	}
}

func TestAttempt_leaves_a_message_that_is_held_or_not_yet_due_alone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		assignments []string
		sent        bool
	}{
		{"held", []string{"hold_reason='held'"}, false},
		{"not yet due", []string{fmt.Sprintf("next_eligible_at=%d", guardNow+10)}, false},
		{"due exactly now", []string{fmt.Sprintf("next_eligible_at=%d", guardNow)}, true},
		{"already dispatched", []string{"state='dispatched'"}, false},
		{"held uncertain", []string{"state='held_uncertain'"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given: the fixture's message with one thing about it changed.
			f := fixture24(t)
			f.c.Settings = &delivery.TaskSettings{}
			id := stagedID(t, f)
			for _, a := range tc.assignments {
				sweepExec(t, f, fmt.Sprintf("UPDATE supervisor_messages SET %s WHERE message_id='%s'", a, id))
			}
			host := newGuardHost()
			before, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			// When: it is attempted.
			record, err := f.c.Attempt(f.ctx, id, host, guardNow)
			// Then: a message that cannot be claimed now is passed over without a word to the host,
			// and one that can is sent.
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.sent {
				if record == nil || host.sends != 1 {
					t.Fatalf("record %v sends %d", record, host.sends)
				}
				return
			}
			if record != nil || host.reads != 0 || host.sends != 0 || after != before {
				t.Fatalf("record %v reads %d sends %d, row %+v -> %+v", record, host.reads, host.sends, before, after)
			}
		})
	}
}

func TestDeferAutoFault_moves_only_an_unheld_unsent_message(t *testing.T) {
	t.Parallel()
	// Given: one message per state, and one held message.
	f := fixture24(t)
	states := []string{"queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "dispatched", "read"}
	for i, state := range states {
		seedMessage(t, f, "d-"+state, state, fmt.Sprintf("2023-11-14T22:13:%02d.000000+00:00", i))
	}
	seedMessage(t, f, "d-held", "queued", "2023-11-14T22:13:30.000000+00:00", "hold_reason='held'")
	for _, id := range append(states, "held") {
		// When: an attempt that failed in an unclassified way is deferred.
		journalBefore := journalCount(t, f)
		if err := f.c.deferAutoFault(f.ctx, "d-"+id, guardNow, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, "d-"+id)
		if err != nil {
			t.Fatal(err)
		}
		// Then: only a message that is unsent and carries no hold is pushed back, and the fault is journaled once.
		moved := store.SupervisorUnsent(id) && id != "held"
		if row.NextEligibleAt.Valid != moved || (journalCount(t, f)-journalBefore == 1) != moved {
			t.Fatalf("%s: next_eligible_at %+v, journal rows %d, want moved=%v", id, row.NextEligibleAt, journalCount(t, f)-journalBefore, moved)
		}
	}
}

func TestStage_restates_only_a_message_nothing_of_which_has_gone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, state, attempt string
		restated             bool
	}{
		{"queued", "queued", "", true},
		{"deferred busy", "deferred_busy", "", true},
		{"withheld before the send", "withheld_pre_send", "", true},
		{"queued after an attempt that sent nothing and was retry safe", "queued", "no/1", true},
		{"queued after an attempt that sent", "queued", "yes/1", false},
		{"queued after an attempt whose retry was not shown safe", "queued", "no/0", false},
		{"sending", "sending", "", false},
		{"dispatched", "dispatched", "", false},
		{"held uncertain", "held_uncertain", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given: a staged message that was composed from an older statement than the obligation now rests on.
			f := fixture24(t)
			o, staged := f.staged(t)
			id := staged["messageId"].(string)
			sweepExec(t, f, fmt.Sprintf("UPDATE supervisor_messages SET state='%s', event_id='older-event' WHERE message_id='%s'", tc.state, id))
			if tc.attempt != "" {
				sent, safe, _ := strings.Cut(tc.attempt, "/")
				sweepExec(t, f, fmt.Sprintf("INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, observed_at) VALUES ('req-1','%s',1,'m','withheld_pre_send','%s',%s,'{}','t','t')", id, sent, safe))
			}
			// When: the obligation is staged again.
			result, err := f.c.Stage(f.ctx, o, "", f.at)
			if err != nil {
				t.Fatal(err)
			}
			// Then: the packet is restated for a message nothing of which has gone, and left alone otherwise.
			row, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := result["restated"] == true; got != tc.restated || (row.EventID.String == "event-1") != tc.restated {
				t.Fatalf("restated %v (want %v), event %q", result["restated"], tc.restated, row.EventID.String)
			}
		})
	}
}

func TestAttempt_releases_a_derived_hold_only_from_an_unsent_message(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{"hierarchy_unresolved", "superseded_by_report"} {
		for _, state := range []string{"queued", "deferred_busy", "withheld_pre_send", "held_uncertain", "dispatched"} {
			t.Run(hold+"/"+state, func(t *testing.T) {
				// Given: a message holding a derived hold that the live hierarchy and the report no longer justify.
				f := fixture24(t)
				f.c.Settings = &delivery.TaskSettings{}
				id := stagedID(t, f)
				// A message that has left the queue also holds a packet that no longer says what is owed.
				stale := ""
				if !store.SupervisorUnsent(state) {
					stale = ", packet='" + stalePacket + "'"
				}
				sweepExec(t, f, fmt.Sprintf("UPDATE supervisor_messages SET state='%s', hold_reason='%s'%s WHERE message_id='%s'", state, hold, stale, id))
				reopened := func() int {
					var n int
					if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM journal WHERE kind='supervisor_message_reopened'").Scan(&n); err != nil {
						t.Fatal(err)
					}
					return n
				}
				// When: it is attempted.
				_, _ = f.c.Attempt(f.ctx, id, newGuardHost(), guardNow)
				// Then: the hold is released, and journaled, from an unsent message and from no other.
				row, err := f.c.Get(f.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				// The same attempt never rewrites the packet of a message that has left the queue.
				if want := store.SupervisorUnsent(state); (reopened() == 1) != want || (!row.HoldReason.Valid) != want || (!want && row.Packet != stalePacket) {
					t.Fatalf("reopen journal rows %d, hold %+v, packet kept %v, want released=%v", reopened(), row.HoldReason, row.Packet == stalePacket, want)
				}
			})
		}
	}
}

const stalePacket = "{\"stale\":true}"

func TestStage_releases_a_hierarchy_hold_only_from_an_unsent_message(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"queued", "deferred_busy", "withheld_pre_send", "held_uncertain", "dispatched"} {
		t.Run(state, func(t *testing.T) {
			// Given: a staged message holding a hierarchy hold that the live hierarchy no longer justifies.
			f := fixture24(t)
			o, staged := f.staged(t)
			id := staged["messageId"].(string)
			sweepExec(t, f, fmt.Sprintf("UPDATE supervisor_messages SET state='%s', hold_reason='hierarchy_unresolved' WHERE message_id='%s'", state, id))
			// When: the obligation is staged again.
			if _, err := f.c.Stage(f.ctx, o, "", f.at); err != nil {
				t.Fatal(err)
			}
			// Then: the hold is released from an unsent message and from no other.
			row, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if want := store.SupervisorUnsent(state); (!row.HoldReason.Valid) != want {
				t.Fatalf("state %s: hold %+v, want released=%v", state, row.HoldReason, want)
			}
		})
	}
}
