package daemon

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-668: a turn that already reported is not observed again. When the relay sees a turn end
// failed or interrupted and that very turn already holds its own final, unsuppressed child
// receipt with a delivery owed to the parent, the end is not news: the parent is told through
// that receipt's delivery. settle settles the turn with no event, like the later-receipt case,
// and leaves the reason in the report note. A child receipt owed no delivery (--no-enqueue, or a
// refused enqueue nobody recorded), one a newer revision replaced, or one that is not final is
// observed exactly as before. These tests use temporary synthetic stores and a fake host only.

// ownReceipt stores a child event of "r"'s generation 1 on turn, in the given stage, with the
// delivery the parent is owed for it: a deliveries row in the named state, "intent" for a
// delivery_intent, or "" for none. A suppressed reason makes a final event read as suppressed.
func ownReceipt(t *testing.T, s *store.Store, turn, outcome, stage, suppressed, owed string) string {
	t.Helper()
	event := "own-" + turn
	finalized, reason := "'2023-11-14T22:13:20Z'", "NULL"
	if stage == "staged" {
		finalized = "NULL"
	}
	if suppressed != "" {
		reason = "'" + suppressed + "'"
	}
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,finalized_at,finalizing_status,suppressed_reason,first_seen_at,last_seen_at) VALUES(?,'r',1,'rev',?,'child','child',?,'completed','{}',?,'2023-11-14T22:13:20Z',"+finalized+",'completed',"+reason+",'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
		event, outcome, turn, stage)
	switch owed {
	case "":
	case owedIntent:
		exec(t, s, "INSERT INTO delivery_intent(event_id,relationship_id,kind,recipient_task_id,attempts,next_retry_at,last_error,noted_at) VALUES(?,'r','completion','parent',1,99999999999,'the recipient is busy','2023-11-14T22:13:20Z')", event)
	default:
		exec(t, s, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,created_at,updated_at) VALUES(?,'r','completion','parent','parent',?,0,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')", event, owed)
	}
	return event
}

// c1/c2: a turn whose own final child receipt is owed to the parent is settled without an
// observation; one whose receipt is owed nothing, superseded, suppressed or not final is observed
// as before.
func TestOwnReceipt01_ATurnThatAlreadyReportedIsNotObservedAgain(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		observed   string
		status     string
		outcome    string
		stage      string
		suppressed string
		owed       string
		silent     bool
	}{
		{"the observed turn reported ready_for_review and is owed a queued delivery", "business", "interrupted", "ready_for_review", "final", "", owedQueued, true},
		{"the observed turn reported ready_for_review and is owed a withheld delivery", "business", "interrupted", "ready_for_review", "final", "", "withheld_pre_send", true},
		{"the observed turn reported ready_for_review and its enqueue will be retried", "business", "failed", "ready_for_review", "final", "", owedIntent, true},
		{"the observed turn asked a question and is owed a queued delivery", "business", "interrupted", "blocked_needs_input", "final", "", owedQueued, true},
		{"the anchor turn reported and is owed a queued delivery", "anchor", "failed", "ready_for_review", "final", "", owedQueued, true},
		{"control: the observed turn reported and is owed no delivery", "business", "interrupted", "ready_for_review", "final", "", "", false},
		{"control: the observed turn's delivery was superseded", "business", "interrupted", "ready_for_review", "final", "", "superseded", false},
		{"control: the observed turn's receipt was withdrawn", "business", "interrupted", "ready_for_review", "final", "the claim was withdrawn", owedQueued, false},
		{"control: the observed turn's receipt is not final yet", "business", "interrupted", "ready_for_review", "staged", "", owedQueued, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			ownReceipt(t, s, c.observed, c.outcome, c.stage, c.suppressed, c.owed)
			host := &observationHost{status: "completed", statuses: map[string]string{c.observed: c.status}}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}

			asserted, withheld := 1, 0
			if c.silent {
				asserted, withheld = 0, 1
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id=?", c.observed); n != asserted {
				t.Errorf("daemon observations of %s: got %d want %d", c.observed, n, asserted)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.producer='daemon_observation' AND e.turn_id=?", c.observed); n != asserted {
				t.Errorf("deliveries of the observation of %s: got %d want %d", c.observed, n, asserted)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM delivery_intent i JOIN events e ON e.event_id=i.event_id WHERE e.producer='daemon_observation' AND e.turn_id=?", c.observed); n != 0 {
				t.Errorf("delivery intents of the observation of %s: %d", c.observed, n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE relationship_id='r' AND thread_id='child' AND turn_id=? AND terminal_status=?", c.observed, c.status); n != 1 {
				t.Errorf("settlements of %s: got %d want 1", c.observed, n)
			}
			var event sql.NullString
			if err := s.DB.QueryRow("SELECT event_id FROM observations WHERE thread_id='child' AND turn_id=? AND terminal_status=?", c.observed, c.status).Scan(&event); err != nil || event.Valid == c.silent {
				t.Errorf("observation of %s: event %v err %v", c.observed, event, err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind='observation_not_asserted' AND subject=?", c.observed); n != withheld {
				t.Errorf("journal rows for %s: got %d want %d", c.observed, n, withheld)
			}
			noted := 0
			for _, note := range report.Notes {
				if strings.Contains(note, "not asserted") {
					noted++
				}
			}
			if noted != withheld {
				t.Errorf("notes %q: want %d saying the observation was not asserted", report.Notes, withheld)
			}
			if !c.silent || t.Failed() {
				return
			}
			// Settled, so the turn is not read again.
			// Settled, so the turn is not read again.
			if _, err = d.Tick(ctx); err != nil || len(host.reads) != 3 {
				t.Fatalf("reads %v err %v", host.reads, err)
			}
		})
	}
}

// c1: the own-receipt check is made again inside the settlement transaction. When the store has
// moved since the first check, the turn is not settled and is judged again on the next pass.
func TestOwnReceipt02_TheOwnReceiptIsCheckedAgainInsideTheTransaction(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		move   []string
		moved  bool
		resume []string
		events int
	}{
		{"control: the store does not move", nil, false, nil, 0},
		{"the receipt's delivery is superseded in the gap", []string{"UPDATE deliveries SET state='superseded' WHERE event_id='own-business'"}, true, nil, 1},
		{"a new generation is opened on the observed turn", []string{
			"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('r',2,'dispatch-r-2','bound','business','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
			"UPDATE relationships SET execution_generation=2 WHERE relationship_id='r'",
		}, true, nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			ownReceipt(t, s, "business", "ready_for_review", "final", "", owedQueued)
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
			if !moved {
				t.Fatal("the turn was never settled, so the store never moved")
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
			if !c.moved {
				if settled() != 1 || events() != 0 || withheld() != 1 || report.Observed != 1 {
					t.Fatalf("control: settlements %d events %d withheld %d observed %d notes %q", settled(), events(), withheld(), report.Observed, report.Notes)
				}
				return
			}
			if settled() != 0 || events() != 0 || withheld() != 0 {
				t.Errorf("settlements %d events %d withheld %d: the turn was settled on a check that no longer held", settled(), events(), withheld())
			}
			if report.Observed != 0 || !report.Quiet() || notesSaying(report.Notes, "judged again") != 1 {
				t.Errorf("observed %d quiet %v notes %q: want a quiet pass that says the turn is judged again", report.Observed, report.Quiet(), report.Notes)
			}
			for _, statement := range c.resume {
				exec(t, s, statement)
			}
			if _, err = d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if settled() != 1 || events() != c.events {
				t.Errorf("after the next pass: settlements %d events %d, want 1 and %d", settled(), events(), c.events)
			}
		})
	}
}
