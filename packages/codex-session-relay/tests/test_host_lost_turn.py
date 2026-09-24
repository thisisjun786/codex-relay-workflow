"""A turn the host accepted and then lost is told apart from a lost acknowledgement (CRW-224).

CRW-124's H0 rehearsal killed the App Server right after it accepted turn/start for a delivery to
the parent. The transport receipt carried a turn id, the relay settled the delivery as
dispatched, and after the restart the host had no such turn: nothing read the parent's turns, so
the child's report stayed dispatched_awaiting_ack for ever and looked exactly like a lost ACK.

The loss is staged here the way K5 left the host: the turn and its items are gone from the
parent's thread. A real lost acknowledgement (W5a) keeps both - the turn is listed, interrupted,
with the delivered message in it - and a normal acknowledgement settles the delivery before any
check is owed. Those two are the controls: they must read the same before and after the fix.
"""

import json
import unittest

from codex_session_relay import identity
from codex_session_relay.assignment import AssignmentView
from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.daemon import RelayDaemon
from codex_session_relay.delivery import REVISION
from codex_session_relay.hostadapter import HostUnavailable
from codex_session_relay.policy import RetryPolicy
from codex_session_relay.transport import ACKNOWLEDGED, DISPATCHED, HELD_UNCERTAIN, QUEUED

from .support import CHILD, PARENT, DeliveryTestCase

HOST_LOST = "host_lost_turn"
SENT_AT = 1_700_000_000.0


class CountingLookups:
    """A plain delegating wrapper that records every recipient-turn lookup the relay makes."""

    def __init__(self, inner):
        self._inner = inner
        self.lookups = []

    def find_dispatched_turn(self, thread_id, turn_id, *, sent_at):
        self.lookups.append(turn_id)
        return self._inner.find_dispatched_turn(thread_id, turn_id, sent_at=sent_at)

    def __getattr__(self, name):
        return getattr(self._inner, name)


class SettlesDuringTheRead:
    """A daemon pass that records the loss while a manual reconcile is reading the same turn.

    The reconcile read the attempt as dispatched before this lookup; the settlement commits in
    between; the reconcile then writes. That is the interleaving review found (PR #156).
    """

    def __init__(self, inner, settle):
        self._inner = inner
        self._settle = settle
        self.fired = 0

    def find_dispatched_turn(self, thread_id, turn_id, *, sent_at):
        if not self.fired:
            self.fired += 1
            self._settle()
        return self._inner.find_dispatched_turn(thread_id, turn_id, sent_at=sent_at)

    def __getattr__(self, name):
        return getattr(self._inner, name)


class AcksDuringTheSend:
    """The parent acknowledges while the relay is still inside its send (delivery 'sending')."""

    def __init__(self, inner, during):
        self._inner = inner
        self._during = during

    def send_message(self, *args, **kwargs):
        receipt = self._inner.send_message(*args, **kwargs)
        self._during(receipt)
        return receipt

    def __getattr__(self, name):
        return getattr(self._inner, name)


class CountingReads:
    """Records the receipt and item reads a confirmation could make."""

    def __init__(self, inner):
        self._inner = inner
        self.reads = []

    def get_operation(self, request_id):
        self.reads.append(("get_operation", request_id))
        return self._inner.get_operation(request_id)

    def find_token(self, *args, **kwargs):
        self.reads.append(("find_token", args[1] if len(args) > 1 else kwargs.get("token")))
        return self._inner.find_token(*args, **kwargs)

    def find_token_in_turn(self, *args, **kwargs):
        self.reads.append(("find_token_in_turn", kwargs.get("turn_id")))
        return self._inner.find_token_in_turn(*args, **kwargs)

    def __getattr__(self, name):
        return getattr(self._inner, name)


class HostLossCase(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        self.assignments = AssignmentView(self.store, self.registry, self.clock)
        self.daemon = self.daemon_with(RetryPolicy())

    def daemon_with(self, policy, adapter=None):
        return RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, adapter or self.adapter, clock=self.clock, policy=policy,
        )

    def parent_history(self):
        """A parent with a turn well before any send, as every real parent has."""
        self.adapter.start_turn(PARENT, turn_id="parent-earlier", status="completed")
        self.clock.advance(300)

    def dispatched(self, **kwargs):
        self.parent_history()
        _relationship, event_id = self.queued_event(**kwargs)
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        return event_id, record["requestId"], record["turnId"]

    def completions(self, count):
        """COUNT delivered completions to one parent, in event-id order, the order the pass reads."""
        self.parent_history()
        delivered = []
        for index in range(count):
            relationship = self.register(issue_key=f"REL-{index + 2}",
                                         dispatch_request_id=f"dispatch-{index + 2}")
            self._rid = relationship["relationshipId"]
            payload = self.ready_payload(
                relationship, [self.artifact(f"out-{index}.txt", f"deliverable {index}")])
            self.accept(payload)
            self.delivery.enqueue(payload["eventId"])
            record = self.attempt(payload["eventId"])
            self.assertEqual(record["deliveryState"], DISPATCHED)
            delivered.append((payload["eventId"], record["requestId"], record["turnId"]))
            self.clock.advance(10)
        return sorted(delivered)

    def acknowledge(self, event_id, ack_turn_id):
        ack_turn = self.adapter.start_turn(PARENT, turn_id=ack_turn_id, status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=ack_turn.turn_id,
            ack_proof=identity.ack_proof(event_id, ack_turn.turn_id), accepted=True,
            adapter=self.adapter,
        )

    def token_confirmed(self):
        """A completion whose uncertain send reconciliation confirmed from its token."""
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        request_id = self.attempt(event_id)["requestId"]
        turn = self.adapter.start_turn(PARENT, status="completed", text=f"...{request_id}...")
        confirmed = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(confirmed["evidence"], "turn_found")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        return event_id, request_id, turn.turn_id

    def evidence_of(self, event_id):
        return [row["affirmative_evidence"] for row in self.attempts_for(event_id)]

    def host_reloads_losing(self, turn_id):
        """K5c: the host died on the accepted turn, and a later load of the parent brought it back
        listed interrupted with none of its items."""
        thread = self.adapter.threads[PARENT]
        self.adapter.finish_turn(PARENT, turn_id, "interrupted")
        thread.items = [item for item in thread.items if item[0] != turn_id]

    def echo(self, turn_id, text):
        """A relay command's output printed inside a parent turn: not the delivered message."""
        self.adapter.threads[PARENT].items.append((turn_id, text, "commandExecution"))

    def uncertain_delivery(self, *, message=True):
        """K5u: the host ran the delivery turn but the relay lost the turn/start receipt."""
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        request_id = self.attempt(event_id)["requestId"]
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.clock.advance(2)
        turn = self.adapter.start_turn(
            PARENT, status="inProgress",
            text=f"[codex-session-relay] verification request\nrequestId: {request_id}" if message
            else "another prompt")
        return event_id, request_id, turn.turn_id

    def services(self, adapter=None):
        from types import SimpleNamespace
        return SimpleNamespace(ack=self.ack, reconciler=self.reconciler,
                               adapter=adapter or self.adapter)

    def cli_ack(self, event_id, turn_id, *, adapter=None):
        from types import SimpleNamespace
        from codex_session_relay import cli
        return cli.cmd_ack(self.services(adapter), SimpleNamespace(
            event=event_id, ack_turn=turn_id, ack_proof=identity.ack_proof(event_id, turn_id),
            reject=None))

    def cli_verdict(self, event_id, turn_id, *, adapter=None):
        from types import SimpleNamespace
        from codex_session_relay import cli
        return cli.cmd_verdict(self.services(adapter), SimpleNamespace(
            event=event_id, verdict="verified", verdict_turn=turn_id, criterion=None,
            finding=None, criteria=None, restoration=None, reason=None,
            expect_criteria_digest=None))

    def ack_row(self, event_id):
        return self.store.one("SELECT a.verified, e.last_reason FROM acks a"
                              " LEFT JOIN ack_evidence e ON e.event_id = a.event_id"
                              " WHERE a.event_id = ?", (event_id,))

    def host_loses(self, turn_id, *, items=True):
        """K5: the App Server died before the accepted turn reached the rollout."""
        thread = self.adapter.threads[PARENT]
        thread.turns = [turn for turn in thread.turns if turn.turn_id != turn_id]
        if items:
            thread.items = [item for item in thread.items if item[0] != turn_id]

    def status_of(self, event_id):
        return next(item for item in self.delivery.snapshot()["deliveries"]
                    if item["eventId"] == event_id)

    def journalled(self, kind):
        return self.store.one("SELECT COUNT(*) AS c FROM journal WHERE kind = ?", (kind,))["c"]

    def attempt_states(self, event_id):
        return [row["state"] for row in self.attempts_for(event_id)]

    def next_action(self):
        return self.assignments.state(self._rid)["nextExpectedAction"]

    def completion_delivery(self):
        return self.assignments.state(self._rid)["projection"]["completion"]["delivery"]


class TheHostLostTheAcceptedTurn(HostLossCase):
    def test_the_daemon_redelivers_the_same_event_once_under_the_next_attempt(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        report = self.daemon.tick()
        attempts = self.attempts_for(event_id)
        self.assertEqual([row["state"] for row in attempts], [HOST_LOST, DISPATCHED])
        self.assertEqual([row["event_id"] for row in attempts], [event_id, event_id])
        self.assertEqual([send[0] for send in self.adapter.sends],
                         [first, attempts[1]["request_id"]])
        self.assertNotEqual(first, attempts[1]["request_id"])
        self.assertEqual(report.as_dict().get("turnsLost"), 1)
        self.assertEqual(self.journalled(HOST_LOST), 1)
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack")
        # Nothing further is owed: later ticks read the redelivery's turn and send nothing.
        for _ in range(3):
            self.clock.advance(120)
            self.daemon.tick()
        self.assertEqual(len(self.adapter.sends), 2)
        self.assertEqual(self.journalled(HOST_LOST), 1)

    def test_a_second_loss_holds_the_obligation_under_its_name_instead_of_a_third_send(self):
        event_id, _first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.daemon.tick()
        second = self.attempts_for(event_id)[1]
        self.host_loses(json.loads(second["record"])["turnId"])
        for _ in range(4):
            self.clock.advance(120)
            self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, HOST_LOST])
        self.assertEqual(len(self.adapter.sends), 2)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], HOST_LOST)
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("held:host_lost_turn", "held:host_lost_turn"))

    def test_a_turn_first_read_in_progress_and_lost_later_is_still_caught(self):
        event_id, _first, turn = self.dispatched()
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.host_loses(turn)
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id)[0], HOST_LOST)

    def test_a_completion_confirmed_by_its_token_is_checked_like_an_accepted_one(self):
        """Devin review of e8ff3f44 (thread 4092237821): the check read only accepted sends.

        An uncertain send that reconciliation confirmed from the token in the parent's items
        advances the delivery to dispatched while its attempt keeps the honest held_uncertain
        snapshot. The candidate query asked for dispatched attempts only, so if the host then
        lost that turn, the report waited for an acknowledgement nobody could send.
        """
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        request_id = self.attempt(event_id)["requestId"]
        turn = self.adapter.start_turn(PARENT, status="completed", text=f"...{request_id}...")
        confirmed = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(confirmed["evidence"], "turn_found")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.host_loses(turn.turn_id)
        self.clock.advance(120)
        report = self.daemon.tick().as_dict()
        self.assertEqual(report.get("turnsLost"), 1)
        attempts = self.attempts_for(event_id)
        self.assertEqual([row["state"] for row in attempts], [HOST_LOST, DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 2)
        # The lost attempt keeps the evidence it was confirmed on.
        lost = json.loads(attempts[0]["record"])
        self.assertEqual(lost["reconciliation"]["affirmativeEvidence"], "turn_found")
        self.assertEqual(lost["deliveryState"], HELD_UNCERTAIN)
        for _ in range(3):
            self.clock.advance(120)
            self.daemon.tick()
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_manual_reconcile_keeps_a_token_confirmed_send_confirmed(self):
        """Review 9 of 6407baaa: a later read that found nothing wrote the attempt back to none.

        A token already found is affirmative evidence (I-41); not finding it again is not
        evidence against it. Rewriting it to none also made a later loss record none.
        """
        event_id, request_id, turn = self.token_confirmed()
        self.host_loses(turn)
        self.clock.advance(5)
        early = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(early["evidence"], "turn_found")
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, DISPATCHED])
        self.assertEqual(self.evidence_of(event_id)[0], "turn_found")
        self.assertEqual(json.loads(self.attempts_for(event_id)[0]["record"])
                         ["reconciliation"]["affirmativeEvidence"], "turn_found")

    def test_reconciling_a_lost_token_confirmed_attempt_reports_its_own_evidence(self):
        """Review 9 of 6407baaa: the recorded-loss answer said receipt_turn_id for every loss."""
        event_id, request_id, turn = self.token_confirmed()
        self.host_loses(turn)
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id)[0], HOST_LOST)
        again = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual((again["state"], again["evidence"]), (HOST_LOST, "turn_found"))
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_manual_reconcile_keeps_the_name_on_a_token_confirmed_send(self):
        """Review 9 of 6407baaa: re-confirming from the token wrote over the undecided name."""
        event_id, request_id, turn = self.token_confirmed()
        self.host_loses(turn, items=False)
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:token_without_turn")
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(outcome["evidence"], "turn_found")
        self.assertEqual(outcome.get("recipientTurn", {}).get("undecided"), "token_without_turn")
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:token_without_turn")
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack:turn_check_undecided")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_tick_whose_only_change_is_the_loss_is_not_quiet(self):
        event_id, _first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        report = self.daemon_with(RetryPolicy(max_sends_per_tick=0)).tick()
        counters = report.as_dict()
        self.assertEqual(counters.get("turnsLost"), 1)
        others = {key: value for key, value in counters.items()
                  if key not in ("turnsLost", "notes", "skipped", "quiet")}
        self.assertEqual(others, {key: 0 for key in others})
        self.assertFalse(report.quiet)
        # And the row names what happened while the redelivery waits.
        item = self.status_of(event_id)
        self.assertEqual((item["phase"], item["reported"]),
                         ("redelivering:host_lost_turn", "redelivering:host_lost_turn"))
        self.assertEqual(item["attemptDetail"][0]["state"], HOST_LOST)
        self.assertEqual([m["status"] for m in self.delivery.attempt_messages(event_id)],
                         [HOST_LOST])

    def test_a_send_too_recent_to_judge_is_unknown_until_the_allowance_has_passed(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(5)
        early = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(early.get("recipientTurn", {}).get("finding"), "unknown")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.clock.advance(120)
        later = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(later.get("recipientTurn", {}).get("finding"), HOST_LOST)

    def test_the_budget_bounds_lookups_and_every_delivery_is_reached(self):
        self.parent_history()
        turns = []
        for index in range(5):
            relationship = self.register(issue_key=f"REL-{index + 2}",
                                         dispatch_request_id=f"dispatch-{index + 2}")
            self._rid = relationship["relationshipId"]
            payload = self.ready_payload(
                relationship, [self.artifact(f"out-{index}.txt", f"deliverable {index}")])
            self.accept(payload)
            self.delivery.enqueue(payload["eventId"])
            record = self.attempt(payload["eventId"])
            self.assertEqual(record["deliveryState"], DISPATCHED)
            self.assertEqual(record["attemptNo"], 1)
            turns.append(record["turnId"])
            self.clock.advance(10)
        for turn in turns[:2]:
            self.adapter.finish_turn(PARENT, turn, "completed")
        counting = CountingLookups(self.adapter)
        daemon = self.daemon_with(RetryPolicy(max_turn_checks_per_tick=2, max_sends_per_tick=0),
                                  adapter=counting)
        per_tick = []
        for _ in range(3):
            before = len(counting.lookups)
            daemon.tick()
            per_tick.append(len(counting.lookups) - before)
        self.assertEqual(per_tick, [2, 2, 2])
        self.assertEqual(set(counting.lookups), set(turns))
        for _ in range(3):
            daemon.tick()
        self.assertEqual([counting.lookups.count(turn) for turn in turns[:2]], [1, 1])
        self.assertTrue(all(counting.lookups.count(turn) > 1 for turn in turns[2:]))
        self.assertEqual(len(self.adapter.sends), 5)

    def test_a_finished_turn_is_read_once_even_when_the_candidates_span_pages(self):
        """Devin review of 7b90b974: pruning the skip set per page re-read finished turns.

        Every turn is finished, so a pass that forgets any of them reads it again whatever
        order the event ids sort in: with a page of two, the third and fourth candidates fall
        outside the window the pass after them lists.
        """
        self.parent_history()
        turns = []
        for index in range(5):
            relationship = self.register(issue_key=f"REL-{index + 2}",
                                         dispatch_request_id=f"dispatch-{index + 2}")
            self._rid = relationship["relationshipId"]
            payload = self.ready_payload(
                relationship, [self.artifact(f"out-{index}.txt", f"deliverable {index}")])
            self.accept(payload)
            self.delivery.enqueue(payload["eventId"])
            turns.append(self.attempt(payload["eventId"])["turnId"])
            self.clock.advance(10)
        for turn in turns:
            self.adapter.finish_turn(PARENT, turn, "completed")
        counting = CountingLookups(self.adapter)
        daemon = self.daemon_with(RetryPolicy(max_turn_checks_per_tick=2, max_sends_per_tick=0),
                                  adapter=counting)
        daemon.turn_check_page = 2
        for _ in range(8):
            daemon.tick()
        self.assertEqual(sorted(counting.lookups), sorted(turns))

    def test_a_delivery_behind_two_pages_of_finished_turns_is_still_reached(self):
        """Review 3 of c0338f96: the pass wrapped after a full page as well as a short one.

        Once the first two pages held only finished turns, every tick walked the second page,
        wrapped to the first and stopped where it began, so a lost turn on the third page was
        never read and its report waited for an acknowledgement nobody could send.
        """
        delivered = self.completions(5)
        for _event, _request, turn in delivered[:4]:
            self.adapter.finish_turn(PARENT, turn, "completed")
        lost_event, _request, lost_turn = delivered[4]
        self.host_loses(lost_turn)
        self.clock.advance(120)
        daemon = self.daemon_with(RetryPolicy(max_turn_checks_per_tick=4))
        daemon.turn_check_page = 2
        for _ in range(6):
            daemon.tick()
            self.clock.advance(20)
        self.assertEqual(self.attempt_states(lost_event), [HOST_LOST, DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 6)

    def test_the_finished_turns_remembered_are_only_those_still_awaiting(self):
        """Review 3 of c0338f96: the memory of finished turns was pruned only when every
        candidate fit one page.

        With more candidates than that, an acknowledged delivery stayed remembered for the life
        of the process, and a daemon under steady traffic grew with everything it had delivered.
        """
        delivered = self.completions(5)
        for _event, _request, turn in delivered:
            self.adapter.finish_turn(PARENT, turn, "completed")
        self.clock.advance(120)
        daemon = self.daemon_with(RetryPolicy(max_turn_checks_per_tick=4, max_sends_per_tick=0))
        daemon.turn_check_page = 2
        for _ in range(4):
            daemon.tick()
        acknowledged = {request for _event, request, _turn in delivered[:3]}
        self.assertLessEqual(acknowledged, set(daemon._turns_settled))
        for index, (event_id, _request, _turn) in enumerate(delivered[:3]):
            self.acknowledge(event_id, f"ack-{index}")
        for _ in range(4):
            self.clock.advance(20)
            daemon.tick()
        self.assertEqual(set(daemon._turns_settled) & acknowledged, set())
        self.assertEqual(len(self.adapter.sends), 5)


class ReconcileReportsTheRecipientTurn(HostLossCase):
    def test_reconcile_names_the_loss_and_queues_the_redelivery_without_sending(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        reading = outcome.get("recipientTurn", {})
        self.assertEqual((reading.get("turnId"), reading.get("finding")), (turn, HOST_LOST))
        self.assertEqual(outcome["record"]["reconciliation"]["recipientTurnsChecked"], True)
        self.assertEqual(outcome.get("state"), HOST_LOST)
        self.assertEqual(outcome.get("redelivery"), "queued")
        self.assertEqual(self.delivery_row(event_id)["state"], QUEUED)
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST])
        self.assertEqual(len(self.adapter.sends), 1, "reconcile never sends")

    def test_an_unreadable_turn_list_is_reported_as_unchecked_and_changes_nothing(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.adapter.fail_reads("find_dispatched_turn")
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), "unknown")
        self.assertEqual(outcome["record"]["reconciliation"]["recipientTurnsChecked"], False)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_the_delivery_token_in_the_parents_items_vetoes_a_missing_turn_row(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn, items=False)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), "present")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])

    def test_a_delivery_turn_gone_from_the_list_with_its_token_kept_is_named(self):
        """Review 7 of d817117a: the veto kept the report from a second send, and hid it.

        The turn the delivery started is gone from the parent's list while the message stays in
        its items. Sending again would duplicate a message the parent holds, but the turn that
        would have acted on it is gone, and the delivery read as an ordinary acknowledgement wait
        with no reason anybody could query.
        """
        event_id, first, turn = self.dispatched()
        self.host_loses(turn, items=False)
        self.clock.advance(120)
        report = self.daemon.tick().as_dict()
        self.assertEqual(report.get("turnsUndecided"), 1)
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack:turn_check_undecided")
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:token_without_turn")
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        reading = outcome.get("recipientTurn", {})
        self.assertEqual((reading.get("finding"), reading.get("undecided")),
                         ("present", "token_without_turn"))
        self.assertEqual(outcome["record"]["reconciliation"]["recipientTurnsChecked"], True)
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:token_without_turn")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_reconciling_a_lost_attempt_again_changes_nothing(self):
        """The lost attempt is the once-count. Re-settling it from its receipt after the
        redelivery went out would set it back to dispatched, and a later loss of the redelivery
        would then be treated as the first one and sent a third time."""
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        with self.store.transaction() as db:
            db.execute("UPDATE attempts SET state = ? WHERE request_id = ?", (HOST_LOST, first))
            db.execute("UPDATE deliveries SET state = ?, dispatch_evidence = ? WHERE event_id = ?",
                       (QUEUED, HOST_LOST, event_id))
        second = self.attempt(event_id)
        self.assertEqual(second["deliveryState"], DISPATCHED)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), HOST_LOST)
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, DISPATCHED])
        self.assertEqual(self.journalled(HOST_LOST), 0, "reporting the old loss recorded nothing")
        self.host_loses(second["turnId"])
        for _ in range(3):
            self.clock.advance(120)
            self.daemon.tick()
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], HOST_LOST)
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_reconcile_racing_the_daemons_settlement_cannot_undo_the_loss(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        racing = SettlesDuringTheRead(
            self.adapter, lambda: self.reconciler.check_dispatched_turn(first, self.adapter))
        outcome = self.reconciler.reconcile_attempt(first, racing)
        self.assertEqual(racing.fired, 1)
        self.assertEqual(outcome.get("state"), HOST_LOST)
        # The answer says what the racing check found and did, not only that the row stood.
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), HOST_LOST)
        self.assertEqual(outcome.get("redelivery"), "queued")
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST])
        self.assertEqual(self.delivery_row(event_id)["state"], QUEUED)
        self.assertEqual(self.journalled(HOST_LOST), 1)
        # And the once-count still holds: the redelivery's loss is held, not sent a third time.
        second = self.attempt(event_id)
        self.host_loses(second["turnId"])
        for _ in range(3):
            self.clock.advance(120)
            self.daemon.tick()
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], HOST_LOST)
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_recorded_acknowledgement_wins_over_a_missing_turn(self):
        event_id, first, turn = self.dispatched()
        self.ack.acknowledge(
            event_id, ack_turn_id="ack-later",
            ack_proof=identity.ack_proof(event_id, "ack-later"), accepted=True, adapter=None,
        )
        self.host_loses(turn)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), HOST_LOST)
        self.assertEqual(outcome.get("redelivery"), "not_moved")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.daemon.tick()
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_start_that_steered_a_turn_begun_before_the_send_is_present(self):
        self.adapter.start_turn(PARENT, turn_id="parent-running", status="inProgress")
        self.clock.advance(300)
        _relationship, event_id = self.queued_event()
        self.adapter.script("steer_existing")
        record = self.attempt(event_id)
        self.assertEqual(record["turnId"], "parent-running")
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), "present")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_a_token_deeper_than_the_scan_in_a_later_turn_is_undecided_not_lost(self):
        """Review of 35aa454c: a scan that stops at its bound has not shown the token is absent.

        The parent ran a long turn after the send and the delivered message sits under 201 newer
        items of it. Calling the turn lost there would send the report a second time.
        """
        event_id, first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        message = next(text for owner, text in thread.items if owner == turn)
        self.host_loses(turn)
        self.adapter.start_turn(PARENT, turn_id="parent-later", status="completed")
        thread.items.append(("parent-later", message))
        thread.items.extend(("parent-later", f"work item {n}") for n in range(201))
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        reading = outcome.get("recipientTurn", {})
        self.assertEqual((reading.get("finding"), reading.get("undecided")),
                         ("unknown", "token_scan_bounded"))
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 1)
        # Undecided is recorded under its name, where status and assignment-show can read it.
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack:turn_check_undecided")
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:token_scan_bounded")

    def test_a_short_turn_after_the_send_is_read_through_and_the_loss_still_found(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.adapter.start_turn(PARENT, turn_id="parent-later", status="completed",
                                text="unrelated work")
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), HOST_LOST)
        self.assertEqual(self.delivery_row(event_id)["state"], QUEUED)

    def test_a_token_behind_the_items_of_other_dropped_turns_still_vetoes_the_loss(self):
        """Review 6 of e7859976: an item of an unlisted turn was taken for older history.

        The host dropped the delivery turn and two later turns from its list but kept their
        items. The scan stopped at the first item of a turn it had not been given, called the
        turns since the send covered, and the report was sent a second time although the
        parent's items still held it.
        """
        event_id, first, turn = self.dispatched()
        self.clock.advance(5)
        self.adapter.start_turn(PARENT, turn_id="parent-later", status="completed",
                                text="later work")
        self.adapter.start_turn(PARENT, turn_id="parent-later-2", status="completed",
                                text="more later work")
        for dropped in (turn, "parent-later", "parent-later-2"):
            self.host_loses(dropped, items=False)
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.journalled(HOST_LOST), 0)
        reading = self.reconciler.reconcile_attempt(first, self.adapter).get("recipientTurn", {})
        self.assertEqual((reading.get("finding"), reading.get("status")), ("present", None))

    def test_the_items_of_a_turn_listed_before_the_send_end_the_scan(self):
        """The one boundary the scan keeps: a turn the listing showed began before the send.

        Its items and everything older are history, so a loss behind 250 of them is still
        found instead of reading as a scan stopped at its bound.
        """
        event_id, first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        thread.items[0:0] = [("parent-earlier", f"earlier item {n}") for n in range(250)]
        self.host_loses(turn)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), HOST_LOST)
        self.assertEqual(self.delivery_row(event_id)["state"], QUEUED)

    def test_a_listing_too_long_to_reach_the_send_is_recorded_as_undecided(self):
        from codex_session_relay.hostadapter import ListingBounded

        class NeverReachesTheSend:
            def __init__(self, inner):
                self._inner = inner

            def find_dispatched_turn(self, thread_id, turn_id, *, sent_at):
                raise ListingBounded("1000 newer turns and the send not reached")

            def __getattr__(self, name):
                return getattr(self._inner, name)

        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        outcome = self.reconciler.reconcile_attempt(first, NeverReachesTheSend(self.adapter))
        reading = outcome.get("recipientTurn", {})
        self.assertEqual((reading.get("finding"), reading.get("undecided")),
                         ("unknown", "listing_bounded"))
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack:turn_check_undecided")

    def test_a_parent_whose_only_turn_was_lost_names_its_empty_listing(self):
        """Review 4 of 272e240e: an empty turn list read as a transient failure for ever.

        When the lost delivery turn was the parent's only turn, the listing is empty. That is no
        evidence the turn is gone, but it is no transient either, and nothing named it: the
        delivery read dispatched_awaiting_ack, exactly like a lost acknowledgement.
        """
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.host_loses(record["turnId"])
        self.assertEqual(self.adapter.threads[PARENT].turns, [])
        self.clock.advance(5)
        self.daemon.tick()
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack")
        self.clock.advance(120)
        report = self.daemon.tick().as_dict()
        self.assertEqual(report.get("turnsUndecided"), 1)
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack:turn_check_undecided")
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:listing_empty")
        outcome = self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        reading = outcome.get("recipientTurn", {})
        self.assertEqual((reading.get("finding"), reading.get("undecided")),
                         ("unknown", "listing_empty"))
        self.assertEqual(outcome["record"]["reconciliation"]["recipientTurnsChecked"], False)
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_an_undecided_reading_is_cleared_once_the_turn_is_found(self):
        event_id, first, _turn = self.dispatched()
        with self.store.transaction() as db:
            db.execute("UPDATE attempts SET recipient_scan = ? WHERE request_id = ?",
                       ("turn_check_undecided:listing_bounded", first))
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack")

    def test_an_undecided_name_survives_a_reconcile_whose_read_fails(self):
        """Review of 7b90b974: reconciliation's own settlement must not erase the name.

        Only a reading that decides clears it; a failed read decides nothing.
        """
        event_id, first, _turn = self.dispatched()
        with self.store.transaction() as db:
            db.execute("UPDATE attempts SET recipient_scan = ? WHERE request_id = ?",
                       ("turn_check_undecided:listing_bounded", first))
        self.clock.advance(120)
        self.adapter.fail_reads("find_dispatched_turn")
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(outcome.get("recipientTurn", {}).get("finding"), "unknown")
        self.assertEqual(self.status_of(event_id)["phase"], "awaiting_ack:turn_check_undecided")
        self.assertEqual(self.completion_delivery().get("turnCheck"),
                         "turn_check_undecided:listing_bounded")

    def test_the_daemon_records_an_undecided_reading_once(self):
        event_id, _first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        message = next(text for owner, text in thread.items if owner == turn)
        self.host_loses(turn)
        self.adapter.start_turn(PARENT, turn_id="parent-later", status="completed")
        thread.items.append(("parent-later", message))
        thread.items.extend(("parent-later", f"work item {n}") for n in range(201))
        self.clock.advance(120)
        first_tick = self.daemon.tick().as_dict()
        self.assertEqual((first_tick.get("turnsUndecided"), first_tick.get("turnsLost")), (1, 0))
        self.clock.advance(20)
        self.assertEqual(self.daemon.tick().as_dict().get("turnsUndecided"), 0)
        self.assertEqual(len(self.adapter.sends), 1)


class TheControlsReadAsBefore(HostLossCase):
    """A real lost ACK and a normal ACK: the delivery paths are unchanged by the check."""

    def test_a_lost_acknowledgement_keeps_waiting_and_is_read_as_present(self):
        event_id, first, turn = self.dispatched()
        self.adapter.finish_turn(PARENT, turn, "interrupted")
        self.clock.advance(120)
        for _ in range(2):
            self.daemon.tick()
            self.clock.advance(120)
        item = self.status_of(event_id)
        self.assertEqual((item["state"], item["reported"], item["phase"]),
                         (DISPATCHED, "dispatched_awaiting_ack", "awaiting_ack"))
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.journalled(HOST_LOST), 0)
        outcome = self.reconciler.reconcile_attempt(first, self.adapter)
        reading = outcome.get("recipientTurn", {})
        self.assertEqual((reading.get("finding"), reading.get("status")), ("present", "interrupted"))
        self.assertEqual(outcome["record"]["reconciliation"]["recipientTurnsChecked"], True)

    def test_a_normal_acknowledgement_settles_before_any_check_is_owed(self):
        event_id, _first, _turn = self.dispatched()
        self.clock.advance(5)
        ack_turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=ack_turn.turn_id,
            ack_proof=identity.ack_proof(event_id, ack_turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        counting = CountingLookups(self.adapter)
        daemon = self.daemon_with(RetryPolicy(), adapter=counting)
        for _ in range(2):
            self.clock.advance(120)
            daemon.tick()
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(counting.lookups, [])
        self.assertEqual(self.journalled(HOST_LOST), 0)


class AssignmentShowNamesTheNextActor(HostLossCase):
    def test_a_delivered_completion_waits_for_the_parents_acknowledgement(self):
        self.dispatched()
        self.assertEqual(self.next_action(), "parent_acknowledges")

    def test_a_completion_not_yet_sent_is_still_the_daemons(self):
        self.queued_event()
        self.assertEqual(self.next_action(), "daemon_delivers")

    def test_an_acknowledged_completion_waits_for_the_parents_verification(self):
        event_id, _first, _turn = self.dispatched()
        self.clock.advance(5)
        ack_turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=ack_turn.turn_id,
            ack_proof=identity.ack_proof(event_id, ack_turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.assertEqual(self.next_action(), "parent_verifies")

    def test_a_recorded_acknowledgement_is_the_daemons_to_verify(self):
        event_id, _first, _turn = self.dispatched()
        self.ack.acknowledge(
            event_id, ack_turn_id="ack-later",
            ack_proof=identity.ack_proof(event_id, "ack-later"), accepted=True, adapter=None,
        )
        self.assertEqual(self.next_action(), "daemon_verifies_acknowledgement")

    def test_a_refused_acknowledgement_is_the_parents_to_restate(self):
        event_id, _first, _turn = self.dispatched()
        self.ack.acknowledge(
            event_id, ack_turn_id="ack-early",
            ack_proof=identity.ack_proof(event_id, "ack-early"), accepted=True, adapter=None,
        )
        with self.store.transaction() as db:
            db.execute("UPDATE ack_evidence SET last_reason = ? WHERE event_id = ?",
                       ("ack_turn_unverified", event_id))
        self.assertEqual(self.next_action(), "parent_reacknowledges")

    def test_an_uncertain_send_is_the_daemons_to_reconcile(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("transport_unknown")
        self.attempt(event_id)
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")

    def test_an_interrupted_claim_is_the_daemons_to_reconcile(self):
        _relationship, event_id = self.queued_event()
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'sending' WHERE event_id = ?", (event_id,))
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")

    def test_an_inbox_only_completion_is_the_parents_to_acknowledge(self):
        event_id, _first, _turn = self.dispatched()
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'inbox_only', hold_reason ="
                       " 'push_channel_closed' WHERE event_id = ?", (event_id,))
        self.assertEqual(self.next_action(), "parent_acknowledges")

    def test_the_host_loss_is_named_at_every_step_of_its_recovery(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.assertEqual(self.next_action(), "daemon_redelivers_host_lost_turn")
        self.assertEqual(self.completion_delivery().get("hostLostAttempts"), 1)
        # A claim interrupted on the redelivery is reconciliation's, and still carries the loss.
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'sending' WHERE event_id = ?", (event_id,))
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'queued' WHERE event_id = ?", (event_id,))
        # The redelivery reaches the parent and waits for its acknowledgement.
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.next_action(), "parent_acknowledges")
        self.assertEqual(self.completion_delivery().get("hostLostAttempts"), 1)
        # Lost again: held under its name, for the parent to recover.
        self.host_loses(record["turnId"])
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], HOST_LOST)
        self.assertEqual(self.next_action(), "parent_recovers_host_lost_turn")
        self.assertEqual(self.completion_delivery().get("hostLostAttempts"), 2)

    def test_a_redelivery_held_for_another_reason_is_still_the_parents_after_a_loss(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'withheld_pre_send', hold_reason ="
                       " 'attempt_cap' WHERE event_id = ?", (event_id,))
        self.assertEqual(self.next_action(), "parent_recovers_host_lost_turn")

    def test_an_uncertain_redelivery_after_a_loss_is_reconciled_not_resent(self):
        event_id, first, turn = self.dispatched()
        self.host_loses(turn)
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(first, self.adapter)
        self.adapter.script("transport_unknown")
        self.attempt(event_id)
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")

    def test_a_corrected_completion_reads_its_own_delivery(self):
        _relationship, first = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(first)
        self.clock.advance(5)
        ack_turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            first, ack_turn_id=ack_turn.turn_id,
            ack_proof=identity.ack_proof(first, ack_turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(
            first, verdict="needs_changes", verdict_turn_id="v1",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix the shape"}],
        )
        correction = self.store.one(
            "SELECT event_id FROM deliveries WHERE kind = ?", (REVISION,))["event_id"]
        self.delivery.attempt(correction, self.adapter)
        self.clock.advance(300)
        relationship = self.registry.get(self._rid)
        generation = relationship["executionGeneration"]
        self.registry.bind_anchor(self._rid, generation, dispatch_turn_id="revision-turn",
                                  source="dispatch_receipt")
        payload = self.ready_payload(
            relationship, [self.artifact("revised.txt", "the corrected deliverable")],
            generation=generation, turn=self.assigned_turn(turn="revision-turn"),
        )
        self.accept(payload, observation=self.assigned_turn(turn="revision-turn"))
        self.delivery.enqueue(payload["eventId"])
        self.assertEqual(self.assignments.state(self._rid)["state"], "corrected")
        self.assertEqual(self.next_action(), "daemon_delivers")
        record = self.attempt(payload["eventId"])
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.next_action(), "parent_acknowledges")
        self.host_loses(record["turnId"])
        self.clock.advance(120)
        self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        self.assertEqual(self.next_action(), "daemon_redelivers_host_lost_turn")


class TheHostListsALostTurnAfterAReload(HostLossCase):
    """H0R3-F1 (CRW-124 R3, K5c): the host died on the accepted delivery turn, then a routine load
    of the parent brought the turn back listed interrupted with none of its items. A listed
    turn read as present, the finished-turn memory kept it for good, and the report was lost."""

    def test_a_turn_reloaded_interrupted_without_its_message_is_lost_and_redelivered_once(self):
        event_id, first, turn = self.dispatched()
        self.host_reloads_losing(turn)
        self.clock.advance(5)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.clock.advance(120)
        self.daemon.tick()
        attempts = self.attempts_for(event_id)
        self.assertEqual([row["state"] for row in attempts], [HOST_LOST, DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 2)
        self.assertEqual(self.journalled(HOST_LOST), 1)
        reading = self.reconciler.reconcile_attempt(first, self.adapter).get("recipientTurn", {})
        self.assertEqual(reading.get("finding"), HOST_LOST)
        self.assertIn("interrupted", reading.get("detail") or "")

    def test_a_message_found_only_under_another_turn_is_read_again_and_its_loss_caught(self):
        event_id, first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        message = next(item[1] for item in thread.items if item[0] == turn)
        self.host_reloads_losing(turn)
        self.adapter.start_turn(PARENT, turn_id="parent-later", status="completed")
        thread.items.append(("parent-later", message))
        self.clock.advance(120)
        self.daemon.tick()
        reading = self.reconciler.reconcile_attempt(first, self.adapter)["recipientTurn"]
        self.assertEqual((reading["finding"], reading["status"], reading["undecided"]),
                         ("present", None, None))
        self.assertIn("parent-later", reading["detail"])
        self.assertEqual(len(self.adapter.sends), 1)
        # A second load empties that turn as well: the next reading catches the loss.
        self.host_reloads_losing("parent-later")
        self.clock.advance(20)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_relay_command_echoing_the_request_id_does_not_veto_the_loss(self):
        event_id, first, turn = self.dispatched()
        self.host_reloads_losing(turn)
        self.adapter.start_turn(PARENT, turn_id="parent-later", status="completed")
        self.echo("parent-later", json.dumps({"delivery": {"requestId": first}}))
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [HOST_LOST, DISPATCHED])

    def test_a_message_the_host_types_unfamiliarly_still_blocks_the_loss(self):
        event_id, first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        message = next(item[1] for item in thread.items if item[0] == turn)
        self.host_reloads_losing(turn)
        thread.items.append((turn, message, "hookPrompt"))
        self.clock.advance(120)
        self.daemon.tick()
        self.assertEqual(self.attempt_states(event_id), [DISPATCHED])
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_long_finished_turn_is_read_from_its_first_item(self):
        event_id, first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        thread.items.extend((turn, f"work item {n}", "commandExecution") for n in range(300))
        self.adapter.finish_turn(PARENT, turn, "completed")
        self.clock.advance(120)
        reading = self.reconciler.reconcile_attempt(first, self.adapter)["recipientTurn"]
        self.assertEqual((reading["finding"], reading["status"], reading["undecided"]),
                         ("present", "completed", None))

    def test_an_in_progress_listed_turn_is_present_without_reading_its_items(self):
        event_id, first, turn = self.dispatched()
        thread = self.adapter.threads[PARENT]
        thread.items = [item for item in thread.items if item[0] != turn]
        self.clock.advance(120)
        reading = self.reconciler.reconcile_attempt(first, self.adapter)["recipientTurn"]
        self.assertEqual((reading["finding"], reading["status"]), ("present", "inProgress"))


class AnAcknowledgementBeforeTheSendIsConfirmed(HostLossCase):
    """H0R3-F2 (CRW-124 R3, K5u): the host ran the delivery turn but the relay lost the
    turn/start receipt, so the parent's ACK inside that very turn was refused as held_uncertain;
    reconciliation confirmed the delivery seconds later and nothing asked the parent again."""

    def test_an_ack_in_the_delivery_turn_confirms_the_send_and_the_verdict_follows(self):
        event_id, request_id, turn = self.uncertain_delivery()
        self.assertEqual(self.ack.claim_verification(event_id, turn_id=turn), "proceed")
        record = self.cli_ack(event_id, turn)
        self.assertEqual(record["_verified"], "verified")
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)
        self.assertEqual(self.attempt_states(event_id), [HELD_UNCERTAIN])
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        verdict = self.cli_verdict(event_id, turn)
        self.assertEqual(verdict["verdict"], "verified")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_an_ack_the_relay_cannot_confirm_yet_is_kept_not_refused(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.ack.claim_verification(event_id, turn_id=turn)
        record = self.cli_ack(event_id, turn)
        self.assertEqual(record["_verified"], "unverified_turn")
        row = self.ack_row(event_id)
        self.assertEqual((row["verified"], row["last_reason"]),
                         ("unverified_turn", "delivery_unconfirmed"))
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.assertEqual(self.next_action(), "daemon_reconciles_delivery")

    def test_a_kept_ack_completes_once_the_delivery_is_confirmed(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.ack.claim_verification(event_id, turn_id=turn)
        self.cli_ack(event_id, turn)
        # The host catches up: the delivery turn now shows the message.
        self.adapter.threads[PARENT].items.append((turn, f"requestId: {request_id}"))
        self.clock.advance(5)
        self.daemon.tick()
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)
        self.assertEqual(self.next_action(), "parent_verifies")
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_confirmed_delivery_with_a_kept_ack_is_the_daemons_to_verify(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.ack.claim_verification(event_id, turn_id=turn)
        self.cli_ack(event_id, turn)
        self.adapter.threads[PARENT].items.append((turn, f"requestId: {request_id}"))
        self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.next_action(), "daemon_verifies_acknowledgement")

    def test_a_verdict_completes_a_kept_ack_first(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.ack.claim_verification(event_id, turn_id=turn)
        self.cli_ack(event_id, turn)
        self.adapter.threads[PARENT].items.append((turn, f"requestId: {request_id}"))
        verdict = self.cli_verdict(event_id, turn)
        self.assertEqual(verdict["verdict"], "verified")
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_a_claimed_completion_without_an_ack_is_the_parents_to_acknowledge(self):
        event_id, _first, turn = self.dispatched()
        self.ack.claim_verification(event_id, turn_id="parent-reading")
        self.assertEqual(self.next_action(), "parent_acknowledges")

    def test_an_ack_while_the_relay_is_still_sending_is_kept_and_completes_after(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        kept = {}
        counting = CountingReads(self.adapter)

        def parent_acks(receipt):
            kept["state"] = self.delivery_row(event_id)["state"]
            kept["record"] = self.cli_ack(event_id, receipt["turnId"], adapter=counting)

        self.delivery.attempt(event_id, AcksDuringTheSend(self.adapter, parent_acks))
        self.assertEqual(kept["state"], "sending")
        self.assertEqual(kept["record"]["_verified"], "unverified_turn")
        self.assertEqual(counting.reads, [])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.clock.advance(5)
        self.daemon.tick()
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_an_ordinary_ack_makes_no_confirmation_reads(self):
        event_id, _first, _turn = self.dispatched()
        self.clock.advance(5)
        ack_turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        counting = CountingReads(self.adapter)
        record = self.cli_ack(event_id, ack_turn.turn_id, adapter=counting)
        self.assertEqual(record["_verified"], "verified")
        self.assertEqual(counting.reads, [])
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_a_wrong_proof_makes_no_host_reads(self):
        from types import SimpleNamespace
        from codex_session_relay import cli
        from codex_session_relay.errors import AckRefused
        event_id, request_id, turn = self.uncertain_delivery()
        counting = CountingReads(self.adapter)
        with self.assertRaises(AckRefused):
            cli.cmd_ack(self.services(counting), SimpleNamespace(
                event=event_id, ack_turn=turn, ack_proof="0" * 64, reject=None))
        self.assertEqual(counting.reads, [])
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)

    def test_a_message_deep_in_a_long_delivery_turn_is_confirmed_through_the_ack_turn(self):
        event_id, request_id, turn = self.uncertain_delivery()
        thread = self.adapter.threads[PARENT]
        thread.items.extend((turn, f"work item {n}", "commandExecution") for n in range(250))
        self.assertFalse(self.adapter.find_token(PARENT, request_id).found)
        record = self.cli_ack(event_id, turn)
        self.assertEqual(record["_verified"], "verified")
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_an_echo_in_the_ack_turn_does_not_confirm_the_send(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.echo(turn, json.dumps({"delivery": {"requestId": request_id}}))
        record = self.cli_ack(event_id, turn)
        self.assertEqual(record["_verified"], "unverified_turn")
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.assertEqual(self.evidence_of(event_id), ["none"])

    def test_a_kept_ack_is_confirmed_by_the_daemon_through_its_turn(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.cli_ack(event_id, turn)
        thread = self.adapter.threads[PARENT]
        position = next(i for i, item in enumerate(thread.items) if item[0] == turn)
        thread.items.insert(position, (turn, f"requestId: {request_id}"))
        thread.items.extend((turn, f"work item {n}", "commandExecution") for n in range(250))
        self.clock.advance(5)
        self.daemon.tick()
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_an_echo_alone_never_confirms_an_uncertain_send(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.echo(turn, f"status: attempt {request_id} held_uncertain")
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(outcome["state"], HELD_UNCERTAIN)
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)


class EverySettlementIsACompareAndSet(HostLossCase):
    """D10: an attempt row has several writers (the sender, the daemon's and a manual reconcile,
    the confirmation through an ACK's turn), and each settles only the attempt it read. A reader
    that found nothing never undoes what another reader established meanwhile."""

    def test_a_replacement_ack_written_during_the_read_is_not_promoted_with_the_old_evidence(self):
        event_id, request_id, turn = self.uncertain_delivery(message=False)
        self.cli_ack(event_id, turn)
        self.adapter.threads[PARENT].items.append((turn, f"requestId: {request_id}"))
        self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        later = self.adapter.start_turn(PARENT, turn_id="later-turn", status="inProgress")
        ack = self.ack

        class ReplacesDuringTheRead:
            def __init__(self, inner):
                self._inner = inner
                self.fired = False

            def read_turn(self, thread_id, turn_id):
                if not self.fired:
                    self.fired = True
                    # A host-less re-acknowledgement from another turn: the parent's own upsert.
                    ack.acknowledge(event_id, ack_turn_id=later.turn_id,
                                    ack_proof=identity.ack_proof(event_id, later.turn_id),
                                    accepted=True, adapter=None)
                return self._inner.read_turn(thread_id, turn_id)

            def __getattr__(self, name):
                return getattr(self._inner, name)

        results = self.ack.verify_pending_acks(ReplacesDuringTheRead(self.adapter))
        self.assertEqual([r.get("outcome") for r in results], ["replaced"])
        row = self.store.one("SELECT ack_turn_id, verified FROM acks WHERE event_id = ?", (event_id,))
        self.assertEqual((row["ack_turn_id"], row["verified"]), ("later-turn", "unverified_turn"))
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_a_reader_that_found_nothing_cannot_undo_a_confirmation_made_meanwhile(self):
        event_id, request_id, turn = self.uncertain_delivery()
        thread = self.adapter.threads[PARENT]
        thread.items.extend((turn, f"work item {n}", "commandExecution") for n in range(250))
        reconciler = self.reconciler
        adapter = self.adapter

        class ConfirmsDuringTheScan:
            def __init__(self, inner):
                self._inner = inner
                self.fired = False

            def find_token(self, *args, **kwargs):
                scan = self._inner.find_token(*args, **kwargs)
                if not self.fired:
                    self.fired = True
                    reconciler.confirm_delivery(event_id, adapter, turn_id=turn)
                return scan

            def __getattr__(self, name):
                return getattr(self._inner, name)

        outcome = self.reconciler.reconcile_attempt(request_id, ConfirmsDuringTheScan(self.adapter))
        self.assertEqual(outcome["evidence"], "turn_found")
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_a_reader_that_found_nothing_cannot_undo_a_pre_send_rejection_made_meanwhile(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        request_id = self.attempt(event_id)["requestId"]
        reconciler = self.reconciler
        adapter = self.adapter

        class RejectsDuringTheScan:
            def __init__(self, inner):
                self._inner = inner
                self.fired = False

            def find_token(self, *args, **kwargs):
                scan = self._inner.find_token(*args, **kwargs)
                if not self.fired:
                    self.fired = True
                    adapter.ledger[request_id] = _pre_send_rejection(request_id)
                    reconciler.reconcile_attempt(request_id, adapter)
                return scan

            def __getattr__(self, name):
                return getattr(self._inner, name)

        outcome = self.reconciler.reconcile_attempt(request_id, RejectsDuringTheScan(self.adapter))
        self.assertTrue(outcome.get("changed"))
        self.assertEqual(self.evidence_of(event_id), ["confirmed_pre_send_rejection"])
        self.assertNotEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.clock.advance(100000)
        self.assertEqual(self.attempt(event_id, now=self.clock.now())["attemptNo"], 2)

    def test_a_refused_stale_write_leaves_the_attempt_owed_to_the_next_tick(self):
        event_id, request_id, turn = self.uncertain_delivery()
        thread = self.adapter.threads[PARENT]
        thread.items.extend((turn, f"work item {n}", "commandExecution") for n in range(250))
        reconciler = self.reconciler
        adapter = self.adapter

        class ConfirmsDuringTheScan:
            def __init__(self, inner):
                self._inner = inner
                self.fired = False

            def find_token(self, *args, **kwargs):
                scan = self._inner.find_token(*args, **kwargs)
                if not self.fired:
                    self.fired = True
                    reconciler.confirm_delivery(event_id, adapter, turn_id=turn)
                return scan

            def __getattr__(self, name):
                return getattr(self._inner, name)

        self.daemon_with(RetryPolicy(max_sends_per_tick=0),
                         adapter=ConfirmsDuringTheScan(self.adapter)).tick()
        gate = self.store.one("SELECT retry_required FROM reconcile_gate WHERE request_id = ?",
                              (request_id,))
        self.assertEqual(gate["retry_required"], 1)
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])

    def test_a_reconcile_during_the_send_that_found_nothing_leaves_the_accepted_send_to_the_next_tick(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        reconciler = self.reconciler
        adapter = self.adapter
        seen = {}

        class ReconcilesBeforeTheSend:
            def __init__(self, inner):
                self._inner = inner

            def send_message(self, request_id, *args, **kwargs):
                seen["outcome"] = reconciler.reconcile_attempt(request_id, adapter)
                return self._inner.send_message(request_id, *args, **kwargs)

            def __getattr__(self, name):
                return getattr(self._inner, name)

        result = self.delivery.attempt(event_id, ReconcilesBeforeTheSend(self.adapter))
        self.assertEqual(seen["outcome"]["state"], HELD_UNCERTAIN)
        self.assertTrue(result.get("_settledElsewhere"))
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        self.clock.advance(20)
        self.daemon_with(RetryPolicy(max_sends_per_tick=0)).tick()
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_reconcile_during_the_send_that_confirmed_it_survives_the_senders_unknown_result(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        reconciler = self.reconciler
        adapter = self.adapter
        started = {}

        class HostRunsTheTurnAndAReconcileFindsIt:
            def __init__(self, inner):
                self._inner = inner

            def send_message(self, request_id, *args, **kwargs):
                receipt = self._inner.send_message(request_id, *args, **kwargs)
                started["turn"] = adapter.start_turn(
                    PARENT, status="inProgress", text=f"requestId: {request_id}").turn_id
                reconciler.reconcile_attempt(request_id, adapter)
                return receipt

            def __getattr__(self, name):
                return getattr(self._inner, name)

        result = self.delivery.attempt(event_id, HostRunsTheTurnAndAReconcileFindsIt(self.adapter))
        self.assertTrue(result.get("_settledElsewhere"))
        self.assertEqual(result.get("_deliveryState"), DISPATCHED)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["dispatch_turn_id"]), (DISPATCHED, started["turn"]))
        self.assertEqual(self.evidence_of(event_id), ["turn_found"])

    def test_a_later_reconcile_that_cannot_read_the_receipt_keeps_a_pre_send_rejection(self):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        request_id = self.attempt(event_id)["requestId"]
        self.adapter.ledger[request_id] = _pre_send_rejection(request_id)
        self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(self.evidence_of(event_id), ["confirmed_pre_send_rejection"])
        before = self.delivery_row(event_id)["state"]
        self.adapter.ledger.pop(request_id)
        self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(self.evidence_of(event_id), ["confirmed_pre_send_rejection"])
        self.assertEqual(self.delivery_row(event_id)["state"], before)

    def test_a_typed_item_leaves_the_fingerprint_and_readback_working(self):
        thread = self.adapter.threads[PARENT]
        thread.items.append(("t-x", "plain"))
        before = self.adapter.recipient_fingerprint(PARENT)
        thread.items.append(("t-x", "del-echo-a1", "commandExecution"))
        self.assertNotEqual(self.adapter.recipient_fingerprint(PARENT), before)
        # Supervisor readback keeps the default: any item that carries the token.
        self.assertTrue(self.adapter.find_token(PARENT, "del-echo-a1").found)
        self.assertFalse(self.adapter.find_token(PARENT, "del-echo-a1", message_only=True).found)


def _pre_send_rejection(request_id):
    """The transport's attributable pre-send refusal, as the ledger records it."""
    return {"requestId": request_id, "status": "failed", "error": "thread/read: transport refused",
            "rpcError": {"code": "internal", "message": "refused"}}


def _pages(*pages):
    """A thread/turns/list answer per call, newest first, chained by cursor."""
    calls = []

    def call(method, params):
        if method != "thread/turns/list":
            raise AssertionError(f"unexpected call {method}")
        calls.append(dict(params))
        index = int(params.get("cursor") or 0)
        data = pages[index]
        return {"data": data, "nextCursor": str(index + 1) if index + 1 < len(pages) else None}

    return call, calls


def _turn(turn_id, started_at, status="completed"):
    return {"id": turn_id, "status": status, "startedAt": started_at, "items": []}


class TheAdapterLooksBackOnlyToTheSend(unittest.TestCase):
    def lookup(self, call, turn_id="wanted"):
        return BridgeHostAdapter(call=call, page=2).find_dispatched_turn(
            "thread", turn_id, sent_at=SENT_AT)

    def test_a_turn_on_a_later_page_is_found(self):
        call, _calls = _pages([_turn("newer-1", SENT_AT + 50), _turn("newer-2", SENT_AT + 40)],
                              [_turn("wanted", SENT_AT + 1, "inProgress")])
        presence = self.lookup(call)
        self.assertEqual((presence.finding, presence.turn.turn_id, presence.turn.status),
                         ("present", "wanted", "inProgress"))

    def test_a_turn_older_than_the_send_ends_the_search_without_reading_further(self):
        call, calls = _pages([_turn("newer", SENT_AT + 30), _turn("before", SENT_AT - 600)],
                             [_turn("wanted", SENT_AT - 900)])
        presence = self.lookup(call)
        self.assertEqual((presence.finding, presence.stop, presence.scanned),
                         ("absent", "older_than_send", 2))
        self.assertEqual(len(calls), 1)

    def test_the_end_of_the_listing_is_absence(self):
        call, _calls = _pages([_turn("newer", SENT_AT + 30)])
        presence = self.lookup(call)
        self.assertEqual((presence.finding, presence.stop), ("absent", "listing_end"))

    def test_an_empty_listing_is_not_evidence(self):
        call, _calls = _pages([])
        with self.assertRaises(HostUnavailable):
            self.lookup(call)

    def test_a_bounded_scan_that_never_reached_the_send_is_not_evidence(self):
        pages = [[_turn(f"newer-{n}-{m}", SENT_AT + 1000 - n) for m in range(2)] for n in range(25)]
        call, calls = _pages(*pages)
        with self.assertRaises(HostUnavailable):
            self.lookup(call)
        self.assertEqual(len(calls), 20)

    def test_a_long_history_after_the_send_is_read_through_to_the_send(self):
        """Review of 35aa454c: 250 turns after a lost send still reach an answer."""
        pages = [[_turn(f"newer-{n}-{m}", SENT_AT + 1000 - n) for m in range(50)] for n in range(5)]
        pages.append([_turn("before", SENT_AT - 600)])
        call, calls = _pages(*pages)
        presence = BridgeHostAdapter(call=call, page=50).find_dispatched_turn(
            "thread", "wanted", sent_at=SENT_AT)
        self.assertEqual((presence.finding, presence.stop, presence.scanned),
                         ("absent", "older_than_send", 251))
        self.assertEqual(len(calls), 6)

    def test_a_steered_turn_begun_before_the_send_is_matched_before_the_cutoff(self):
        call, _calls = _pages([_turn("wanted", SENT_AT - 600, "inProgress"),
                               _turn("before", SENT_AT - 900)])
        self.assertEqual(self.lookup(call).finding, "present")

    def test_a_turn_with_no_start_time_never_ends_the_search(self):
        call, _calls = _pages([_turn("undated", None), _turn("wanted", SENT_AT + 2)])
        self.assertEqual(self.lookup(call).finding, "present")

    def test_it_asks_for_the_newest_turns_first_without_their_items(self):
        call, calls = _pages([_turn("wanted", SENT_AT + 1)])
        self.lookup(call)
        self.assertEqual(calls[0], {"threadId": "thread", "limit": 2, "itemsView": "notLoaded",
                                    "sortDirection": "desc"})

    def test_a_match_reads_on_to_the_first_turn_older_than_the_send(self):
        call, calls = _pages([_turn("newer", SENT_AT + 30), _turn("wanted", SENT_AT + 1)],
                             [_turn("between", SENT_AT - 10), _turn("before", SENT_AT - 600)],
                             [_turn("older", SENT_AT - 900)])
        presence = self.lookup(call)
        self.assertEqual((presence.finding, presence.turn.turn_id, presence.scanned),
                         ("present", "wanted", 2))
        self.assertEqual(presence.older, ("before",))
        self.assertEqual(len(calls), 2)

    def test_a_failure_reading_on_after_a_match_keeps_the_match(self):
        first = [_turn("newer", SENT_AT + 30), _turn("wanted", SENT_AT + 1)]

        def call(method, params):
            if params.get("cursor"):
                raise HostUnavailable("the next page could not be read")
            return {"data": first, "nextCursor": "1"}

        presence = self.lookup(call)
        self.assertEqual((presence.finding, presence.turn.turn_id, presence.older),
                         ("present", "wanted", ()))

    def test_the_in_turn_read_asks_for_that_turns_items_oldest_first(self):
        calls = []

        def call(method, params):
            calls.append((method, dict(params)))
            return {"data": [_item("t1", "userMessage", "requestId: tok")], "nextCursor": None}

        scan = BridgeHostAdapter(call=call, page=2).find_token_in_turn("thread", "tok", turn_id="t1")
        self.assertTrue(scan.found)
        self.assertEqual(calls, [("thread/items/list", {"threadId": "thread", "turnId": "t1",
                                                         "sortDirection": "asc", "limit": 2})])

    def test_the_in_turn_read_counts_only_that_turns_user_message(self):
        def call(method, params):
            return {"data": [_item("other", "userMessage", "requestId: tok"),
                             _item("t1", "commandExecution", "requestId: tok")],
                    "nextCursor": None}

        scan = BridgeHostAdapter(call=call, page=8).find_token_in_turn("thread", "tok", turn_id="t1")
        self.assertEqual((scan.found, scan.exhausted), (False, True))

    def test_the_thread_reads_tell_the_message_from_agent_output(self):
        items = [_item("t2", "commandExecution", "del-x-a1"), _item("t2", "hookPrompt", "del-x-a1")]

        def call(method, params):
            return {"data": items, "nextCursor": None}

        adapter = BridgeHostAdapter(call=call, page=8)
        self.assertFalse(adapter.find_token("thread", "del-x-a1", message_only=True).found)
        self.assertTrue(adapter.find_token("thread", "del-x-a1").found)
        self.assertTrue(adapter.find_token_since("thread", "del-x-a1", older=()).found)
        items[1] = _item("t2", "agentMessage", "del-x-a1")
        self.assertFalse(adapter.find_token_since("thread", "del-x-a1", older=()).found)


def _item(turn_id, kind, text):
    if kind == "userMessage":
        item = {"type": kind, "id": "i", "content": [{"type": "text", "text": text}]}
    elif kind == "commandExecution":
        item = {"type": kind, "id": "i", "aggregatedOutput": text}
    elif kind == "agentMessage":
        item = {"type": kind, "id": "i", "text": text}
    else:
        item = {"type": kind, "id": "i", "fragments": [{"text": text}]}
    return {"turnId": turn_id, "item": item}

if __name__ == "__main__":
    unittest.main()
