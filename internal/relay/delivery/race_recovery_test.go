package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_ack_disposition_race.py ADR-1..ADR-3 and test_recovery_negatives.py RCN-1..RCN-3, mirrored
// test for test (hostloss_harness_test.go mirror + testdata/capture.py).

const adr = "test_ack_disposition_race"
const rcn = "test_recovery_negatives"

// rival is CompetingAcknowledgements.rival: a second process, its own store connection.
func (h *hl) rival() *Ack {
	h.t.Helper()
	s, err := store.Open(context.Background(), h.store.Path, "")
	mustDo(h.t, err)
	h.t.Cleanup(func() { _ = s.Close() })
	return NewAck(NewService(s, h.clock))
}

type ackCall struct {
	turn      string
	accepted  bool
	rejection any
}

func (h *hl) ackWith(service *Ack, event string, call ackCall, adapter Adapter) Obj {
	h.t.Helper()
	out, err := service.Acknowledge(h.ctx, event, call.turn, AckProof(event, call.turn), call.accepted, call.rejection, adapter)
	mustDo(h.t, err)
	return out
}

// racing commits the winner's acknowledgement inside the loser's pre-transaction host read and
// returns the adapter the loser uses plus what the winner got.
func (h *hl) racing(event string, winner *Ack, call ackCall) (Adapter, *bool, *Obj) {
	raced, won := false, Obj(nil)
	adapter := actsDuringTheTurnRead(h.host, func() {
		raced = true
		won = h.ackWith(winner, event, call, h.host)
	})
	return adapter, &raced, &won
}

func (h *hl) stored(event string) (Row, Obj) {
	row := h.one("SELECT * FROM acks WHERE event_id = ?", event)
	return row, loadsObj(row.S("record"))
}

func (h *hl) raceDispatched() string {
	event := h.queuedEvent(regOpts{recipients: []string{parent, child}})
	h.attemptOn(event, h.host, nil)
	h.clock.Advance(5)
	return event
}

func Test21_ADR01_a_verified_disposition_is_never_overwritten(t *testing.T) {
	t.Run("losing rejection", func(t *testing.T) {
		mirror(t, adr, "CompetingAcknowledgements.test_a_losing_rejection_does_not_overwrite_a_verified_acceptance", func(h *hl) {
			event := h.raceDispatched()
			accepting := h.host.startTurn(parent, "accepting-turn", "inProgress", "")
			rejecting := h.host.startTurn(parent, "rejecting-turn", "inProgress", "")
			adapter, raced, won := h.racing(event, h.rival(), ackCall{accepting.TurnID, true, nil})
			result := h.ackWith(h.ack, event, ackCall{rejecting.TurnID, false, "revision_mismatch"}, adapter)
			h.eq(*raced)
			h.eq(field(*won, "_verified"))
			row, record := h.stored(event)
			h.eq(truthy(field(record, "accepted")))
			h.eq(field(record, "rejectionReason"))
			h.eq(row.S("ack_turn_id"))
			h.eq(row.S("verified"))
			h.eq(row.I("accepted"))
			h.eq(truthy(field(result, "accepted")))
			h.eq(field(result, "ackTurnId"))
			h.eq(truthy(field(result, "_replay")))
			h.eq(field(result, "_verified"))
			h.eq(h.row(event).S("state"))
		})
	})
	t.Run("losing acceptance", func(t *testing.T) {
		mirror(t, adr, "CompetingAcknowledgements.test_a_losing_acceptance_does_not_overwrite_a_verified_rejection", func(h *hl) {
			event := h.raceDispatched()
			rejecting := h.host.startTurn(parent, "rejecting-turn", "inProgress", "")
			accepting := h.host.startTurn(parent, "accepting-turn", "inProgress", "")
			adapter, raced, _ := h.racing(event, h.rival(), ackCall{rejecting.TurnID, false, "revision_mismatch"})
			result := h.ackWith(h.ack, event, ackCall{accepting.TurnID, true, nil}, adapter)
			h.eq(*raced)
			row, record := h.stored(event)
			h.eq(truthy(field(record, "accepted")))
			h.eq(field(record, "rejectionReason"))
			h.eq(row.S("ack_turn_id"))
			h.eq(row.S("verified"))
			h.eq(truthy(field(result, "accepted")))
			h.eq(field(result, "ackTurnId"))
			h.eq(truthy(field(result, "_replay")))
		})
	})
	t.Run("sequential", func(t *testing.T) {
		mirror(t, adr, "CompetingAcknowledgements.test_a_sequential_second_disposition_is_unchanged", func(h *hl) {
			event := h.raceDispatched()
			accepting := h.host.startTurn(parent, "accepting-turn", "inProgress", "")
			rejecting := h.host.startTurn(parent, "rejecting-turn", "inProgress", "")
			h.ackWith(h.ack, event, ackCall{accepting.TurnID, true, nil}, h.host)
			result := h.ackWith(h.ack, event, ackCall{rejecting.TurnID, false, "revision_mismatch"}, h.host)
			h.eq(truthy(field(result, "accepted")))
			_, record := h.stored(event)
			h.eq(truthy(field(record, "accepted")))
			h.eq(truthy(field(result, "_replay")))
			h.eq(field(result, "_verified"))
		})
	})
}

func Test21_ADR02_a_losing_rejection_leaves_the_verdict_path_open(t *testing.T) {
	mirror(t, adr, "CompetingAcknowledgements.test_the_losing_rejection_leaves_the_verdict_path_open", func(h *hl) {
		event := h.raceDispatched()
		accepting := h.host.startTurn(parent, "accepting-turn", "inProgress", "")
		rejecting := h.host.startTurn(parent, "rejecting-turn", "inProgress", "")
		adapter, _, _ := h.racing(event, h.rival(), ackCall{accepting.TurnID, true, nil})
		h.ackWith(h.ack, event, ackCall{rejecting.TurnID, false, "revision_mismatch"}, adapter)
		record, err := h.verdict(event, "verified", "v1")
		mustDo(t, err)
		h.eq(field(record, "verdict"))
	})
}

func Test21_ADR03_an_unverified_acknowledgement_is_still_upgradable(t *testing.T) {
	mirror(t, adr, "CompetingAcknowledgements.test_an_unverified_acknowledgement_is_still_upgradable", func(h *hl) {
		event := h.raceDispatched()
		turn := h.host.startTurn(parent, "parent-own-turn", "inProgress", "")
		h.ackWith(h.ack, event, ackCall{turn.TurnID, true, nil}, nil)
		row, _ := h.stored(event)
		h.eq(row.S("verified"))
		results, err := h.ack.VerifyPendingAcks(h.ctx, h.host, 8, nil)
		mustDo(t, err)
		outcomes := []any{}
		for _, r := range results {
			outcomes = append(outcomes, str(r.(Obj), "outcome"))
		}
		h.eq(outcomes)
	})
}

// ---------------------------------------------------------------- test_recovery_negatives.py

func Test21_RCN01_a_restart_sweep_never_resends_or_retries(t *testing.T) {
	mirror(t, rcn, "RecoveryRefusesToInvent.test_a_restart_sweep_never_resends", func(h *hl) {
		event := h.queuedEvent(regOpts{recipients: []string{parent, child}})
		h.attemptOn(event, h.host, nil)
		report := h.recoverOnStart()
		h.eq(field(report, "resent"))
		h.eq(len(h.host.sends))
	})
	mirror(t, rcn, "RecoveryRefusesToInvent.test_an_uncertain_attempt_is_held_rather_than_retried", func(h *hl) {
		event := h.queuedEvent(regOpts{recipients: []string{parent, child}})
		h.host.script = []string{"process_death"}
		h.attemptOn(event, h.host, nil)
		state := h.one("SELECT state FROM deliveries WHERE event_id = ?", event).S("state")
		h.eq(state == HeldUncertain || state == Queued || state == DeferredBusy)
		h.eq(len(h.attemptsFor(event)))
	})
}

func Test21_RCN02_reading_a_transcript_settles_nothing(t *testing.T) {
	mirror(t, rcn, "RecoveryRefusesToInvent.test_reading_a_transcript_settles_nothing", func(h *hl) {
		h.register(regOpts{recipients: []string{parent, child}})
		before := str(h.assignment(), "state")
		events := h.count("SELECT COUNT(*) AS c FROM events")
		_, err := h.host.ReadThread(child)
		mustDo(t, err)
		_, err = h.host.ListTurnIDs(child, 20)
		mustDo(t, err)
		after := str(h.assignment(), "state")
		if after != before {
			t.Fatalf("reading the transcript moved the assignment from %s to %s", before, after)
		}
		h.eq(after)
		h.eq(h.count("SELECT COUNT(*) AS c FROM events"))
		_ = events
	})
}

// resumeRestating is registry._resume_in_transaction's restatement check (registry.resume is todo
// 25's; this is the refusing half, decided inside the write transaction, the same rows): a wrong
// restatement is refused relationship_not_active and writes nothing.
func (h *hl) resumeRestating(generation int64, roots, recipients []string, actor string) error {
	return h.store.Transaction(h.ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, err := one(ctx, h.store, "SELECT * FROM relationships WHERE relationship_id = ?", h.rid)
		if err != nil {
			return err
		}
		var mismatches []string
		if row.I("execution_generation") != generation {
			mismatches = append(mismatches, fmt.Sprintf("generation is %d, not %d", row.I("execution_generation"), generation))
		}
		var gotRoots, gotRecipients []string
		mustDo(h.t, json.Unmarshal([]byte(row.S("artifact_roots")), &gotRoots))
		mustDo(h.t, json.Unmarshal([]byte(row.S("allowed_recipients")), &gotRecipients))
		if !reflect.DeepEqual(gotRoots, roots) {
			mismatches = append(mismatches, "artifact roots differ from the restated scope")
		}
		if !reflect.DeepEqual(gotRecipients, recipients) {
			mismatches = append(mismatches, "allowed recipients differ from the restated scope")
		}
		if truthy(row.Opt("superseded_by")) {
			mismatches = append(mismatches, "superseded by "+row.S("superseded_by"))
		}
		if len(mismatches) > 0 {
			return refuse(RelationshipNotActive, "resume refused: %s", strings.Join(mismatches, "; "))
		}
		if _, err := execSQL(ctx, h.store, "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?", "active", h.clock.ISO(), h.rid); err != nil {
			return err
		}
		return journal(ctx, h.store, "status_changed", h.rid, Obj{{Key: "status", Value: "active"}, {Key: "actor", Value: actor}}, h.clock.ISO())
	})
}

func Test21_RCN03_a_paused_assignment_is_never_auto_resumed(t *testing.T) {
	mirror(t, rcn, "RecoveryRefusesToInvent.test_a_paused_assignment_is_never_auto_resumed", func(h *hl) {
		h.register(regOpts{recipients: []string{parent, child}})
		h.setStatusBy("paused", parent)
		h.recoverOnStart()
		r, err := LoadRelationship(h.ctx, h.store, h.rid)
		mustDo(t, err)
		h.eq(r.Status)
		_, err = RequireActive(h.ctx, h.store, h.rid)
		h.refusal(nil, err)
	})
	mirror(t, rcn, "RecoveryRefusesToInvent.test_resuming_demands_a_restatement_rather_than_a_status_flip", func(h *hl) {
		h.register(regOpts{recipients: []string{parent, child}})
		h.setStatusBy("paused", parent)
		err := h.resumeRestating(99, []string{"/wrong"}, []string{parent}, parent)
		if Reason(err) != RelationshipNotActive {
			t.Fatalf("a wrong restatement was not refused: %v", err)
		}
		r, err := LoadRelationship(h.ctx, h.store, h.rid)
		mustDo(t, err)
		h.eq(r.Status)
	})
}
