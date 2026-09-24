"""On-request recipients are carried; the relay neither sends nor answers an approval (CRW-225).

Measured on codex-cli 0.154.0 (codex-thread-bridge README "Approval requests, as measured"): the
host sends every approval request of a turn to each subscribed client, replays a pending one to a
client that resumes the thread later, applies the first answer, and keeps a thread's own policy
across unload and restart. A relay-started turn on an on-request thread therefore leaves every
approval with the thread's own approver, provided the relay neither answers one (the bridge change)
nor sets the policy on resume (this package).

Each test is labelled RED where it fails on the pre-change relay, or GREEN where it pins behaviour
that must not change.
"""

import dataclasses
import unittest

from codex_session_relay import identity
from codex_session_relay.errors import DeliveryRefused, RefusalReason
from codex_session_relay.settings import TaskSettings
from codex_session_relay.transport import (
    DEFERRED_BUSY,
    DISPATCHED,
    INBOX_ONLY,
    WITHHELD_PRE_SEND,
    classify_operation_receipt,
)

from . import test_bridge_adapter as seam
from .support import PARENT, DeliveryTestCase, task_settings
from .test_supervisor_channel import SUPERVISOR, ChannelTestCase
from codex_session_relay import supervisorchannel as channel_module


def on_request(path="/parent"):
    return task_settings(path, approvalPolicy="on-request")


def record_based(policy="never"):
    """A supervisor's record: no pair policy derived it, so it is resumed with nothing sent."""
    settings = TaskSettings(dict(seam.AUTHORIZED.data, approvalPolicy=policy))
    settings.settings_free_resume = True
    return settings


class TheRecord(unittest.TestCase):
    def test_an_on_request_record_is_usable(self):
        """RED: require_usable refused every policy but never."""
        TaskSettings(on_request()).require_usable()

    def test_untrusted_and_granular_records_stay_refused(self):
        """GREEN: only never and on-request are carried."""
        for recorded in ("untrusted", {"granular": {}}, None):
            with self.subTest(recorded=recorded):
                with self.assertRaises(DeliveryRefused) as caught:
                    TaskSettings(task_settings("/parent", approvalPolicy=recorded)).require_usable()
                self.assertIn(caught.exception.reason, (RefusalReason.UNSUPPORTED_APPROVAL_POLICY,
                                                        RefusalReason.SETTINGS_MISTYPED,
                                                        RefusalReason.SETTINGS_INCOMPLETE))

    def test_the_transmitted_resume_sends_no_approval_policy(self):
        """RED: resume_params carried the recorded policy, which the host applies to a thread the
        resume loads, so a recorded never was forced onto a thread its owner had switched."""
        for recorded in (task_settings("/parent"), on_request()):
            with self.subTest(policy=recorded["approvalPolicy"]):
                self.assertNotIn("approvalPolicy", TaskSettings(recorded).resume_params("t-1"))


class ParentChildDelivery(DeliveryTestCase):
    def test_an_on_request_parent_is_woken_once(self):
        """RED: the record was refused before any send and the report was stored, not woken."""
        _relationship, event_id = self.queued_event(settings=on_request())
        self.adapter.threads[PARENT].approval_policy = "on-request"
        record = self.attempt(event_id)
        self.assertIsNotNone(record, "the delivery was withheld")
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(record["recipientApprovalPolicy"], "on-request")
        self.assertEqual(len(self.adapter.sends), 1)
        self.clock.advance(100000)
        self.assertEqual(self.delivery.eligible(now=self.clock.now()), [],
                         "a dispatched report is not offered again")

    def test_a_parent_waiting_on_its_approver_is_busy_then_woken_exactly_once(self):
        """RED (record) / GREEN (route): a turn waiting for approval reads active, so the send is
        deferred on the existing busy route and made once the parent is idle."""
        _relationship, event_id = self.queued_event(settings=on_request())
        self.adapter.threads[PARENT].approval_policy = "on-request"
        self.adapter.script("busy")
        busy = self.attempt(event_id)
        self.assertEqual((busy["deliveryState"], busy["sendAttempted"]), (DEFERRED_BUSY, "no"))
        row = self.delivery_row(event_id)
        self.assertEqual(row["state"], DEFERRED_BUSY)
        record = self.attempt(event_id, now=row["next_eligible_at"])
        self.assertEqual(record["deliveryState"], DISPATCHED)
        started = [send for send in self.adapter.sends if send[3] == "accepted"]
        self.assertEqual(len(started), 1, "exactly one turn/start for the logical message")

    def test_a_folded_start_settles_once_across_an_app_server_and_relay_restart(self):
        """RED (record): the send lands in a turn that was already running (the host folds a
        turn/start into an active turn and answers with its id). Dispatch alone is no ACK and no
        completion; an ACK repeated settles once; and after the App Server and the relay restart,
        the same logical message starts nothing more."""
        from codex_session_relay.delivery import DeliveryService

        _relationship, event_id = self.queued_event(settings=on_request())
        self.adapter.threads[PARENT].approval_policy = "on-request"
        existing = self.adapter.start_turn(PARENT, status="inProgress")
        self.adapter.script("steer_existing")
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(record["turnId"], existing.turn_id)
        self.assertIsNone(self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,)),
                          "dispatch alone is not an acknowledgement")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

        proof = identity.ack_proof(event_id, existing.turn_id)
        for _ in range(2):
            self.ack.acknowledge(event_id, ack_turn_id=existing.turn_id, ack_proof=proof,
                                 accepted=True, adapter=self.adapter)
        rows = self.store.all("SELECT * FROM acks WHERE event_id = ?", (event_id,))
        self.assertEqual(len(rows), 1)

        # Completed before the restart, so this is not the interrupted-turn case (CRW-224's).
        turns = self.adapter.threads[PARENT].turns
        turns[turns.index(existing)] = dataclasses.replace(existing, status="completed")
        self.adapter.restart()
        for thread in self.adapter.threads.values():
            thread.status = "notLoaded"
        restarted = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.clock.advance(100000)
        self.assertIsNone(restarted.attempt(event_id, self.adapter, now=self.clock.now()))
        sends = [send for send in self.adapter.sends if send[1] == PARENT]
        self.assertEqual(len(sends), 1, "the same logical message was sent again")


class RealAdapterRoutes(unittest.TestCase):
    """The real adapter and the real guarded send, against a fake RPC endpoint."""

    setUp = seam.GuardedSettingsSeam.setUp
    _adapter = seam.GuardedSettingsSeam._adapter
    _methods = staticmethod(seam.GuardedSettingsSeam._methods)

    def test_a_thread_on_request_is_started_and_its_difference_from_the_record_noted(self):
        """RED: an on-request answer to a record of never was unsupported_approval_policy."""
        for label, settings in (("transmitted", seam.AUTHORIZED), ("settings_free", record_based())):
            with self.subTest(route=label):
                adapter, calls = self._adapter(
                    resume=seam.authorized_resume_response(approvalPolicy="on-request"))
                receipt = adapter.send_message("del-a1c000000000-" + label[:2], "thread-1", "hi",
                                               settings)
                self.assertIn("turn/start", self._methods(calls), receipt)
                self.assertEqual(receipt["status"], "accepted")
                self.assertEqual(receipt["settingsNotes"], [{
                    "code": "approval_policy_differs_from_record",
                    "field": "approvalPolicy", "recorded": "never", "observed": "on-request"}])
                resumes = [params for method, params in calls if method == "thread/resume"]
                self.assertTrue(resumes)
                self.assertTrue(all("approvalPolicy" not in params for params in resumes),
                                "the relay sent an approval policy")

    def test_untrusted_stays_stored_not_woken(self):
        """GREEN: a policy outside the carried set keeps the closed push channel (I-249)."""
        adapter, calls = self._adapter(
            resume=seam.authorized_resume_response(approvalPolicy="untrusted"))
        receipt = adapter.send_message("del-a1c000000000-u1", "thread-1", "hi", seam.AUTHORIZED)
        self.assertNotIn("turn/start", self._methods(calls))
        self.assertEqual(receipt["rpcError"]["code"], "unsupported_approval_policy")
        self.assertEqual(classify_operation_receipt(receipt).delivery_state, INBOX_ONLY)


class OnRequestSupervisor(ChannelTestCase):
    def build_channel(self, **kw):
        kw.setdefault("settings", lambda task, runtime=None: record_based())
        return super().build_channel(**kw)

    def test_an_on_request_supervisor_receives_the_push(self):
        """RED: the supervisor's own on-request answer withheld the report (withheld_pre_send
        carrying inbox_only) until its policy went back to never."""
        self.adapter.threads[SUPERVISOR].approval_policy = "on-request"
        _one, message_id = self.staged()
        record = self.channel.attempt(message_id, self.adapter)
        self.assertIsNotNone(record, "the push was withheld")
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_an_untrusted_supervisor_is_still_not_pushed(self):
        """GREEN: I-249 holds for a policy outside the carried set."""
        self.adapter.threads[SUPERVISOR].approval_policy = "untrusted"
        _one, message_id = self.staged()
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual((record["deliveryState"], record["sendAttempted"], record["turnId"]),
                         (WITHHELD_PRE_SEND, "no", None))
        self.assertEqual(record["transportDeliveryState"], INBOX_ONLY)
        self.assertEqual(self.channel.get(message_id)["state"], WITHHELD_PRE_SEND)

    def test_a_push_folded_into_a_running_turn_is_not_a_completion(self):
        """RED (policy): the send lands in the supervisor's running turn; the chronology guard says
        that turn predates the send, nothing verifies, and a second attempt sends nothing."""
        self.adapter.threads[SUPERVISOR].approval_policy = "on-request"
        existing = self.adapter.start_turn(SUPERVISOR, status="inProgress")
        self.clock.advance(600)
        _one, message_id = self.staged()
        self.adapter.script("steer_existing")
        record = self.channel.attempt(message_id, self.adapter)
        self.assertIsNotNone(record, "the push was withheld")
        self.assertEqual(record["turnId"], existing.turn_id)
        answer = self.read_back(message_id, existing.turn_id)
        self.assertEqual(answer["verified"], channel_module.TURN_PREDATES_SEND)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED,
                         "no read and no completion is claimed for the folded turn")
        before = len(self.adapter.sends)
        self.clock.advance(100000)
        self.channel.attempt(message_id, self.adapter, now=self.clock.now())
        self.assertEqual(len(self.adapter.sends), before, "the same report was pushed again")
