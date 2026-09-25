"""An uncertain send that left no trace is held for the parent by name, never sent again (CRW-231).

CRW-124's H0-R4 K5ctl killed the isolated App Server after the relay sent turn/start for a
completion to the parent and before it read the answer. The attempt settled held_uncertain on an
outcome_unknown receipt with no turn id, the restarted host held no trace of the message, and
reconciliation kept the attempt held_uncertain_awaiting_evidence for ever while assignment-show
named a daemon that could not settle it and the only fault stayed below its publish threshold.

Nothing shows that a live App Server cannot still apply a turn/start whose answer never came back
(independent reviews of bb1b6af6 and c367abb7), so no reading sends it again. The recipient's own
turns and items since the send are read instead: no trace holds the delivery unknown_send_lost,
a reading no wait can decide holds it unknown_send_undecided, and both name the parent with the
command that reads the report. The delivery stays held_uncertain, so a message found later still
confirms it.

The loss is staged the way K5ctl left the host: the fake transport answers outcome_unknown and
neither starts a turn nor keeps the message. The four cases are read apart: that loss; the same
unknown send whose turn the host ran and kept (K5u, confirmed from its token); an accepted turn the
host lost afterwards (CRW-224, host_lost_turn); and a delivery waiting on the recipient's hourly
cap (O-H0R4-2), staged by filling the recipient's window.
"""

import json
import os
import shlex
import unittest

from codex_session_relay import faults, faultsweep
from codex_session_relay.hostadapter import TURN_ABSENT, ListingBounded, TurnInfo, find_in_listing
from codex_session_relay.transport import DISPATCHED, HELD_UNCERTAIN, QUEUED

from .support import CHILD, PARENT
from .test_host_lost_turn import HostLossCase

UNKNOWN_LOST = "unknown_send_lost"
UNDECIDED = "unknown_send_undecided"
HOST_LOST = "host_lost_turn"
NO_TRACE = UNKNOWN_LOST + ":no_trace"


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


def message(request_id):
    return f"[codex-session-relay] verification request\nrequestId: {request_id}"


class UnknownSendCase(HostLossCase):
    def unknown_send(self, *, history=True, restarted=True):
        """K5ctl: turn/start went out, the App Server died before answering, and the host kept
        nothing of it. restarted leaves the parent notLoaded, as the restarted host did; the
        reading does not depend on it."""
        if history:
            self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("transport_unknown")
        record = self.attempt(event_id)
        self.assertEqual(record["transportReceiptStatus"], "outcome_unknown")
        self.assertIsNone(record["turnId"])
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        if restarted:
            self.adapter.set_status(PARENT, "notLoaded")
        return event_id, record["requestId"]

    def ticks(self, count, seconds=120):
        for _ in range(count):
            self.clock.advance(seconds)
            self.daemon.tick()

    def sends_to(self, recipient=PARENT):
        return [send[0] for send in self.adapter.sends if send[1] == recipient]

    def recovery(self):
        return self.assignments.state(self._rid).get("recovery") or {}

    def mark(self, request_id):
        return self.store.one("SELECT recipient_scan FROM attempts WHERE request_id = ?",
                              (request_id,))["recipient_scan"]

    def assert_reads_the_event_from_this_store(self, command, event_id):
        """The recovery line runs a relay on THIS store: a shell without the original --state or
        environment override would otherwise open another one (Devin on bb1b6af6)."""
        argv = shlex.split(command or "")
        store_dir = os.path.dirname(os.path.abspath(str(self.store.path)))
        self.assertEqual(argv[-5:], ["--state", store_dir, "show", "--event", event_id])

    def assert_held(self, event_id, request_id, hold, mark):
        """Held for the parent by name: the delivery, the attempt, status and assignment-show all
        say so, and the parent is given the command that reads the report."""
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (HELD_UNCERTAIN, hold))
        self.assertEqual((row["dispatch_evidence"], row["dispatch_turn_id"]), (None, None))
        self.assertEqual(self.mark(request_id), mark)
        self.assertEqual(self.attempt_states(event_id)[-1], HELD_UNCERTAIN)
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]), (f"held:{hold}", f"held:{hold}"))
        self.assertEqual(self.next_action(), f"parent_recovers_{hold}")
        self.assertEqual(self.completion_delivery().get("turnCheck"), mark)
        recovery = self.recovery()
        self.assertEqual((recovery.get("actor"), recovery.get("reason")), ("parent", hold))
        self.assert_reads_the_event_from_this_store(recovery.get("command"), event_id)

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
    def test_it_is_held_for_the_parent_by_name_and_never_sent_again(self):
        """Independent reviews of bb1b6af6 and c367abb7: a live App Server can still apply a
        turn/start whose answer never came back, and nothing the relay reads shows it will not."""
        event_id, first = self.unknown_send()
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.ticks(6, seconds=700)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_a_recipient_its_host_still_holds_is_held_the_same_way(self):
        event_id, first = self.unknown_send(restarted=False)
        self.ticks(3)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_reconcile_names_the_parent_the_reason_and_the_command_without_sending(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((outcome["state"], outcome["evidence"]), (HELD_UNCERTAIN, "none"))
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("parent_recovers_unknown_send_lost", UNKNOWN_LOST))
        self.assertEqual((outcome.get("recipientTrace") or {}).get("finding"), UNKNOWN_LOST)
        recovery = outcome.get("recovery") or {}
        self.assertEqual(recovery.get("actor"), "parent")
        self.assert_reads_the_event_from_this_store(recovery.get("command"), event_id)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_reconciling_it_again_keeps_the_hold_and_sends_nothing(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        again = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((again["state"], again.get("nextExpectedAction")),
                         (HELD_UNCERTAIN, "parent_recovers_unknown_send_lost"))
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(self.sends_to(), [first])

    def test_a_hold_that_loses_its_race_to_a_confirmation_reports_the_confirmation(self):
        """Another reader confirmed the send between this reading and its held write: nothing is
        written over the confirmation and no hold is named."""
        from unittest import mock
        from codex_session_relay import hostloss

        event_id, first = self.unknown_send()
        self.clock.advance(120)
        original = hostloss.read_unknown_send

        def confirmed_first(*args, **kwargs):
            reading = original(*args, **kwargs)
            self.adapter.start_turn(PARENT, status="completed", text=message(first))
            self.assertEqual(self.reconciler.reconcile_attempt(first, self.adapter)["evidence"],
                             "turn_found")
            return reading

        with mock.patch.object(hostloss, "read_unknown_send", confirmed_first):
            outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((outcome.get("deliveryState"), outcome["evidence"]),
                         (DISPATCHED, "turn_found"))
        self.assertNotIn("nextExpectedAction", outcome)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (DISPATCHED, None))
        self.assertEqual(self.next_action(), "parent_acknowledges")
        self.assertEqual(self.sends_to(), [first])

    def test_an_acknowledgement_without_a_trace_is_held_the_same_way(self):
        """The parent acknowledged after the send, so it read this attempt whatever its items show
        now. Nothing is sent again either way; the hold names the parent, who holds the answer."""
        from codex_session_relay import identity

        event_id, first = self.unknown_send()
        self.clock.advance(5)
        ack_turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="completed")
        self.ack.acknowledge(event_id, ack_turn_id=ack_turn.turn_id,
                             ack_proof=identity.ack_proof(event_id, ack_turn.turn_id),
                             accepted=True, adapter=self.adapter)
        self.assertEqual(self.ack_row(event_id)["last_reason"], "delivery_unconfirmed")
        self.ticks(3)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])
        self.assertIn((faults.OPEN, faults.BROKEN), self.fault_states())

    def test_a_send_too_recent_to_judge_is_read_again_until_it_is_held(self):
        event_id, first = self.unknown_send()
        self.ticks(1, seconds=20)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertIsNone(self.delivery_row(event_id)["hold_reason"])
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")
        # Nothing on the host changed, so only the owed re-read can reach the decision.
        self.ticks(1, seconds=60)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_a_turn_still_running_since_the_send_holds_the_decision_until_it_ends(self):
        event_id, first = self.unknown_send()
        self.clock.advance(5)
        running = self.adapter.start_turn(PARENT, status="inProgress")
        self.ticks(1)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertIsNone(self.delivery_row(event_id)["hold_reason"])
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")
        self.adapter.finish_turn(PARENT, running.turn_id, "completed")
        self.ticks(1, seconds=30)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_a_message_that_turns_up_after_the_hold_confirms_the_send_and_clears_it(self):
        event_id, first = self.unknown_send()
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.adapter.start_turn(PARENT, status="completed", text=message(first))
        self.ticks(1)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (DISPATCHED, None))
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.next_action(), "parent_acknowledges")
        self.assertNotIn("recovery", self.assignments.state(self._rid))
        self.assertEqual(self.sends_to(), [first])

    def test_an_unknown_send_after_a_host_lost_turn_is_held_and_claims_no_dispatch(self):
        """CRW-224's one redelivery goes out as before; when that send's own answer is lost and
        the host keeps no trace of it, the delivery is held, with the first loss's dispatch
        evidence and turn cleared from a row whose current send is uncertain."""
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.adapter.script("transport_unknown")
        self.ticks(1)
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, HELD_UNCERTAIN])
        second = self.attempts_for(event_id)[1]["request_id"]
        self.adapter.set_status(PARENT, "notLoaded")
        self.ticks(3)
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, HELD_UNCERTAIN])
        self.assert_held(event_id, second, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first, second])

    def folded_unknown_send(self, *, later, running=False, kind=None):
        """The send was folded into a parent turn begun two minutes before it (a steer), and the
        App Server died before answering; the turn went on writing LATER items after the
        message, so the thread-wide scan's 200 newest items no longer reach it. kind types the
        item carrying the token (None: the message itself)."""
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.start_turn(PARENT, turn_id="folded", status="inProgress")
        self.clock.advance(120)
        self.adapter.script("transport_unknown")
        first = self.attempt(event_id)["requestId"]
        thread = self.adapter.threads[PARENT]
        if kind is None:
            thread.items.append(("folded", message(first)))
        else:
            thread.items.append(("folded", f"hook saw {first}", kind))
        thread.items.extend(("folded", f"later work {n}", "commandExecution")
                            for n in range(later))
        if not running:
            self.adapter.finish_turn(PARENT, "folded", "completed")
        return event_id, first

    def test_a_send_folded_into_an_older_turn_is_found_there_and_not_sent_again(self):
        """Devin on d369a9e7: the since-send scan stops at a turn begun before the send, and a
        turn/start that steered that turn put the message in it."""
        event_id, first = self.folded_unknown_send(later=205)
        self.ticks(1)
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.ticks(3)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(self.sends_to(), [first])

    def test_a_token_only_in_a_hook_prompt_of_the_folded_turn_is_held_by_name(self):
        """Independent review of c367abb7: the folded turn's own items were read for the message
        alone, so a token that sat there only in a hook prompt passed as no trace."""
        event_id, first = self.folded_unknown_send(later=205, kind="hookPrompt")
        # Restarted, as K5ctl's host was, so no reading of the host's own state stands in for
        # the one in the folded turn's items.
        self.adapter.set_status(PARENT, "notLoaded")
        self.ticks(1)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:token_in_other_item")
        self.ticks(3, seconds=700)
        self.assertEqual(self.sends_to(), [first])

    def test_a_turn_begun_just_after_the_send_does_not_hide_the_folded_one(self):
        """Independent review of aaa190a6: a turn begun half a second after the send was read as
        the one the send could have been folded into, and the turn that was running at the send,
        which the listing stopped at, was never read."""
        event_id, first = self.folded_unknown_send(later=205)
        self.clock.advance(0.5)
        self.adapter.start_turn(PARENT, turn_id="later", status="completed", text="unrelated")
        self.ticks(1)
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.sends_to(), [first])

    def test_a_hook_prompt_in_the_folded_turn_behind_a_later_turn_is_held_by_name(self):
        event_id, first = self.folded_unknown_send(later=205, kind="hookPrompt")
        self.clock.advance(0.5)
        self.adapter.start_turn(PARENT, turn_id="later", status="completed", text="unrelated")
        self.adapter.set_status(PARENT, "notLoaded")
        self.ticks(1)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:token_in_other_item")
        self.assertEqual(self.sends_to(), [first])

    def test_a_send_folded_into_a_turn_begun_just_before_it_is_found_there(self):
        """Independent review of 668890b0: a turn begun inside the allowance before the send is
        read by the since-send scan only as far as its bound, and the message it took sat behind
        205 later items of the same turn."""
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.start_turn(PARENT, turn_id="just-before", status="inProgress")
        self.clock.advance(10)
        self.adapter.script("transport_unknown")
        first = self.attempt(event_id)["requestId"]
        thread = self.adapter.threads[PARENT]
        thread.items.append(("just-before", message(first)))
        thread.items.extend(("just-before", f"later work {n}", "commandExecution")
                            for n in range(205))
        self.adapter.finish_turn(PARENT, "just-before", "completed")
        self.adapter.set_status(PARENT, "notLoaded")
        self.ticks(1)
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.sends_to(), [first])

    def test_an_older_turn_still_running_holds_the_decision(self):
        event_id, first = self.folded_unknown_send(later=205, running=True)
        self.adapter.threads[PARENT].items = [
            item for item in self.adapter.threads[PARENT].items if first not in item[1]]
        self.ticks(1)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertIsNone(self.delivery_row(event_id)["hold_reason"])
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")
        self.assertEqual(self.sends_to(), [first])

    def test_an_older_turn_too_long_to_read_through_holds_the_send_by_name(self):
        event_id, first = self.folded_unknown_send(later=0)
        thread = self.adapter.threads[PARENT]
        # The message sits past the in-turn read's bound, behind 2000 items of the same turn.
        carried = thread.items.pop()
        thread.items.extend(("folded", f"earlier work {n}", "commandExecution")
                            for n in range(2000))
        thread.items.append(carried)
        thread.items.extend(("folded", f"later work {n}", "commandExecution") for n in range(205))
        self.ticks(1)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:token_scan_bounded")
        self.assertEqual(self.sends_to(), [first])

    def test_a_message_that_lands_between_the_two_scans_confirms_the_send(self):
        event_id, first = self.unknown_send()
        self.clock.advance(3)
        late = self.adapter.start_turn(PARENT, status="completed")
        self.clock.advance(120)
        adapter = MessageLandsAfterTheThreadScan(self.adapter, late.turn_id, message(first))
        outcome = self.reconciler.reconcile_attempt(first, adapter)
        self.assertEqual(outcome["evidence"], "turn_found")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(self.sends_to(), [first])


class TheListingSinceASendWithNoTurnId(unittest.TestCase):
    def test_a_listed_turn_without_an_id_is_not_taken_for_the_sends_turn(self):
        presence = find_in_listing(
            [([TurnInfo(None, "completed", 100.0), TurnInfo("older", "completed", 10.0)], False)],
            None, 150.0)
        self.assertEqual((presence.finding, presence.stop, presence.seen, presence.older),
                         (TURN_ABSENT, "older_than_send", (None,), ("older",)))
        self.assertEqual([turn.status for turn in presence.seen_turns], ["completed"])
        self.assertEqual(presence.stop_turn.turn_id, "older")


class ASendTheTransportHasNotAnsweredIsHeldByName(UnknownSendCase):
    """An unfinished receipt - what a transport that crashed mid-request leaves for good - or none
    at all is held for the parent under the receipt's name where the recipient keeps no trace
    (independent review of d369a9e7)."""

    def unfinished(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        request_id = self.attempt(event_id)["requestId"]
        self.adapter.set_status(PARENT, "notLoaded")
        return event_id, request_id

    def test_an_unfinished_receipt_is_held_for_the_parent_by_name(self):
        event_id, first = self.unfinished()
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("parent_recovers_unknown_send_undecided",
                          "unknown_send_undecided:receipt_unsettled"))
        self.ticks(3)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:receipt_unsettled")
        self.assertEqual(self.sends_to(), [first])

    def test_a_receipt_that_settles_later_lets_the_rule_decide(self):
        event_id, first = self.unfinished()
        self.ticks(1)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], UNDECIDED)
        # The transport comes back and settles the request as sent with no answer.
        self.adapter.ledger[first] = dict(self.adapter.ledger[first], status="outcome_unknown",
                                          error="TransportError: turn/start: no answer")
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_a_claim_with_no_receipt_is_held_for_the_parent_by_name(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        _attempt_no, request_id, _message = self.delivery._claim(
            event_id, now=self.clock.now(), owner="relay", recipient=PARENT)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("parent_recovers_unknown_send_undecided",
                          "unknown_send_undecided:receipt_missing"))
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(self.adapter.sends, [])

    def test_an_unreadable_receipt_takes_no_reading(self):
        event_id, first = self.unfinished()
        self.clock.advance(120)
        self.adapter.fail_reads("get_operation")
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertNotIn("recipientTrace", outcome)
        self.assertEqual(outcome.get("nextExpectedAction"), "daemon_reconciles_delivery")
        self.assertIsNone(self.delivery_row(event_id)["hold_reason"])


class AnUndecidedReadingIsHeldByName(UnknownSendCase):
    def assert_held_undecided(self, event_id, request_id, reason):
        self.assert_held(event_id, request_id, UNDECIDED, f"{UNDECIDED}:{reason}")

    def test_a_parent_listing_no_turns_holds_the_send_by_name(self):
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "listing_empty")
        self.ticks(3, seconds=700)
        self.assertEqual(self.sends_to(), [first])

    def test_a_token_only_in_another_item_type_is_held_by_name(self):
        event_id, first = self.unknown_send()
        self.clock.advance(2)
        hook = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.append((hook.turn_id, f"hook saw {first}",
                                                   "hookPrompt"))
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "token_in_other_item")
        self.assertEqual(self.sends_to(), [first])

    def test_a_scan_bounded_before_the_send_is_held_by_name(self):
        event_id, first = self.unknown_send()
        self.clock.advance(2)
        busy = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.extend(
            (busy.turn_id, f"work {n}", "commandExecution") for n in range(5))
        self.adapter.scan_limit = 3
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "token_scan_bounded")
        self.assertEqual(self.sends_to(), [first])

    def test_a_listing_that_never_reaches_the_send_is_held_by_name(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, ListingNeverReachesTheSend(self.adapter))
        self.assertEqual(outcome.get("nextExpectedAction"),
                         "parent_recovers_unknown_send_undecided")
        self.assert_held_undecided(event_id, first, "listing_bounded")
        self.assertEqual(self.sends_to(), [first])

    def test_a_later_reading_that_decides_replaces_the_undecided_hold(self):
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "listing_empty")
        # The parent works on; its list now reaches past the send and shows nothing of it.
        self.adapter.start_turn(PARENT, status="completed", text="the operator asked something")
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_a_pass_that_cannot_decide_keeps_the_undecided_hold(self):
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "listing_empty")
        # The parent starts a turn that is still running: the next reading is not ready, which
        # decides nothing, so the name and the hold stay. Reconciled directly: nothing the
        # daemon's gate fingerprints changed, so a tick would not reach the reading at all.
        self.adapter.start_turn(PARENT, status="inProgress")
        self.clock.advance(20)
        pending = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertTrue(pending["recipientTrace"]["pending"])
        # Still held for the parent, and the answer says so (Devin on d369a9e7).
        self.assertEqual(pending.get("nextExpectedAction"),
                         "parent_recovers_unknown_send_undecided")
        self.assert_held_undecided(event_id, first, "listing_empty")
        # A pass whose own read fails takes no reading at all, and keeps them too.
        self.adapter.fail_reads("find_token")
        unread = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertNotIn("recipientTrace", unread)
        self.assertEqual((unread.get("nextExpectedAction"), unread.get("reason")),
                         ("parent_recovers_unknown_send_undecided",
                          "unknown_send_undecided:listing_empty"))
        self.assert_held_undecided(event_id, first, "listing_empty")
        self.assertEqual(self.sends_to(), [first])

    def test_an_older_reading_does_not_overwrite_a_newer_hold(self):
        """Devin on aaa190a6: two reconciliations read the host at different moments, and the
        one that read first wrote last, replacing the newer decision with its older one."""
        from unittest import mock
        from codex_session_relay import hostloss

        event_id, first = self.unknown_send(history=False)
        # Reconciled once already, so both readers start from the same settled attempt.
        self.ticks(1)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:listing_empty")
        original = hostloss.read_unknown_send
        raced = []

        def newer_first(*args, **kwargs):
            reading = original(*args, **kwargs)
            if not raced:
                raced.append(reading)
                # The parent works on, and another reconciliation reads that and decides first.
                self.adapter.start_turn(PARENT, status="completed", text="the operator asked")
                newer = self.reconciler.reconcile_attempt(first, self.adapter)
                self.assertEqual(newer.get("reason"), UNKNOWN_LOST)
            return reading

        with mock.patch.object(hostloss, "read_unknown_send", newer_first):
            outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(raced[0]["undecided"], "listing_empty")
        self.assertTrue(outcome.get("changed"))
        # The answer it gives is the hold as it now stands, with the actor and the command that
        # recovers it (independent review of c86dc362).
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("parent_recovers_unknown_send_lost", UNKNOWN_LOST))
        self.assert_reads_the_event_from_this_store((outcome.get("recovery") or {}).get("command"),
                                                    event_id)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_an_undecided_hold_is_read_again_later_though_the_items_did_not_change(self):
        """Devin on d369a9e7: the gate fingerprints the parent's items, and a turn that shows up
        without any leaves them unchanged, so an undecided reading was never taken again."""
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held_undecided(event_id, first, "listing_empty")
        self.adapter.start_turn(PARENT, status="completed")
        self.ticks(1, seconds=30)
        self.assert_held_undecided(event_id, first, "listing_empty")
        self.ticks(1, seconds=600)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assertEqual(self.sends_to(), [first])

    def test_a_message_found_later_clears_the_undecided_hold(self):
        event_id, first = self.unknown_send()
        self.clock.advance(2)
        hook = self.adapter.start_turn(PARENT, status="completed")
        self.adapter.threads[PARENT].items.append((hook.turn_id, f"hook saw {first}",
                                                   "hookPrompt"))
        self.ticks(1)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], UNDECIDED)
        self.adapter.threads[PARENT].items.append((hook.turn_id, message(first)))
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
    def test_a_lost_hold_opens_its_fault_instead_of_staying_observed(self):
        self.unknown_send()
        self.ticks(1)
        self.assertIn((faults.OPEN, faults.BROKEN), self.fault_states())

    def test_an_undecided_hold_opens_its_fault_instead_of_staying_observed(self):
        self.unknown_send(history=False)
        self.ticks(1)
        self.assertIn((faults.OPEN, faults.BROKEN), self.fault_states())

    def test_a_wait_the_daemon_still_ends_stays_below_the_threshold(self):
        """A send too recent to judge is still the daemon's, and its fault is not published as
        broken for it."""
        self.unknown_send()
        self.ticks(1, seconds=20)
        self.assertNotIn((faults.OPEN, faults.BROKEN), self.fault_states())


class ACorrectionLostTheSameWayIsTheParentsToRecover(UnknownSendCase):
    def test_a_revision_request_left_without_trace_is_held_not_resent(self):
        _completion, correction = self.correction_after_needs_changes()
        self.adapter.start_turn(CHILD, turn_id="child-earlier", status="completed")
        self.clock.advance(300)
        self.adapter.script("transport_unknown")
        record = self.attempt(correction)
        self.assertEqual(self.delivery_row(correction)["state"], HELD_UNCERTAIN)
        self.adapter.set_status(CHILD, "notLoaded")
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        self.assertEqual(outcome.get("nextExpectedAction"), "parent_recovers_held_correction")
        self.assert_reads_the_event_from_this_store((outcome.get("recovery") or {}).get("command"),
                                                    correction)
        row = self.delivery_row(correction)
        self.assertEqual((row["state"], row["hold_reason"]), (HELD_UNCERTAIN, UNKNOWN_LOST))
        self.ticks(3, seconds=700)
        self.assertEqual(self.sends_to(CHILD), [record["requestId"]])
        self.assertEqual(self.next_action(), "parent_recovers_held_correction")
        # Independent review of bb1b6af6: the parent is named with the command that supports it.
        self.assert_reads_the_event_from_this_store(self.recovery().get("command"), correction)


class TheFourCasesReadApart(UnknownSendCase):
    """Criterion 4: the same K5 trigger at four moments, each with its own reading, and none sent
    twice except CRW-224's one redelivery of a turn the host accepted and lost."""

    def reading(self, event_id):
        item = self.status_of(event_id)
        return (self.attempt_states(event_id), item["phase"], self.next_action())

    def test_death_before_the_answer_with_the_turn_lost(self):
        event_id, first = self.unknown_send()
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(self.reading(event_id),
                         ([HELD_UNCERTAIN], "held:unknown_send_lost",
                          "parent_recovers_unknown_send_lost"))
        self.assertEqual(self.sends_to(), [first])

    def test_death_before_the_answer_with_the_turn_run_and_kept(self):
        event_id, first = self.unknown_send()
        self.clock.advance(3)
        self.adapter.start_turn(PARENT, status="completed", text=message(first))
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(self.reading(event_id),
                         ([HELD_UNCERTAIN], "awaiting_ack", "parent_acknowledges"))
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.sends_to(), [first])

    def test_death_after_the_answer_with_the_accepted_turn_lost(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(self.reading(event_id),
                         ([HOST_LOST], "redelivering:host_lost_turn",
                          "daemon_redelivers_host_lost_turn"))
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"], row["dispatch_evidence"]),
                         (QUEUED, None, HOST_LOST))

    def test_a_delivery_waiting_on_the_hourly_cap(self):
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        window = self.fill_window(now)
        self.assertIsNone(self.attempt(event_id, now=now))
        self.assertEqual(self.reading(event_id), ([], "awaiting_send:hourly_cap",
                                                  "daemon_delivers"))
        self.assertEqual((self.status_of(event_id).get("pacing") or {}).get("reopensAt"),
                         window + 3600)
        self.assertEqual(self.adapter.sends, [])


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
        # The budget is read again within a minute, so a changed policy takes effect; the
        # reopen time the cap names does not move (Devin on c27051a5).
        self.assertEqual(item["nextEligibleAt"], now + 60)
        self.assertIsNone(item["holdReason"])
        self.clock.advance(30)
        self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual((self.status_of(event_id).get("pacing") or {}).get("reopensAt"),
                         window + 3600)
        self.clock.advance(60)
        self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual((self.status_of(event_id).get("pacing") or {}).get("reopensAt"),
                         window + 3600)
        self.assertEqual(self.adapter.sends, [])

    def test_assignment_show_names_the_cap_and_its_reopen_time(self):
        event_id, now, window = self.capped()
        self.assertIsNone(self.attempt(event_id, now=now))
        pacing = self.completion_delivery().get("pacing") or {}
        self.assertEqual((pacing.get("reason"), pacing.get("reopensAt")),
                         ("hourly_cap", window + 3600))
        self.assertEqual(self.next_action(), "daemon_delivers")

    def test_a_claim_refused_on_the_cap_reads_the_budget_again_within_a_minute(self):
        event_id, now, window = self.capped()
        self.delivery._rate_limited = lambda recipient, at: False
        self.assertIsNone(self.attempt(event_id, now=now))
        self.assertEqual(self.delivery_row(event_id)["next_eligible_at"], now + 60)

    def test_a_raised_cap_releases_the_delivery_within_a_minute(self):
        """Devin on c27051a5: a refusal scheduled for the window's end kept the delivery waiting
        under the old cap after the operator raised it, with nothing in status saying why."""
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.policy import RetryPolicy

        event_id, now, window = self.capped()
        self.assertIsNone(self.attempt(event_id, now=now))
        raised = DeliveryService(self.store, self.registry, self.intake, self.clock,
                                 policy=RetryPolicy(max_sends_per_recipient_per_hour=24))
        self.clock.advance(60)
        self.assertLess(self.clock.now(), window + 3600)
        record = raised.attempt(event_id, self.adapter, now=self.clock.now())
        self.assertEqual((record or {}).get("deliveryState"), DISPATCHED)

    def test_a_lifted_zero_cap_releases_the_delivery_within_a_minute(self):
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.policy import RetryPolicy

        zero = DeliveryService(self.store, self.registry, self.intake, self.clock,
                               policy=RetryPolicy(max_sends_per_recipient_per_hour=0))
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        self.assertIsNone(zero.attempt(event_id, self.adapter, now=now))
        self.clock.advance(60)
        record = self.attempt(event_id, now=self.clock.now())
        self.assertEqual((record or {}).get("deliveryState"), DISPATCHED)

    def test_a_spent_cap_is_named_before_the_gap_when_both_hold(self):
        """Independent review of d369a9e7: with the cap spent and a send a moment ago, the gap was
        named and the reopen time was five seconds away rather than the window's end."""
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        window = int(now // 3600) * 3600
        self.store.db.execute(
            "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
            " VALUES (?,?,?,?)",
            (PARENT, window, self.delivery.policy.max_sends_per_recipient_per_hour, now - 1))
        self.store.db.commit()
        self.assertIsNone(self.attempt(event_id, now=now))
        item = self.status_of(event_id)
        self.assertEqual(((item.get("pacing") or {}).get("reason"),
                          (item.get("pacing") or {}).get("reopensAt"), item["phase"],
                          item["nextEligibleAt"]),
                         ("hourly_cap", window + 3600, "awaiting_send:hourly_cap", now + 60))

    def test_a_cap_of_zero_says_nothing_reopens_it_but_a_changed_policy(self):
        from codex_session_relay.policy import RetryPolicy

        pacing = RetryPolicy(max_sends_per_recipient_per_hour=0).pacing(
            self.clock.now(), sends=0, last=None)
        self.assertEqual((pacing["reason"], pacing["reopensAt"]), ("hourly_cap", None))
        self.assertIn("changed policy", pacing.get("detail") or "")

    def test_a_cap_of_zero_is_read_again_each_minute_and_names_the_operator(self):
        """Independent review of bb1b6af6: a cap of zero was rescheduled every five seconds and
        read daemon_delivers, which under that policy never can. It is read again once a minute,
        so a lifted cap takes effect (Devin on c27051a5)."""
        from codex_session_relay.assignment import AssignmentView
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.policy import RetryPolicy

        policy = RetryPolicy(max_sends_per_recipient_per_hour=0)
        delivery = DeliveryService(self.store, self.registry, self.intake, self.clock,
                                   policy=policy)
        view = AssignmentView(self.store, self.registry, self.clock, policy=policy)
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        self.assertIsNone(delivery.attempt(event_id, self.adapter, now=now))
        self.assertEqual(self.delivery_row(event_id)["next_eligible_at"], now + 60)
        self.assertEqual(view.state(self._rid)["nextExpectedAction"],
                         "operator_changes_send_policy")
        self.assertEqual(self.adapter.sends, [])

    def test_a_zero_cap_names_the_operator_for_a_correction(self):
        """Independent review of 668890b0: the correction's answer still named the daemon under a
        cap of zero."""
        from codex_session_relay.assignment import AssignmentView
        from codex_session_relay.policy import RetryPolicy

        _completion, _correction = self.correction_after_needs_changes()
        policy = RetryPolicy(max_sends_per_recipient_per_hour=0)
        view = AssignmentView(self.store, self.registry, self.clock, policy=policy)
        self.assertEqual(view.state(self._rid)["nextExpectedAction"],
                         "operator_changes_send_policy")

    def test_assignment_show_reads_the_budget_the_delivery_service_paces_by(self):
        from types import SimpleNamespace
        from codex_session_relay import cli
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.policy import RetryPolicy

        services = cli.Services(SimpleNamespace(state=self.tmp + "/cli-state", socket=None))
        services._store = self.store
        services._delivery = DeliveryService(
            self.store, self.registry, self.intake, self.clock,
            policy=RetryPolicy(max_sends_per_recipient_per_hour=0))
        self.assertIs(services.assignments.policy, services.delivery.policy)

    def test_a_delivery_held_by_its_own_later_backoff_is_not_reported_as_paced(self):
        """Devin on bb1b6af6: a delivery waiting out its own backoff past the window's end is held
        by that backoff, not by the recipient's spent cap."""
        event_id, now, window = self.capped()
        later = window + 3600 + 1800
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'deferred_busy', next_eligible_at = ?"
                       " WHERE event_id = ?", (later, event_id))
        item = self.status_of(event_id)
        self.assertEqual((item.get("pacing"), item["phase"], item["nextEligibleAt"]),
                         (None, "parent_busy", later))
        self.assertIsNone(self.completion_delivery().get("pacing"))

    def test_the_delivery_goes_out_when_the_window_reopens(self):
        event_id, now, window = self.capped()
        self.assertIsNone(self.attempt(event_id, now=now))
        self.clock.advance(window + 3600 - now)
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertIsNone(self.status_of(event_id).get("pacing"))

class AHoldReachesItsFaultWhateverTheSweepSawFirst(UnknownSendCase):
    """CRW-124 R5 F-R5-1: in live timing the fault sweep reads the settled uncertain attempt about
    twenty seconds after the send, before the 61 s allowance lets any reading name a hold, and
    records it degraded. The hold, named later, has to reach the same fault as broken and name
    itself whatever the sweep recorded first."""

    def stall(self):
        return self.store.one("SELECT * FROM fault_ledger WHERE fault_class = 'delivery_stalled'")

    def keys(self):
        return sorted(row["occurrence_key"] for row in self.store.all(
            "SELECT o.occurrence_key FROM fault_occurrences o"
            "  JOIN fault_ledger f ON f.fault_id = o.fault_id"
            " WHERE f.fault_class = 'delivery_stalled'"))

    def publications(self):
        return [(row["trigger_key"], row["summary"]) for row in self.store.all(
            "SELECT p.trigger_key, p.summary FROM fault_publications p"
            "  JOIN fault_ledger f ON f.fault_id = p.fault_id"
            " WHERE f.fault_class = 'delivery_stalled' ORDER BY p.rowid")]

    def assert_broken_naming(self, hold):
        self.assertEqual(self.fault_states(), [(faults.OPEN, faults.BROKEN)])
        self.assertIn(f"is held: {hold}", self.stall()["detail"])

    def test_a_hold_named_after_the_sweep_recorded_its_attempt_breaks_the_fault(self):
        event_id, first = self.unknown_send()
        self.clock.advance(20)
        self.assertEqual(self.fault_states(), [(faults.OBSERVED, faults.DEGRADED)])
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assert_broken_naming(UNKNOWN_LOST)
        # Two readings of one attempt: the attempt, and the hold named on it.
        self.assertEqual(self.keys(), [f"delivery:{first}", f"delivery:{first}:held:{UNKNOWN_LOST}"])
        self.assertEqual(self.stall()["occurrence_count"], 2)
        opened = [summary for trigger, summary in self.publications() if trigger == "open"]
        self.assertTrue(opened, self.publications())
        self.assertIn("severity: broken", opened[-1])
        self.assertIn(f"is held: {UNKNOWN_LOST}", opened[-1])
        self.assertEqual(self.sends_to(), [first])

    def test_an_undecided_hold_named_after_the_sweep_breaks_the_fault_too(self):
        event_id, first = self.unknown_send(history=False)
        self.clock.advance(20)
        self.assertEqual(self.fault_states(), [(faults.OBSERVED, faults.DEGRADED)])
        self.ticks(1)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:listing_empty")
        self.assert_broken_naming(UNDECIDED)

    def test_a_fault_already_open_degraded_escalates_when_the_hold_is_named(self):
        """The R5 shape: earlier occurrences for the same recipient and attempt state had already
        opened the fault degraded when this hold was named."""
        event_id, first = self.unknown_send()
        self.clock.advance(20)
        batch = faultsweep.sweep(self.store)
        seen = [entry for entry in batch["observations"]
                if entry["faultClass"] == "delivery_stalled"]
        self.assertEqual([entry["occurrenceKey"] for entry in seen], [f"delivery:{first}"])
        ledger = faults.FaultLedger(self.store, self.clock)
        faultsweep.record_all(ledger, batch, store=self.store)
        for n in (1, 2):
            ledger.record(dict(seen[0], occurrenceKey=f"delivery:earlier-{n}"))
        self.assertEqual(self.fault_states(), [(faults.OPEN, faults.DEGRADED)])
        self.ticks(1)
        self.assert_broken_naming(UNKNOWN_LOST)
        escalated = [summary for trigger, summary in self.publications()
                     if trigger == "escalate:broken"]
        self.assertTrue(escalated, self.publications())
        self.assertIn(f"is held: {UNKNOWN_LOST}", escalated[-1])

    def test_a_hold_named_before_any_sweep_keeps_its_name_through_later_sweeps(self):
        event_id, first = self.unknown_send()
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assert_broken_naming(UNKNOWN_LOST)
        self.clock.advance(700)
        self.assert_broken_naming(UNKNOWN_LOST)
        self.assertEqual(self.keys(), [f"delivery:{first}:held:{UNKNOWN_LOST}"])
        self.assertEqual(self.stall()["occurrence_count"], 1)

    def test_a_host_lost_turn_held_after_two_losses_keeps_its_keys(self):
        """CRW-224 unchanged: each lost attempt is one occurrence under its own request id."""
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.ticks(1)
        second = self.attempts_for(event_id)[1]
        self.host_loses(json.loads(second["record"])["turnId"])
        self.ticks(4)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], HOST_LOST)
        self.fault_states()
        self.assertEqual(self.keys(), [f"delivery:{first}", f"delivery:{second['request_id']}"])

    def test_an_unknown_send_held_after_a_host_loss_reads_both_attempts(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.adapter.script("transport_unknown")
        self.ticks(1)
        second = self.attempts_for(event_id)[1]["request_id"]
        self.adapter.set_status(PARENT, "notLoaded")
        self.ticks(3)
        self.assert_held(event_id, second, UNKNOWN_LOST, NO_TRACE)
        self.fault_states()
        self.assertEqual(self.keys(),
                         [f"delivery:{first}", f"delivery:{second}:held:{UNKNOWN_LOST}"])

    def test_a_hold_that_changes_name_updates_the_one_fault_it_is_on(self):
        """Independent review of ea197703: an undecided hold that a later reading decides as lost
        is a new occurrence on the same open, broken fault. Its detail - what fault-show reads -
        names the current hold; the published record is the one the fault opened with, because the
        ledger publishes on open, escalation and reopening, never on a changed detail, for every
        fault kind (assignment-show and reconcile name the current hold, and the actor and command
        are the same for both)."""
        event_id, first = self.unknown_send(history=False)
        self.ticks(1)
        self.assert_held(event_id, first, UNDECIDED, f"{UNDECIDED}:listing_empty")
        self.assert_broken_naming(UNDECIDED)
        opened = self.publications()
        self.adapter.start_turn(PARENT, status="completed", text="the operator asked something")
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.assert_broken_naming(UNKNOWN_LOST)
        self.assertEqual(self.keys(), [f"delivery:{first}:held:{UNKNOWN_LOST}",
                                       f"delivery:{first}:held:{UNDECIDED}"])
        self.assertEqual(len(self.store.all(
            "SELECT fault_id FROM fault_ledger WHERE fault_class = 'delivery_stalled'")), 1)
        self.assertEqual(self.publications(), opened)
        self.assertEqual(self.sends_to(), [first])


class ASupersededHoldNamesTheSupersession(UnknownSendCase):
    """CRW-124 R5 O-R5-1: after the parent recovered by opening a new generation, reconcile on the
    old generation's held attempt still named parent_recovers_unknown_send_lost, while status
    called the delivery superseded. Nothing is owed on it any more."""

    def open_generation_two(self):
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-2", status="inProgress")
        self.registry.open_generation(self._rid, dispatch_request_id="dispatch-2",
                                      reason="needs_changes_revision",
                                      dispatch_turn_id="turn-dispatch-2")

    def test_a_held_send_a_new_generation_replaced_names_the_supersession(self):
        event_id, first = self.unknown_send()
        self.ticks(1)
        self.assert_held(event_id, first, UNKNOWN_LOST, NO_TRACE)
        self.open_generation_two()
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("none", "superseded:stale_generation"))
        self.assertNotIn("recovery", outcome)
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("superseded:stale_generation", "superseded:stale_generation"))
        self.ticks(3, seconds=700)
        self.assertEqual(self.sends_to(), [first])

    def test_a_send_superseded_before_its_hold_is_named_reports_the_supersession(self):
        """Independent review of ea197703: superseded inside the allowance, before any reading
        could name a hold, the delivery's phase said superseded while its reported state still
        said it was waiting for evidence."""
        event_id, first = self.unknown_send()
        self.clock.advance(20)
        self.open_generation_two()
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("superseded:stale_generation", "superseded:stale_generation"))
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("none", "superseded:stale_generation"))
        self.assertNotIn("recovery", outcome)
        self.assertEqual(self.sends_to(), [first])

    def answered_correction(self):
        """A correction held unknown_send_lost whose generation a final event then answered."""
        _completion, correction = self.correction_after_needs_changes()
        self.adapter.start_turn(CHILD, turn_id="child-earlier", status="completed")
        self.clock.advance(300)
        self.adapter.script("transport_unknown")
        record = self.attempt(correction)
        self.adapter.set_status(CHILD, "notLoaded")
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        row = self.delivery_row(correction)
        self.assertEqual((row["state"], row["hold_reason"]), (HELD_UNCERTAIN, UNKNOWN_LOST))
        # The App Server applied the unanswered turn/start after all; the operator binds the
        # generation to the turn its dispatch receipt names (generation-bind), and the child
        # answers the generation with a final event of its own.
        self.adapter.start_turn(CHILD, turn_id="child-late", status="failed")
        self.registry.bind_anchor(self._rid, 2, dispatch_turn_id="child-late",
                                  source="dispatch_receipt")
        relationship = self.registry.get(self._rid)
        turn = self.assigned_turn("failed", thread=CHILD, turn="child-late")
        answer = self.execution_payload(relationship, "failed", generation=2, turn=turn)
        self.accept(answer)
        # Queued as the real path queues a final event, which annotates what it answers.
        self.delivery.enqueue(answer["eventId"])
        return correction, record["requestId"]

    def test_a_held_correction_its_generation_answered_names_the_child_disposition(self):
        correction, request_id = self.answered_correction()
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("parent_reads_child_disposition", "superseded:superseded_revision"))
        self.assertNotIn("recovery", outcome)
        self.assertEqual(self.sends_to(CHILD), [request_id])

    def test_an_answered_correction_a_later_generation_passed_keeps_its_answer(self):
        """Devin on a4c13aec: once another generation opened after the answer, the live rule
        says stale_generation first, and reconcile answered none while status, which reads the
        stored note, still said superseded:superseded_revision."""
        correction, request_id = self.answered_correction()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-3", status="inProgress")
        self.registry.open_generation(self._rid, dispatch_request_id="dispatch-3",
                                      reason="needs_changes_revision",
                                      dispatch_turn_id="turn-dispatch-3")
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual((outcome.get("nextExpectedAction"), outcome.get("reason")),
                         ("parent_reads_child_disposition", "superseded:superseded_revision"))
        self.assertNotIn("recovery", outcome)
        item = self.status_of(correction)
        self.assertEqual((item["phase"], item["reported"]),
                         ("superseded:superseded_revision", "superseded:superseded_revision"))
