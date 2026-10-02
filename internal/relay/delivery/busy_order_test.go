package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

// CRW-261: the busy backoff counts busy answers, and a delivery does not overtake an older delivery
// to the same recipient that is waiting out a busy backoff.
//
// The world is the one scale_test.go builds: one parent, one relationship (one child) per index, the
// daemon's delivery pass over a fake host. A busy parent is the fake host's status "active".

// journaledBusyAnswers is how many busy answers the journal holds for a delivery.
func journaledBusyAnswers(f *fixture, event string) int64 {
	return f.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'delivery_deferred_busy' AND subject = ?", event)
}

// busy makes the parent mid-turn or idle.
func (w *scaleWorld) busy(is bool) {
	status := "idle"
	if is {
		status = "active"
	}
	w.f.host.threads[scaleParent].status = status
}

// pass is one delivery pass at the clock's instant. The clock is the test's to move.
func (w *scaleWorld) pass() {
	w.t.Helper()
	mustDo(w.t, w.sc.Deliver(w.f.ctx, w.f.host, w.f.clock.Now(), &TickCounts{}))
}

// answerBusy has the busy parent answer one attempt of the delivery at the clock's instant, checks that
// the delivery now waits the delay the curve gives for this answer, and moves the clock to the instant it
// is due again.
func answerBusy(t *testing.T, f *fixture, event string, answer int64) *Row {
	t.Helper()
	now := f.clock.Now()
	if record := f.mustAttempt(event, at(now)); record != nil {
		t.Fatalf("answer %d: a busy recipient returns nothing, got %v", answer, record)
	}
	row := f.row(event)
	policy := f.delivery.Policy
	if got, want := row.F("next_eligible_at")-now, policy.DelayFor(answer, "busy"); math.Abs(got-want) > 1e-6 {
		t.Fatalf("after busy answer %d the delivery waits %.0f s, want %.0f s (BusyBase %.0f s doubling to BusyMax %.0f s)", answer, got, want, policy.BusyBase, policy.BusyMax)
	}
	f.clock.T = row.F("next_eligible_at")
	return &row
}

// busyUntilDue has the busy parent answer n attempts of the delivery, each at the instant the previous
// answer made it due, without judging the delays.
func busyUntilDue(t *testing.T, f *fixture, event string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if record := f.mustAttempt(event, at(f.clock.Now())); record != nil {
			t.Fatalf("a busy recipient returns nothing, got %v", record)
		}
		f.clock.T = f.row(event).F("next_eligible_at")
	}
}

func TestBusy_the_backoff_grows_with_each_busy_answer_and_reaches_the_cap(t *testing.T) {
	t.Run("the delay doubles from BusyBase up to BusyMax", func(t *testing.T) {
		w := newScaleWorld(t, 1)
		w.busy(true)
		event := w.emit(0)
		for answer := int64(1); answer <= 8; answer++ {
			answerBusy(t, w.f, event, answer)
		}
	})
	t.Run("the delivery is held with busy_cap on the BusyMaxAttempts-th answer and not before", func(t *testing.T) {
		w := newScaleWorld(t, 1)
		f := w.f
		w.busy(true)
		event := w.emit(0)
		capAnswers := f.delivery.Policy.BusyMaxAttempts
		for answer := int64(1); answer <= capAnswers; answer++ {
			now := f.clock.Now()
			if record := f.mustAttempt(event, at(now)); record != nil {
				t.Fatalf("answer %d: a busy recipient returns nothing, got %v", answer, record)
			}
			row := f.row(event)
			if answer < capAnswers && !row.N("hold_reason") {
				t.Fatalf("held with %s after %d busy answers, before the cap %d", row.S("hold_reason"), answer, capAnswers)
			}
			f.clock.T = row.F("next_eligible_at")
		}
		row := f.row(event)
		if row.S("hold_reason") != BusyCap || row.S("state") != DeferredBusy {
			t.Errorf("after %d busy answers the delivery is %s with hold %q, want %s held with %s", capAnswers, row.S("state"), row.S("hold_reason"), DeferredBusy, BusyCap)
		}
		if n := journaledBusyAnswers(f, event); n != capAnswers {
			t.Errorf("%d busy answers are journaled, want %d", n, capAnswers)
		}
		if row.I("attempt_count") != 0 || f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", event) != 0 || len(f.host.sends) != 0 {
			t.Errorf("a busy recipient is never claimed or sent to: attempt_count %d, sends %d", row.I("attempt_count"), len(f.host.sends))
		}
		// A held delivery is no longer due, however long the recipient stays busy.
		f.clock.Advance(1e6)
		if len(f.eligible()) != 0 || f.mustAttempt(event, at(f.clock.Now())) != nil {
			t.Error("a delivery held at the busy cap is still attempted")
		}
	})
	t.Run("the count survives a restart", func(t *testing.T) {
		w := newScaleWorld(t, 1)
		f := w.f
		w.busy(true)
		event := w.emit(0)
		for answer := int64(1); answer <= 5; answer++ {
			answerBusy(t, f, event, answer)
		}
		f.delivery = NewService(f.store, f.clock)
		answerBusy(t, f, event, 6)
	})
}

// The same walk through the daemon's delivery pass, a tick every 20 s: the recipient is attempted
// when its delivery is due, so the 40th answer comes 20+40+60+120+240+34*300 seconds after the first.
func TestBusy_the_scheduler_walks_a_busy_recipient_to_the_cap(t *testing.T) {
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	capAnswers := f.delivery.Policy.BusyMaxAttempts
	answerAt := map[int64]float64{}
	var seen int64
	ticks := 0
	for ; ticks < 700; ticks++ {
		w.pass()
		if n := journaledBusyAnswers(f, event); n > seen {
			seen = n
			answerAt[n] = f.clock.Now()
		}
		if !f.row(event).N("hold_reason") {
			break
		}
		f.clock.Advance(scaleTick)
	}
	if seen != capAnswers {
		t.Fatalf("after %d ticks (%.0f s) the scheduler had recorded %d busy answers, want %d", ticks, float64(ticks)*scaleTick, seen, capAnswers)
	}
	want := 0.0
	for k := int64(1); k < capAnswers; k++ {
		want += math.Ceil(f.delivery.Policy.DelayFor(k, "busy")/scaleTick) * scaleTick
	}
	if got := answerAt[capAnswers] - answerAt[1]; math.Abs(got-want) > 1e-6 {
		t.Errorf("the %dth answer came %.0f s after the first, want %.0f s", capAnswers, got, want)
	}
	if hold := f.row(event).S("hold_reason"); hold != BusyCap {
		t.Errorf("the delivery is held with %q, want %s", hold, BusyCap)
	}
	if len(f.host.sends) != 0 {
		t.Errorf("%d messages went to a busy recipient", len(f.host.sends))
	}
}

// Pre-claim answers hold on the combined count (pre-claim answers and attempts the transport found
// busy); a transport-only streak is held by settle's own attempt cap, as before.
func TestBusy_the_combined_count_holds_a_delivery_a_transport_also_found_busy(t *testing.T) {
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	capAnswers := f.delivery.Policy.BusyMaxAttempts
	busyUntilDue(t, f, event, int(capAnswers-1))
	if row := f.row(event); !row.N("hold_reason") {
		t.Fatalf("held with %s after %d answers", row.S("hold_reason"), capAnswers-1)
	}
	// The recipient reads idle, but the transport finds it busy: a claimed attempt that settles as busy.
	w.busy(false)
	f.host.script = []string{"busy"}
	now := f.clock.Now()
	record := f.mustAttempt(event, at(now))
	if record == nil || str(record, "deliveryState") != DeferredBusy {
		t.Fatalf("the transport's busy answer was not recorded as a deferred attempt: %v", record)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ? AND state = ?", event, DeferredBusy); n != 1 {
		t.Fatalf("%d attempts settled as busy, want 1", n)
	}
	row := f.row(event)
	if !row.N("hold_reason") {
		t.Fatalf("the transport answer held the delivery with %s: settle counts its own attempts", row.S("hold_reason"))
	}
	if got, want := row.F("next_eligible_at")-now, f.delivery.Policy.DelayFor(2, "busy"); math.Abs(got-want) > 1e-6 {
		t.Fatalf("settle waits %.0f s after its first attempt, want %.0f s", got, want)
	}
	// The next pre-claim answer sees 39 + 1 busy answers before it and holds.
	f.clock.T = row.F("next_eligible_at")
	w.busy(true)
	if record := f.mustAttempt(event, at(f.clock.Now())); record != nil {
		t.Fatalf("a busy recipient returns nothing, got %v", record)
	}
	if hold := f.row(event).S("hold_reason"); hold != BusyCap {
		t.Errorf("the pre-claim answer after %d busy answers left the delivery with hold %q, want %s", capAnswers, hold, BusyCap)
	}
}

// accepted is the events of the messages the host accepted, in order: a send the transport answered
// busy is in the host's list too, and is not a delivery.
func accepted(f *fixture) []string {
	var out []string
	for _, s := range f.host.sends {
		if s.outcome == "accepted" {
			out = append(out, f.one("SELECT event_id FROM attempts WHERE request_id = ?", s.requestID).S("event_id"))
		}
	}
	return out
}

// overtaking is the c2 situation: an older delivery O waits out a busy backoff, a newer delivery Y to
// the same recipient exists and the recipient is idle again.
type overtaking struct {
	w            *scaleWorld
	older, newer string
	// due is the instant O's backoff ends.
	due float64
	// t0 is when O first met the busy recipient.
	t0 float64
}

// newOvertaking builds it. The newer delivery is a second event of relationship 0 when sameRelationship
// is set, else an event of relationship 1. With transport the older delivery meets an idle-looking
// recipient whose transport answers busy (a claimed attempt, a 30 s backoff); otherwise it meets a
// busy recipient before any claim (a 15 s backoff).
func newOvertaking(t *testing.T, sameRelationship, transport bool) *overtaking {
	t.Helper()
	w := newScaleWorld(t, 2)
	f := w.f
	o := &overtaking{w: w, t0: f.clock.Now()}
	if transport {
		f.host.script = []string{"busy"}
	} else {
		w.busy(true)
	}
	o.older = w.emit(0)
	w.pass()
	row := f.row(o.older)
	if row.S("state") != DeferredBusy || !row.N("hold_reason") {
		t.Fatalf("the older delivery is %s with hold %q, want a busy backoff", row.S("state"), row.S("hold_reason"))
	}
	if transport && f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ? AND state = ?", o.older, DeferredBusy) != 1 {
		t.Fatal("the older delivery's busy answer did not come from the transport")
	}
	o.due = row.F("next_eligible_at")
	f.clock.Advance(1)
	if sameRelationship {
		o.newer = w.emit(0)
	} else {
		o.newer = w.emit(1)
	}
	w.busy(false)
	return o
}

func TestBusy_a_newer_delivery_does_not_overtake_an_older_one_in_backoff(t *testing.T) {
	// The daemon passes at the given interval until both have gone out; no pass sends anything before
	// the older delivery is due, and the two go out in creation order.
	walk := func(t *testing.T, o *overtaking, interval float64) {
		t.Helper()
		f := o.w.f
		for step := 0; step < 40; step++ {
			f.clock.Advance(interval)
			sent := len(accepted(f))
			o.w.pass()
			if f.clock.Now() < o.due && len(accepted(f)) > sent {
				t.Fatalf("at +%.0f s the newer delivery went out while the older one waits out a busy backoff until +%.0f s", f.clock.Now()-o.t0, o.due-o.t0)
			}
			if len(accepted(f)) == 2 {
				break
			}
		}
		if got := accepted(f); !slices.Equal(got, []string{o.older, o.newer}) {
			t.Fatalf("sent in order %v, want the older then the newer %v", got, []string{o.older, o.newer})
		}
	}
	for _, c := range []struct {
		name                        string
		sameRelationship, transport bool
		interval                    float64
	}{
		{"a busy recipient before any claim, a 5 s poll, the same relationship", true, false, 5},
		{"a busy recipient before any claim, a 5 s poll, another relationship", false, false, 5},
		{"a busy answer from the transport, a 20 s tick, the same relationship", true, true, scaleTick},
		{"a busy answer from the transport, a 20 s tick, another relationship", false, true, scaleTick},
	} {
		t.Run(c.name, func(t *testing.T) {
			walk(t, newOvertaking(t, c.sameRelationship, c.transport), c.interval)
		})
	}
	// Each place that decides whether a delivery may go refuses the newer one on its own.
	t.Run("the due list does not offer it", func(t *testing.T) {
		o := newOvertaking(t, false, false)
		f := o.w.f
		f.clock.Advance(4)
		rows, err := f.delivery.EligibleRows(f.ctx, scaleParent, f.clock.Now())
		mustDo(t, err)
		for _, r := range rows {
			if r.S("event_id") == o.newer {
				t.Fatal("the newer delivery is due while the older one waits out a busy backoff")
			}
		}
		if ids := f.eligible(); slices.Contains(ids, o.newer) {
			t.Fatalf("Eligible lists the newer delivery: %v", ids)
		}
	})
	t.Run("a direct attempt does not take it, and does not even read the host", func(t *testing.T) {
		o := newOvertaking(t, false, false)
		f := o.w.f
		f.clock.Advance(4)
		c := &counted{fakeHost: f.host}
		record, err := f.delivery.Attempt(f.ctx, o.newer, c, at(f.clock.Now()), "")
		mustDo(t, err)
		if record != nil || len(f.host.sends) != 0 {
			t.Fatalf("a direct attempt sent the newer delivery: %v", record)
		}
		if len(c.calls) != 0 {
			t.Fatalf("the attempt read the host (%v) before deciding it was not its turn", c.calls)
		}
		row := f.row(o.newer)
		if row.S("state") != Queued || row.I("attempt_count") != 0 {
			t.Fatalf("the newer delivery moved: %s after %d attempts", row.S("state"), row.I("attempt_count"))
		}
	})
	t.Run("a direct claim does not take it", func(t *testing.T) {
		o := newOvertaking(t, false, false)
		f := o.w.f
		f.clock.Advance(4)
		_, err := f.delivery.claim(f.ctx, o.newer, f.clock.Now(), "relay", scaleParent)
		if !errors.Is(err, errNotClaimable) {
			t.Fatalf("claim of the newer delivery: %v, want it refused as not claimable", err)
		}
		if n := f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", o.newer); n != 0 {
			t.Fatalf("the refused claim left %d attempt rows", n)
		}
	})
	t.Run("a busy recipient gives the newer delivery no answer of its own", func(t *testing.T) {
		o := newOvertaking(t, false, false)
		f := o.w.f
		f.clock.Advance(4)
		o.w.busy(true)
		if record := f.mustAttempt(o.newer, at(f.clock.Now())); record != nil {
			t.Fatalf("got %v", record)
		}
		row := f.row(o.newer)
		if row.S("state") != Queued || journaledBusyAnswers(f, o.newer) != 0 ||
			f.count("SELECT COUNT(*) AS c FROM failed_operations WHERE scope_key = ?", o.newer) != 0 {
			t.Fatalf("the newer delivery was deferred as busy (%s) though it never met the recipient", row.S("state"))
		}
	})
}

// A wait that is not a busy backoff leaves the delivery behind it free, and so does a busy backoff whose
// delivery a claim could not take anyway. A resumed relationship holds its line again.
func TestBusy_only_a_busy_backoff_holds_the_line(t *testing.T) {
	// heldBack is O waiting out a busy backoff (two answers, due at +45 s) with Y queued behind it at +16 s.
	heldBack := func(t *testing.T) (*overtaking, string, float64) {
		t.Helper()
		w := newScaleWorld(t, 2)
		f := w.f
		o := &overtaking{w: w, t0: f.clock.Now()}
		w.busy(true)
		o.older = w.emit(0)
		busyUntilDue(t, f, o.older, 2)
		o.due = f.row(o.older).F("next_eligible_at")
		f.clock.T = o.t0 + 16
		o.newer = w.emit(1)
		w.busy(false)
		return o, w.rels[0].rid, f.clock.Now()
	}
	// listed reports whether Y is due, and claimable and sent when attempted; the two must agree.
	check := func(t *testing.T, o *overtaking, now float64, blocked bool) {
		t.Helper()
		f := o.w.f
		rows, err := f.delivery.EligibleRows(f.ctx, scaleParent, now)
		mustDo(t, err)
		listed := false
		for _, r := range rows {
			listed = listed || r.S("event_id") == o.newer
		}
		if listed == blocked {
			t.Fatalf("the newer delivery listed as due is %v, want %v", listed, !blocked)
		}
		record := f.mustAttempt(o.newer, at(now))
		sent := record != nil && str(record, "deliveryState") == Dispatched
		if sent == blocked {
			t.Fatalf("a direct attempt of the newer delivery sent=%v, want %v (the list said %v)", sent, !blocked, listed)
		}
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, o *overtaking, rid string, now float64)
	}{
		{"it is held at the busy cap", func(t *testing.T, o *overtaking, _ string, _ float64) {
			o.w.exec("UPDATE deliveries SET hold_reason = ? WHERE event_id = ?", BusyCap, o.older)
		}},
		{"it was withheld before the send", func(t *testing.T, o *overtaking, _ string, _ float64) {
			o.w.exec("UPDATE deliveries SET state = ? WHERE event_id = ?", WithheldPreSend, o.older)
		}},
		{"its relationship is paused", func(t *testing.T, o *overtaking, rid string, _ float64) {
			o.w.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", rid)
		}},
		{"its event belongs to an earlier generation", func(t *testing.T, o *overtaking, rid string, _ float64) {
			o.w.exec("UPDATE relationships SET execution_generation = 2 WHERE relationship_id = ?", rid)
		}},
		{"its relationship has spent the hourly cap on delivery attempts", func(t *testing.T, o *overtaking, rid string, now float64) {
			o.w.f.spendHour(rid, scaleParent, int(o.w.f.delivery.Policy.MaxSendsPerRelationshipPerHour), now)
		}},
		{"its relationship has spent the hourly cap on supervisor transports", func(t *testing.T, o *overtaking, rid string, now float64) {
			stamp := ISOOf(math.Floor(now/3600) * 3600)
			o.w.exec("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES ('m-cap','m-cap','report',?,'p','k','sender',?,'s','{}','sent',?,?)", rid, scaleParent, stamp, stamp)
			for i := 1; i <= int(o.w.f.delivery.Policy.MaxSendsPerRelationshipPerHour); i++ {
				o.w.exec("INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES (?,'m-cap',?,'m','settled','unknown',0,'{}',?,?,?)", fmt.Sprintf("m-cap-t%d", i), i, stamp, stamp, stamp)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, rid, now := heldBack(t)
			c.setup(t, o, rid, now)
			check(t, o, now, false)
		})
	}
	t.Run("the delivery goes to another recipient", func(t *testing.T) {
		o, _, now := heldBack(t)
		f := o.w.f
		request := o.w.create(1)
		o.w.queue(request, Revision, o.w.rels[1].child)
		rows, err := f.delivery.EligibleRows(f.ctx, scaleParent, now)
		mustDo(t, err)
		var ids []string
		for _, r := range rows {
			ids = append(ids, r.S("event_id"))
		}
		if !slices.Contains(ids, request) {
			t.Fatalf("a request to the child is not due while a completion to the parent waits: %v", ids)
		}
		if record := f.mustAttempt(request, at(now)); record == nil || str(record, "deliveryState") != Dispatched {
			t.Fatalf("a request to the child was not sent: %v", record)
		}
	})
	t.Run("a relationship that is resumed holds its line again", func(t *testing.T) {
		o, rid, now := heldBack(t)
		f := o.w.f
		o.w.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", rid)
		check(t, o, now, false)
		o.w.exec("UPDATE relationships SET status = 'active' WHERE relationship_id = ?", rid)
		f.clock.Advance(1)
		o.newer = o.w.emit(1)
		check(t, o, f.clock.Now(), true)
	})
	t.Run("the delivery that is held back stays held back", func(t *testing.T) {
		o, _, now := heldBack(t)
		check(t, o, now, true)
	})
}

// The scheduler lists a recipient's rows before it attempts them. If the older row is deferred by a
// busy recipient after the listing (here by a direct attempt, as the relay CLI's deliver would make),
// the newer row's attempt ends the recipient's queue: it is not a refusal, so no marker makes the next
// tick start after it.
func TestBusy_the_scheduler_ends_the_queue_when_an_older_busy_row_appears_after_listing(t *testing.T) {
	w := newScaleWorld(t, 2)
	f := w.f
	w.busy(true)
	older := w.emit(0)
	f.clock.Advance(1)
	newer := w.emit(1)
	now := f.clock.Now()
	rows, err := f.delivery.EligibleRows(f.ctx, scaleParent, now)
	mustDo(t, err)
	if len(rows) != 2 || rows[0].S("event_id") != older || rows[1].S("event_id") != newer {
		t.Fatalf("listed %d rows, want the older then the newer", len(rows))
	}
	if record := f.mustAttempt(older, at(now)); record != nil {
		t.Fatalf("got %v", record)
	}
	w.busy(false)
	if refused := w.sc.attempt(f.ctx, f.host, rows[1], now, &TickCounts{}); refused {
		t.Error("the scheduler took the newer row's wait behind a busy older row for a refusal")
	}
	if len(f.host.sends) != 0 {
		t.Fatalf("the newer delivery was sent: %v", w.sentOrder())
	}
}

// A listing with many waiting heads is one pass over the rows, not a scan of the heads for each
// candidate, and a head's relationship is counted once however many deliveries it holds.
func TestBusy_many_waiting_heads_are_listed_in_linear_time(t *testing.T) {
	for _, shape := range []struct {
		name                  string
		recipients, followers int
	}{
		{"one younger delivery behind each of 2000 waiting heads", 2000, 1},
		{"ten younger deliveries behind each of 300 waiting heads", 300, 10},
	} {
		t.Run(shape.name, func(t *testing.T) {
			w := newScaleWorld(t, 1)
			f := w.f
			rel := w.rels[0]
			base := f.clock.Now()
			const free = 5
			seq := 0
			put := func(ctx context.Context, event, recipient, state string, next any) {
				stamp := ISOOf(base - 100000 + float64(seq)*0.01)
				seq++
				_, err := execSQL(ctx, f.store, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES (?,?,1,'h','ready_for_review','child',?,?,'completed','{}',?,?)", event, rel.rid, rel.child, rel.turn, stamp, stamp)
				mustDo(t, err)
				_, err = execSQL(ctx, f.store, "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, next_eligible_at, created_at, updated_at) VALUES (?,?,'completion',?,?,?,0,?,?,?)", event, rel.rid, recipient, recipient, state, next, stamp, stamp)
				mustDo(t, err)
			}
			mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
				for r := 0; r < shape.recipients; r++ {
					put(ctx, fmt.Sprintf("head-%d", r), fmt.Sprintf("rcpt-%d", r), DeferredBusy, base+250)
				}
				for r := 0; r < shape.recipients; r++ {
					for k := 0; k < shape.followers; k++ {
						put(ctx, fmt.Sprintf("follow-%d-%d", r, k), fmt.Sprintf("rcpt-%d", r), Queued, nil)
					}
				}
				for r := 0; r < free; r++ {
					put(ctx, fmt.Sprintf("free-%d", r), fmt.Sprintf("free-rcpt-%d", r), Queued, nil)
				}
				return nil
			}))
			began := time.Now()
			parents, err := f.delivery.EligibleParents(f.ctx, base)
			mustDo(t, err)
			rows, err := f.delivery.EligibleRows(f.ctx, scaleParent, base)
			mustDo(t, err)
			if elapsed := time.Since(began); elapsed > 3*time.Second {
				t.Errorf("listing %d deliveries behind %d waiting heads took %v", shape.recipients*(1+shape.followers)+free, shape.recipients, elapsed)
			}
			var ids []string
			for _, r := range rows {
				ids = append(ids, r.S("event_id"))
			}
			want := make([]string, free)
			for r := range want {
				want[r] = fmt.Sprintf("free-%d", r)
			}
			if !slices.Equal(parents, []string{scaleParent}) || !slices.Equal(ids, want) {
				t.Errorf("due: parents %v and %d rows; want the parent and only the %d deliveries that have no waiting head ahead of them", parents, len(ids), free)
			}
		})
	}
}

// A stale observation of a busy recipient cannot overwrite a hold that another path set between its
// read and its write, and a busy answer that changed nothing is not counted.
func TestBusy_a_stale_observation_cannot_overwrite_a_hold_set_in_between(t *testing.T) {
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	stale := f.row(event)
	_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET hold_reason = ? WHERE event_id = ?", AttemptCap, event)
	mustDo(t, err)
	mustDo(t, f.delivery.DeferBusy(f.ctx, event, stale, f.clock.Now()))
	row := f.row(event)
	if row.S("hold_reason") != AttemptCap || row.S("state") != Queued {
		t.Errorf("the hold is %q on a %s delivery, want %s kept on the queued one", row.S("hold_reason"), row.S("state"), AttemptCap)
	}
	if n := journaledBusyAnswers(f, event); n != 0 {
		t.Errorf("%d busy answers were journaled for a write that changed nothing", n)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM failed_operations WHERE scope_key = ?", event); n != 0 {
		t.Errorf("%d failure rows were recorded for a write that changed nothing", n)
	}
}
