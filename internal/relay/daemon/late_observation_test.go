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
type lateClaim struct{ turn, producer, stage string }

const finalWithReason = "final with a suppressed_reason"

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
	if _, err := s.DB.Exec("INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,finalized_at,finalizing_status,suppressed_reason,first_seen_at,last_seen_at) VALUES(?,'r',1,'rev',?,?,'child',?,?,'{}',?,'2023-11-14T22:13:20Z',"+finalized+",?,"+reason+",'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
		"claim-"+c.turn, outcome, c.producer, c.turn, status, stage, status); err != nil {
		t.Fatal(err)
	}
}

// A turn that ended failed or interrupted after a later turn of its generation reported is not news:
// the daemon settles it and asserts nothing, so the parent is not woken for it. A turn whose later
// turns reported nothing is still observed and delivered as it always was.
func TestLateEndOfAnEarlierTurnIsNotReportedOnceALaterTurnReported(t *testing.T) {
	final := &lateClaim{"continuation", "child", "final"}
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
		{"control: the later turn reported nothing", "business", "interrupted", "completed", nil, false},
		{"the later claim is only staged", "business", "interrupted", "inProgress", &lateClaim{"continuation", "child", "staged"}, false},
		{"the later claim was suppressed", "business", "interrupted", "completed", &lateClaim{"continuation", "child", "suppressed"}, false},
		{"the later claim is final but carries a suppressed_reason", "business", "interrupted", "completed", &lateClaim{"continuation", "child", finalWithReason}, false},
		{"the later event is the daemon's own observation", "business", "interrupted", "completed", &lateClaim{"continuation", "daemon_observation", "final"}, false},
		{"the receipt sits on an earlier admission, not a later one", "continuation", "interrupted", "completed", &lateClaim{"business", "child", "final"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			if n := count("SELECT COUNT(*) FROM deliveries"); n != asserted {
				t.Errorf("deliveries: got %d want %d", n, asserted)
			}
			if n := count("SELECT COUNT(*) FROM delivery_intent"); n != 0 {
				t.Errorf("delivery intents: %d", n)
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
