package faults

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// CRW-397: a delivery the recipient acknowledged has reached it, so it is settled like a dispatched,
// inbox-only or superseded one. The sources that derive delivery_stalled and delivery_refused faults, and
// the presence check that closes them, all read it that way: a delivery moves from dispatched to
// acknowledged when its acknowledgement is verified, and its earlier failed attempts must not be derived
// again, or counted as still present, after that.

// seedDeliveryStates inserts one delivery per state, each to its own recipient and relationship, held,
// with one settled failed attempt and one refusal, so that every source the sweep reads about a delivery
// has a row to say something about it. Its events are evt-000 and on, in the order of states.
func seedDeliveryStates(t *testing.T, l *Ledger, c context.Context, states []string) {
	t.Helper()
	for i, state := range states {
		event, recipient := fmt.Sprintf("evt-%03d", i), "recipient-"+state
		for _, query := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES(?,?,'completion',?,'thread',?,2,'host_lost_turn','stamp','stamp')", []any{event, "rel-" + state, recipient, state}},
			{"INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES(?,?,1,'completion','settled','withheld_pre_send','stamp')", []any{"req-" + event, event}},
			{"INSERT INTO journal(at,kind,subject,detail) VALUES('stamp','delivery_withheld',?,json_object('reason','role_policy_unconfigured'))", []any{event}},
		} {
			if _, err := l.Store.Q(c).ExecContext(c, query.sql, query.args...); err != nil {
				t.Fatalf("%s: %v", query.sql, err)
			}
		}
	}
}

// eventOf is the delivery an observation is about: every source of the sweep names it in its facts.
func eventOf(o Observation) string {
	for _, item := range o.Evidence {
		if entry, _ := item.(map[string]any); entry["kind"] == "facts" {
			observed, _ := entry["observed"].(map[string]any)
			event, _ := observed["event"].(string)
			return event
		}
	}
	return ""
}

func TestCRW397AnAcknowledgedDeliveryIsSettledForEverySource(t *testing.T) {
	states := []string{"withheld_pre_send", "deferred_busy", "dispatched", "inbox_only", "superseded", "acknowledged"}
	stalled := map[string]bool{"withheld_pre_send": true, "deferred_busy": true}
	l, c := testLedger(t)
	seedDeliveryStates(t, l, c, states)
	// Every delivery is current by the send path's own verdict, so only its state keeps one out of the sweep.
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6,
		SupersessionReason: func(context.Context, string) (string, error) { return "", nil },
		Current:            func(context.Context, string) (bool, error) { return true, nil }}

	t.Run("the sources derive nothing for a settled delivery", func(t *testing.T) {
		batch, err := sw.Sweep(c, "crw")
		if err != nil {
			t.Fatal(err)
		}
		derived := map[string]map[string]bool{}
		for _, o := range batch.Observations {
			if derived[o.FaultClass] == nil {
				derived[o.FaultClass] = map[string]bool{}
			}
			derived[o.FaultClass][eventOf(o)] = true
		}
		var want []string
		for i, state := range states {
			if stalled[state] {
				want = append(want, fmt.Sprintf("evt-%03d", i))
			}
		}
		for _, class := range []string{"delivery_stalled", "delivery_refused"} {
			var got []string
			for event := range derived[class] {
				got = append(got, event)
			}
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s was derived for deliveries %v, want %v: a delivery that is settled was derived", class, got, want)
			}
		}
	})

	t.Run("the presence check does not count a settled delivery", func(t *testing.T) {
		for _, state := range states {
			present, undetermined, err := sw.stillPresent(c, map[string]any{"recipient": "recipient-" + state, "attemptState": "withheld_pre_send"})
			if err != nil || undetermined {
				t.Fatalf("%s: undetermined %v, error %v", state, undetermined, err)
			}
			if present != stalled[state] {
				t.Errorf("a delivery in state %s is present: %v, want %v", state, present, stalled[state])
			}
		}
		for _, state := range states {
			present, err := sw.derivedStillPresent(c, "delivery_refused", map[string]any{"relationship": "rel-" + state, "errorCode": "role_policy_unconfigured"})
			if err != nil {
				t.Fatal(err)
			}
			if present != stalled[state] {
				t.Errorf("a refusal on a delivery in state %s is present: %v, want %v", state, present, stalled[state])
			}
		}
	})
}
