package delivery

import (
	"strings"
	"testing"
	"time"
)

// test_ack_reconcile.py ACR-1..ACR-23. Each Test21_ACR<n> runs the Go twin of every Python test the
// property lists (a subtest per Python test), in the Python test's own tree, and compares every
// asserted value, the delivery tables and the sends with what Python produced
// (hostloss_harness_test.go mirror + testdata/capture.py).

const acr = "test_ack_reconcile"

// noArchiveInfo is the Python tests' NoArchiveInfo: is_archived answers None, the rest passes through.
type noArchiveInfo struct{ Adapter }

func (noArchiveInfo) IsArchived(string, any) (*bool, error) { return nil, nil }

// sentAt is datetime.fromtimestamp(1789420929.360483, timezone.utc).isoformat().
func sentAt() string {
	return time.UnixMicro(1789420929360483).UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

// ackDispatched is Acknowledgement._dispatched.
func (h *hl) ackDispatched() (string, TurnInfo) {
	event := h.queuedEvent(regOpts{})
	h.attemptOn(event, h.host, nil)
	h.clock.Advance(5)
	return event, h.host.startTurn(parent, "parent-ack-turn", "inProgress", "")
}

// refusal records the reason a Python assertRefused compared (it records caught.exception.reason).
func (h *hl) refusal(_ any, err error) {
	h.t.Helper()
	if err == nil {
		h.t.Fatal("expected a refusal")
	}
	h.eq(Reason(err))
}

func Test21_ACR01_turn_start_precision(t *testing.T) {
	mirror(t, acr, "TurnStartPrecision.test_a_turn_starting_in_the_same_second_as_its_send_is_not_refused", func(h *hl) {
		h.eq(certainlyBefore(1789420929, sentAt()))
	})
	mirror(t, acr, "TurnStartPrecision.test_a_genuinely_earlier_whole_second_turn_is_still_refused", func(h *hl) {
		h.eq(certainlyBefore(1789420920, sentAt()))
		h.eq(certainlyBefore(1789420928, sentAt()))
	})
	mirror(t, acr, "TurnStartPrecision.test_a_later_turn_is_never_refused", func(h *hl) {
		h.eq(certainlyBefore(1789420930, sentAt()))
	})
}

func Test21_ACR02_archive_observation(t *testing.T) {
	mirror(t, acr, "UnknownArchiveState.test_an_unknown_archive_observation_is_not_evidence_of_being_unarchived", func(h *hl) {
		o := Observe(noArchiveInfo{h.host}, parent, nil, true)
		h.eq(o.Deliverable)
		h.eq(o.WithholdReason)
		h.eq(o.MaySend())
	})
	mirror(t, acr, "UnknownArchiveState.test_a_confirmed_unarchived_recipient_stays_deliverable", func(h *hl) {
		f := false
		h.host.threads[parent].archived = &f
		h.eq(Observe(h.host, parent, nil, true).MaySend())
	})
	mirror(t, acr, "UnknownArchiveState.test_a_confirmed_archived_recipient_stays_blocked", func(h *hl) {
		yes := true
		h.host.threads[parent].archived = &yes
		o := Observe(h.host, parent, nil, true)
		h.eq(o.MaySend())
		h.eq(o.WithholdReason)
	})
	mirror(t, acr, "UnknownArchiveState.test_relaxing_the_evidence_requirement_is_explicit", func(h *hl) {
		h.eq(Observe(noArchiveInfo{h.host}, parent, nil, false).MaySend())
	})
}

func Test21_ACR03_a_correct_proof_from_a_real_later_turn_closes_the_attempt(t *testing.T) {
	mirror(t, acr, "Acknowledgement.test_a_correct_proof_from_a_real_later_turn_closes_the_attempt", func(h *hl) {
		event, turn := h.ackDispatched()
		result, err := h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, h.host)
		mustDo(t, err)
		h.eq(field(result, "accepted"))
		h.eq(field(result, "rejectionReason"))
		h.eq(field(result, "_verified"))
		h.eq(h.row(event).S("state"))
	})
}

func Test21_ACR04_an_ack_proof_mismatch_is_refused(t *testing.T) {
	mirror(t, acr, "Acknowledgement.test_an_echo_without_the_proof_never_closes_the_attempt", func(h *hl) {
		event, turn := h.ackDispatched()
		h.refusal(h.ack.Acknowledge(h.ctx, event, turn.TurnID, strings.Repeat("0", 64), true, nil, h.host))
		h.eq(h.row(event).S("state"))
	})
	mirror(t, acr, "Acknowledgement.test_a_proof_computed_over_someone_else_turn_is_refused", func(h *hl) {
		event, turn := h.ackDispatched()
		h.refusal(h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, "a-different-turn"), true, nil, h.host))
	})
}

func Test21_ACR05_an_unknown_turn_is_kept_unverified(t *testing.T) {
	mirror(t, acr, "Acknowledgement.test_a_turn_that_does_not_exist_does_not_close_the_attempt", func(h *hl) {
		event, _ := h.ackDispatched()
		result, err := h.ack.Acknowledge(h.ctx, event, "invented-turn", AckProof(event, "invented-turn"), true, nil, h.host)
		mustDo(t, err)
		h.eq(field(result, "_verified"))
		h.eq(h.row(event).S("state"))
	})
}

func Test21_ACR06_a_turn_that_started_before_the_delivery_is_refused(t *testing.T) {
	mirror(t, acr, "Acknowledgement.test_a_turn_that_started_before_the_delivery_is_refused", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		old := h.host.startTurn(parent, "older-turn", "completed", "")
		h.clock.Advance(120)
		h.attemptOn(event, h.host, at(h.clock.Now()))
		h.refusal(h.ack.Acknowledge(h.ctx, event, old.TurnID, AckProof(event, old.TurnID), true, nil, h.host))
	})
}

func Test21_ACR07_an_advanced_generation_is_a_disposition_conflict(t *testing.T) {
	t.Run("advanced before the ack", func(t *testing.T) {
		mirror(t, acr, "Acknowledgement.test_a_generation_that_advanced_after_dispatch_cannot_be_accepted", func(h *hl) {
			event, turn := h.ackDispatched()
			h.openGeneration("later", "needs_changes_revision", "later-turn")
			h.refusal(h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, h.host))
		})
	})
	t.Run("advanced during the ack", func(t *testing.T) {
		mirror(t, acr, "Acknowledgement.test_a_generation_advancing_during_the_acknowledgement_is_caught", func(h *hl) {
			event, turn := h.ackDispatched()
			racy := actsDuringTheTurnRead(h.host, func() { h.openGeneration("racy", "needs_changes_revision", "racy-turn") })
			// Python replaces read_turn permanently; the action here runs on the first read, the
			// only one the acknowledgement makes.
			h.refusal(h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, racy))
			h.eq(h.one("SELECT * FROM acks WHERE event_id = ?", event))
		})
	})
}

func Test21_ACR08_one_event_is_claimed_for_verification_once(t *testing.T) {
	mirror(t, acr, "Acknowledgement.test_one_event_is_verified_only_once", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		first, err := h.ack.ClaimVerification(h.ctx, event, "t1")
		mustDo(t, err)
		h.eq(first)
		second, err := h.ack.ClaimVerification(h.ctx, event, "t2")
		mustDo(t, err)
		h.eq(second)
	})
	mirror(t, acr, "Acknowledgement.test_a_duplicate_delivery_cannot_cause_a_second_verification", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		first, err := h.ack.ClaimVerification(h.ctx, event, "t1")
		mustDo(t, err)
		second, err := h.ack.ClaimVerification(h.ctx, event, "t1")
		mustDo(t, err)
		h.eq(first, second)
		h.eq(h.count("SELECT COUNT(*) AS c FROM verification_claims"))
	})
}

// verdictAcknowledged is Verdicts._acknowledged (and VerdictAtomicity._acknowledged).
func (h *hl) verdictAcknowledged(recipients []string) string {
	event := h.queuedEvent(regOpts{recipients: recipients})
	h.attemptOn(event, h.host, nil)
	h.clock.Advance(5)
	turn := h.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err := h.ack.Acknowledge(h.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, h.host)
	mustDo(h.t, err)
	return event
}

func (h *hl) verdict(event, verdict, turn string) (Obj, error) {
	return h.ack.RecordVerdict(h.ctx, event, verdict, turn, nil, nil, nil, nil)
}

func Test21_ACR09_a_verdict_requires_a_verified_acceptance(t *testing.T) {
	mirror(t, acr, "Verdicts.test_a_verdict_requires_a_verified_acceptance", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		h.attemptOn(event, h.host, nil)
		h.refusal(h.verdict(event, "verified", "v1"))
	})
}

func Test21_ACR10_needs_changes_opens_a_generation_and_queues_a_revision(t *testing.T) {
	mirror(t, acr, "Verdicts.test_needs_changes_opens_a_generation_and_queues_a_revision_to_the_same_child", func(h *hl) {
		event := h.verdictAcknowledged([]string{parent, child})
		record, err := h.verdict(event, "needs_changes", "verdict-1")
		mustDo(t, err)
		h.eq(field(record, "nextExecutionGeneration"))
		revision := h.one("SELECT * FROM deliveries WHERE kind = ?", Revision)
		h.eq(revision != nil)
		h.eq(revision.S("recipient_task_id"))
	})
}
