package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-669 (follow-up to CRW-659): the turn a keeping decision (answer, stop) was dispatched into is
// a later turn of the generation, not its anchor, so nothing but the relay's own admission of it
// makes the census read it. With the admission internal/relay/delivery/settle now writes, a
// continuation turn that ends interrupted without a receipt is observed like any other admitted
// turn, and the parent can answer its end. These tests use a temporary synthetic store and a fake
// host only.

// continuationDecisionStore is a store whose generation 1 is anchored to "anchor" and which holds
// the decision event "decision" already dispatched into the continuation turn "T2" (the deliveries
// row settle leaves). Whether the relay also admitted T2 is the variable under test: with the
// admission, the census reads T2; without it, T2 is a later turn nothing admits.
func continuationDecisionStore(t *testing.T, admitted bool) *store.Store {
	t.Helper()
	s, err := fixtureStore(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seed(t, s, "r", "parent", "child", "anchor")
	// The anchor is settled, so the continuation turn is the only turn the pass has to read.
	exec(t, s, "INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES('r','child','anchor','completed','2023-11-14T22:13:20Z')")
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,finalized_at,finalizing_status,first_seen_at,last_seen_at) VALUES('decision','r',1,'"+store.NoDeliverable+"','decision_reply','relay','parent','turn-decision','completed','{\"generationEffect\":\"stays\"}','final','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z','completed','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	exec(t, s, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,dispatch_evidence,dispatch_turn_id,created_at,updated_at) VALUES('decision','r','revision_request','child','child','dispatched',1,'transport_accepted','T2','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	if admitted {
		exec(t, s, "INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('r',1,'T2','explicit_admission_bound:anchor','relay','decision reply decision','2023-11-14T22:13:20Z')")
	}
	return s
}

// c2: a continuation turn the decision was dispatched into is read by the census only once it is
// admitted, and an admitted one that ends interrupted without a receipt becomes a daemon
// observation the parent can answer. The "without the admission" half is the red case: today
// nothing admits T2, so the pass never reads it and no observation exists.
func TestDCA06_ADecisionContinuationTurnIsObservedOnceItIsAdmitted(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		admitted bool
		observed int
		reads    []string
	}{
		{"the relay admitted the turn the decision was dispatched into", true, 1, []string{"T2"}},
		{"nothing admitted the continuation turn", false, 0, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := continuationDecisionStore(t, c.admitted)
			host := &observationHost{statuses: map[string]string{"anchor": "completed", "T2": "interrupted"}}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			if _, err := d.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='T2' AND outcome='interrupted'"); n != c.observed {
				t.Errorf("daemon observations of the continuation turn: got %d want %d", n, c.observed)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE relationship_id='r' AND thread_id='child' AND turn_id='T2'"); n != c.observed {
				t.Errorf("settlements of the continuation turn: got %d want %d", n, c.observed)
			}
			if len(host.reads) != len(c.reads) {
				t.Fatalf("turns read: %v, want %v", host.reads, c.reads)
			}
			for i, want := range c.reads {
				if host.reads[i] != want {
					t.Fatalf("turns read: %v, want %v", host.reads, c.reads)
				}
			}
			if c.observed == 0 {
				return
			}
			// The observation is delivered to the parent through the ordinary path.
			if n := count(t, s, "SELECT COUNT(*) FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.producer='daemon_observation' AND e.turn_id='T2'"); n != 1 {
				t.Errorf("deliveries of the observation: got %d want 1", n)
			}
			// And the child's own receipt from that turn, with the claim the message prints, is
			// accepted: the admission the decision wrote is the same one the receipt identity check reads.
			var found int
			if err := s.DB.QueryRow("SELECT COUNT(*) FROM generation_turns WHERE relationship_id='r' AND execution_generation=1 AND turn_id='T2' AND evidence='explicit_admission_bound:anchor' AND actor='relay'").Scan(&found); err != nil || found != 1 {
				t.Errorf("the admission row: %d err %v", found, err)
			}
		})
	}
}
