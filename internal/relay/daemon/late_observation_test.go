package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// lateStore seeds relationship "r" (child "child", parent "parent"): generation 1 is anchored at
// "anchor" and admits "business" and then "continuation", in that order. That is the shape a managed
// child leaves: the standby turn is the anchor, the turn that delivered the assignment is admitted at
// start, and a goal-continuation turn is admitted when it first emits.
func lateStore(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seed(t, s, "r", "parent", "child", "anchor")
	for _, turn := range []string{"business", "continuation"} {
		if _, err = s.DB.Exec("INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('r',1,?,'explicit_admission_bound:anchor','child','admitted','2023-11-14T22:13:20Z')", turn); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, s
}

// lateClaim is an event already stored for one turn of relationship "r". Its stage is final, staged or
// suppressed, or finalWithReason: a final event that nevertheless carries a suppressed_reason, which
// the readers of "unsuppressed" receipts exclude (registry/currency.go, delivery/service.go).
//
// Delivery is what the parent is owed for it: a deliveries row in the named state (as the daemon's
// own finalization and `emit` leave one), "intent" for an enqueue that was refused and is retried,
// or empty for none (`emit --no-enqueue`, or a refused enqueue nobody recorded).
type lateClaim struct{ turn, producer, stage, delivery string }

const (
	finalWithReason = "final with a suppressed_reason"
	owedIntent      = "intent"
)

func (c lateClaim) insert(t *testing.T, s *store.Store) {
	t.Helper()
	outcome, status, finalized := "ready_for_review", "completed", "'2023-11-14T22:13:20Z'"
	if c.producer == "daemon_observation" {
		outcome, status = "failed", "failed"
	}
	if c.stage == "staged" {
		finalized = "NULL"
	}
	reason, stage := "NULL", c.stage
	if c.stage == "suppressed" || c.stage == finalWithReason {
		reason = "'the claim was withdrawn'"
	}
	if c.stage == finalWithReason {
		stage = "final"
	}
	event := "claim-" + c.turn
	if _, err := s.DB.Exec("INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,finalized_at,finalizing_status,suppressed_reason,first_seen_at,last_seen_at) VALUES(?,'r',1,'rev',?,?,'child',?,?,'{}',?,'2023-11-14T22:13:20Z',"+finalized+",?,"+reason+",'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
		event, outcome, c.producer, c.turn, status, stage, status); err != nil {
		t.Fatal(err)
	}
	switch c.delivery {
	case "":
	case owedIntent:
		// Not due for a long time, so the tick leaves it as an intent.
		if _, err := s.DB.Exec("INSERT INTO delivery_intent(event_id,relationship_id,kind,recipient_task_id,attempts,next_retry_at,last_error,noted_at) VALUES(?,'r','completion','parent',1,99999999999,'the recipient is busy','2023-11-14T22:13:20Z')", event); err != nil {
			t.Fatal(err)
		}
	default:
		if _, err := s.DB.Exec("INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,created_at,updated_at) VALUES(?,'r','completion','parent','parent',?,0,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')", event, c.delivery); err != nil {
			t.Fatal(err)
		}
	}
}

// A turn that ended failed or interrupted after a later turn of its generation reported is not news:
// the daemon settles it and asserts nothing, so the parent is not woken for it. A turn whose later
// turns reported nothing, or nothing the parent is owed, is still observed and delivered as it
// always was.
func TestLateEndOfAnEarlierTurnIsNotReportedOnceALaterTurnReported(t *testing.T) {
	t.Parallel()
	final := &lateClaim{"continuation", "child", "final", owedQueued}
	for _, c := range []struct {
		name     string
		observed string // the turn the host reports as ended badly
		status   string // failed or interrupted
		later    string // what the host reports for the other turns
		claim    *lateClaim
		silent   bool // the daemon asserts nothing
	}{
		{"interrupted business turn, a later turn holds a final child receipt", "business", "interrupted", "completed", final, true},
		{"failed business turn, a later turn holds a final child receipt", "business", "failed", "completed", final, true},
		{"the anchor, a later admitted turn holds a final child receipt", "anchor", "interrupted", "completed", final, true},
		{"the later receipt's delivery is withheld", "business", "interrupted", "completed", &lateClaim{"continuation", "child", "final", "withheld_pre_send"}, true},
		{"the later receipt's enqueue was refused and will be retried", "business", "interrupted", "completed", &lateClaim{"continuation", "child", "final", owedIntent}, true},
		{"control: the later turn reported nothing", "business", "interrupted", "completed", nil, false},
		{"the later receipt is owed no delivery (emitted with --no-enqueue)", "business", "interrupted", "completed", &lateClaim{"continuation", "child", "final", ""}, false},
		{"the later claim is only staged", "business", "interrupted", "inProgress", &lateClaim{"continuation", "child", "staged", owedQueued}, false},
		{"the later claim was suppressed", "business", "interrupted", "completed", &lateClaim{"continuation", "child", "suppressed", owedQueued}, false},
		{"the later claim is final but carries a suppressed_reason", "business", "interrupted", "completed", &lateClaim{"continuation", "child", finalWithReason, owedQueued}, false},
		{"the later event is the daemon's own observation", "business", "interrupted", "completed", &lateClaim{"continuation", "daemon_observation", "final", owedQueued}, false},
		{"the receipt sits on an earlier admission, not a later one", "continuation", "interrupted", "completed", &lateClaim{"business", "child", "final", owedQueued}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			if c.claim != nil {
				c.claim.insert(t, s)
			}
			statuses := map[string]string{"anchor": c.later, "business": c.later, "continuation": c.later}
			statuses[c.observed] = c.status
			host := &observationHost{statuses: statuses}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			// All three turns are read in one tick, and a delivery is queued but not sent.
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := func(query string, args ...any) int {
				t.Helper()
				var n int
				if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			// asserted is how many daemon observations, and so deliveries, the turn's end leaves behind;
			// withheld is how many records of the decision not to assert it.
			asserted, withheld := 1, 0
			if c.silent {
				asserted, withheld = 0, 1
			}
			if n := count("SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id=? AND outcome=?", c.observed, c.status); n != asserted {
				t.Errorf("daemon observations of %s: got %d want %d", c.observed, n, asserted)
			}
			if n := count("SELECT COUNT(*) FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.producer='daemon_observation' AND e.turn_id=?", c.observed); n != asserted {
				t.Errorf("deliveries of the observation of %s: got %d want %d", c.observed, n, asserted)
			}
			if n := count("SELECT COUNT(*) FROM delivery_intent i JOIN events e ON e.event_id=i.event_id WHERE e.producer='daemon_observation' AND e.turn_id=?", c.observed); n != 0 {
				t.Errorf("delivery intents of the observation of %s: %d", c.observed, n)
			}
			if n := count("SELECT COUNT(*) FROM assignment_settlements WHERE relationship_id='r' AND thread_id='child' AND turn_id=? AND terminal_status=?", c.observed, c.status); n != 1 {
				t.Errorf("settlements of %s: got %d want 1", c.observed, n)
			}
			var event sql.NullString
			if err := s.DB.QueryRow("SELECT event_id FROM observations WHERE thread_id='child' AND turn_id=? AND terminal_status=?", c.observed, c.status).Scan(&event); err != nil || event.Valid == c.silent {
				t.Errorf("observation of %s: event %v err %v", c.observed, event, err)
			}
			if n := count("SELECT COUNT(*) FROM journal WHERE kind='observation_not_asserted' AND subject=?", c.observed); n != withheld {
				t.Errorf("journal rows for %s: got %d want %d", c.observed, n, withheld)
			}
			noted := 0
			for _, note := range report.Notes {
				if strings.Contains(note, "not asserted") {
					noted++
				}
			}
			if noted != withheld {
				t.Errorf("notes: got %q, want %d saying the observation was not asserted", report.Notes, withheld)
			}
			if !c.silent || t.Failed() {
				return
			}
			// Settled, so the turn is not read again.
			if _, err = d.Tick(ctx); err != nil || len(host.reads) != 3 {
				t.Fatalf("reads %v err %v", host.reads, err)
			}
		})
	}
}

// owedQueued is the state a later receipt's delivery is normally in when the parent is owed it.
const owedQueued = "queued"

// The relationship the pass loaded may move while the pass reads the host: the observation is then
// decided on the state the store has now, exactly as it was before a later receipt could suppress one.
func TestAnEarlierTurnIsDecidedOnTheCurrentRelationshipWhenItMovesDuringThePass(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		change  []string
		settled int    // settlements of the business turn afterwards
		events  int    // daemon observations of the business turn afterwards
		note    string // what the pass reports
	}{
		{"the relationship is paused", []string{"UPDATE relationships SET status='paused' WHERE relationship_id='r'"}, 0, 0, "observation deferred"},
		{"a new generation is opened on the observed turn", []string{
			"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('r',2,'dispatch-r-2','bound','business','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
			"UPDATE relationships SET execution_generation=2 WHERE relationship_id='r'",
		}, 1, 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			lateClaim{"continuation", "child", "final", owedQueued}.insert(t, s)
			moved := false
			host := &observationHost{statuses: map[string]string{"anchor": "completed", "business": "interrupted", "continuation": "completed"}}
			host.onRead = func() {
				if moved {
					return
				}
				moved = true
				for _, statement := range c.change {
					if _, err := s.DB.Exec(statement); err != nil {
						t.Error(err)
					}
				}
			}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := func(query string) (n int) {
				t.Helper()
				if err := s.DB.QueryRow(query).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if n := count("SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='business'"); n != c.settled {
				t.Errorf("settlements of business: got %d want %d", n, c.settled)
			}
			if n := count("SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='business'"); n != c.events {
				t.Errorf("daemon observations of business: got %d want %d", n, c.events)
			}
			if n := count("SELECT COUNT(*) FROM journal WHERE kind='observation_not_asserted'"); n != 0 {
				t.Errorf("an observation was withheld on stale state: %d", n)
			}
			if c.note != "" && !strings.Contains(strings.Join(report.Notes, "\n"), c.note) {
				t.Errorf("notes %q lack %q", report.Notes, c.note)
			}
		})
	}
}
