"""An uncertain send that left no trace is redelivered once or held by name (CRW-231).

CRW-124's H0-R4 K5ctl killed the isolated App Server after the relay sent turn/start for a
completion to the parent and before it read the answer. The attempt settled held_uncertain on an
outcome_unknown receipt with no turn id, the restarted host held no trace of the message, and
reconciliation kept the attempt held_uncertain_awaiting_evidence for ever while assignment-show
named a daemon that could not settle it and the only fault stayed below its publish threshold.

The loss is staged the way K5ctl left the host: the fake transport answers outcome_unknown and
neither starts a turn nor keeps the message. The four cases are read apart: that loss; the same
unknown send whose turn the host ran and kept (K5u, confirmed from its token); an accepted turn the
host lost afterwards (CRW-224, host_lost_turn); and a delivery waiting on the recipient's hourly
cap (O-H0R4-2), staged by filling the recipient's window.
"""

import json

from codex_session_relay import faults, faultsweep
from codex_session_relay.hostadapter import ListingBounded
from codex_session_relay.transport import DISPATCHED, HELD_UNCERTAIN, QUEUED

from .support import CHILD, PARENT
from .test_host_lost_turn import HostLossCase

UNKNOWN_LOST = "unknown_send_lost"
UNDECIDED = "unknown_send_undecided"
HOST_LOST = "host_lost_turn"


class _Wrapped:
    """A fake host with one read replaced, everything else passed through."""

    def __init__(self, inner):
        self.inner = inner

    def __getattr__(self, name):
        return getattr(self.inner, name)


class ListingNeverReachesTheSend(_Wrapped):
    def find_dispatched_turn(self, thread_id, turn_id, *, sent_at):
        raise ListingBounded("the bounded listing never reached the send")


class MessageLandsAfterTheThreadScan(_Wrapped):
    """The message reaches the parent between reconciliation's thread-wide scan and its read of
    the turns since the send."""

    def __init__(self, inner, turn_id, text):
        super().__init__(inner)
        self.turn_id, self.text = turn_id, text

    def find_token(self, thread_id, token, **kwargs):
        scan = self.inner.find_token(thread_id, token, **kwargs)
        self.inner.threads[PARENT].items.append((self.turn_id, self.text))
        return scan


class UnknownSendCase(HostLossCase):
    def unknown_send(self, *, history=True):
        """K5ctl: turn/start went out, the App Server died before answering, and the host kept
        nothing of it."""
        if history:
            self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("transport_unknown")
        record = self.attempt(event_id)
        self.assertEqual(record["transportReceiptStatus"], "outcome_unknown")
        self.assertIsNone(record["turnId"])
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        return event_id, record["requestId"]

    def ticks(self, count, seconds=120):
        for _ in range(count):
            self.clock.advance(seconds)
            self.daemon.tick()

    def recovery(self):
        return self.assignments.state(self._rid).get("recovery") or {}

    def fill_window(self, now):
        window = int(now // 3600) * 3600
        self.store.db.execute(
            "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
            " VALUES (?,?,?,?)",
            (PARENT, window, self.delivery.policy.max_sends_per_recipient_per_hour, now - 600))
        self.store.db.commit()
        return window

    def fault_states(self):
        ledger = faults.FaultLedger(self.store, self.clock)
        faultsweep.record_all(ledger, faultsweep.sweep(self.store), store=self.store)
        return sorted((row["state"], row["severity"]) for row in self.store.all(
            "SELECT state, severity FROM fault_ledger WHERE fault_class = 'delivery_stalled'"))


class AnUnknownSendTheHostKeptNoTraceOf(UnknownSendCase):
    def test_the_daemon_redelivers_the_same_event_once_under_the_next_attempt(self):
        event_id, first = self.unknown_send()
        self.ticks(1)
        attempts = self.attempts_for(event_id)
        self.assertEqual([row["state"] for row in attempts], [UNKNOWN_LOST, DISPATCHED])
        self.assertEqual([row["event_id"] for row in attempts], [event_id, event_id])
        self.assertEqual([send[0] for send in self.adapter.sends],
                         [first, attempts[1]["request_id"]])
        self.assertEqual(self.journalled(UNKNOWN_LOST), 1)
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack")
        self.assertEqual(self.next_action(), "parent_acknowledges")
        self.assertEqual(self.completion_delivery().get("unknownSendLostAttempts"), 1)
        # Nothing further is owed: later ticks read the redelivery and send nothing.
        self.ticks(3)
        self.assertEqual(len(self.adapter.sends), 2)
        self.assertEqual(self.journalled(UNKNOWN_LOST), 1)

    def test_reconcile_names_the_loss_and_the_actor_that_moves_it_without_sending(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome["state"], UNKNOWN_LOST)
        self.assertEqual(outcome.get("redelivery"), "queued")
        self.assertEqual(outcome.get("nextExpectedAction"), "daemon_redelivers_unknown_send_lost")
        self.assertEqual((outcome.get("recipientTrace") or {}).get("finding"), UNKNOWN_LOST)
        self.assertEqual(len(self.adapter.sends), 1)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (QUEUED, None))
        # No dispatch evidence exists for a send nobody can show arrived, so none is claimed.
        self.assertIsNone(row["dispatch_evidence"])
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("redelivering:unknown_send_lost", "redelivering:unknown_send_lost"))
        self.assertEqual(self.next_action(), "daemon_redelivers_unknown_send_lost")
        self.assertEqual(self.completion_delivery().get("unknownSendLostAttempts"), 1)

    def test_reconciling_a_lost_attempt_again_reports_it_and_changes_nothing(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        again = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((again["state"], again["evidence"], again.get("redelivery")),
                         (UNKNOWN_LOST, "none", "queued"))
        self.assertEqual(self.journalled(UNKNOWN_LOST), 1)
        self.assertEqual(self.delivery_row(event_id)["state"], QUEUED)
        self.assertEqual(self.attempt_states(event_id), [UNKNOWN_LOST])

    def test_a_second_unknown_loss_holds_the_obligation_by_name_instead_of_a_third_send(self):
        event_id, first = self.unknown_send()
        self.adapter.script("transport_unknown")
        self.ticks(4)
        self.assertEqual(self.attempt_states(event_id), [UNKNOWN_LOST, UNKNOWN_LOST])
        self.assertEqual(len(self.adapter.sends), 2)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], UNKNOWN_LOST)
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("held:unknown_send_lost", "held:unknown_send_lost"))
        self.assertEqual(self.next_action(), "parent_recovers_unknown_send_lost")
        self.assertEqual(self.recovery().get("command"),
                         f"codex-session-relay show --event {event_id}")

    def test_an_unknown_loss_after_a_host_lost_turn_is_held_and_claims_no_dispatch(self):
        event_id, _first, turn = self.dispatched()
        self.host_loses(turn)
        self.adapter.script("transport_unknown")
        self.ticks(4)
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, UNKNOWN_LOST])
        self.assertEqual(len(self.adapter.sends), 2)
        row = self.delivery_row(event_id)
        self.assertEqual((row["hold_reason"], row["dispatch_evidence"]), (UNKNOWN_LOST, None))
        self.assertEqual(self.next_action(), "parent_recovers_unknown_send_lost")

    def test_a_host_lost_turn_after_an_unknown_loss_is_held_under_its_own_name(self):
        event_id, _first = self.unknown_send()
        self.ticks(1)
        self.assertEqual(self.attempt_states(event_id), [UNKNOWN_LOST, DISPATCHED])
        second = self.attempts_for(event_id)[1]
        self.host_loses(json.loads(second["record"])["turnId"])
        self.ticks(4)
        self.assertEqual(self.attempt_states(event_id), [UNKNOWN_LOST, HOST_LOST])
        self.assertEqual(len(self.adapter.sends), 2)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], HOST_LOST)
        self.assertEqual(self.next_action(), "parent_recovers_host_lost_turn")

    def test_a_send_too_recent_to_judge_is_the_daemons_and_is_read_again_until_it_decides(self):
        event_id, _first = self.unknown_send()
        self.ticks(1, seconds=20)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")
        # Nothing on the host changed, so only the owed re-read can reach the decision.
        self.ticks(1, seconds=60)
        self.assertEqual(self.attempt_states(event_id)[0], UNKNOWN_LOST)
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_turn_still_running_since_the_send_holds_the_decision_until_it_ends(self):
        event_id, _first = self.unknown_send()
        self.clock.advance(5)
        running = self.adapter.start_turn(PARENT, status="inProgress")
        self.ticks(1)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")
        self.adapter.finish_turn(PARENT, running.turn_id, "completed")
        self.ticks(1, seconds=30)
        self.assertEqual(self.attempt_states(event_id)[0], UNKNOWN_LOST)
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_message_that_lands_between_the_two_scans_confirms_the_send(self):
        event_id, first = self.unknown_send()
        self.clock.advance(3)
        late = self.adapter.start_turn(PARENT, status="completed")
        self.clock.advance(120)
        adapter = MessageLandsAfterTheThreadScan(
            self.adapter, late.turn_id,
            f"[codex-session-relay] verification request\nrequestId: {first}")
        outcome = self.reconciler.reconcile_attempt(first, adapter)
        self.assertEqual(outcome["evidence"], "turn_found")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(len(self.adapter.sends), 1)


class ASendTheTransportHasNotAnsweredIsNotReadAsLost(UnknownSendCase):
    """The rule reads only a receipt the transport settled. An unfinished one is still the
    transport's to answer, and no receipt at all says nothing: neither is ever redelivered."""

    def test_an_unfinished_receipt_stays_the_daemons_to_reconcile(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        first = self.attempt(event_id)["requestId"]
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertNotIn("recipientTrace", outcome)
        self.assertEqual(outcome.get("nextExpectedAction"), "daemon_reconciles_delivery")
        self.ticks(3)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_claim_with_no_receipt_stays_the_daemons_to_reconcile(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        _attempt_no, request_id, _message = self.delivery._claim(
            event_id, now=self.clock.now(), owner="relay", recipient=PARENT)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertNotIn("recipientTrace", outcome)
        self.assertEqual(outcome.get("nextExpectedAction"), "daemon_reconciles_delivery")
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(self.adapter.sends, [])


class AnUndecidedReadingIsHeldByName(UnknownSendCase):
    def assert_held_undecided(self, event_id, request_id, reason):
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (HELD_UNCERTAIN, UNDECIDED))
        attempt = self.store.one("SELECT recipient_scan FROM attempts WHERE request_id = ?",
                                 (request_id,))
        self.assertEqual(attempt["recipient_scan"], f"{UNDECIDED}:{reason}")
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("held:unknown_send_undecided", "held:unknown_send_undecided"))
        self.assertEqual(self.next_action(), "parent_recovers_unknown_send_undecided")
        self.assertEqual(self.completion_delivery().get("turnCheck"), f"{UNDECIDED}:{reason}")
        self.assertEqual(self.recovery().get("command"),
                         f"codex-session-relay show --event {event_id}")

    def test_a_parent_listing_no_turns_holds_the_send_by_name(self):
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "listing_empty")
        self.ticks(3, seconds=700)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_token_only_in_another_item_type_is_held_by_name(self):
        event_id, first = self.unknown_send()
        self.clock.advance(2)
        hook = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.append((hook.turn_id, f"hook saw {first}",
                                                   "hookPrompt"))
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "token_in_other_item")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_scan_bounded_before_the_send_is_held_by_name(self):
        event_id, first = self.unknown_send()
        self.clock.advance(2)
        busy = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.extend(
            (busy.turn_id, f"work {n}", "commandExecution") for n in range(5))
        self.adapter.scan_limit = 3
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "token_scan_bounded")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_listing_that_never_reaches_the_send_is_held_by_name(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, ListingNeverReachesTheSend(self.adapter))
        self.assertEqual(outcome.get("nextExpectedAction"),
                         "parent_recovers_unknown_send_undecided")
        self.assert_held_undecided(event_id, first, "listing_bounded")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_later_reading_that_decides_replaces_the_undecided_hold(self):
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "listing_empty")
        # The parent works on; its list now reaches past the send and shows nothing of it.
        self.adapter.start_turn(PARENT, status="completed", text="the operator asked something")
        self.ticks(1)
        self.assertEqual(self.attempt_states(event_id)[0], UNKNOWN_LOST)
        self.assertIsNone(self.delivery_row(event_id)["hold_reason"])
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_message_found_later_clears_the_undecided_hold(self):
        event_id, first = self.unknown_send()
        self.clock.advance(2)
        hook = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.append((hook.turn_id, f"hook saw {first}",
                                                   "hookPrompt"))
        self.ticks(1)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], UNDECIDED)
        self.adapter.threads[PARENT].items.append(
            (hook.turn_id, f"[codex-session-relay] verification request\nrequestId: {first}"))
        self.ticks(1)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (DISPATCHED, None))
        self.assertEqual(self.next_action(), "parent_acknowledges")

    def test_an_undecided_redelivery_after_a_host_loss_keeps_no_dispatch_evidence(self):
        """The redelivery's sender died after the transport answered and before it settled, so
        reconciliation settles the attempt first."""
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        row = self.delivery_row(event_id)
        self.assertEqual((row["dispatch_evidence"], row["dispatch_turn_id"]), (HOST_LOST, turn))
        _attempt_no, request_id, _message = self.delivery._claim(
            event_id, now=self.clock.now(), owner="relay", recipient=PARENT)
        self.adapter.ledger[request_id] = {
            "requestId": request_id, "operation": "send_message_to_thread",
            "status": "outcome_unknown", "threadId": PARENT, "retrySafe": False,
            "error": "TransportError: turn/start: response unavailable; do not resend"}
        self.clock.advance(2)
        hook = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.append((hook.turn_id, f"hook saw {request_id}",
                                                   "hookPrompt"))
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(request_id, self.adapter)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (HELD_UNCERTAIN, UNDECIDED))
        self.assertEqual((row["dispatch_evidence"], row["dispatch_turn_id"]), (None, None))


class APersistingHoldReachesTheOperator(UnknownSendCase):
    def test_an_undecided_hold_opens_its_fault_instead_of_staying_observed(self):
        self.unknown_send(history=False)
        self.ticks(1)
        self.assertIn((faults.OPEN, faults.BROKEN), self.fault_states())

    def test_a_second_loss_opens_its_fault(self):
        self.unknown_send()
        self.adapter.script("transport_unknown")
        self.ticks(4)
        self.assertIn((faults.OPEN, faults.BROKEN), self.fault_states())


class ACorrectionLostTheSameWayIsTheParentsToRecover(UnknownSendCase):
    def test_a_revision_request_left_without_trace_is_held_not_resent(self):
        _completion, correction = self.correction_after_needs_changes()
        self.adapter.start_turn(CHILD, turn_id="child-earlier", status="completed")
        self.clock.advance(300)
        self.adapter.script("transport_unknown")
        record = self.attempt(correction)
        self.assertEqual(self.delivery_row(correction)["state"], HELD_UNCERTAIN)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        self.assertEqual(outcome.get("nextExpectedAction"), "parent_recovers_held_correction")
        row = self.delivery_row(correction)
        self.assertEqual((row["state"], row["hold_reason"]), (QUEUED, UNKNOWN_LOST))
        self.ticks(2)
        self.assertEqual([send[0] for send in self.adapter.sends
                          if send[1] == CHILD], [record["requestId"]])
        self.assertEqual(self.next_action(), "parent_recovers_held_correction")


class TheFourCasesReadApart(UnknownSendCase):
    """Criterion 4: the same K5 trigger at four moments, each with its own reading."""

    def reading(self, event_id):
        item = self.status_of(event_id)
        return (self.attempt_states(event_id), item["phase"], self.next_action())

    def test_death_before_the_answer_with_the_turn_lost(self):
        event_id, _first = self.unknown_send()
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(self.attempts_for(event_id)[0]["request_id"],
                                          self.adapter)
        self.assertEqual(self.reading(event_id),
                         ([UNKNOWN_LOST], "redelivering:unknown_send_lost",
                          "daemon_redelivers_unknown_send_lost"))

    def test_death_before_the_answer_with_the_turn_run_and_kept(self):
        event_id, first = self.unknown_send()
        self.clock.advance(3)
        self.adapter.start_turn(
            PARENT, status="completed",
            text=f"[codex-session-relay] verification request\nrequestId: {first}")
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(self.reading(event_id),
                         ([HELD_UNCERTAIN], "awaiting_ack", "parent_acknowledges"))
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])

    def test_death_after_the_answer_with_the_accepted_turn_lost(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(self.reading(event_id),
                         ([HOST_LOST], "redelivering:host_lost_turn",
                          "daemon_redelivers_host_lost_turn"))
        self.assertEqual(self.completion_delivery().get("unknownSendLostAttempts"), 0)

    def test_a_delivery_waiting_on_the_hourly_cap(self):
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        window = self.fill_window(now)
        self.assertIsNone(self.attempt(event_id, now=now))
        self.assertEqual(self.reading(event_id), ([], "awaiting_send:hourly_cap",
                                                  "daemon_delivers"))
        self.assertEqual((self.status_of(event_id).get("pacing") or {}).get("reopensAt"),
                         window + 3600)


class TheHourlyCapIsNamedWithItsReopenTime(UnknownSendCase):
    def capped(self):
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        return event_id, now, self.fill_window(now)

    def test_status_names_the_cap_and_a_reopen_time_that_does_not_move(self):
        event_id, now, window = self.capped()
        self.assertIsNone(self.attempt(event_id, now=now))
        item = self.status_of(event_id)
        pacing = item.get("pacing") or {}
        self.assertEqual((pacing.get("reason"), pacing.get("reopensAt")),
                         ("hourly_cap", window + 3600))
        self.assertEqual((pacing.get("sends"), pacing.get("cap")),
                         (self.delivery.policy.max_sends_per_recipient_per_hour,) * 2)
        self.assertEqual(item["nextEligibleAt"], window + 3600)
        self.assertIsNone(item["holdReason"])
        self.clock.advance(30)
        self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual(self.status_of(event_id)["nextEligibleAt"], window + 3600)

    def test_assignment_show_names_the_cap_and_its_reopen_time(self):
        event_id, now, window = self.capped()
        self.assertIsNone(self.attempt(event_id, now=now))
        pacing = self.completion_delivery().get("pacing") or {}
        self.assertEqual((pacing.get("reason"), pacing.get("reopensAt")),
                         ("hourly_cap", window + 3600))
        self.assertEqual(self.next_action(), "daemon_delivers")

    def test_a_claim_refused_on_the_cap_waits_for_the_window(self):
        event_id, now, window = self.capped()
        self.delivery._rate_limited = lambda recipient, at: False
        self.assertIsNone(self.attempt(event_id, now=now))
        self.assertEqual(self.delivery_row(event_id)["next_eligible_at"], window + 3600)

    def test_the_delivery_goes_out_when_the_window_reopens(self):
        event_id, now, window = self.capped()
        self.assertIsNone(self.attempt(event_id, now=now))
        self.clock.advance(window + 3600 - now)
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertIsNone(self.status_of(event_id).get("pacing"))
