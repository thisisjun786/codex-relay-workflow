package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-271: the order in which one observation pass settles the ends of turns, and what a settlement rests on when
// it commits. The fixture is lateStore: relationship "r", anchored at "anchor", with "business" and then
// "continuation" admitted after it. A staged child claim on "continuation" is what a managed child leaves when a
// goal-continuation turn emits from inside its turn.

// The earlier turns of the generation are read before "continuation" in these tests, because the one read of a turn
// with no staged claim that goes first (observe) takes the longest-waiting such turn, and the anchor ranks ahead of
// an admitted turn. The tests assert that order, so a change of the schedule cannot turn them into passes.

// A turn that ended failed or interrupted is not news once a later turn of its generation holds a final child
// receipt the parent is owed. When that receipt is the staged claim of a later turn that this very pass confirms,
// the earlier turn must be settled after it, whichever of the two the pass read first.
func TestAnEarlierTurnEndedBadlyIsNotReportedWhenALaterStagedClaimIsConfirmedInTheSamePass(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		settled  string // a turn settled before the tick, so the next one in line is read first
		observed string // the earlier turn the host reports as ended badly
		status   string
		reads    int // reads the first tick makes
	}{
		{"the anchor was interrupted", "", "anchor", "interrupted", 3},
		{"the anchor failed", "", "anchor", "failed", 3},
		{"the anchor is settled, the admitted turn was interrupted", "anchor", "business", "interrupted", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			lateClaim{"continuation", "child", "staged", ""}.insert(t, s)
			if c.settled != "" {
				exec(t, s, "INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES('r','child',?,'completed','2023-11-14T22:13:20Z')", c.settled)
			}
			host := &observationHost{status: "completed", statuses: map[string]string{c.observed: c.status}}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(host.reads) != c.reads || host.reads[0] != c.observed || slices.Index(host.reads, "continuation") < 1 {
				t.Fatalf("precondition: the pass read %v, want %d reads, %s first and continuation after it", host.reads, c.reads, c.observed)
			}
			if report.Observed != c.reads {
				t.Errorf("observed %d, want %d: %+v", report.Observed, c.reads, report)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id=?", c.observed); n != 0 {
				t.Errorf("daemon observations of %s: %d", c.observed, n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.producer='daemon_observation' AND e.turn_id=?", c.observed); n != 0 {
				t.Errorf("deliveries of the observation of %s: %d", c.observed, n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM delivery_intent i JOIN events e ON e.event_id=i.event_id WHERE e.producer='daemon_observation' AND e.turn_id=?", c.observed); n != 0 {
				t.Errorf("delivery intents of the observation of %s: %d", c.observed, n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE relationship_id='r' AND turn_id=? AND terminal_status=?", c.observed, c.status); n != 1 {
				t.Errorf("settlements of %s: %d", c.observed, n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM observations WHERE turn_id=? AND event_id IS NULL", c.observed); n != 1 {
				t.Errorf("observations of %s that name no event: %d", c.observed, n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind='observation_not_asserted' AND subject=?", c.observed); n != 1 {
				t.Errorf("journal rows saying %s was not asserted: %d", c.observed, n)
			}
			if noted := notesSaying(report.Notes, "not asserted"); noted != 1 {
				t.Errorf("notes %q: want one saying the observation was not asserted", report.Notes)
			}
			// The later turn's claim is the receipt that was confirmed, and the parent is owed it.
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE event_id='claim-continuation' AND stage='final'"); n != 1 {
				t.Errorf("the staged claim was not confirmed")
			}
			if n := count(t, s, "SELECT COUNT(*) FROM deliveries WHERE event_id='claim-continuation' AND state='queued'"); n != 1 {
				t.Errorf("deliveries of the confirmed claim: %d", n)
			}
			// Settled, so nothing is read again.
			if _, err = d.Tick(ctx); err != nil || len(host.reads) != c.reads {
				t.Fatalf("reads %v err %v", host.reads, err)
			}
		})
	}
}

// What the order must not change: with no later claim confirmed in the pass, the earlier end is reported as it
// always was.
func TestAnEarlierTurnEndedBadlyIsStillReportedWhenNoLaterClaimIsConfirmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		continuation string // what the host reports for the turn that holds the staged claim
		claim        string // the stage of its claim afterwards
		events       int    // daemon observations of the continuation turn
	}{
		{"the later turn is still running", "inProgress", "staged", 0},
		{"the later turn failed, so its claim is suppressed", "failed", "suppressed", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			lateClaim{"continuation", "child", "staged", ""}.insert(t, s)
			host := &observationHost{status: "completed", statuses: map[string]string{"anchor": "interrupted", "continuation": c.continuation}}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='anchor' AND outcome='interrupted'"); n != 1 {
				t.Errorf("daemon observations of anchor: %d", n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.producer='daemon_observation' AND e.turn_id='anchor'"); n != 1 {
				t.Errorf("deliveries of the observation of anchor: %d", n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='continuation'"); n != c.events {
				t.Errorf("daemon observations of continuation: %d, want %d", n, c.events)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE event_id='claim-continuation' AND stage=?", c.claim); n != 1 {
				t.Errorf("the claim is not %s", c.claim)
			}
			if noted := notesSaying(report.Notes, "not asserted"); noted != 0 {
				t.Errorf("notes %q", report.Notes)
			}
		})
	}
}

func notesSaying(notes []string, text string) (n int) {
	for _, note := range notes {
		if strings.Contains(note, text) {
			n++
		}
	}
	return n
}

// A pass that ends early still settles the ends it has read, in the same order. Three exits: the read budget, the
// time bound and a failure. A cancelled context is the fourth, and leaves those turns to the next tick.
func TestTheEndsAPassHasReadAreSettledWhenItStopsEarly(t *testing.T) {
	t.Parallel()
	// One read, the anchor's: nothing later is confirmed in this pass, so the end is reported, and it is settled
	// in the tick that read it (the budget ended the loop with the settlement still waiting).
	t.Run("the read budget is spent", func(t *testing.T) {
		t.Parallel()
		ctx, s := lateStore(t)
		lateClaim{"continuation", "child", "staged", ""}.insert(t, s)
		host := &observationHost{status: "completed", statuses: map[string]string{"anchor": "interrupted"}}
		d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
		d.Policy.MaxTurnReads, d.Policy.MaxSends = 1, -1
		report, err := d.Tick(ctx)
		if err != nil || len(host.reads) != 1 || host.reads[0] != "anchor" || report.Observed != 1 {
			t.Fatalf("reads %v observed %d err %v", host.reads, report.Observed, err)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='anchor' AND terminal_status='interrupted'"); n != 1 {
			t.Errorf("settlements of anchor: %d", n)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='anchor'"); n != 1 {
			t.Errorf("daemon observations of anchor: %d", n)
		}
	})

	// The two reads the pass makes whatever the clock says are the anchor's and the staged continuation's; the
	// time bound then ends the loop with "business" unread and the anchor's settlement still waiting. The anchor
	// is settled in this tick, and not reported, because the claim was confirmed in the pass.
	t.Run("the time bound is spent", func(t *testing.T) {
		t.Parallel()
		ctx, s := lateStore(t)
		lateClaim{"continuation", "child", "staged", ""}.insert(t, s)
		watch := &stopwatch{}
		host := &observationHost{status: "completed", statuses: map[string]string{"anchor": "interrupted"}}
		host.onRead = func() { watch.seconds += 6 }
		d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
		d.Policy.MaxSends = -1
		d.mono = watch.now
		report, err := d.Tick(ctx)
		if err != nil || !slices.Equal(host.reads, []string{"anchor", "continuation"}) {
			t.Fatalf("reads %v err %v", host.reads, err)
		}
		if notesSaying(report.Notes, "observation stopped after") != 1 || report.Observed != 2 {
			t.Errorf("observed %d notes %q: want the time bound to end the pass after two settlements", report.Observed, report.Notes)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='anchor'"); n != 0 {
			t.Errorf("daemon observations of anchor: %d", n)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='anchor'"); n != 1 {
			t.Errorf("settlements of anchor: %d", n)
		}
		watch.seconds = 0
		if _, err = d.Tick(ctx); err != nil || !slices.Equal(host.reads, []string{"anchor", "continuation", "business"}) {
			t.Fatalf("the next tick read %v err %v, want only business", host.reads, err)
		}
	})

	// A failure writing the poll row of a later read ends the pass with an error, as it did when each end was
	// settled as it was read: the anchor, read before it, is settled all the same, and the next tick finds the rest.
	t.Run("a later read fails to be recorded", func(t *testing.T) {
		t.Parallel()
		ctx, s := lateStore(t)
		lateClaim{"continuation", "child", "staged", ""}.insert(t, s)
		exec(t, s, "CREATE TRIGGER refuse_poll BEFORE INSERT ON poll_observations WHEN NEW.turn_id='continuation' BEGIN SELECT RAISE(ABORT, 'poll row refused'); END")
		host := &observationHost{status: "completed", statuses: map[string]string{"anchor": "interrupted"}}
		d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
		d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
		if _, err := d.Tick(ctx); err == nil || !strings.Contains(err.Error(), "poll row refused") {
			t.Fatalf("tick error %v, want the refused poll row", err)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='anchor' AND terminal_status='interrupted'"); n != 1 {
			t.Errorf("settlements of anchor: %d", n)
		}
		exec(t, s, "DROP TRIGGER refuse_poll")
		if _, err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		for _, turn := range []string{"anchor", "business", "continuation"} {
			if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id=?", turn); n != 1 {
				t.Errorf("settlements of %s: %d", turn, n)
			}
		}
		if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='anchor'"); n != 1 {
			t.Errorf("daemon observations of anchor: %d", n)
		}
	})

	// A context cancelled during a later read refuses every store call after it, so the anchor's waiting settlement
	// is not attempted. Nothing is half done, and a tick on a fresh context reads and settles every turn once, the
	// anchor's end not reported because the claim is confirmed in that pass.
	t.Run("the context is cancelled during a later read", func(t *testing.T) {
		t.Parallel()
		ctx, s := lateStore(t)
		lateClaim{"continuation", "child", "staged", ""}.insert(t, s)
		cancelled, cancel := context.WithCancel(ctx)
		host := &observationHost{status: "completed", statuses: map[string]string{"anchor": "interrupted"}}
		host.onRead = func() {
			if len(host.reads) == 2 {
				cancel()
			}
		}
		d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
		d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
		report, err := d.Tick(cancelled)
		if err == nil {
			t.Fatal("a cancelled tick returned no error")
		}
		// The waiting end is not attempted, so the report carries no failed settlement either.
		if failed := notesSaying(report.Notes, "lookup failed") + notesSaying(report.Notes, "rolled back"); failed != 0 {
			t.Errorf("notes %q: a settlement was attempted on a cancelled context", report.Notes)
		}
		if len(host.reads) != 2 || host.reads[0] != "anchor" {
			t.Fatalf("reads %v, want the anchor and then the read that cancelled", host.reads)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements"); n != 0 {
			t.Errorf("settlements after a cancelled tick: %d", n)
		}
		if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation'"); n != 0 {
			t.Errorf("daemon observations after a cancelled tick: %d", n)
		}
		host.onRead = nil
		if _, err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		for _, turn := range []string{"anchor", "business", "continuation"} {
			if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id=?", turn); n != 1 {
				t.Errorf("settlements of %s: %d", turn, n)
			}
		}
		if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='anchor'"); n != 0 {
			t.Errorf("daemon observations of anchor: %d", n)
		}
	})
}

// The ends a pass kept waiting are settled in the order the pass read them, however many there are.
func TestTheDeferredEndsAreSettledInTheOrderTheyWereRead(t *testing.T) {
	t.Parallel()
	ctx, s := lateStore(t)
	host := &observationHost{statuses: map[string]string{"anchor": "interrupted", "business": "failed", "continuation": "interrupted"}}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.All(ctx, "SELECT turn_id FROM assignment_settlements ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var settled []string
	for _, row := range rows {
		settled = append(settled, row.Get("turn_id").(string))
	}
	if len(host.reads) != 3 || !slices.Equal(settled, host.reads) {
		t.Errorf("read %v, settled %v: want the same turns in the same order", host.reads, settled)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation'"); n != 3 {
		t.Errorf("daemon observations: %d, want one for each end", n)
	}
}

// The check that a later receipt silences the end of an earlier turn is made again in the transaction that
// settles it. If the store has moved since the first check, so that the receipt no longer silences the turn, the
// turn is not settled: it is judged again on the next pass, on the state the store has then.
func TestASilencedEndIsJudgedAgainWhenTheStoreMovesBeforeItsSettlementCommits(t *testing.T) {
	t.Parallel()
	const gap = "a generation is opened on the observed turn"
	for _, c := range []struct {
		name   string
		move   []string // statements run once, in the gap between the check and the commit
		moved  bool     // whether the turn is withdrawn from this commit
		resume []string // run before the next tick
		events int      // daemon observations of the turn once the next tick has judged it
	}{
		{"control: the store does not move", nil, false, nil, 0},
		{"the relationship is paused", []string{"UPDATE relationships SET status='paused' WHERE relationship_id='r'"}, true,
			[]string{"UPDATE relationships SET status='active' WHERE relationship_id='r'"}, 0},
		{gap, []string{
			"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('r',2,'dispatch-r-2','bound','business','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
			"UPDATE relationships SET execution_generation=2 WHERE relationship_id='r'",
		}, true, nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			lateClaim{"continuation", "child", "final", owedQueued}.insert(t, s)
			// The other turns are settled, so the turn under test is the only one the passes read.
			for _, turn := range []string{"anchor", "continuation"} {
				exec(t, s, "INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES('r','child',?,'completed','2023-11-14T22:13:20Z')", turn)
			}
			host := &observationHost{status: "completed", statuses: map[string]string{"business": "interrupted"}}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			moved := false
			d.beforeSettle = func(turn store.TurnReference) {
				if turn.TurnID != "business" || moved {
					return
				}
				moved = true
				for _, statement := range c.move {
					exec(t, s, statement)
				}
			}
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			settled := func() int {
				return count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='business'")
			}
			events := func() int {
				return count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='business'")
			}
			withheld := func() int {
				return count(t, s, "SELECT COUNT(*) FROM journal WHERE kind='observation_not_asserted' AND subject='business'")
			}
			if !moved {
				t.Fatal("the turn was never settled, so the store never moved")
			}
			if !c.moved {
				if settled() != 1 || events() != 0 || withheld() != 1 || report.Observed != 1 {
					t.Fatalf("control: settlements %d events %d withheld %d observed %d notes %q", settled(), events(), withheld(), report.Observed, report.Notes)
				}
				return
			}
			if settled() != 0 || events() != 0 || withheld() != 0 {
				t.Errorf("settlements %d events %d withheld %d: the turn was settled on a check that no longer held", settled(), events(), withheld())
			}
			if report.Observed != 0 || !report.Quiet() || notesSaying(report.Notes, "judged again") != 1 || notesSaying(report.Notes, "not asserted") != 0 {
				t.Errorf("observed %d quiet %v notes %q: want a quiet pass that says the turn is judged again", report.Observed, report.Quiet(), report.Notes)
			}
			for _, statement := range c.resume {
				exec(t, s, statement)
			}
			if _, err = d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(host.reads, []string{"business", "business"}) {
				t.Errorf("reads %v, want the turn read again", host.reads)
			}
			if settled() != 1 || events() != c.events {
				t.Errorf("after the next pass: settlements %d events %d, want 1 and %d", settled(), events(), c.events)
			}
			if c.events == 0 && withheld() != 1 {
				t.Errorf("after the next pass: journal rows saying the end was not asserted: %d", withheld())
			}
		})
	}
}
