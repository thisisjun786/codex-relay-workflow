"""The parent-to-supervisor channel: what may be staged, what is sent, what came back.

The roundtrip case is the one to read first. It stages a real obligation, sends it through the
real claim and the real transport path against the fake host, and has the recipient answer from
inside its own turn - and then checks the two things that make that a roundtrip rather than a
log line: the bytes the supervisor holds carry the request id, and they do not carry the turn
id the proof is computed over.

The clock is injected everywhere and nothing here waits on anything real.

One deliberate stub. A task bound to a scope is subject to the role policy, which is read from
a file this suite does not write, so the ordinary cases pass a settings callable. The gate
itself is asserted separately, by letting the real one run and watching it refuse - which is
the direction that matters: a send that cannot say what it is preserving does not happen.
"""

import json
import os
import shlex
import unittest
from contextlib import contextmanager
from unittest import mock

from codex_session_relay import envelope, identity, manifest, packets, supervision
from codex_session_relay import report as report_module
from codex_session_relay import supervisorchannel as channel_module
from codex_session_relay.errors import DeliveryRefused, RefusalReason
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.identity import supervisor_read_proof
from codex_session_relay.linkage import Linkage
from codex_session_relay.models import Endpoint, TurnRef
from codex_session_relay.registry import record_settings
from codex_session_relay.store import Store
from codex_session_relay.supervisorchannel import SupervisorChannel
from codex_session_relay.transport import (
    DEFERRED_BUSY, DISPATCHED, HELD_UNCERTAIN, INBOX_ONLY, QUEUED, WITHHELD_PRE_SEND,
    classify_operation_receipt,
)

from .support import (
    CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, RelayTestCase, WorkerKilled, task_settings,
)

SUPERVISOR = "01supervisor-task"
PROJECT = "PRJ-1"
INITIATIVE = "INI-1"


class _Reader:
    """A linkage reader answering exactly what it was told to, for shapes a real store cannot
    be asked to produce on demand."""

    def __init__(self, answer):
        self.answer = answer

    def up(self, **kwargs):
        return self.answer


class ChannelTestCase(RelayTestCase):
    """One assignment, under a project, under an initiative, with a host for all three."""

    def setUp(self):
        super().setUp()
        self.linkage = Linkage(self.store, self.clock)
        self.adapter = FakeHostAdapter(self.clock)
        for task in (PARENT, CHILD, SUPERVISOR):
            self.adapter.add_thread(task)
        self.relationship = self.register()
        self.rid = self.relationship["relationshipId"]
        record_settings(self.store, self.clock, SUPERVISOR, task_settings("/supervisor"),
                        source="creation_result")
        self.linkage.register_supervision(
            initiative_key=INITIATIVE, project_key=PROJECT,
            supervisor=Endpoint(SUPERVISOR, HOST, cwd="/supervisor", cxc_session="cxc-sup"),
            parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"))
        self.linkage.attach_issue(self.rid, PROJECT)
        self.channel = self.build_channel()

    def build_channel(self, **kw):
        kw.setdefault("settings", lambda task, runtime=None: {"authorized": task})
        return SupervisorChannel(self.store, self.registry, self.linkage, self.clock, **kw)

    # ------------------------------------------------------------------- fixtures

    def completed(self, *, text="the deliverable", status="DONE"):
        """A final reviewable event with a work report, which is news for the level above."""
        path = self.artifact("out.txt", text)
        payload = self.ready_payload(self.relationship, [path])
        self.accept(payload)
        report_module.record(
            self.store, self.clock, event_id=payload["eventId"],
            repository="thisisjun786/codex-relay-workflow", cxc_status=status,
            cxc_reason="every recorded criterion was proved", summary="the work is done",
            next_action="merge", evidence=["pytest tests/test_supervisor_channel.py passed"])
        return payload["eventId"]

    def obligation(self, event_id=None):
        event_id = event_id or self.completed()
        return supervision.from_event(
            self.store, event_id, report_module.read(self.store, event_id))

    def halted(self, status="NEEDS_HUMAN"):
        """A turn the child stopped for a judgment the parent may not make for the user."""
        payload = self.execution_payload(self.relationship, "blocked_needs_input")
        self.accept(payload)
        report_module.record(
            self.store, self.clock, event_id=payload["eventId"],
            repository="thisisjun786/codex-relay-workflow", cxc_status=status,
            cxc_reason="two readings of the criterion are defensible",
            summary="which reading of the criterion is the agreed one",
            next_action="ask the user", evidence=["both readings are in the review thread"])
        return payload["eventId"]

    def staged(self, **kw):
        one = self.obligation(self.completed(**kw))
        return one, self.channel.stage(one)["messageId"]

    def delivered(self, **kw):
        one, message_id = self.staged(**kw)
        record = self.channel.attempt(message_id, self.adapter)
        return one, message_id, record

    def read_back(self, message_id, turn_id, *, adapter=None):
        return self.channel.read_back(
            message_id, read_turn_id=turn_id,
            proof=supervisor_read_proof(message_id, turn_id),
            adapter=self.adapter if adapter is None else adapter)

    def bytes_of(self, message_id):
        return self.store.one(
            "SELECT message FROM supervisor_attempts WHERE message_id = ?"
            " ORDER BY attempt_no DESC LIMIT 1", (message_id,))["message"]


class OnlyTheLiveHierarchyDecidesWhoIsTold(ChannelTestCase):
    def test_the_project_owner_sends_and_the_initiative_owner_receives(self):
        who = self.channel.resolve(self.rid)
        self.assertEqual(who["sender"], PARENT)
        self.assertEqual(who["recipient"], SUPERVISOR)
        self.assertEqual(who["projectKey"], PROJECT)
        self.assertEqual(who["initiativeKey"], INITIATIVE)
        self.assertEqual(who["source"], "linkage")

    def test_a_project_nobody_supervises_has_nowhere_to_report_and_still_owes_one(self):
        """The difference between having nowhere to send a report and not owing one."""
        channel = self.build_channel()
        channel.linkage = _Reader({
            "state": "resolved", "readable": True, "contention": [],
            "gaps": [{"gap": "no_supervisor", "scopeKind": "project", "scopeKey": PROJECT}],
            "levels": [{"scopeKind": "project", "scopeKey": PROJECT,
                        "owner": {"taskId": PARENT}, "depth": 1}]})
        refusal = self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, channel.resolve, self.rid)
        self.assertIn("nobody to report to", refusal.detail)
        # And the obligation itself is untouched by the refusal.
        one = self.obligation()
        self.assertEqual(
            supervision.select(self.store, one, recipient=None)["standing"],
            supervision.STANDING)

    def test_an_unreadable_hierarchy_is_not_a_missing_supervisor(self):
        channel = self.build_channel()
        channel.linkage = _Reader({"state": "unreadable", "readable": False, "levels": [],
                                   "gaps": [], "contention": []})
        self.assertRefused(RefusalReason.RELATION_UNREADABLE, channel.resolve, self.rid)

    def test_two_candidates_above_are_not_resolved_by_choosing_one(self):
        channel = self.build_channel()
        channel.linkage = _Reader({
            "state": "ambiguous", "readable": True, "levels": [], "gaps": [],
            "contention": [{"contention": "competing_owners", "scopeKind": "initiative",
                            "scopeKey": INITIATIVE, "candidates": ["01a", "01b"]}]})
        self.assertRefused(RefusalReason.DUPLICATE_SCOPE_OWNER, channel.resolve, self.rid)

    def test_a_drifting_owner_holds_the_report_rather_than_picking_a_side(self):
        channel = self.build_channel()
        channel.linkage = _Reader({
            "state": "resolved", "readable": True, "gaps": [],
            "contention": [{"contention": "owner_drift", "linkId": "lnk-1",
                            "recorded": PARENT, "live": "01someone-else"}],
            "levels": [{"scopeKind": "project", "scopeKey": PROJECT,
                        "owner": {"taskId": PARENT}, "depth": 1}]})
        self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, channel.resolve, self.rid)

    def test_a_retained_refusal_row_does_not_silence_a_project_for_good(self):
        """linkage keeps every refused write, and nothing deletes them."""
        channel = self.build_channel()
        channel.linkage = _Reader({
            "state": "resolved", "readable": True, "gaps": [],
            "contention": [{"reason": "role_already_bound", "scopeKind": "project",
                            "scopeKey": PROJECT, "at": "2026-01-01T00:00:00Z"}],
            "levels": [
                {"scopeKind": "project", "scopeKey": PROJECT,
                 "owner": {"taskId": PARENT}, "depth": 1},
                {"scopeKind": "initiative", "scopeKey": INITIATIVE,
                 "owner": {"taskId": SUPERVISOR}, "depth": 2}]})
        self.assertEqual(channel.resolve(self.rid)["recipient"], SUPERVISOR)


class WhatMayBeStaged(ChannelTestCase):
    def test_one_fact_is_one_message_however_often_it_is_staged(self):
        one = self.obligation()
        first = self.channel.stage(one)
        second = self.channel.stage(one)
        self.assertTrue(first["staged"])
        self.assertFalse(second["staged"])
        self.assertEqual(first["messageId"], second["messageId"])
        self.assertEqual(
            len(self.store.all("SELECT message_id FROM supervisor_messages")), 1)

    def test_the_staged_row_outlives_the_process_that_wrote_it(self):
        _one, message_id = self.staged()
        self.store.close()
        reopened = Store(self.store.path)
        self.addCleanup(reopened.close)
        row = reopened.one(
            "SELECT * FROM supervisor_messages WHERE message_id = ?", (message_id,))
        self.assertIsNotNone(row, "staging that does not survive a restart is not staging")
        self.assertEqual(row["state"], QUEUED)
        packet = json.loads(row["packet"])
        self.assertEqual(packet["version"], packets.VERSION)
        self.assertEqual(packet["envelope"]["direction"], envelope.PARENT_TO_SUPERVISOR)

    def test_an_ordinary_event_is_not_news_and_has_no_obligation_to_stage(self):
        payload = self.execution_payload(self.relationship, "failed")
        self.accept(payload, observation=TurnRef(CHILD, DISPATCH_TURN, "failed"))
        self.assertIsNone(
            supervision.from_event(self.store, payload["eventId"]),
            "how a turn ended is the parent's business, not news for the level above")

    def test_a_fact_already_reported_is_refused_and_the_obligation_stays_standing(self):
        one = self.obligation()
        supervision.record_report(self.store, one, at=self.clock.iso(), messageId="deadbeef")
        refusal = self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.channel.stage, one)
        self.assertIn(supervision.ALREADY_REPORTED, refusal.detail)
        self.assertEqual(
            supervision.select(self.store, one, recipient=None)["standing"],
            supervision.STANDING)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])

    def test_a_caller_naming_another_recipient_is_the_finding(self):
        one = self.obligation()
        refusal = self.assertRefused(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            self.channel.stage, one, expect_recipient="01someone-else")
        self.assertIn(SUPERVISOR, refusal.detail)

    def test_staging_records_the_report_so_the_next_reading_converges(self):
        one, message_id = self.staged()
        recorded = supervision.prior_report(self.store, one["obligationId"])
        self.assertIsNotNone(recorded)
        self.assertEqual(recorded["detail"]["messageId"], message_id)
        self.assertFalse(supervision.select(self.store, one, recipient=None)["report"])

    def test_a_project_is_staged_in_one_call_and_what_is_refused_is_reported_beside_it(self):
        self.completed()
        answer = self.channel.stage_standing(PROJECT)
        self.assertEqual(len(answer["staged"]), 1)
        self.assertEqual(answer["refused"], [])
        self.assertTrue(answer["staged"][0]["staged"])
        again = self.channel.stage_standing(PROJECT)
        # The obligation is still STANDING - only a confirmed Linear record discharges one -
        # and staging it again produces no second message.
        self.assertFalse(any(one["staged"] for one in again["staged"]))
        self.assertEqual(
            len(self.store.all("SELECT message_id FROM supervisor_messages")), 1)


class WhatTheMessageCarries(ChannelTestCase):
    def test_a_completion_names_the_issue_the_generation_and_where_to_read_it(self):
        one = self.obligation()
        packet = self.channel.compose(
            one, resolution=self.channel.resolve(self.rid), observed_at=self.clock.iso())
        packets.check(packet)
        self.assertEqual(packet[packets.ISSUE], ISSUE)
        self.assertEqual(packet[packets.GENERATION], 1)
        self.assertTrue(packet[packets.EVIDENCE])
        self.assertIn("show --event", packet[packets.EVIDENCE][0])
        self.assertEqual(packet["envelope"]["kind"], envelope.NOTIFICATION)

    def test_a_decision_upward_names_what_is_being_decided(self):
        one = self.obligation(self.halted())
        self.assertEqual(one["kind"], supervision.DECISION)
        packet = self.channel.compose(
            one, resolution=self.channel.resolve(self.rid), observed_at=self.clock.iso())
        self.assertEqual(packet["envelope"]["kind"], envelope.DECISION)
        self.assertEqual(packet["envelope"]["answerOwedBy"], envelope.USER)
        self.assertTrue(str(packet["envelope"]["decision"]).strip())

    def test_the_packet_table_covers_this_direction_now(self):
        for purpose in ("completion", "blocked", "decision_request"):
            self.assertIn(
                packets.ISSUE, packets.required_for(envelope.PARENT_TO_SUPERVISOR, purpose))
        with self.assertRaises(packets.PacketRefused):
            packets.required_for(envelope.SUPERVISOR_TO_PARENT, "midpoint_check")

    def test_a_report_upward_is_checked_against_the_supervisor_own_reading(self):
        """The receiver compares the packet against what IT read, not against itself."""
        one = self.obligation()
        packet = self.channel.compose(
            one, resolution=self.channel.resolve(self.rid), observed_at=self.clock.iso())
        agreeing = packets.reception(packet, {
            "relationId": self.rid, "parentTaskId": PARENT, "supervisorTaskId": SUPERVISOR,
            "issue": ISSUE, "generation": 1, "relationRevision": None,
            "relationStatus": "active"})
        self.assertEqual(agreeing["disposition"], packets.ACCEPTED)
        wrong = packets.reception(packet, {
            "relationId": self.rid, "parentTaskId": PARENT,
            "supervisorTaskId": "01another-supervisor", "issue": ISSUE, "generation": 1,
            "relationRevision": None, "relationStatus": "active"})
        self.assertEqual(wrong["disposition"], packets.REFUSAL)
        self.assertIn("recipient.taskId", [m["field"] for m in wrong["mismatches"]])


class SendingIt(ChannelTestCase):
    def test_a_send_freezes_its_bytes_and_the_bytes_carry_its_request_id(self):
        _one, message_id, record = self.delivered()
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(record["sendAttempted"], "yes")
        self.assertTrue(record["turnId"])
        self.assertIn(record["requestId"], self.bytes_of(message_id))
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_the_bytes_the_recipient_holds_are_the_bytes_that_were_frozen(self):
        _one, message_id, record = self.delivered()
        sent = [message for _request, _thread, message, _outcome in self.adapter.sends]
        self.assertEqual(sent, [self.bytes_of(message_id)])
        self.assertEqual(self.adapter.settings_seen[0][1], {"authorized": SUPERVISOR})

    def test_a_busy_recipient_is_left_alone_with_no_attempt_to_explain(self):
        _one, message_id = self.staged()
        self.adapter.set_status(SUPERVISOR, "active")
        self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        row = self.channel.get(message_id)
        self.assertEqual(row["state"], DEFERRED_BUSY)
        self.assertEqual(
            self.store.all("SELECT request_id FROM supervisor_attempts"), [],
            "no transport call was made, so there is no receipt to classify")

    def test_an_archived_recipient_holds_the_report_where_it_is(self):
        _one, message_id = self.staged()
        self.adapter.threads[SUPERVISOR].archived = True
        self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(self.channel.get(message_id)["state"], WITHHELD_PRE_SEND)
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [])

    def test_a_backoff_is_waited_out_rather_than_ignored(self):
        _one, message_id = self.staged()
        self.adapter.set_status(SUPERVISOR, "active")
        self.channel.attempt(message_id, self.adapter)
        self.adapter.set_status(SUPERVISOR, "idle")
        self.assertIsNone(self.channel.attempt(message_id, self.adapter),
                          "the message is eligible by state and is still inside its backoff")
        row = self.channel.get(message_id)
        self.assertIsNotNone(row["next_eligible_at"])
        self.assertIsNotNone(
            self.channel.attempt(message_id, self.adapter, now=row["next_eligible_at"]))

    def test_the_per_recipient_bound_is_the_recipient_s_and_is_shared_on_purpose(self):
        _one, message_id, _record = self.delivered()
        counted = self.store.one(
            "SELECT sends FROM recipient_rate WHERE recipient_task_id = ?", (SUPERVISOR,))
        self.assertEqual(counted["sends"], 1,
                         "the bound limits how often one task is woken, whichever queue woke it")

    def test_a_hierarchy_that_moved_under_a_staged_report_holds_it(self):
        _one, message_id = self.staged()
        self.linkage.handover(
            role="supervisor", scope_key=INITIATIVE, expect_task_id=SUPERVISOR,
            endpoint=Endpoint("01new-supervisor", HOST, cwd="/new", cxc_session="cxc-new"),
            acknowledged=[], evidence="the initiative changed hands", actor="a test")
        self.assertRefused(
            RefusalReason.RELATION_OWNER_DRIFT, self.channel.attempt, message_id, self.adapter)
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [])


class TheSettingsGateIsNotThisChannels(ChannelTestCase):
    def test_a_send_that_cannot_say_what_it_preserves_does_not_happen(self):
        """The real gate, not a stub: a bound role with no readable policy refuses.

        What matters is the direction. This channel resumes a task exactly as a delivery does,
        so it asks the same question, and a refusal holds the report instead of sending it
        under whatever the host would have defaulted to.
        """
        _one, message_id = self.staged()
        real = SupervisorChannel(self.store, self.registry, self.linkage, self.clock)
        self.assertIsNone(real.attempt(message_id, self.adapter))
        self.assertEqual(self.channel.get(message_id)["state"], WITHHELD_PRE_SEND)
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [])
        withheld = [json.loads(row["detail"]) for row in self.store.all(
            "SELECT detail FROM journal WHERE kind = 'supervisor_message_withheld'")]
        self.assertTrue(withheld)
        self.assertIn(withheld[-1]["reason"], (
            RefusalReason.ROLE_POLICY_UNCONFIGURED.value,
            RefusalReason.SETTINGS_UNAVAILABLE.value))


class TheTwoQueuesAreDisjoint(ChannelTestCase):
    def test_the_supervisor_queue_holds_no_deliveries_and_the_delivery_queue_no_messages(self):
        from codex_session_relay.delivery import DeliveryService

        event_id = self.completed()
        delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        delivery.enqueue(event_id)
        one = supervision.from_event(
            self.store, event_id, report_module.read(self.store, event_id))
        message_id = self.channel.stage(one)["messageId"]
        self.assertNotEqual(message_id, event_id)
        self.assertRefused(RefusalReason.NOT_CLAIMABLE, self.channel.get, event_id)
        self.assertRefused(RefusalReason.NOT_CLAIMABLE, delivery.get, message_id)
        self.assertEqual(
            [row["event_id"] for row in self.store.all("SELECT event_id FROM deliveries")],
            [event_id])

    def test_the_oldest_staged_message_is_sent_first(self):
        first = self.channel.stage(self.obligation(self.completed(text="one")))["messageId"]
        self.clock.advance(5)
        second = self.channel.stage(
            self.obligation(self.completed(text="two")))["messageId"]
        self.assertEqual(
            [row["message_id"] for row in self.channel.eligible(now=self.clock.now())],
            [first, second])


class TheRoundtrip(ChannelTestCase):
    def test_a_delivered_message_is_read_back_and_only_then_says_received(self):
        _one, message_id, record = self.delivered()
        ladder = self.channel.reach(message_id)
        self.assertTrue(envelope.stage_holds(ladder, envelope.TRANSPORT_ACCEPTED))
        self.assertEqual(ladder[envelope.RECEIVED]["state"], envelope.UNMEASURED,
                         "a send that was accepted is not a message that was read")

        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertTrue(answer["delivered"]["found"])
        token = self.store.one("SELECT delivery_token FROM supervisor_attempts"
                               " WHERE request_id = ?", (record["requestId"],))["delivery_token"]
        self.assertEqual(answer["delivered"]["token"], token)
        self.assertTrue(token.startswith(record["requestId"] + "."))
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ)

        ladder = self.channel.reach(message_id)
        self.assertTrue(envelope.stage_holds(ladder, envelope.RECEIVED))
        self.assertEqual(ladder[envelope.RECEIVED]["source"], "supervisor_readbacks")
        self.assertEqual(envelope.promotion_refused(ladder), [])

    def test_the_delivered_bytes_cannot_produce_the_proof(self):
        """The property the whole readback rests on, asserted against the real bytes."""
        _one, message_id, record = self.delivered()
        sent = self.bytes_of(message_id)
        self.assertIn(message_id, sent, "the recipient has to be able to quote the message id")
        self.assertNotIn(record["turnId"], sent)
        self.assertNotIn(supervisor_read_proof(message_id, record["turnId"]), sent)

    def test_a_proof_that_is_not_this_message_and_turn_is_refused(self):
        _one, message_id, record = self.delivered()
        self.assertRefused(
            RefusalReason.ACK_PROOF_MISMATCH, self.channel.read_back, message_id,
            read_turn_id=record["turnId"], proof="0" * 64, adapter=self.adapter)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_a_turn_the_host_does_not_list_does_not_verify_and_is_still_recorded(self):
        _one, message_id, _record = self.delivered()
        answer = self.read_back(message_id, "turn-nobody-has")
        self.assertEqual(answer["verified"], channel_module.TURN_NOT_FOUND)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)
        self.assertEqual(
            self.channel.reach(message_id)[envelope.RECEIVED]["state"], envelope.UNMEASURED)
        self.assertIsNotNone(
            self.store.one("SELECT * FROM supervisor_readbacks WHERE message_id = ?",
                           (message_id,)),
            "somebody answered, and hiding that would lose the fact that they did")

    def test_without_a_host_nothing_about_the_turn_is_established(self):
        _one, message_id, record = self.delivered()
        answer = self.channel.read_back(
            message_id, read_turn_id=record["turnId"],
            proof=supervisor_read_proof(message_id, record["turnId"]), adapter=None)
        self.assertEqual(answer["verified"], channel_module.NO_HOST)
        self.assertFalse(answer["delivered"]["scanned"])
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_which_turn_answered_is_recorded_rather_than_averaged_into_one_word(self):
        """The sender knows the id of the turn its own send opened. Say which one answered."""
        _one, message_id, record = self.delivered()
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["turnOrigin"], channel_module.RELAY_OPENED)

        _other, second = self.staged(text="a second deliverable")
        sent = self.channel.attempt(second, self.adapter, now=self.clock.now() + 3600)
        later = self.adapter.start_turn(SUPERVISOR, status="completed")
        self.assertNotEqual(later.turn_id, sent["turnId"])
        answer = self.read_back(second, later.turn_id)
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(answer["turnOrigin"], channel_module.RECIPIENT_OPENED)

    def test_a_turn_that_began_before_the_send_cannot_be_the_turn_that_read_it(self):
        earlier = self.adapter.start_turn(SUPERVISOR, status="completed")
        self.clock.advance(600)
        _one, message_id, _record = self.delivered()
        answer = self.read_back(message_id, earlier.turn_id)
        self.assertEqual(answer["verified"], channel_module.TURN_PREDATES_SEND)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_a_second_readback_of_one_message_converges_on_the_first(self):
        _one, message_id, record = self.delivered()
        first = self.read_back(message_id, record["turnId"])
        again = self.read_back(message_id, record["turnId"])
        self.assertTrue(first["recorded"])
        self.assertFalse(again["recorded"])
        self.assertEqual(again["verified"], channel_module.HOST_READ)
        self.assertEqual(
            len(self.store.all("SELECT message_id FROM supervisor_readbacks")), 1)

    def test_nothing_is_read_back_before_it_is_delivered(self):
        _one, message_id = self.staged()
        self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.channel.read_back, message_id,
            read_turn_id="turn-1", proof="x", adapter=self.adapter)

    def test_a_readback_never_credits_the_supervisor_with_agreeing(self):
        _one, message_id, record = self.delivered()
        self.read_back(message_id, record["turnId"])
        ladder = self.channel.reach(message_id)
        for name in (envelope.AGREED, envelope.APPLIED, envelope.VERIFIED):
            self.assertEqual(ladder[name]["state"], envelope.IMPOSSIBLE)
        envelope.check_reach(envelope.PARENT_TO_SUPERVISOR, ladder)

    def test_a_verified_readback_does_not_discharge_the_reporting_obligation(self):
        """What discharges one is the Linear record the supervisor reads, confirmed."""
        one, message_id, record = self.delivered()
        self.read_back(message_id, record["turnId"])
        self.assertEqual(
            supervision.discharge_of(self.store, one)["standing"], supervision.STANDING)

    def test_the_whole_record_reads_back_as_one_answer(self):
        _one, message_id, record = self.delivered()
        self.read_back(message_id, record["turnId"])
        shown = self.channel.show(message_id)
        self.assertEqual(shown["state"], channel_module.READ)
        self.assertEqual(shown["recipient"], SUPERVISOR)
        self.assertEqual(len(shown["attempts"]), 1)
        self.assertEqual(shown["readback"]["verified"], channel_module.HOST_READ)
        self.assertEqual(shown["packet"]["version"], packets.VERSION)
        self.assertIn("Linear record", shown["limits"])


class WhatTheReviewFound(ChannelTestCase):
    """One case per finding, each asserting the behaviour the finding said was missing."""

    def test_a_real_turn_is_not_receipt_when_the_transcript_does_not_confirm_it(self):
        """RED: a valid turn used to say host_read whatever the transcript answered.

        The whole point of scanning the recipient's items is that the delivered half stops
        resting on our own send receipt. Recording the scan and then ignoring it put the claim
        back where it started, and the reach ladder said received on the strength of a turn.
        """
        _one, message_id, record = self.delivered()
        self.adapter.threads[SUPERVISOR].items = []
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["verified"], channel_module.TRANSCRIPT_UNCONFIRMED)
        self.assertFalse(answer["delivered"]["found"])
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)
        self.assertEqual(
            self.channel.reach(message_id)[envelope.RECEIVED]["state"], envelope.UNMEASURED)

    def test_an_unreadable_transcript_is_not_confirmation_either(self):
        _one, message_id, record = self.delivered()
        self.adapter.fail_reads("find_token")
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["verified"], channel_module.TRANSCRIPT_UNCONFIRMED)
        self.assertFalse(answer["delivered"]["scanned"])

    def test_a_send_interrupted_after_its_claim_is_recovered_rather_than_stranded(self):
        """RED: the claim commits before the transport call, so a death in between leaves
        sending - which is not claimable, so nothing ever touched the row again.

        The claim is called directly, because that IS the window: attempt() converts a
        transport fault into an unknown outcome and settles it, so the only way to reach this
        state is for the process to stop existing between the two. It died before its
        transport started, so the attempt has no stamp and provably sent nothing: the recovery
        records that, gives the reserved send back and queues the report again.
        """
        _one, message_id = self.staged()
        self.channel._claim(message_id, now=self.clock.now(), owner="a worker that died",
                            recipient=SUPERVISOR,
                            resolution=self.channel.resolve(self.rid))
        row = self.channel.get(message_id)
        self.assertEqual(row["state"], "sending")
        self.assertEqual(self.channel.stranded(now=row["lease_until"] + 1)[0]["message_id"],
                         message_id)

        self.channel._recover_if_stranded(row, row["lease_until"] + 1)
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)
        attempt = self.store.one(
            "SELECT state, send_attempted, retry_safe FROM supervisor_attempts"
            " WHERE message_id = ?", (message_id,))
        self.assertEqual(tuple(attempt), (WITHHELD_PRE_SEND, "no", 1))
        released = [json.loads(one["detail"]) for one in self.store.all(
            "SELECT detail FROM journal WHERE kind = 'supervisor_message_released'")]
        self.assertEqual(len(released), 1)
        self.assertIn("before the transport started", released[0]["reason"])

    def test_an_unverified_readback_cannot_replace_a_verified_one(self):
        """The two readings straddle the write, so the second one has to re-read inside it."""
        _one, message_id, record = self.delivered()
        self.assertEqual(self.read_back(message_id, record["turnId"])["verified"],
                         channel_module.HOST_READ)
        answer = self.read_back(message_id, "turn-nobody-has")
        self.assertFalse(answer["recorded"])
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(
            self.store.one("SELECT verified FROM supervisor_readbacks WHERE message_id = ?",
                           (message_id,))["verified"],
            channel_module.HOST_READ)
        self.assertTrue(
            envelope.stage_holds(self.channel.reach(message_id), envelope.RECEIVED))

    def test_naming_a_newer_message_does_not_send_it_ahead_of_an_older_one(self):
        """Ordering was a property of eligible() and of nothing else, and any caller may name
        any message."""
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        refusal = self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.channel.attempt, second, self.adapter)
        self.assertIn(first, refusal.detail)
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [])
        self.assertIsNotNone(self.channel.attempt(first, self.adapter))
        self.clock.advance(3600)
        self.assertIsNotNone(
            self.channel.attempt(second, self.adapter, now=self.clock.now()))

    def test_a_held_older_message_does_not_block_the_queue_behind_it(self):
        _one, first = self.staged()
        self.store.db.execute(
            "UPDATE supervisor_messages SET hold_reason = 'attempt_cap' WHERE message_id = ?",
            (first,))
        self.store.db.commit()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        self.assertIsNotNone(self.channel.attempt(second, self.adapter))


class TheHostRequiredCommandsRefuseWithoutOne(ChannelTestCase):
    """HOST_REQUIRED_COMMANDS is what doctor reports; it enforces nothing on its own."""

    class _Services:
        def __init__(self, channel):
            self.adapter_requested = False
            self.supervisor_channel = channel

    def test_a_send_with_no_host_refuses_instead_of_recording_a_withholding(self):
        from codex_session_relay import cli

        _one, message_id = self.staged()
        args = type("Args", (), {"message": message_id})()
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_supervisor_send(self._Services(self.channel), args)
        self.assertIn("--socket", str(caught.exception))
        self.assertEqual(self.store.all("SELECT task_id FROM recipient_lifecycle"), [])
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)

    def test_a_readback_with_no_host_refuses_instead_of_recording_an_unverified_one(self):
        from codex_session_relay import cli

        _one, message_id, record = self.delivered()
        args = type("Args", (), {"message": message_id, "turn": record["turnId"],
                                 "proof": supervisor_read_proof(message_id, record["turnId"])})()
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_supervisor_read(self._Services(self.channel), args)
        self.assertIn("--socket", str(caught.exception))
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_readbacks"), [])

    def test_both_are_declared_host_required_as_well_as_enforced(self):
        from codex_session_relay import cli

        for name in ("supervisor-send", "supervisor-read"):
            self.assertIn(name, cli.HOST_REQUIRED_COMMANDS)
        for name in ("supervisor-stage", "supervisor-show"):
            self.assertIn(name, cli.OFFLINE_COMMANDS)

    def test_a_subject_that_cannot_be_used_is_refused_rather_than_ignored(self):
        """Silently dropping an argument somebody typed is how a check goes unnoticed."""
        from codex_session_relay import cli

        services = self._Services(self.channel)
        event = type("Args", (), {"event": "abc", "project": None, "observation": ["a.json"],
                                  "recipient": None})()
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_supervisor_stage(services, event)
        self.assertIn("--observation", str(caught.exception))

        project = type("Args", (), {"event": None, "project": PROJECT, "observation": None,
                                    "recipient": "01someone"})()
        with self.assertRaises(cli.SystemExit2) as second:
            cli.cmd_supervisor_stage(services, project)
        self.assertIn("--recipient", str(second.exception))

    def test_a_standalone_observation_is_a_subject_of_its_own(self):
        from codex_session_relay import cli

        parser = cli.build_parser()
        args = parser.parse_args(["supervisor-stage", "--observation", "a.json"])
        self.assertEqual(args.observation, ["a.json"])
        self.assertIsNone(args.event)
        self.assertIsNone(args.project)

    def test_the_socket_is_global_and_belongs_before_the_subcommand(self):
        from codex_session_relay import cli

        parser = cli.build_parser()
        args = parser.parse_args(["--socket", "/tmp/s", "supervisor-send", "--message", "m"])
        self.assertEqual(args.socket, "/tmp/s")
        with self.assertRaises(SystemExit):
            parser.parse_args(["supervisor-send", "--message", "m", "--socket", "/tmp/s"])

    def test_project_staging_accepts_the_readings_only_it_can_see(self):
        """A turn that ended without reporting writes no row any query here can find."""
        from codex_session_relay import cli

        parser = cli.build_parser()
        args = parser.parse_args(
            ["supervisor-stage", "--project", PROJECT, "--observation", "a.json",
             "--observation", "b.json"])
        self.assertEqual(args.project, PROJECT)
        self.assertEqual(args.observation, ["a.json", "b.json"])


class TheReadbackRaceIsClosedInTheWrite(ChannelTestCase):
    """The guard that matters is the one inside the transaction.

    A sequential second call is answered by the precheck, so it passes whether or not the write
    is guarded - which is how the first attempt at this fix shipped in the wrong method and a
    green test said nothing. These cases make the precheck answer nothing, which is exactly what
    a caller that lost the race sees, and then assert what the write did.
    """

    def raced(self, message_id, turn_id):
        """read_back with the precheck blinded, so only the in-transaction guard can refuse."""
        original = self.channel._settled_readback
        self.channel._settled_readback = lambda _message_id: None
        try:
            return self.channel.read_back(
                message_id, read_turn_id=turn_id,
                proof=supervisor_read_proof(message_id, turn_id), adapter=self.adapter)
        finally:
            self.channel._settled_readback = original

    def test_a_verified_readback_survives_a_racing_unverified_one(self):
        _one, message_id, record = self.delivered()
        self.assertEqual(self.read_back(message_id, record["turnId"])["verified"],
                         channel_module.HOST_READ)

        answer = self.raced(message_id, "turn-nobody-has")
        self.assertFalse(answer["recorded"])
        self.assertTrue(answer.get("raced"))
        self.assertEqual(answer["verified"], channel_module.HOST_READ)

        stored = self.store.one(
            "SELECT * FROM supervisor_readbacks WHERE message_id = ?", (message_id,))
        self.assertEqual(stored["verified"], channel_module.HOST_READ)
        self.assertEqual(stored["read_turn_id"], record["turnId"])
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ,
                         "the message row and the readback row say the same thing")
        self.assertTrue(
            envelope.stage_holds(self.channel.reach(message_id), envelope.RECEIVED))

    def test_an_unsettled_row_is_still_updated_by_the_next_answer(self):
        """The guard refuses a settled row, not every row: an unverified one is replaced."""
        _one, message_id, record = self.delivered()
        self.assertEqual(self.read_back(message_id, "turn-nobody-has")["verified"],
                         channel_module.TURN_NOT_FOUND)
        answer = self.raced(message_id, record["turnId"])
        self.assertTrue(answer["recorded"])
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(
            len(self.store.all("SELECT message_id FROM supervisor_readbacks")), 1)

    def test_a_settled_message_answers_without_checking_the_proof_it_was_handed(self):
        """The fast path returns before the proof is checked, and the row says so."""
        _one, message_id, record = self.delivered()
        self.read_back(message_id, record["turnId"])
        answer = self.channel.read_back(
            message_id, read_turn_id="turn-anything", proof="not-a-proof",
            adapter=self.adapter)
        self.assertFalse(answer["recorded"])
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(answer["readTurnId"], record["turnId"])


class WhichTurnAnsweredCanBeUnknown(ChannelTestCase):
    def test_a_settled_uncertain_attempt_with_no_turn_id_leaves_the_origin_unknown(self):
        """A send nobody heard back from carries no turn id, so unknown is reachable when a
        readback settles it; nothing names which turn the transport opened."""
        _one, message_id, record = self.delivered()
        self.store.db.execute(
            "UPDATE supervisor_attempts SET state = ?, turn_id = NULL WHERE message_id = ?",
            (HELD_UNCERTAIN, message_id))
        self.store.db.execute(
            "UPDATE supervisor_messages SET state = ? WHERE message_id = ?",
            (HELD_UNCERTAIN, message_id))
        self.store.db.commit()
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["turnOrigin"], channel_module.ORIGIN_UNKNOWN)


class WhatTheMergeBoundaryReviewFound(ChannelTestCase):
    """Four findings against the pushed head, each pinned by the case that would catch it."""

    def rate_row(self, sends, now):
        window = int(now // 3600) * 3600
        self.store.db.execute(
            "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
            " VALUES (?,?,?,NULL) ON CONFLICT(recipient_task_id, window_start) DO UPDATE SET"
            " sends = excluded.sends, last_send_at = NULL",
            (SUPERVISOR, window, sends))
        self.store.db.commit()

    def test_the_hourly_bound_is_enforced_where_the_counter_moves(self):
        """_rate_limited runs before the host reads, so it is a preflight two callers pass."""
        _one, message_id = self.staged()
        now = self.clock.now()
        cap = self.channel.policy.max_sends_per_recipient_per_hour
        self.rate_row(cap, now)
        with self.assertRaises(Exception) as caught:
            self.channel._claim(message_id, now=now, owner="a racing caller",
                                recipient=SUPERVISOR,
                            resolution=self.channel.resolve(self.rid))
        self.assertIsInstance(caught.exception, channel_module._Paced)
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED,
                         "the claim rolled back, so nothing was spent")

    def test_the_last_send_inside_the_bound_is_still_allowed(self):
        _one, message_id = self.staged()
        now = self.clock.now()
        self.rate_row(self.channel.policy.max_sends_per_recipient_per_hour - 1, now)
        attempt_no, request_id, _message = self.channel._claim(
            message_id, now=now, owner="the last one in", recipient=SUPERVISOR,
                            resolution=self.channel.resolve(self.rid))
        self.assertEqual(attempt_no, 1)
        self.assertTrue(request_id)

    def test_an_older_send_in_flight_holds_the_one_behind_it(self):
        """The moment ordering matters most is while the older message is being sent."""
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        self.channel._claim(first, now=self.clock.now(), owner="in flight",
                            recipient=SUPERVISOR,
                            resolution=self.channel.resolve(self.rid))
        self.assertEqual(self.channel.get(first)["state"], "sending")
        refusal = self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.channel.attempt, second, self.adapter)
        self.assertIn(first, refusal.detail)

    def test_a_stranded_older_send_does_not_hold_it_for_ever(self):
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        self.channel._claim(first, now=self.clock.now(), owner="a worker that died",
                            recipient=SUPERVISOR,
                            resolution=self.channel.resolve(self.rid))
        expired = self.channel.get(first)["lease_until"] + 1
        self.assertIsNotNone(self.channel.attempt(second, self.adapter, now=expired))

    def test_a_read_turn_past_the_listing_bound_still_verifies(self):
        """The listing is the last 25 turns, and a recipient takes turns of its own."""
        _one, message_id, record = self.delivered()
        for _ in range(30):
            self.adapter.start_turn(SUPERVISOR, status="completed")
        self.assertNotIn(record["turnId"], self.adapter.list_turn_ids(SUPERVISOR, limit=25))
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["verified"], channel_module.HOST_READ)

    def test_a_turn_the_send_steered_rather_than_opened_does_not_verify(self):
        """A send can steer an EXISTING turn, and that turn predates the message."""
        existing = self.adapter.start_turn(SUPERVISOR, status="inProgress")
        self.clock.advance(600)
        _one, message_id = self.staged()
        self.adapter.script("steer_existing")
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["turnId"], existing.turn_id,
                         "the transport reports the turn it steered, not a new one")
        answer = self.read_back(message_id, existing.turn_id)
        self.assertEqual(answer["verified"], channel_module.TURN_PREDATES_SEND)
        self.assertEqual(answer["turnOrigin"], channel_module.RELAY_OPENED,
                         "the attempt names it, which is exactly why it cannot be exempt")
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)


class WhatTheSecondReviewRoundFound(ChannelTestCase):
    def test_a_recipient_that_recovers_releases_the_report_it_was_holding(self):
        """Archived is a state somebody can undo; a hold is not."""
        _one, message_id = self.staged()
        self.adapter.threads[SUPERVISOR].archived = True
        self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        row = self.channel.get(message_id)
        self.assertEqual(row["state"], WITHHELD_PRE_SEND)
        self.assertIsNone(row["hold_reason"],
                          "a lifecycle answer is not a bound this channel chose")

        self.adapter.threads[SUPERVISOR].archived = False
        record = self.channel.attempt(
            message_id, self.adapter, now=row["next_eligible_at"])
        self.assertIsNotNone(record, "unarchiving has to release what it stranded")
        self.assertEqual(record["deliveryState"], DISPATCHED)

    def test_a_busy_recipient_backs_off_further_each_time_and_is_finally_held(self):
        """attempt_count only moves inside the claim, which a busy recipient never reaches."""
        _one, message_id = self.staged()
        self.adapter.set_status(SUPERVISOR, "active")
        now = self.clock.now()
        waits = []
        for _ in range(3):
            self.assertIsNone(self.channel.attempt(message_id, self.adapter, now=now))
            row = self.channel.get(message_id)
            waits.append(row["next_eligible_at"] - now)
            now = row["next_eligible_at"]
        self.assertEqual(waits, sorted(waits))
        self.assertLess(waits[0], waits[-1], "a fixed interval is not a backoff")

        for _ in range(self.channel.policy.busy_max_attempts):
            row = self.channel.get(message_id)
            if row["hold_reason"]:
                break
            self.channel.attempt(message_id, self.adapter, now=row["next_eligible_at"])
        self.assertEqual(self.channel.get(message_id)["hold_reason"], "busy_cap")

    def test_a_refused_transport_is_not_reported_as_a_send(self):
        from codex_session_relay import cli

        _one, message_id = self.staged()
        self.adapter.script("read_fail")
        answer = cli.cmd_supervisor_send(
            self._sending_services(), type("Args", (), {"message": message_id})())
        self.assertTrue(answer["attempted"])
        self.assertFalse(answer["sent"], "sendAttempted no is not a delivery")
        self.assertEqual(answer["sendAttempted"], "no")
        self.assertEqual(answer["deliveryState"], WITHHELD_PRE_SEND)

    def test_a_real_send_is_reported_as_one(self):
        from codex_session_relay import cli

        _one, message_id = self.staged()
        answer = cli.cmd_supervisor_send(
            self._sending_services(), type("Args", (), {"message": message_id})())
        self.assertTrue(answer["attempted"])
        self.assertTrue(answer["sent"])
        self.assertEqual(answer["deliveryState"], DISPATCHED)

    def _sending_services(self):
        channel, adapter = self.channel, self.adapter

        class _Services:
            adapter_requested = True
            supervisor_channel = channel

            def __init__(self):
                self.adapter = adapter

        return _Services()


class WhatTheThirdReviewRoundFound(ChannelTestCase):
    def test_a_handover_committed_after_resolve_does_not_wake_the_former_supervisor(self):
        """resolve() runs before the host reads and the claim; that window is real.

        The check before the lifecycle reads cannot see a handover that commits while they
        run, so the predicate that matters is the one inside the write.
        """
        _one, message_id = self.staged()
        resolution = self.channel.resolve(self.rid)
        self.linkage.handover(
            role="supervisor", scope_key=INITIATIVE, expect_task_id=SUPERVISOR,
            endpoint=Endpoint("01new-supervisor", HOST, cwd="/new", cxc_session="cxc-new"),
            acknowledged=[], evidence="the initiative changed hands mid-send", actor="a test")
        with self.assertRaises(Exception) as caught:
            self.channel._claim(message_id, now=self.clock.now(), owner="mid-handover",
                                recipient=SUPERVISOR, resolution=resolution)
        self.assertIn("NotClaimable", type(caught.exception).__name__)
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [],
                         "the former supervisor was never resumed")

    def test_a_second_live_owner_stops_the_claim_even_when_one_of_them_matches(self):
        """A store that holds two live owners is a contest, not a resolved hierarchy."""
        _one, message_id = self.staged()
        resolution = self.channel.resolve(self.rid)
        self.store.db.execute("DROP INDEX IF EXISTS scope_bindings_one_live_owner")
        self.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
            " host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"
            " handover_note, created_at, updated_at)"
            " VALUES ('bnd-contest','supervisor','initiative',?,'01other-supervisor',?,NULL,"
            " NULL,'active',9,NULL,NULL,NULL,?,?)",
            (INITIATIVE, HOST, self.clock.iso(), self.clock.iso()))
        self.store.db.commit()
        with self.assertRaises(Exception) as caught:
            self.channel._claim(message_id, now=self.clock.now(), owner="contested",
                                recipient=SUPERVISOR, resolution=resolution)
        self.assertIn("NotClaimable", type(caught.exception).__name__)

    def test_two_messages_sharing_a_request_prefix_are_refused_by_name(self):
        """The id keeps 12 of 32 hex characters and is the attempts table's primary key."""
        _one, message_id, record = self.delivered()
        other, second = self.staged(text="a second deliverable")
        self.store.db.execute(
            "UPDATE supervisor_attempts SET request_id = ? WHERE message_id = ?",
            ("sup-" + second[:12] + "-a1", message_id))
        self.store.db.commit()
        refusal = self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.channel._claim, second,
            now=self.clock.now() + 3600, owner="colliding", recipient=SUPERVISOR,
            resolution=self.channel.resolve(self.rid))
        self.assertIn("already belongs to message", refusal.detail)

    def test_an_omission_is_refused_without_the_reading_that_found_it(self):
        """The pointer needs five selectors the obligation does not carry."""
        one = supervision.from_observation(self.observation())
        self.assertEqual(one["kind"], supervision.UNREPORTED)
        refusal = self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT, self.channel.stage, one)
        self.assertIn("marker root", refusal.detail)

    def test_an_omission_staged_with_its_reading_carries_a_runnable_pointer(self):
        reading = self.observation()
        one = supervision.from_observation(reading)
        staged = self.channel.stage(one, reading=reading)
        packet = json.loads(
            self.channel.get(staged["messageId"])["packet"])
        # The packet points at the frozen record; the command that re-reads the turn lives
        # there, whole, as the recheck.
        self.assertIn("supervisor-show", packet[packets.EVIDENCE][0])
        pointer = self.channel.show(staged["messageId"])["stagedFrom"]["recheck"]
        for flag in ("--state", "--marker-root", "--workspace", "--assignment", "--session",
                     "--turn"):
            self.assertIn(flag, pointer)
        self.assertIn("reporting-show", pointer)

    def test_a_readback_records_who_asserted_it_and_refuses_a_foreign_claim(self):
        """A declaration, not an authentication - and the difference is written down."""
        _one, message_id, record = self.delivered()
        self.assertRefused(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED, self.channel.read_back, message_id,
            read_turn_id=record["turnId"],
            proof=supervisor_read_proof(message_id, record["turnId"]),
            adapter=self.adapter, asserted_by="01someone-else")
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_readbacks"), [])

        answer = self.channel.read_back(
            message_id, read_turn_id=record["turnId"],
            proof=supervisor_read_proof(message_id, record["turnId"]),
            adapter=self.adapter, asserted_by=SUPERVISOR)
        self.assertEqual(answer["assertedBy"], SUPERVISOR)
        stored = json.loads(self.store.one(
            "SELECT detail FROM supervisor_readbacks WHERE message_id = ?",
            (message_id,))["detail"])
        self.assertEqual(stored["assertedBy"], SUPERVISOR)

    def test_an_undeclared_readback_says_so_rather_than_implying_the_recipient(self):
        _one, message_id, record = self.delivered()
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["assertedBy"], "undeclared")

    def observation(self):
        """A reporting-observation/1 reading shaped the way omitted.observe answers one."""
        return {
            "schema": "reporting-observation/1",
            "reportingState": "unreported",
            "relationshipId": self.rid,
            "reason": "the turn settled without a report",
            # Taken against this channel's own store, which staging requires of a reading.
            "selectors": {"state": self.channel.state_directory, "markerRoot": "/marker",
                          "workspace": self.root, "assignment": "asg-1",
                          "session": "01child-session", "turn": "turn-unreported-1"},
        }


class WhatTheFourthReviewRoundFound(ChannelTestCase):
    """Pacing where the counter moves, a line argparse accepts, and the turn tie."""

    def test_a_second_claim_inside_the_send_interval_is_refused_in_the_transaction(self):
        """_rate_limited reads last_send_at before the host reads, so two callers pass it.

        The first send commits its own last_send_at while the second is still deciding, and
        the second never sees it. The hourly COUNT was already re-read inside the claim; the
        interval was not, so the pacing held only when nothing raced - which is the one
        condition a bound is for.
        """
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        self.assertEqual(self.channel.attempt(first, self.adapter)["deliveryState"],
                         DISPATCHED)
        sent_at = self.clock.now()

        with self.assertRaises(Exception) as caught:
            self.channel._claim(second, now=sent_at + 1, owner="a racing caller",
                                recipient=SUPERVISOR,
                                resolution=self.channel.resolve(self.rid))
        self.assertIsInstance(caught.exception, channel_module._Paced)
        self.assertEqual(self.channel.get(second)["state"], QUEUED)
        self.assertEqual(
            self.store.one("SELECT sends FROM recipient_rate WHERE recipient_task_id = ?",
                           (SUPERVISOR,))["sends"], 1,
            "the claim rolled back, so the refused send was not counted against the hour")

    def test_the_same_claim_past_the_interval_goes_through(self):
        """The bound paces the recipient; it does not stop the queue."""
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        self.channel.attempt(first, self.adapter)
        past = self.clock.now() + self.channel.policy.min_send_interval_seconds + 1
        attempt_no, request_id, _message = self.channel._claim(
            second, now=past, owner="paced", recipient=SUPERVISOR,
            resolution=self.channel.resolve(self.rid))
        self.assertEqual(attempt_no, 1)
        self.assertTrue(request_id)

    def test_the_rendered_readback_line_is_accepted_by_the_real_parser(self):
        """A command the CLI refuses is a suggestion, not an instruction.

        The message asks the recipient to run one line. It is parsed here by the parser that
        would actually receive it, because a rendered string only LOOKS runnable.
        """
        from codex_session_relay import cli

        _one, message_id, _record = self.delivered()
        argv = self._rendered_readback(self.bytes_of(message_id))
        self.assertEqual(argv[0], "codex-session-relay")
        args = cli.build_parser().parse_args(argv[1:])
        self.assertEqual(args.command, "supervisor-read")
        self.assertTrue(args.socket, "--socket is global and has to be on the line itself")
        self.assertEqual(args.message, message_id)
        self.assertEqual(args.asserted_by, SUPERVISOR,
                         "the recipient is known when these bytes are rendered")
        self.assertEqual([token for token in argv if token.startswith("YOUR_")],
                         [channel_module.SOCKET_PLACEHOLDER, channel_module.TURN_PLACEHOLDER,
                          channel_module.PROOF_PLACEHOLDER],
                         "what is left to the reader is the socket, the turn and the proof")

    def test_a_readback_naming_the_sends_turn_with_the_token_elsewhere_does_not_verify(self):
        """Two claims about one message that cannot both be true.

        The readback names the turn the send opened, and the recipient's transcript holds the
        delivered bytes in a different one. What the token is IN is where the message landed,
        so a claim to have read it in the turn it landed in has to name that turn.
        """
        _one, message_id, record = self.delivered()
        elsewhere = self.adapter.start_turn(SUPERVISOR, status="completed")
        thread = self.adapter.threads[SUPERVISOR]
        thread.items = [(elsewhere.turn_id, text) if turn == record["turnId"] else (turn, text)
                        for turn, text in thread.items]

        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["turnOrigin"], channel_module.RELAY_OPENED)
        self.assertEqual(answer["verified"], channel_module.TRANSCRIPT_TURN_MISMATCH)
        self.assertIn(elsewhere.turn_id, answer["detail"])
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED,
                         "an inconsistent claim does not move the message to read")

    def test_a_turn_the_recipient_opened_is_not_required_to_hold_the_delivered_bytes(self):
        """The tie is the relay-opened case, and only that case.

        A recipient that reads the message and answers from a turn of its own is the STRONGER
        reading - the sender never knew that turn's id - and the delivered token is not
        expected to be in it.
        """
        _one, message_id, _record = self.delivered()
        self.clock.advance(30)
        own = self.adapter.start_turn(SUPERVISOR, status="completed")
        answer = self.read_back(message_id, own.turn_id)
        self.assertEqual(answer["turnOrigin"], channel_module.RECIPIENT_OPENED)
        self.assertEqual(answer["verified"], channel_module.HOST_READ)

    @staticmethod
    def _rendered_readback(message):
        """The invocation as it stands in the bytes, split the way a POSIX shell splits it."""
        for line in message.splitlines():
            if (line.strip().startswith("codex-session-relay ")
                    and " supervisor-read " in line):
                return shlex.split(line)
        raise AssertionError("the message carries no readback line")


SUCCESSOR = "01successor-supervisor"


class WhatTheFifthReviewRoundFound(ChannelTestCase):
    """A handover under a staged report, one gap for two senders, and quoted command lines."""

    def hand_over(self):
        self.adapter.add_thread(SUCCESSOR)
        self.linkage.handover(
            role="supervisor", scope_key=INITIATIVE, expect_task_id=SUPERVISOR,
            endpoint=Endpoint(SUCCESSOR, HOST, cwd="/successor", cxc_session="cxc-next"),
            acknowledged=[], evidence="the initiative changed hands", actor="a test")

    def reports_for(self, obligation):
        return self.store.all(
            "SELECT seq FROM journal WHERE kind = ? AND subject = ?",
            (supervision.JOURNAL_KIND, obligation["obligationId"]))

    # ------------------------------------------------------------------ the handover

    def test_a_report_staged_before_a_handover_reaches_the_successor_exactly_once(self):
        """Staged for S1, handed to S2 before any send: restaging recovers it for S2.

        The id is the fact's, so it is still one message and one supervisor_report entry. What
        moved is who it is for, and the former supervisor is never resumed.
        """
        one, message_id = self.staged()
        self.hand_over()
        refusal = self.assertRefused(
            RefusalReason.RELATION_OWNER_DRIFT, self.channel.attempt, message_id,
            self.adapter)
        self.assertIn("staging it again re-addresses it", refusal.detail)

        moved = self.channel.stage(one)
        self.assertTrue(moved["readdressed"])
        self.assertEqual(moved["to"]["recipient"], SUCCESSOR)
        self.assertEqual(moved["from"]["recipient"], SUPERVISOR)
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUCCESSOR)
        self.assertEqual(len(self.store.all("SELECT message_id FROM supervisor_messages")), 1)
        self.assertEqual(len(self.reports_for(one)), 1,
                         "one obligation is still one report after it changes hands")

        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["recipientTaskId"], SUCCESSOR)
        self.assertEqual([thread for _r, thread, _m, _o in self.adapter.sends], [SUCCESSOR])
        self.assertIn("--as " + SUCCESSOR, self.bytes_of(message_id))

        again = self.channel.stage(one)
        self.assertFalse(again["staged"])
        self.assertNotIn("readdressed", again)
        self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(len(self.adapter.sends), 1, "the successor is told once")

    def test_a_report_already_attempted_for_the_former_supervisor_stays_frozen(self):
        """Bytes that went to S1 are never re-addressed; the drift is refused by name."""
        one, message_id, _record = self.delivered()
        self.hand_over()
        refusal = self.assertRefused(
            RefusalReason.RELATION_OWNER_DRIFT, self.channel.stage, one)
        self.assertIn("never re-addressed", refusal.detail)
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUPERVISOR)
        self.assertEqual(len(self.adapter.sends), 1)
        self.assertEqual(self.store.all(
            "SELECT seq FROM journal WHERE kind = ?", (channel_module.READDRESSED,)), [])

        standing = self.channel.stage_standing(PROJECT)
        self.assertEqual([one["reason"] for one in standing["refused"]],
                         [RefusalReason.RELATION_OWNER_DRIFT.value],
                         "a project-wide staging reports the frozen report rather than hiding it")

    def test_a_claim_that_commits_first_turns_the_readdress_into_a_refusal(self):
        """The no-attempt condition is a predicate inside the write, not a check before it."""
        one, message_id = self.staged()
        before = self.channel.get(message_id)
        self.channel._claim(message_id, now=self.clock.now(), owner="first",
                            recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        self.hand_over()
        resolution = self.channel.resolve(self.rid)
        packet = self.channel.compose(one, resolution=resolution, observed_at=self.clock.iso())
        self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, self.channel._readdress,
                           before, packet, resolution)
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUPERVISOR)

    def test_what_the_former_supervisor_being_busy_decided_does_not_follow_the_report(self):
        """A busy cap is a bound about THAT task, so the successor starts from nothing."""
        one, message_id = self.staged()
        self.adapter.set_status(SUPERVISOR, "active")
        for _ in range(self.channel.policy.busy_max_attempts + 1):
            row = self.channel.get(message_id)
            if row["hold_reason"]:
                break
            self.channel.attempt(message_id, self.adapter, now=row["next_eligible_at"])
        self.assertEqual(self.channel.get(message_id)["hold_reason"], "busy_cap")

        self.hand_over()
        self.channel.stage(one)
        row = self.channel.get(message_id)
        self.assertIsNone(row["hold_reason"])
        self.assertEqual(row["state"], QUEUED)
        self.assertEqual(self.channel._deferrals(message_id), 0)
        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)

    # --------------------------------------------------------- one gap, two senders

    def queued_delivery(self, event_id):
        """The parent-child queue over the same store, holding this event."""
        from codex_session_relay.delivery import DeliveryService

        service = DeliveryService(self.store, self.registry, self.intake, self.clock)
        service.enqueue(event_id)
        return service

    def test_a_delivery_claimed_inside_the_gap_after_a_report_is_refused(self):
        """Supervisor first, delivery second: the order the delivery claim never re-read.

        The recipient passed to delivery's claim is the rate key its own attempt passes, the
        task being resumed. Naming the supervisor makes it the task that is both a parent and a
        supervisor, which is the shape a shared budget exists for.
        """
        from codex_session_relay import delivery as delivery_module

        one, message_id = self.staged()
        event_id = one["basis"]["eventId"]
        service = self.queued_delivery(event_id)
        self.channel.attempt(message_id, self.adapter)
        with self.assertRaises(delivery_module._Paced):
            service._claim(event_id, now=self.clock.now() + 1, owner="the other queue",
                           recipient=SUPERVISOR)
        self.assertEqual(service.get(event_id)["state"], QUEUED)
        self.assertEqual(self.store.all(
            "SELECT request_id FROM attempts WHERE event_id = ?", (event_id,)), [])

    def test_a_report_claimed_inside_the_gap_after_a_delivery_is_refused(self):
        """Delivery first, supervisor second."""
        one, message_id = self.staged()
        event_id = one["basis"]["eventId"]
        self.queued_delivery(event_id)._claim(event_id, now=self.clock.now(),
                                              owner="the other queue", recipient=SUPERVISOR)
        with self.assertRaises(Exception) as caught:
            self.channel._claim(message_id, now=self.clock.now() + 1, owner="second",
                                recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        self.assertIsInstance(caught.exception, channel_module._Paced)
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)

    def test_the_gap_is_read_across_the_hour_boundary(self):
        """last_send_at lives on the hour's row, so reading one hour let two sends straddle it."""
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        boundary = (int(self.clock.now() // 3600) + 1) * 3600
        self.channel.attempt(first, self.adapter, now=boundary - 1)
        with self.assertRaises(Exception) as caught:
            self.channel._claim(second, now=boundary + 1, owner="next hour",
                                recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        self.assertIsInstance(caught.exception, channel_module._Paced)

    def test_a_report_refused_inside_its_claim_is_rescheduled_by_the_gap(self):
        """Refused on the budget INSIDE the claim, through attempt(): deferred, never held.

        The preflight is blinded, which is what a caller sees when a delivery to the same
        task commits after its preflight read.
        """
        _one, message_id = self.staged()
        now = self.clock.now()
        self.store.db.execute(
            "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
            " VALUES (?,?,1,?)", (SUPERVISOR, int(now // 3600) * 3600, now))
        self.store.db.commit()
        self.channel._rate_limited = lambda recipient, at: False

        self.assertIsNone(self.channel.attempt(message_id, self.adapter, now=now + 1))
        row = self.channel.get(message_id)
        self.assertEqual(row["state"], QUEUED)
        self.assertIsNone(row["hold_reason"])
        self.assertEqual(row["attempt_count"], 0)
        self.assertEqual(row["next_eligible_at"],
                         now + 1 + self.channel.policy.min_send_interval_seconds)
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [])
        self.assertEqual(self.adapter.sends, [])

        del self.channel._rate_limited
        record = self.channel.attempt(message_id, self.adapter, now=row["next_eligible_at"])
        self.assertEqual(record["deliveryState"], DISPATCHED)

    # ------------------------------------------------------------ quoted command lines

    def test_an_omission_pointer_with_spaces_and_quotes_parses_into_its_own_values(self):
        """Concatenated, /tmp/My Project was two arguments. Quoted, it is one."""
        from codex_session_relay import cli

        selectors = {"state": "/state/a \"quoted\" dir", "markerRoot": "/marker/it's here",
                     "workspace": "/tmp/My Project", "assignment": "asg 1",
                     "session": "01child-session", "turn": "turn-unreported-1"}
        reading = {"schema": "reporting-observation/1", "reportingState": "unreported",
                   "relationshipId": self.rid, "reason": "the turn settled without a report",
                   "selectors": selectors}
        # A channel whose store is that awkward directory: staging requires the reading to be
        # taken against the store it is staged in.
        channel = self.build_channel(state_directory=selectors["state"])
        staged = channel.stage(supervision.from_observation(reading), reading=reading)
        pointer = channel.show(staged["messageId"])["stagedFrom"]["recheck"]
        argv = shlex.split(pointer)
        args = cli.build_parser().parse_args(argv[1:])
        self.assertEqual(
            (args.state, args.marker_root, args.workspace, args.assignment, args.session,
             args.turn),
            (selectors["state"], selectors["markerRoot"], selectors["workspace"],
             selectors["assignment"], selectors["session"], selectors["turn"]))

    def test_every_command_a_report_carries_is_one_the_real_parser_accepts(self):
        """The class, not the instance: each line this channel renders, split and parsed."""
        from codex_session_relay import cli

        _one, message_id, _record = self.delivered()
        packet = json.loads(self.channel.get(message_id)["packet"])
        lines = list(packet[packets.EVIDENCE])
        lines += [line.strip() for line in self.bytes_of(message_id).splitlines()
                  if line.strip().startswith("codex-session-relay ")]
        lines += [line.split("Full record: ", 1)[1]
                  for line in self.bytes_of(message_id).splitlines()
                  if line.startswith("Full record: ")]
        lines += self.channel._evidence({}, {"projectKey": "a project, spaced"})
        commands = set()
        for line in lines:
            argv = shlex.split(line)
            self.assertEqual(argv[0], "codex-session-relay", line)
            commands.add(cli.build_parser().parse_args(argv[1:]).command)
        self.assertEqual(commands, {"show", "supervisor-read", "supervisor-show",
                                    "supervisor-standing"})


class WhatTheSixthReviewRoundFound(ChannelTestCase):
    """Decisions read before the write lock and acted on under it, at staging and readback."""

    hand_over = WhatTheFifthReviewRoundFound.hand_over
    reports_for = WhatTheFifthReviewRoundFound.reports_for

    def reconciliations(self):
        return self.store.all("SELECT detail FROM journal WHERE kind = ?",
                              ("supervisor_message_reconciled",))

    def lost_response(self):
        """A send the host carried out and whose answer never came back."""
        _one, message_id = self.staged()
        self.adapter.script("transport_unknown")
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], HELD_UNCERTAIN)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)
        self.clock.advance(1)
        return message_id, record

    # ------------------------------------------------------------ uncertain sends

    def test_a_lost_response_is_settled_by_a_readback_proving_this_attempt_arrived(self):
        message_id, record = self.lost_response()
        landed = self.adapter.start_turn(SUPERVISOR, status="completed",
                                         text=self.bytes_of(message_id))
        answer = self.read_back(message_id, landed.turn_id)
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(answer["reconciled"]["requestId"], record["requestId"])
        self.assertEqual(answer["reconciled"]["from"], HELD_UNCERTAIN)
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ)
        self.assertEqual(len(self.reconciliations()), 1, "how it was settled is recorded")
        self.assertEqual(len(self.adapter.sends), 1, "settling it sent nothing")

    def test_an_uncertain_send_is_never_settled_by_the_answer_alone(self):
        """A real turn and a valid proof, and no trace of this attempt in the transcript."""
        message_id, _record = self.lost_response()
        own = self.adapter.start_turn(SUPERVISOR, status="completed")
        answer = self.read_back(message_id, own.turn_id)
        self.assertEqual(answer["verified"], channel_module.TRANSCRIPT_UNCONFIRMED)
        self.assertIsNone(answer["reconciled"])
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)
        self.assertEqual(self.store.one(
            "SELECT verified FROM supervisor_readbacks WHERE message_id = ?",
            (message_id,))["verified"], channel_module.TRANSCRIPT_UNCONFIRMED)
        self.assertEqual(self.reconciliations(), [])

    def test_a_send_whose_worker_died_is_recovered_and_settled_by_its_readback(self):
        """Claimed, the bytes landed, the worker died before the receipt, the lease ran out."""
        _one, message_id = self.staged()
        request_id, landed = self.died_after_delivering(message_id)
        self.clock.advance(self.channel.policy.lease_seconds + 1)
        answer = self.read_back(message_id, landed)
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(answer["reconciled"]["requestId"], request_id)
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ)

    def test_a_late_receipt_does_not_drag_a_settled_message_back(self):
        """The slow sender's own receipt is recorded; the message it no longer owns is not moved."""
        _one, message_id = self.staged()
        request_id, landed = self.died_after_delivering(message_id)
        self.clock.advance(self.channel.policy.lease_seconds + 1)
        self.read_back(message_id, landed)
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ)

        receipt = {"requestId": request_id, "operation": "send_message_to_thread",
                   "status": "accepted", "threadId": SUPERVISOR, "retrySafe": False,
                   "resumed": {"approvalPolicy": "never"}, "turnId": landed}
        self.channel._settle(message_id, 1, "relay", request_id,
                             classify_operation_receipt(receipt), {"requestId": request_id},
                             self.clock.now())
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ)
        self.assertEqual(self.store.one(
            "SELECT state FROM supervisor_attempts WHERE request_id = ?",
            (request_id,))["state"], DISPATCHED, "the receipt itself is still recorded")

    def test_a_message_that_moves_during_the_checks_records_nothing(self):
        """The checks run outside the lock; the write asks whether they still describe the row."""
        _one, message_id, record = self.delivered()
        scan = self.channel._delivered_evidence

        def moved_meanwhile(row, attempt, adapter):
            found = scan(row, attempt, adapter)
            self.store.db.execute(
                "UPDATE supervisor_messages SET attempt_count = attempt_count + 1"
                " WHERE message_id = ?", (message_id,))
            self.store.db.commit()
            return found

        self.channel._delivered_evidence = moved_meanwhile
        refusal = self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.read_back, message_id, record["turnId"])
        self.assertIn("while this readback was being checked", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_readbacks"), [])

    # ------------------------------------------------------------------- staging

    def test_a_report_recorded_between_the_reading_and_the_write_is_not_staged(self):
        """supervisor-report-recorded commits after every read this caller takes, before its lock.

        Modelled by committing just before the staging transaction opens, which is the last
        moment a concurrent writer can land; a decision read any earlier misses it.
        """
        one = self.obligation()
        with self.committed_before_the_lock(lambda: supervision.record_report(
                self.store, one, at=self.clock.iso(),
                note="a concurrent supervisor-report-recorded")):
            refusal = self.assertRefused(RefusalReason.NOT_CLAIMABLE, self.channel.stage, one)
        self.assertIn("decided under the staging write lock", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        self.assertEqual(len(self.reports_for(one)), 1)

    def test_a_stage_from_the_former_hierarchy_landing_first_is_readdressed(self):
        """Staged for S1 by a caller who read the hierarchy before the handover, committed
        just before this caller's lock: this caller re-addresses it rather than refusing."""
        one, message_id = self.staged()
        frozen = dict(self.channel.get(message_id))
        self.store.db.execute("DELETE FROM supervisor_messages WHERE message_id = ?",
                              (message_id,))
        self.store.db.commit()
        self.hand_over()
        columns = ", ".join(frozen)

        def former_stage_commits():
            self.store.db.execute(
                "INSERT INTO supervisor_messages (" + columns + ") VALUES ("
                + ", ".join("?" for _ in frozen) + ")", tuple(frozen.values()))
            self.store.db.commit()

        with self.committed_before_the_lock(former_stage_commits):
            answer = self.channel.stage(one)
        self.assertTrue(answer["readdressed"])
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUCCESSOR)
        self.assertEqual(len(self.reports_for(one)), 1)

    # ------------------------------------------------------------ the reschedule

    def test_a_stale_reschedule_does_not_clear_a_hold_another_caller_set(self):
        _one, message_id = self.staged()
        stale = self.channel.get(message_id)
        self.store.db.execute(
            "UPDATE supervisor_messages SET state = ?, hold_reason = ? WHERE message_id = ?",
            (DEFERRED_BUSY, "busy_cap", message_id))
        self.store.db.commit()
        self.channel._reschedule(stale, self.clock.now() + 5)
        row = self.channel.get(message_id)
        self.assertEqual((row["state"], row["hold_reason"]), (DEFERRED_BUSY, "busy_cap"))

    def test_a_stale_reschedule_does_not_put_the_former_recipients_delay_back(self):
        one, message_id = self.staged()
        stale = self.channel.get(message_id)
        self.hand_over()
        self.channel.stage(one)
        self.channel._reschedule(stale, self.clock.now() + 600)
        row = self.channel.get(message_id)
        self.assertEqual(row["recipient_task_id"], SUCCESSOR)
        self.assertIsNone(row["next_eligible_at"])

    # ------------------------------------------------------------------ the cap

    def test_an_hourly_cap_of_zero_refuses_the_first_send(self):
        """No row yet means nothing spent, which is still not below a cap of nothing."""
        from codex_session_relay.policy import RetryPolicy

        channel = self.build_channel(policy=RetryPolicy(max_sends_per_recipient_per_hour=0))
        _one, message_id = self.staged()
        with self.assertRaises(channel_module._Paced):
            channel._claim(message_id, now=self.clock.now(), owner="capped",
                           recipient=SUPERVISOR, resolution=channel.resolve(self.rid))
        self.assertEqual(self.store.all("SELECT * FROM recipient_rate"), [])
        self.assertIsNone(channel.attempt(message_id, self.adapter))
        self.assertEqual(self.adapter.sends, [])

    def test_a_send_dated_ahead_by_a_fast_clock_does_not_stall_the_recipient(self):
        """The gap reads the windows it can reach, and a later window is not one of them."""
        from codex_session_relay.delivery import send_refusal

        now = self.clock.now()
        ahead = now + 86400
        self.store.db.execute(
            "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
            " VALUES (?,?,1,?)", (SUPERVISOR, int(ahead // 3600) * 3600, ahead))
        self.store.db.commit()
        self.assertIsNone(send_refusal(self.store.db, self.channel.policy, SUPERVISOR, now))

    # ------------------------------------------------------------ chronology

    def test_a_turn_opened_between_the_claim_and_the_transport_does_not_verify(self):
        """The send is stamped when the transport is called, not when the row was claimed.

        The recipient opens a turn in between and the transport steers the message into it,
        so the token IS in the named turn and only the send time can refuse it.
        """
        _one, message_id = self.staged()
        claim = self.channel._claim
        opened = {}

        def claim_then_a_gap(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            self.clock.advance(5)
            opened["turn"] = self.adapter.start_turn(SUPERVISOR, status="inProgress")
            self.clock.advance(5)
            return claimed

        self.adapter.script("steer_existing")
        with mock.patch.object(self.channel, "_claim", claim_then_a_gap):
            record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["turnId"], opened["turn"].turn_id)
        answer = self.read_back(message_id, opened["turn"].turn_id)
        self.assertEqual(answer["verified"], channel_module.TURN_PREDATES_SEND)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_a_turn_opened_while_the_transport_queued_the_message_does_not_verify(self):
        """Called at one moment, landed at a later one: the named turn has to follow the LANDING."""
        _one, message_id = self.staged()
        deliver = self.adapter.send_message
        opened = {}

        def queued(request_id, thread_id, message, settings=None):
            self.clock.advance(5)
            opened["turn"] = self.adapter.start_turn(SUPERVISOR, status="completed")
            self.clock.advance(5)
            return deliver(request_id, thread_id, message, settings)

        with mock.patch.object(self.adapter, "send_message", queued):
            record = self.channel.attempt(message_id, self.adapter)
        self.assertNotEqual(record["turnId"], opened["turn"].turn_id)
        answer = self.read_back(message_id, opened["turn"].turn_id)
        self.assertEqual(answer["verified"], channel_module.TURN_PREDATES_SEND)
        self.assertIn(record["turnId"], answer["detail"])
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    @contextmanager
    def committed_before_the_lock(self, concurrent_write):
        """Run a concurrent writer's commit immediately before the next staging lock, once."""
        real = self.store.composing
        pending = [concurrent_write]

        @contextmanager
        def composing():
            if pending:
                pending.pop()()
            with real() as db:
                yield db

        with mock.patch.object(self.store, "composing", composing):
            yield

    def died_after_delivering(self, message_id):
        """attempt() whose host takes the bytes and whose worker dies before the receipt.

        The transport was called, so its start is stamped; the receipt was never recorded, so
        the row is left in sending for its lease to lapse. Returns the request id and the turn
        the bytes landed in.
        """
        deliver = self.adapter.send_message

        def delivered_then_died(request_id, thread_id, message, settings=None):
            deliver(request_id, thread_id, message, settings)
            raise WorkerKilled("the worker died after the host took the message")

        with mock.patch.object(self.adapter, "send_message", delivered_then_died):
            with self.assertRaises(WorkerKilled):
                self.channel.attempt(message_id, self.adapter)
        attempt = self.store.one(
            "SELECT request_id, transport_started_at FROM supervisor_attempts"
            " WHERE message_id = ?", (message_id,))
        self.assertIsNotNone(attempt["transport_started_at"])
        self.assertEqual(self.channel.get(message_id)["state"], "sending")
        return attempt["request_id"], self.adapter.threads[SUPERVISOR].turns[-1].turn_id


class WhatTheSeventhReviewRoundFound(ChannelTestCase):
    """A readback verifies only on evidence bound to THIS attempt's transport and turns, and a
    settled one is answered in the shape of the first; the hierarchy is read under each lock."""

    hand_over = WhatTheFifthReviewRoundFound.hand_over
    committed_before_the_lock = WhatTheSixthReviewRoundFound.committed_before_the_lock
    died_after_delivering = WhatTheSixthReviewRoundFound.died_after_delivering

    # ------------------------------------------------------ the hierarchy under the lock

    def test_a_handover_between_resolving_and_the_lock_stages_nothing_for_the_former(self):
        one = self.obligation()
        with self.committed_before_the_lock(self.hand_over):
            refusal = self.assertRefused(
                RefusalReason.RELATION_OWNER_DRIFT, self.channel.stage, one)
        self.assertIn("under the write lock", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        again = self.channel.stage(one)
        self.assertEqual(again["recipient"], SUCCESSOR)

    def test_a_stale_readdress_does_not_move_a_successors_report_back(self):
        one, message_id = self.staged()
        stale = self.channel.resolve(self.rid)
        stale_packet = self.channel.compose(one, resolution=stale, observed_at=self.clock.iso())
        self.hand_over()
        self.channel.stage(one)
        current = self.channel.get(message_id)
        self.assertEqual(current["recipient_task_id"], SUCCESSOR)
        self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, self.channel._readdress,
                           current, stale_packet, stale)
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUCCESSOR)

    # --------------------------------------------------- evidence bound to the attempt

    def test_a_token_in_no_named_turn_does_not_verify(self):
        """Found, and the host would not say in which turn: tied to nothing, so not verified."""
        _one, message_id, record = self.delivered()
        token = record["requestId"]
        thread = self.adapter.threads[SUPERVISOR]
        thread.items = [(None, text) if token in text else (turn, text)
                        for turn, text in thread.items]
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["verified"], channel_module.TRANSCRIPT_UNCONFIRMED)
        self.assertIn("did not say which turn", answer["detail"])
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_an_attempt_whose_transport_never_started_does_not_verify(self):
        """Claimed and never sent: there is no send to measure a turn against.

        The verification rule refuses such an attempt whatever the transcript holds, and is
        checked directly. Through read_back the case no longer arises: the recovery queues a
        message whose attempt never started (I-241), so there is nothing to read back.
        """
        _one, message_id = self.staged()
        _n, request_id, message = self.channel._claim(
            message_id, now=self.clock.now(), owner="died before sending",
            recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        landed = self.adapter.start_turn(SUPERVISOR, status="completed", text=message)
        attempt = self.store.one(
            "SELECT * FROM supervisor_attempts WHERE request_id = ?", (request_id,))
        verified, detail, _origin, _turn = self.channel._verify_read_turn(
            self.channel.get(message_id), attempt, landed.turn_id, self.adapter)
        self.assertEqual(verified, channel_module.NO_HOST)
        self.assertIn("no recorded transport start", detail)

        self.clock.advance(self.channel.policy.lease_seconds + 1)
        self.assertRefused(RefusalReason.NOT_CLAIMABLE, self.read_back, message_id,
                           landed.turn_id)
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)

    # ------------------------------------------------------------- the answer's shape

    def test_every_answer_to_one_readback_has_the_shape_of_the_first(self):
        """Written, answered from the settled row, and lost to a racing settle: one fact."""
        message_id, _record = self.lost_response()
        landed = self.adapter.start_turn(SUPERVISOR, status="completed",
                                         text=self.bytes_of(message_id))
        first = self.read_back(message_id, landed.turn_id)
        settled = self.read_back(message_id, landed.turn_id)
        with mock.patch.object(self.channel, "_settled_readback", lambda message: None):
            raced = self.read_back(message_id, landed.turn_id)
        self.assertEqual((first["recorded"], settled["recorded"], raced["recorded"]),
                         (True, False, False))
        self.assertEqual((first["raced"], settled["raced"], raced["raced"]),
                         (False, False, True))
        for later in (settled, raced):
            self.assertEqual(sorted(later), sorted(first))
            for key in first:
                if key not in ("recorded", "raced"):
                    self.assertEqual(later[key], first[key], key)
        self.assertIsInstance(settled["detail"], str)
        self.assertEqual(settled["reconciled"]["requestId"], _record["requestId"])

    lost_response = WhatTheSixthReviewRoundFound.lost_response


class WhatTheThirdIndependentReviewFound(ChannelTestCase):
    """A report goes to whoever supervises at its transport instant, and a time that is not a
    time establishes nothing."""

    hand_over = WhatTheFifthReviewRoundFound.hand_over

    # ------------------------------------------------ the handover and the transport instant

    def test_a_handover_after_the_claim_and_before_the_transport_sends_nothing(self):
        """The transport-start write is the instant; the hierarchy is asked there again."""
        one, message_id = self.staged()
        claim = self.channel._claim

        def claim_then_hand_over(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            self.hand_over()
            return claimed

        with mock.patch.object(self.channel, "_claim", claim_then_hand_over):
            refusal = self.assertRefused(
                RefusalReason.RELATION_OWNER_DRIFT, self.channel.attempt, message_id,
                self.adapter)
        self.assertIn("Nothing was sent", refusal.detail)
        self.assertEqual(self.adapter.sends, [])
        attempt = self.store.one(
            "SELECT send_attempted, retry_safe, transport_started_at FROM supervisor_attempts"
            " WHERE message_id = ?", (message_id,))
        self.assertEqual(tuple(attempt), ("no", 1, None))

        self.assertTrue(self.channel.stage(one)["readdressed"])
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["recipientTaskId"], SUCCESSOR)
        self.assertEqual([thread for _r, thread, _m, _o in self.adapter.sends], [SUCCESSOR])

    def test_a_handover_after_the_transport_started_leaves_the_report_where_it_went(self):
        """Live at the transport instant is who it was for; the successor is told of the drift."""
        one, message_id = self.staged()
        deliver = self.adapter.send_message

        def hand_over_mid_send(request_id, thread_id, message, settings=None):
            self.hand_over()
            return deliver(request_id, thread_id, message, settings)

        with mock.patch.object(self.adapter, "send_message", hand_over_mid_send):
            record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["recipientTaskId"], SUPERVISOR)
        refusal = self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, self.channel.stage, one)
        self.assertIn("live when its transport started", refusal.detail)

    def test_a_report_the_transport_refused_to_send_follows_a_handover(self):
        """sendAttempted no and retry-safe put the bytes nowhere, so the report can move."""
        one, message_id = self.staged()
        self.adapter.script("read_fail")
        self.assertEqual(self.channel.attempt(message_id, self.adapter)["sendAttempted"], "no")
        self.hand_over()
        self.assertTrue(self.channel.stage(one)["readdressed"])
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUCCESSOR)

    # ----------------------------------------------------------- times that are not times

    def test_a_turn_start_that_is_not_a_time_does_not_verify(self):
        from codex_session_relay.hostadapter import TurnInfo

        _one, message_id, record = self.delivered()
        real = self.adapter.read_turn
        for bad in ("not-a-timestamp", float("nan"), float("inf"), True, None):
            with self.subTest(start=bad):
                def named_turn(thread, turn, bad=bad):
                    if turn == record["turnId"]:
                        return TurnInfo(turn, "completed", bad)
                    return real(thread, turn)

                with mock.patch.object(self.adapter, "read_turn", named_turn):
                    answer = self.read_back(message_id, record["turnId"])
                self.assertEqual(answer["verified"], channel_module.NO_HOST)
                self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_a_landing_turn_whose_start_is_not_a_time_does_not_verify(self):
        """A turn the recipient opened is measured against where the token landed, too."""
        from codex_session_relay.hostadapter import TurnInfo

        _one, message_id, record = self.delivered()
        self.clock.advance(30)
        own = self.adapter.start_turn(SUPERVISOR, status="completed")
        real = self.adapter.read_turn

        def landing_turn(thread, turn):
            if turn == record["turnId"]:
                return TurnInfo(turn, "completed", float("nan"))
            return real(thread, turn)

        with mock.patch.object(self.adapter, "read_turn", landing_turn):
            answer = self.read_back(message_id, own.turn_id)
        self.assertEqual(answer["turnOrigin"], channel_module.RECIPIENT_OPENED)
        self.assertEqual(answer["verified"], channel_module.NO_HOST)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)


class WhatTheEighthReviewRoundFound(ChannelTestCase):
    """A staged packet and the evidence it points at are one fact, bound at staging and
    immutable after it; and a claim is for the endpoint its caller will send to."""

    hand_over = WhatTheFifthReviewRoundFound.hand_over
    committed_before_the_lock = WhatTheSixthReviewRoundFound.committed_before_the_lock

    def report_naming(self, event_id, number, *, submission_no=1):
        from .test_report_contract import a_handoff

        head = "a1b2c3d4e5f60718293a4b5c6d7e8f90123456" + str(number).zfill(2)
        return report_module.record(
            self.store, self.clock, event_id=event_id,
            repository="thisisjun786/codex-relay-workflow", pr_number=number,
            pr_url="https://github.com/thisisjun786/codex-relay-workflow/pull/" + str(number),
            pr_state="ready", base_ref="dev",
            base_sha="c56576d5be412b5bc352dd93b9eb37ab279a12f6",
            head_sha=head, handoff=a_handoff(head),
            cxc_status="DONE", cxc_reason="every recorded criterion was proved",
            summary="the work is done", next_action="merge",
            evidence=["pytest tests/test_supervisor_channel.py passed"],
            submission_no=submission_no)

    def completion_naming(self, number):
        path = self.artifact("out.txt", "the deliverable of pull request " + str(number))
        payload = self.ready_payload(self.relationship, [path])
        self.accept(payload)
        self.report_naming(payload["eventId"], number)
        return payload["eventId"]

    def packet_pr(self, message_id):
        return json.loads(self.channel.get(message_id)["packet"])[packets.ARTIFACT]["number"]

    # ------------------------------------------------------- a corrected work report

    def corrected_before_the_send(self, submission):
        event_id = self.completion_naming(10)
        message_id = self.channel.stage(self.obligation(event_id))["messageId"]
        self.assertEqual(self.packet_pr(message_id), 10)
        from codex_session_relay.errors import ReceiptRefused

        try:
            self.report_naming(event_id, 11, submission_no=submission)
        except ReceiptRefused as refused:
            self.fail("a correction was refused although nothing had been sent: "
                      + refused.detail)
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.packet_pr(message_id), 11)
        self.assertIn("codex-relay-workflow #11 at", self.bytes_of(message_id))
        self.assertEqual(report_module.read(self.store, event_id)["prNumber"], 11)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_staged_report_corrected_in_place_before_the_send_goes_up_corrected(self):
        """Review 14: the freeze refused a correction to a report nothing had sent yet, so the
        packet went up naming #10. A staged row is a proposal (I-247): the correction lands, and
        the send carries it - never bytes naming #10 whose evidence reads #11."""
        self.corrected_before_the_send(1)

    def test_a_staged_report_resubmitted_before_the_send_goes_up_resubmitted(self):
        """The same, as a new submission rather than a correction in place."""
        self.corrected_before_the_send(2)

    def test_a_report_sent_upward_can_no_longer_be_corrected(self):
        """Once the transport started for a packet naming #10, neither an in-place correction nor
        a new submission to #11 lands."""
        from codex_session_relay.errors import ReceiptRefused

        event_id = self.completion_naming(10)
        message_id = self.channel.stage(self.obligation(event_id))["messageId"]
        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)
        for submission in (1, 2):
            with self.subTest(submission=submission):
                with self.assertRaises(ReceiptRefused) as caught:
                    self.report_naming(event_id, 11, submission_no=submission)
                self.assertIn("was sent", caught.exception.detail)
        self.assertEqual(report_module.read(self.store, event_id)["prNumber"], 10)
        self.assertEqual(self.packet_pr(message_id), 10)

    def test_a_correction_landing_before_the_staging_lock_refuses_the_stale_packet(self):
        """The packet was composed from #10 and #11 committed first: nothing staged naming #10."""
        event_id = self.completion_naming(10)
        one = self.obligation(event_id)
        with self.committed_before_the_lock(
                lambda: self.report_naming(event_id, 11, submission_no=2)):
            refusal = self.assertRefused(
                RefusalReason.SUPERSEDED_REVISION, self.channel.stage, one)
        self.assertIn("changed while this was being staged", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        staged = self.channel.stage(self.obligation(event_id))
        self.assertEqual(self.packet_pr(staged["messageId"]), 11)
        self.assertEqual(self.channel.get(staged["messageId"])["submission_no"], 2)

    # ------------------------------------------------------------ omission evidence

    def test_an_omission_is_refused_with_a_reading_that_is_not_its_own(self):
        base = WhatTheThirdReviewRoundFound.observation(self)
        one = supervision.from_observation(base)
        wrong = {
            "schema": dict(base, schema="reporting-observation/0"),
            "state": dict(base, reportingState="reported"),
            "relationship": dict(base, relationshipId="rel-someone-else"),
            "turn": dict(base, selectors=dict(base["selectors"], turn="turn-another")),
        }
        for name, reading in wrong.items():
            with self.subTest(mismatch=name):
                refusal = self.assertRefused(
                    RefusalReason.CONTRADICTORY_OBSERVATION, self.channel.stage, one,
                    reading=reading)
                self.assertIn("contradicts it", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        self.assertEqual(self.store.all(
            "SELECT seq FROM journal WHERE kind = ?", (supervision.JOURNAL_KIND,)), [])
        self.assertTrue(self.channel.stage(one, reading=base)["staged"])

    # --------------------------------------------------- the claim and its send target

    def test_a_readdress_between_the_reads_and_the_claim_sends_to_nobody_stale(self):
        """Observed S1, re-addressed to S2 before the claim: no claim, and the next try goes to S2."""
        one, message_id = self.staged()
        settings = self.channel._settings_for

        def settings_then_a_handover(task_id, runtime_status=None):
            answer = settings(task_id, runtime_status)
            if not self.adapter.threads.get(SUCCESSOR):
                self.hand_over()
                self.channel.stage(one)
            return answer

        with mock.patch.object(self.channel, "_settings_for", settings_then_a_handover):
            self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.channel.get(message_id)["recipient_task_id"], SUCCESSOR)
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["recipientTaskId"], SUCCESSOR)
        self.assertEqual([thread for _r, thread, _m, _o in self.adapter.sends], [SUCCESSOR])
        self.assertIn("--as " + SUCCESSOR, self.bytes_of(message_id))

    def test_the_full_record_shows_when_each_transport_started(self):
        _one, message_id, _record = self.delivered()
        shown = self.channel.show(message_id)["attempts"][0]
        self.assertIsNotNone(shown["transportStartedAt"])


class ASettlementBelongsToTheClaimThatMadeIt(ChannelTestCase):
    """A message leaves sending only through the claim that holds it: this message, this attempt
    and this lease owner. Once recovery has declared an attempt uncertain, nothing its late
    receipt says moves the message; the receipt is recorded on the attempt, and only a verified
    readback settles the message."""

    def outlived_by_its_lease(self, message_id):
        """attempt() whose transport outlives its lease, recovered before its receipt returns.

        The transport is the real fake host, so whatever it was scripted to answer is what the
        late receipt says. Recovery runs where a second worker would run it: after the send
        started and before its answer is recorded.
        """
        deliver = self.adapter.send_message

        def recovered_while_sending(request_id, thread_id, message, settings=None):
            answer = deliver(request_id, thread_id, message, settings)
            self.clock.advance(self.channel.policy.lease_seconds + 1)
            recovered = self.channel._recover_if_stranded(
                self.channel.get(message_id), self.clock.now())
            self.assertEqual(recovered["state"], HELD_UNCERTAIN)
            return answer

        with mock.patch.object(self.adapter, "send_message", recovered_while_sending):
            return self.channel.attempt(message_id, self.adapter)

    def attempt_rows(self, message_id):
        return [tuple(row) for row in self.store.all(
            "SELECT attempt_no, state, send_attempted FROM supervisor_attempts"
            " WHERE message_id = ? ORDER BY attempt_no", (message_id,))]

    def test_a_late_refusal_after_recovery_does_not_reopen_the_send(self):
        """RED: recovery declared attempt 1 unknown, and its retry-safe receipt made it claimable."""
        _one, message_id = self.staged()
        self.adapter.script("read_fail")
        record = self.outlived_by_its_lease(message_id)
        self.assertEqual(record["deliveryState"], WITHHELD_PRE_SEND)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN,
                         "a late receipt does not undo the recovery's declaration")
        self.assertEqual(record["messageState"], HELD_UNCERTAIN)
        self.assertEqual(self.attempt_rows(message_id), [(1, WITHHELD_PRE_SEND, "no")],
                         "the receipt is still recorded on its own attempt")
        journal = json.loads(self.store.one(
            "SELECT detail FROM journal WHERE kind = 'supervisor_message_attempted'")["detail"])
        self.assertFalse(journal["messageMoved"])
        self.assertEqual(journal["messageState"], HELD_UNCERTAIN)

        self.clock.advance(3600)
        self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(len(self.attempt_rows(message_id)), 1,
                         "no second claim, so no second wake for one fact")

    def test_a_late_success_after_recovery_is_not_a_silent_promotion(self):
        """RED: the late dispatched receipt moved held_uncertain to dispatched by itself."""
        _one, message_id = self.staged()
        record = self.outlived_by_its_lease(message_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN,
                         "only a verified readback settles an attempt declared uncertain")
        self.assertEqual(self.attempt_rows(message_id), [(1, DISPATCHED, "yes")])

        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(answer["reconciled"]["requestId"], record["requestId"])
        self.assertEqual(self.channel.get(message_id)["state"], channel_module.READ)

    def test_the_claim_that_holds_the_message_settles_it(self):
        """Positive control: the owning, unexpired sender settles normally."""
        _one, message_id, record = self.delivered()
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)

    def test_an_expired_lease_nobody_recovered_still_belongs_to_its_claim(self):
        """Positive control: expiry makes a claim recoverable; recovery, not the clock, ends it."""
        _one, message_id = self.staged()
        deliver = self.adapter.send_message

        def slow(request_id, thread_id, message, settings=None):
            answer = deliver(request_id, thread_id, message, settings)
            self.clock.advance(self.channel.policy.lease_seconds + 1)
            return answer

        with mock.patch.object(self.adapter, "send_message", slow):
            record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.channel.get(message_id)["state"], DISPATCHED)


class WhatTheFifthIndependentReviewFound(ChannelTestCase):
    """The hierarchy a claim sends under is decided by resolve() inside the claim's own write, and
    a staged packet is composed from exactly the fact its evidence reads."""

    report_naming = WhatTheEighthReviewRoundFound.report_naming
    completion_naming = WhatTheEighthReviewRoundFound.completion_naming
    packet_pr = WhatTheEighthReviewRoundFound.packet_pr
    committed_before_the_lock = WhatTheSixthReviewRoundFound.committed_before_the_lock
    observation = WhatTheThirdReviewRoundFound.observation

    # --------------------------------------------------------- the hierarchy at the claim

    def test_an_assignment_archived_after_the_preflight_is_not_sent(self):
        """RED: the claim compared the two owner bindings, which archiving leaves in place."""
        _one, message_id = self.staged()
        settings = self.channel._settings_for

        def settings_then_archived(task_id, runtime_status=None):
            answer = settings(task_id, runtime_status)
            if self.registry.get(self.rid)["status"] != "archived":
                self.registry.set_status(self.rid, "archived", actor="a test")
            return answer

        with mock.patch.object(self.channel, "_settings_for", settings_then_archived):
            self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.store.all("SELECT request_id FROM supervisor_attempts"), [])
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)

    def test_an_assignment_archived_between_the_claim_and_the_transport_sends_nothing(self):
        _one, message_id = self.staged()
        claim = self.channel._claim

        def claim_then_archive(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            self.registry.set_status(self.rid, "archived", actor="a test")
            return claimed

        with mock.patch.object(self.channel, "_claim", claim_then_archive):
            with self.assertRaises(DeliveryRefused) as caught:
                self.channel.attempt(message_id, self.adapter)
        self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_SCOPE,
                         "the refusal resolve() gives, carried rather than renamed")
        self.assertIn("Nothing was sent", caught.exception.detail)
        self.assertEqual(self.adapter.sends, [])
        attempt = self.store.one(
            "SELECT send_attempted, retry_safe, transport_started_at FROM supervisor_attempts"
            " WHERE message_id = ?", (message_id,))
        self.assertEqual(tuple(attempt), ("no", 1, None))
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)

    # ------------------------------------------------ the packet and the fact it describes

    def test_a_correction_in_place_before_the_staging_lock_refuses_the_stale_packet(self):
        """RED: the lock compared submission numbers, and an in-place correction keeps its number."""
        event_id = self.completion_naming(10)
        one = self.obligation(event_id)
        with self.committed_before_the_lock(
                lambda: self.report_naming(event_id, 11, submission_no=1)):
            refusal = self.assertRefused(
                RefusalReason.SUPERSEDED_REVISION, self.channel.stage, one)
        self.assertIn("changed while this was being staged", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        staged = self.channel.stage(self.obligation(event_id))
        self.assertEqual(self.packet_pr(staged["messageId"]), 11)

    def test_an_obligation_its_caller_altered_is_refused(self):
        """RED: only the id and the detail were compared, and the packet is composed from the copy."""
        event_id = self.completion_naming(10)
        for field, value in (("executionGeneration", 99), ("issueKey", "OTHER-1")):
            with self.subTest(field=field):
                altered = dict(self.obligation(event_id), **{field: value})
                refusal = self.assertRefused(
                    RefusalReason.CONTRADICTORY_OBSERVATION, self.channel.stage, altered)
                self.assertIn(field, refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        self.assertEqual(self.store.all(
            "SELECT seq FROM journal WHERE kind = ?", (supervision.JOURNAL_KIND,)), [])

    def test_two_generations_of_one_omission_are_a_contradiction_not_a_choice(self):
        """RED: the obligation came from the first reading and the evidence from the last."""
        base = self.observation()
        first = dict(base, executionGeneration=1)
        second = dict(base, executionGeneration=2,
                      selectors=dict(base["selectors"], assignment="asg-2"))
        answer = self.channel.stage_standing(PROJECT, observations=[first, second])
        self.assertEqual(answer["staged"], [])
        self.assertEqual([one["reason"] for one in answer["refused"]],
                         [RefusalReason.CONTRADICTORY_OBSERVATION.value])
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])

        refusal = self.assertRefused(
            RefusalReason.CONTRADICTORY_OBSERVATION, self.channel.stage,
            supervision.from_observation(first), reading=second)
        self.assertIn("executionGeneration", refusal.detail)

    def test_the_same_omission_read_twice_is_staged_once(self):
        """Positive control: readings that agree are one reading."""
        first = dict(self.observation(), executionGeneration=1)
        answer = self.channel.stage_standing(PROJECT, observations=[first, dict(first)])
        self.assertEqual(len(answer["staged"]), 1)
        self.assertEqual(answer["refused"], [])


class AParentThatNeverReports(ChannelTestCase):
    """The negative control CRW-148 names: the parent omits the report on purpose.

    The obligation is derived from the event row the child's final receipt wrote, so nothing
    the parent does is needed for it to exist, and nothing the parent fails to do makes it go
    away. This is the reading side. The sending side - the daemon staging and sending what an
    omitting parent owes, and an omitting child's report derived from the store - is
    test_supervisor_autosend and test_supervisor_omission_store.
    """

    def standing(self):
        from argparse import Namespace

        from codex_session_relay import cli
        from codex_session_relay.assignment import AssignmentView

        linkage = Linkage(self.store, self.clock)
        services = type("Services", (), {
            "store": self.store, "linkage": linkage,
            # supervisor-standing adds the omissions this store derives, through the channel,
            # which is built over the store as it is now - the case may have reopened it.
            "supervisor_channel": SupervisorChannel(self.store, self.registry, linkage,
                                                    self.clock),
            "assignments": AssignmentView(self.store, self.registry, self.clock,
                                          linkage=linkage)})()
        return cli.cmd_supervisor_standing(services, Namespace(project=PROJECT, observation=[]))

    def test_a_parent_that_never_stages_or_sends_still_reads_the_report_as_owed(self):
        from codex_session_relay.registry import Registry

        event_id = self.completed()
        # The parent runs nothing on this channel. A day passes and the relay restarts, so
        # nothing in memory can be what keeps the obligation.
        self.clock.advance(86400)
        self.store.close()
        self.store = Store(os.path.join(self.tmp, "state", "relay.sqlite3"))
        self.addCleanup(self.store.close)
        self.registry = Registry(self.store, self.clock)

        owed = [one for one in self.standing()["standing"]
                if (one.get("basis") or {}).get("eventId") == event_id]
        self.assertEqual(len(owed), 1, "the completion is owed without anything the parent did")
        self.assertEqual(owed[0]["kind"], supervision.COMPLETION)
        self.assertEqual(owed[0]["decision"]["standing"], supervision.STANDING)
        self.assertTrue(owed[0]["decision"]["report"], "and a report of it is still owed")
        self.assertIsNone(owed[0]["decision"]["priorReport"],
                          "and nothing says one was produced")
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])
        self.assertEqual(self.store.all(
            "SELECT seq FROM journal WHERE kind = ?", (supervision.JOURNAL_KIND,)), [])


class EveryLineSelectsTheStoreItWasWrittenFrom(ChannelTestCase):
    """Every command the channel writes for a later step reproduces the store selection it was
    written from; an omission's evidence is the reading it froze; and the request asks for no
    more than a readback records."""

    observation = WhatTheThirdReviewRoundFound.observation
    report_naming = WhatTheEighthReviewRoundFound.report_naming
    completion_naming = WhatTheEighthReviewRoundFound.completion_naming
    packet_pr = WhatTheEighthReviewRoundFound.packet_pr

    SOCKET = "app-server.sock"

    @contextmanager
    def elsewhere_by_default(self):
        """The default selection is somewhere else, as it is for every store chosen by --state."""
        environment = {name: value for name, value in os.environ.items()
                       if name != "CODEX_SESSION_RELAY_STATE"}
        environment["XDG_STATE_HOME"] = os.path.join(self.tmp, "xdg-default")
        with mock.patch.dict(os.environ, environment, clear=True):
            yield

    def run_line(self, line, *, turn=None, proof=None):
        """Split, parse with the real parser, select with the real Services, then execute.

        The store is compared BEFORE anything runs, so a line that selects another store fails
        on the comparison and never creates one.
        """
        from codex_session_relay import cli

        argv = shlex.split(line)
        self.assertEqual(argv[0], "codex-session-relay", line)
        filled = {channel_module.SOCKET_PLACEHOLDER: os.path.join(self.tmp, self.SOCKET),
                  channel_module.TURN_PLACEHOLDER: turn,
                  channel_module.PROOF_PLACEHOLDER: proof}
        args = cli.build_parser().parse_args([filled.get(one) or one for one in argv[1:]])
        services = cli.Services(args)
        try:
            self.assertEqual(os.path.realpath(services.selection.db_path),
                             os.path.realpath(self.store.path),
                             "this line selects another store than the one it was written"
                             " from: " + line)
            if args.command == "supervisor-read":
                services._adapter = self.adapter
            return args.command, args.handler(services, args)
        finally:
            services.close()

    # ---------------------------------------------------------------- the store selection

    def test_every_line_a_report_writes_reaches_the_row_it_was_staged_in(self):
        """RED: a bare --socket selects the socket-scoped default, not a store chosen by --state."""
        one, message_id, record = self.delivered()
        written = self.bytes_of(message_id)
        lines = list(json.loads(self.channel.get(message_id)["packet"])[packets.EVIDENCE])
        lines += [line.strip() for line in written.splitlines()
                  if line.strip().startswith("codex-session-relay ")]
        lines += [line.split("Full record: ", 1)[1] for line in written.splitlines()
                  if line.startswith("Full record: ")]
        turn = record["turnId"]
        reached = {}
        with self.elsewhere_by_default():
            for line in lines:
                command, payload = self.run_line(
                    line, turn=turn, proof=supervisor_read_proof(message_id, turn))
                reached[command] = payload
        self.assertEqual(set(reached), {"show", "supervisor-read", "supervisor-show"})
        self.assertEqual(reached["show"]["event"], one["basis"]["eventId"])
        self.assertEqual(reached["supervisor-show"]["messageId"], message_id)
        self.assertEqual(reached["supervisor-read"]["messageId"], message_id)
        self.assertEqual(reached["supervisor-read"]["verified"], channel_module.HOST_READ)

    def test_the_readback_line_names_the_store_and_the_socket_the_send_went_through(self):
        """RED: the CLI built the channel without either, so the line carried a placeholder."""
        from argparse import Namespace

        from codex_session_relay import cli
        from codex_session_relay.store import canonical_socket

        _one, message_id = self.staged()
        socket = os.path.join(self.tmp, self.SOCKET)
        services = cli.Services(Namespace(state=os.path.dirname(self.store.path), socket=socket))
        try:
            written = services.supervisor_channel.render(
                json.loads(self.channel.get(message_id)["packet"]), "sup-000000000000-a1")
        finally:
            services.close()
        argv = WhatTheFourthReviewRoundFound._rendered_readback(written)
        args = cli.build_parser().parse_args(argv[1:])
        self.assertEqual(args.socket, canonical_socket(socket))
        self.assertEqual(os.path.realpath(args.state),
                         os.path.realpath(os.path.dirname(self.store.path)))
        self.assertEqual([token for token in argv if token.startswith("YOUR_")],
                         [channel_module.TURN_PLACEHOLDER, channel_module.PROOF_PLACEHOLDER])

    # ------------------------------------------------------------ the omission's evidence

    def test_an_omission_points_at_the_reading_it_froze_not_at_a_reread(self):
        """RED: the pointer re-read the turn now, so a report reaching it later contradicted the
        packet - and it selected the observer's store rather than this one."""
        reading = self.observation()
        message_id = self.channel.stage(
            supervision.from_observation(reading), reading=reading)["messageId"]
        evidence = json.loads(self.channel.get(message_id)["packet"])[packets.EVIDENCE]
        self.assertEqual(len(evidence), 1)
        with self.elsewhere_by_default():
            command, shown = self.run_line(evidence[0])
        self.assertEqual(command, "supervisor-show", "the evidence is the frozen record")
        self.assertEqual(shown["stagedFrom"]["reading"], reading)
        self.assertIn("reporting-show", shown["stagedFrom"]["recheck"])

        again = self.channel.stage(supervision.from_observation(reading), reading=dict(reading))
        self.assertFalse(again["staged"], "the same reading converges on the frozen one")
        other = dict(reading, selectors=dict(reading["selectors"], session="01another-session"))
        refusal = self.assertRefused(
            RefusalReason.CONTRADICTORY_OBSERVATION, self.channel.stage,
            supervision.from_observation(other), reading=other)
        self.assertIn("keeps the reading it froze", refusal.detail)

    # ------------------------------------------------------------- what is asked for

    def test_the_request_asks_only_for_what_a_readback_records(self):
        """RED: the message asked the recipient to confirm it had read the report."""
        _one, message_id, record = self.delivered()
        written = self.bytes_of(message_id)
        self.assertNotIn("confirm you read", written.lower())
        self.assertIn("It never records that you read", written)

        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["turnOrigin"], channel_module.RELAY_OPENED)
        self.assertTrue(answer["establishes"].startswith("arrival only"))
        shown = self.channel.show(message_id)
        self.assertEqual((shown["state"], shown["turnOrigin"]),
                         (channel_module.READ, channel_module.RELAY_OPENED))
        self.assertTrue(shown["readEstablishes"].startswith("arrival only"))
        self.assertEqual(shown["readback"]["turnOrigin"], channel_module.RELAY_OPENED)
        self.assertIn(channel_module.RELAY_OPENED,
                      self.channel.reach(message_id)[envelope.RECEIVED]["detail"])

    # ------------------------------------------------ corrections once a report is staged

    def test_a_correction_after_staging_goes_up_as_the_next_event(self):
        """Once sent, the report no longer changes - in place or as a new submission. The
        child's next final receipt is a new event, keyed as its own completion, and goes up as
        its own message; the first stays about the report it carried."""
        from codex_session_relay.errors import ReceiptRefused

        first = self.completion_naming(10)
        staged = self.channel.stage(self.obligation(first))["messageId"]
        self.assertEqual(self.channel.attempt(staged, self.adapter)["deliveryState"],
                         DISPATCHED)
        for submission in (1, 2):
            with self.subTest(submission=submission):
                with self.assertRaises(ReceiptRefused):
                    self.report_naming(first, 11, submission_no=submission)
        second = self.completion_naming(11)
        self.assertNotEqual(second, first)
        corrected = self.channel.stage(self.obligation(second))
        self.assertTrue(corrected["staged"])
        self.assertNotEqual(corrected["messageId"], staged)
        self.assertEqual(self.packet_pr(corrected["messageId"]), 11)
        self.assertEqual(self.packet_pr(staged), 10)



class WhatTheSeventhIndependentReviewFound(ChannelTestCase):
    """Every exit between a claim and its transport gives back what the claim took, an omission is
    read against the store it is staged in, and a block goes up as its newest statement while
    nothing has been sent."""

    hand_over = WhatTheFifthReviewRoundFound.hand_over
    observation = WhatTheThirdReviewRoundFound.observation

    def blocked(self, evidence, *, attempt):
        """The same block, stated with the evidence given. Same generation, cause and summary;
        the attempt makes it a new event, which is what a restatement is."""
        from codex_session_relay import cxc

        payload = self.execution_payload(self.relationship, "blocked_needs_input",
                                         attempt=attempt)
        self.accept(payload)
        report_module.record(
            self.store, self.clock, event_id=payload["eventId"],
            repository="thisisjun786/codex-relay-workflow", cxc_status=cxc.BLOCKED,
            cxc_reason="the upstream package has not landed",
            summary="waiting on the upstream package", next_action="wait for it",
            evidence=[evidence])
        return payload["eventId"]

    def rate(self):
        row = self.store.one(
            "SELECT sends, last_send_at FROM recipient_rate WHERE recipient_task_id = ?",
            (SUPERVISOR,))
        return (row["sends"], row["last_send_at"]) if row is not None else (0, None)

    def attempt_rows(self, message_id):
        return [tuple(row) for row in self.store.all(
            "SELECT attempt_no, state, send_attempted FROM supervisor_attempts"
            " WHERE message_id = ? ORDER BY attempt_no", (message_id,))]

    # ------------------------------------------------- a claim that never reached transport

    def test_a_claim_that_died_before_its_transport_started_is_sent_after_all(self):
        """RED: recovered to held_uncertain, which no readback could ever settle."""
        _one, message_id = self.staged()
        self.channel._claim(message_id, now=self.clock.now(), owner="a worker that died",
                            recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        expired = self.channel.get(message_id)["lease_until"] + 1
        self.clock.advance(expired - self.clock.now())
        record = self.channel.attempt(message_id, self.adapter, now=expired)
        self.assertIsNotNone(record, "nothing was sent, so nothing stops the report going out")
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.attempt_rows(message_id),
                         [(1, WITHHELD_PRE_SEND, "no"), (2, DISPATCHED, "yes")])
        self.assertEqual(len(self.adapter.sends), 1, "one wake for one fact")

    def test_a_claim_recovered_before_its_owner_starts_the_transport_sends_nothing(self):
        """RED: the owner coming back after the recovery found the report held uncertain."""
        _one, message_id = self.staged()
        claim = self.channel._claim

        def claimed_then_recovered(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            self.clock.advance(self.channel.policy.lease_seconds + 1)
            self.channel._recover_if_stranded(self.channel.get(message_id), self.clock.now())
            return claimed

        with mock.patch.object(self.channel, "_claim", claimed_then_recovered):
            self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(self.adapter.sends, [], "the owner's transport start found no claim")
        self.assertEqual(self.channel.get(message_id)["state"], QUEUED)
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_send_the_hierarchy_cancelled_gives_its_budget_back(self):
        """RED (Devin PRRT_kwDOUcYZMM6lCxSt): the reserved send stayed spent on nothing. The
        budget is now spent in the transport-start write, so a claim that never gets there
        spends nothing."""
        _one, message_id = self.staged()
        claim = self.channel._claim

        def claimed_then_handed_over(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            self.hand_over()
            return claimed

        with mock.patch.object(self.channel, "_claim", claimed_then_handed_over):
            self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, self.channel.attempt, message_id,
                               self.adapter)
        self.assertEqual(self.rate(), (0, None), "the recipient's row is as it was")

    def test_an_unsent_claim_leaves_a_later_senders_send_as_it_is(self):
        """A send somebody else made after this claim is theirs, and stays spent."""
        from codex_session_relay.delivery import reserve_send

        _one, message_id = self.staged()
        claim = self.channel._claim
        later = self.clock.now() + 1

        def claimed_then_another_sender_then_handed_over(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            with self.store.transaction() as db:
                self.assertIsNone(reserve_send(db, self.channel.policy, SUPERVISOR, later))
            self.hand_over()
            return claimed

        with mock.patch.object(self.channel, "_claim",
                               claimed_then_another_sender_then_handed_over):
            self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, self.channel.attempt,
                               message_id, self.adapter)
        self.assertEqual(self.rate(), (1, later))

    # ------------------------------------------------------ the store a reading is from

    def test_an_omission_read_against_another_store_is_refused(self):
        """RED: the packet pointed at this store and its recheck at another."""
        base = self.observation()
        reading = dict(base, selectors=dict(base["selectors"],
                                            state=os.path.join(self.tmp, "another-store")))
        refusal = self.assertRefused(
            RefusalReason.CONTRADICTORY_OBSERVATION, self.channel.stage,
            supervision.from_observation(reading), reading=reading)
        self.assertIn("another store", refusal.detail)
        self.assertEqual(self.store.all("SELECT message_id FROM supervisor_messages"), [])

    # ------------------------------------------------------- a block stated again

    def test_a_block_stated_again_before_anything_is_sent_goes_up_as_its_newest_statement(self):
        """RED: the second statement derived the same message and the packet kept the first's
        evidence, so a correction made before the send had no way up."""
        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        one = self.obligation(first)
        message_id = self.channel.stage(one)["messageId"]
        self.clock.advance(5)
        second = self.blocked("the log at /logs/right.txt", attempt=2)
        self.assertEqual(self.obligation(second)["obligationId"], one["obligationId"],
                         "the same block, said again")

        # Staged from the first statement, as supervisor-stage --project holds it.
        answer = self.channel.stage(one)
        self.assertTrue(answer.get("restated"))
        self.assertEqual(answer["messageId"], message_id)
        row = self.channel.get(message_id)
        self.assertEqual(row["event_id"], second)
        self.assertIn(second, json.loads(row["packet"])[packets.EVIDENCE][0])
        self.assertEqual(len(self.store.all("SELECT message_id FROM supervisor_messages")), 1)
        self.assertEqual(len(self.store.all(
            "SELECT seq FROM journal WHERE kind = ?", (supervision.JOURNAL_KIND,))), 1)
        record = self.channel.attempt(message_id, self.adapter)
        self.assertIn("show --event " + second, self.bytes_of(message_id))
        self.assertEqual(record["deliveryState"], DISPATCHED)

    def test_a_block_already_sent_is_not_moved_by_being_stated_again(self):
        """Positive control: once an attempt may have sent, the same block said again is not news."""
        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        message_id = self.channel.stage(self.obligation(first))["messageId"]
        self.channel.attempt(message_id, self.adapter)
        self.clock.advance(5)
        second = self.blocked("the log at /logs/right.txt", attempt=2)
        answer = self.channel.stage(self.obligation(second))
        self.assertFalse(answer.get("restated"))
        self.assertEqual(self.channel.get(message_id)["event_id"], first)
        self.assertEqual(len(self.adapter.sends), 1)



class WhatTheEighthIndependentReviewFound(ChannelTestCase):
    """A decision and the write it justifies share one lock, a token older than the send is not
    the send's, and the transport instant is taken where the transport is decided."""

    blocked = WhatTheSeventhIndependentReviewFound.blocked

    def test_a_restatement_is_decided_and_written_under_one_lock(self):
        """RED: stage() decided under its lock and _readdress wrote under a second one, so a
        correction to the newer statement's report could land in between."""
        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        self.channel.stage(self.obligation(first))
        self.clock.advance(5)
        second = self.blocked("the log at /logs/right.txt", attempt=2)
        seen = []
        rewrite = self.channel._readdress

        def watched(*args, **kwargs):
            seen.append(self.store.db.in_transaction)
            return rewrite(*args, **kwargs)

        with mock.patch.object(self.channel, "_readdress", watched):
            answer = self.channel.stage(self.obligation(second))
        self.assertEqual(seen, [True], "the rewrite runs inside the lock its checks ran in")
        self.assertTrue(answer["restated"])

    def test_a_request_id_written_before_the_send_does_not_verify_it(self):
        """RED: the id is derived from the message and the attempt number, so it can be written
        into the recipient's thread first; a lost response then left only that copy to find.
        The transcript is now searched for the attempt's delivery token, drawn in the claim."""
        from codex_session_relay.identity import supervisor_request_id

        _one, message_id = self.staged()
        self.adapter.start_turn(SUPERVISOR, status="completed",
                                text="written ahead: " + supervisor_request_id(message_id, 1))
        self.clock.advance(5)
        self.adapter.script("transport_unknown")
        self.channel.attempt(message_id, self.adapter)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)
        self.clock.advance(5)
        later = self.adapter.start_turn(SUPERVISOR, status="completed")
        answer = self.read_back(message_id, later.turn_id)
        self.assertEqual(answer["verified"], channel_module.TRANSCRIPT_UNCONFIRMED)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)

    def test_the_transport_start_is_stamped_after_the_lock_and_the_hierarchy_check(self):
        """RED: the instant was read before waiting for the lock, dating the start early."""
        _one, message_id = self.staged()
        asked = self.channel._hierarchy_in
        after = []

        def slow(db, message):
            self.clock.advance(10)
            after.append(self.clock.now())
            return asked(db, message)

        with mock.patch.object(self.channel, "_hierarchy_in", slow):
            self.channel.attempt(message_id, self.adapter)
        stamp = self.store.one(
            "SELECT transport_started_at FROM supervisor_attempts WHERE message_id = ?",
            (message_id,))["transport_started_at"]
        self.assertGreaterEqual(channel_module._iso_time(stamp), after[-1])



class WhatTheNinthIndependentReviewFound(ChannelTestCase):
    """What a readback looks for exists nowhere before the claim that renders it, and the newest
    statement of a block is the newest one this store accepted."""

    blocked = WhatTheSeventhIndependentReviewFound.blocked

    def token_of(self, message_id, attempt_no=1):
        return self.store.one(
            "SELECT delivery_token FROM supervisor_attempts WHERE message_id = ?"
            "   AND attempt_no = ?", (message_id, attempt_no))["delivery_token"]

    def test_a_request_id_written_just_before_the_send_does_not_verify_it(self):
        """RED: written half a second ahead, inside the chronology's precision allowance, the
        request id verified a readback of a send whose response was lost."""
        from codex_session_relay.identity import supervisor_request_id

        _one, message_id = self.staged()
        request_id = supervisor_request_id(message_id, 1)
        self.adapter.start_turn(SUPERVISOR, status="completed", text="ahead: " + request_id)
        self.clock.advance(0.5)
        self.adapter.script("transport_unknown")
        self.channel.attempt(message_id, self.adapter)
        self.clock.advance(5)
        # And after the send as well: without the token, a copy of the id proves nothing.
        self.adapter.start_turn(SUPERVISOR, status="completed", text="after: " + request_id)
        self.clock.advance(5)
        later = self.adapter.start_turn(SUPERVISOR, status="completed")
        answer = self.read_back(message_id, later.turn_id)
        self.assertNotEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)

    def test_the_token_is_drawn_per_attempt_and_carried_in_its_bytes(self):
        """Positive control: each attempt draws its own token, and its bytes carry it."""
        _one, message_id = self.staged()
        self.adapter.script("read_fail")
        self.channel.attempt(message_id, self.adapter)
        self.clock.advance(86400)
        self.channel.attempt(message_id, self.adapter, now=self.clock.now())
        first, second = self.token_of(message_id, 1), self.token_of(message_id, 2)
        self.assertNotEqual(first, second)
        self.assertIn("deliveryToken: " + second, self.bytes_of(message_id))

    def test_the_newest_statement_is_the_newest_one_accepted(self):
        """RED: two statements accepted at one instant were ordered by their event ids."""
        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        message_id = self.channel.stage(self.obligation(first))["messageId"]
        # Accepted at the same instant, and with an event id that sorts BELOW the first.
        attempt = 2
        while (self.execution_payload(self.relationship, "blocked_needs_input",
                                      attempt=attempt)["eventId"] > first):
            attempt += 1
        second = self.blocked("the log at /logs/right.txt", attempt=attempt)
        self.assertLess(second, first)
        answer = self.channel.stage(self.obligation(first))
        self.assertTrue(answer.get("restated"))
        self.assertEqual(self.channel.get(message_id)["event_id"], second)



class WhatTheTenthIndependentReviewFound(ChannelTestCase):
    """Settling a send nobody heard back from takes no benefit of the doubt, and a send is charged
    to the recipient's budget when its claim commits, not when its caller started looking."""

    queued_delivery = WhatTheFifthReviewRoundFound.queued_delivery

    def last_send(self):
        return self.store.one(
            "SELECT MAX(last_send_at) AS last FROM recipient_rate WHERE recipient_task_id = ?",
            (SUPERVISOR,))["last"]

    def test_a_token_placed_ahead_of_the_transport_does_not_settle_a_lost_send(self):
        """RED: placed in the named turn half a second before the transport, inside the
        precision allowance, the token settled a send whose bytes never arrived."""
        _one, message_id = self.staged()
        claim = self.channel._claim
        placed = []

        def claimed_then_token_placed(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            token = self.store.one(
                "SELECT delivery_token FROM supervisor_attempts WHERE message_id = ?",
                (message_id,))["delivery_token"]
            placed.append(self.adapter.start_turn(SUPERVISOR, status="completed",
                                                  text="placed: " + token))
            self.clock.advance(0.5)
            return claimed

        self.adapter.script("transport_unknown")
        with mock.patch.object(self.channel, "_claim", claimed_then_token_placed):
            self.channel.attempt(message_id, self.adapter)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)
        self.clock.advance(5)
        answer = self.read_back(message_id, placed[0].turn_id)
        self.assertNotEqual(answer["verified"], channel_module.HOST_READ)
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)

    def test_a_report_claim_charges_the_send_budget_when_it_claims(self):
        """RED (Devin PRRT_kwDOUcYZMM6lD3QZ): charged at the instant attempt() started."""
        _one, message_id = self.staged()
        settings = self.channel._settings_for
        calls = []

        def slow_host_checks(task_id, runtime_status=None):
            # The host checks before the claim are the slow ones; the transport-start write
            # asks the same gate again, from the store, and takes no time here.
            if not calls:
                self.clock.advance(10)
            calls.append(task_id)
            return settings(task_id, runtime_status)

        started = self.clock.now()
        with mock.patch.object(self.channel, "_settings_for", slow_host_checks):
            self.channel.attempt(message_id, self.adapter)
        self.assertEqual(self.last_send(), started + 10)



class WhatTheEleventhIndependentReviewFound(ChannelTestCase):
    """A packet composed before its event's report existed froze nothing, is never sent stale, and
    gives way to what the report says; and nothing spends the send budget before the transport
    start, so nothing has to give it back."""

    hand_over = WhatTheFifthReviewRoundFound.hand_over
    rate = WhatTheSeventhIndependentReviewFound.rate
    report_naming = WhatTheEighthReviewRoundFound.report_naming
    packet_pr = WhatTheEighthReviewRoundFound.packet_pr

    def recorded(self, record):
        from codex_session_relay.errors import ReceiptRefused

        try:
            record()
        except ReceiptRefused:
            return False
        return True

    # -------------------------------------------------- a report after the staging

    def test_a_decision_reported_after_its_block_was_staged_still_goes_up(self):
        """RED: staged from the outcome before the report said it was a decision only the user
        can make, the block froze a report that did not exist yet, and the decision could never
        be recorded or sent."""
        payload = self.execution_payload(self.relationship, "blocked_needs_input")
        self.accept(payload)
        event_id = payload["eventId"]
        staged = self.channel.stage_standing(PROJECT)["staged"]
        self.assertEqual([one["message"]["obligation_kind"] for one in staged],
                         [supervision.BLOCKED])
        blocked = staged[0]["messageId"]

        self.assertTrue(self.recorded(lambda: report_module.record(
            self.store, self.clock, event_id=event_id,
            repository="thisisjun786/codex-relay-workflow", cxc_status="NEEDS_HUMAN",
            cxc_reason="two readings of the criterion are defensible",
            summary="which reading of the criterion is the agreed one",
            next_action="ask the user", evidence=["both readings are in the review thread"])),
            "the first report on a staged event is not a correction of anything")

        refusal = self.assertRefused(RefusalReason.SUPERSEDED_REVISION, self.channel.attempt,
                                     blocked, self.adapter)
        self.assertIn("now raises", refusal.detail)
        self.assertEqual(self.channel.get(blocked)["hold_reason"], channel_module.SUPERSEDED_HOLD)
        self.assertEqual(self.adapter.sends, [])

        again = self.channel.stage_standing(PROJECT)["staged"]
        decision = [one["messageId"] for one in again
                    if one["message"]["obligation_kind"] == supervision.DECISION]
        self.assertEqual(len(decision), 1)
        record = self.channel.attempt(decision[0], self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(len(self.adapter.sends), 1, "one wake, for the decision")

    def test_a_report_recorded_after_its_completion_was_staged_is_carried(self):
        """RED: the first report was refused. Now it is recorded, and the send carries it: the
        claim finds the staged row is not what is owed now and restates it in place (I-247)."""
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(self.relationship, [path])
        self.accept(payload)
        event_id = payload["eventId"]
        message_id = self.channel.stage(self.obligation(event_id))["messageId"]
        self.assertIsNone(json.loads(self.channel.get(message_id)["packet"]).get(packets.ARTIFACT))

        self.assertTrue(self.recorded(lambda: self.report_naming(event_id, 10)))
        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)
        self.assertEqual(self.packet_pr(message_id), 10)
        self.assertEqual(self.channel.get(message_id)["submission_no"], 1)
        self.assertEqual(len(self.adapter.sends), 1)
        restated = self.store.all("SELECT detail FROM journal WHERE kind = ?",
                                  (channel_module.RESTATED,))
        self.assertEqual([json.loads(one["detail"])["at"] for one in restated], ["claim"])

    def test_two_claims_that_never_sent_leave_no_send_time_behind(self):
        """RED: A reserved and stranded, B reserved, A given back, B cancelled by a handover:
        each gave back what it could, and B's time stayed behind with no send under it."""
        _one, first = self.staged()
        self.clock.advance(5)
        _other, second = self.staged(text="a second deliverable")
        self.channel._claim(first, now=self.clock.now(), owner="a worker that died",
                            recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        self.clock.advance(self.channel.policy.lease_seconds + 1)
        claim = self.channel._claim

        def claimed_then_first_recovered_then_handed_over(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            self.channel._recover_if_stranded(self.channel.get(first), self.clock.now())
            self.hand_over()
            return claimed

        with mock.patch.object(self.channel, "_claim",
                               claimed_then_first_recovered_then_handed_over):
            self.assertRefused(RefusalReason.RELATION_OWNER_DRIFT, self.channel.attempt, second,
                               self.adapter)
        self.assertEqual(self.rate(), (0, None), "no send happened, so no send time stands")
        self.assertEqual(self.adapter.sends, [])

    def test_a_send_the_budget_refuses_at_its_transport_start_is_deferred(self):
        """The budget is spent where the transport starts, so a send another queue got in ahead
        of is refused there: nothing sent, deferred past the gap, never failed or held."""
        from codex_session_relay.delivery import reserve_send

        _one, message_id = self.staged()
        claim = self.channel._claim

        def claimed_then_a_delivery_reserved(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            with self.store.transaction() as db:
                self.assertIsNone(reserve_send(db, self.channel.policy, SUPERVISOR,
                                               self.clock.now()))
            return claimed

        with mock.patch.object(self.channel, "_claim", claimed_then_a_delivery_reserved):
            self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        row = self.channel.get(message_id)
        self.assertEqual((row["state"], row["hold_reason"]), (QUEUED, None))
        self.assertEqual(row["next_eligible_at"],
                         self.clock.now() + self.channel.policy.min_send_interval_seconds)
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.store.one(
            "SELECT send_attempted FROM supervisor_attempts WHERE message_id = ?",
            (message_id,))["send_attempted"], "no")
        record = self.channel.attempt(message_id, self.adapter, now=row["next_eligible_at"])
        self.assertEqual(record["deliveryState"], DISPATCHED)


class WhatTheTwelfthIndependentReviewFound(ChannelTestCase):
    """What goes out is the fact as it stands at the transport instant: the transport-start write
    asks the question the claim asked, and the newest statement of a block is the one sent."""

    blocked = WhatTheSeventhIndependentReviewFound.blocked
    report_naming = WhatTheEighthReviewRoundFound.report_naming
    packet_pr = WhatTheEighthReviewRoundFound.packet_pr
    recorded = WhatTheEleventhIndependentReviewFound.recorded

    def attempt_row(self, message_id):
        return self.store.one(
            "SELECT send_attempted, retry_safe, transport_started_at FROM supervisor_attempts"
            " WHERE message_id = ? ORDER BY attempt_no DESC LIMIT 1", (message_id,))

    def claimed_then(self, happen):
        """Run happen() once, after the first claim commits and before its transport start."""
        claim = self.channel._claim
        fired = []

        def claimed_then_something_committed(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            if not fired:
                fired.append(True)
                happen()
            return claimed

        return mock.patch.object(self.channel, "_claim", claimed_then_something_committed)

    def attempts(self, message_id):
        return [tuple(one) for one in self.store.all(
            "SELECT attempt_no, send_attempted, transport_started_at IS NOT NULL"
            "  FROM supervisor_attempts WHERE message_id = ? ORDER BY attempt_no",
            (message_id,))]

    def test_a_report_recorded_between_the_claim_and_the_transport_start_is_not_sent_stale(self):
        """RED: the transport-start write re-read the hierarchy only, so a packet composed before
        the first report went out without it, and staging could never carry it afterwards. Now
        that attempt is voided, the message restated, and the report goes out in the same call."""
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(self.relationship, [path])
        self.accept(payload)
        event_id = payload["eventId"]
        message_id = self.channel.stage(self.obligation(event_id))["messageId"]

        with self.claimed_then(lambda: self.assertTrue(
                self.recorded(lambda: self.report_naming(event_id, 10)))):
            record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.attempts(message_id), [(1, "no", 0), (2, "yes", 1)])
        self.assertEqual(len(self.adapter.sends), 1, "one wake, carrying the report")
        self.assertEqual(self.packet_pr(message_id), 10)
        self.assertIn("codex-relay-workflow #10 at", self.bytes_of(message_id))
        restated = self.store.all("SELECT detail FROM journal WHERE kind = ?",
                                  (channel_module.RESTATED,))
        self.assertEqual([json.loads(one["detail"])["at"] for one in restated],
                         ["transport_start"])

    def test_a_block_that_became_a_decision_before_its_transport_started_wakes_once(self):
        """RED: the stale block went out, then the decision the report raised went out as well -
        two wakes for one blocked turn."""
        payload = self.execution_payload(self.relationship, "blocked_needs_input")
        self.accept(payload)
        event_id = payload["eventId"]
        blocked = self.channel.stage_standing(PROJECT)["staged"][0]["messageId"]

        def the_report_says_it_is_the_users_call():
            self.assertTrue(self.recorded(lambda: report_module.record(
                self.store, self.clock, event_id=event_id,
                repository="thisisjun786/codex-relay-workflow", cxc_status="NEEDS_HUMAN",
                cxc_reason="two readings of the criterion are defensible",
                summary="which reading of the criterion is the agreed one",
                next_action="ask the user", evidence=["both readings are in the review thread"])))

        with self.claimed_then(the_report_says_it_is_the_users_call):
            self.assertRefused(RefusalReason.SUPERSEDED_REVISION, self.channel.attempt, blocked,
                               self.adapter)
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.channel.get(blocked)["hold_reason"], channel_module.SUPERSEDED_HOLD)
        self.assertEqual(self.attempt_row(blocked)["send_attempted"], "no")

        decision = [one["messageId"] for one in self.channel.stage_standing(PROJECT)["staged"]
                    if one["message"]["obligation_kind"] == supervision.DECISION]
        self.assertEqual(len(decision), 1)
        self.assertEqual(self.channel.attempt(decision[0], self.adapter)["deliveryState"],
                         DISPATCHED)
        self.assertIsNone(self.channel.attempt(blocked, self.adapter))
        self.assertEqual(len(self.adapter.sends), 1, "one wake, for the decision")

    # ------------------------------------------------ a block stated again, unsent

    def test_a_block_stated_again_before_its_send_goes_up_as_the_newer_statement(self):
        """RED: the send checked the first statement's own report only and sent it; the newer
        statement's staging then answered that the block had been sent."""
        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        message_id = self.channel.stage(self.obligation(first))["messageId"]
        self.clock.advance(5)
        second = self.blocked("the log at /logs/right.txt", attempt=2)

        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)
        self.assertEqual(self.channel.get(message_id)["event_id"], second)
        self.assertIn("show --event " + second, self.bytes_of(message_id))
        self.assertNotIn("show --event " + first, self.bytes_of(message_id))
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_block_stated_again_between_the_claim_and_the_transport_start_is_not_sent(self):
        """RED: the restatement landed after the claim's check, and the first statement went
        out at the transport start."""
        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        message_id = self.channel.stage(self.obligation(first))["messageId"]
        second = []

        def stated_again():
            self.clock.advance(5)
            second.append(self.blocked("the log at /logs/right.txt", attempt=2))

        with self.claimed_then(stated_again):
            record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.attempts(message_id), [(1, "no", 0), (2, "yes", 1)])
        self.assertIn("show --event " + second[0], self.bytes_of(message_id))
        self.assertNotIn("show --event " + first, self.bytes_of(message_id))
        self.assertEqual(len(self.adapter.sends), 1)


class WhatTheThirteenthIndependentReviewFound(ChannelTestCase):
    """The settings a send carries are the ones authorized at its transport instant."""

    staged = WhatTheSecondReviewRoundFound.staged

    def test_settings_changed_between_the_claim_and_the_transport_start_are_not_sent(self):
        """RED: the settings read before the claim went out although a user transition had
        replaced them before the transport started."""
        from codex_session_relay.registry import load_settings

        self.channel = self.build_channel(
            settings=lambda task, runtime=None: load_settings(self.store, task))
        _one, message_id = self.staged()
        claim = self.channel._claim

        def claimed_then_the_user_moved_the_supervisor(*args, **kwargs):
            claimed = claim(*args, **kwargs)
            record_settings(self.store, self.clock, SUPERVISOR,
                            task_settings("/changed-after-gate"), source="user_transition")
            return claimed

        with mock.patch.object(self.channel, "_claim",
                               claimed_then_the_user_moved_the_supervisor):
            self.assertIsNone(self.channel.attempt(message_id, self.adapter))
        self.assertEqual(self.adapter.sends, [])
        attempt = self.store.one(
            "SELECT send_attempted, retry_safe, transport_started_at FROM supervisor_attempts"
            " WHERE message_id = ?", (message_id,))
        self.assertEqual(tuple(attempt), ("no", 1, None))
        row = self.channel.get(message_id)
        self.assertEqual((row["state"], row["hold_reason"]), (QUEUED, None))

        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)
        self.assertEqual(self.adapter.settings_seen[-1][1].data["cwd"], "/changed-after-gate")
        self.assertEqual(len(self.adapter.sends), 1)


class TheStagedRowIsAProposal(ChannelTestCase):
    """I-247: what goes out is re-derived where the transport starts, for every writer that can
    change an obligation after staging - not only the ones a review happened to find."""

    observation = WhatTheThirdReviewRoundFound.observation

    DOCUMENT = "https://linear.app/example/document/coordination-000000000000"

    def confirmed(self, one):
        """Confirm the verdict row that discharges this completion; returns the outbox."""
        from codex_session_relay.sync import SyncOutbox, render_block

        event_id = one["basis"]["eventId"]
        row = self.store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
        outbox = SyncOutbox(self.store, self.clock)
        outbox.set_target(self.rid, "coordination_document", self.DOCUMENT)
        with self.store.transaction() as db:
            ruling = outbox.enqueue_in(
                db, relationship_id=self.rid, issue_key="REL-1", subject_kind="verdict",
                summary="ruled", event_id=event_id, generation=row["execution_generation"],
                revision=row["revision_hash"], verdict="verified", criteria_digest="digest-a")
        claim = outbox.claim(ruling, owner="test")
        stored = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (ruling,))
        outbox.complete(ruling, claim_token=claim["claimToken"], target_ref=self.DOCUMENT,
                        readback=render_block(stored), external_ref="linear-doc-1")
        return outbox

    def block_reported(self, event_id, status, submission_no):
        report_module.record(
            self.store, self.clock, event_id=event_id,
            repository="thisisjun786/codex-relay-workflow", cxc_status=status,
            cxc_reason="the upstream package has not landed",
            summary="waiting on the upstream package", next_action="wait for it",
            evidence=["the upstream pull request is still open"],
            submission_no=submission_no)

    def test_a_block_corrected_away_and_back_before_its_send_still_goes_up(self):
        """RED (review 15): corrected to a decision, the block was held as obsolete for good;
        corrected back, the same obligation was current again and nothing could send it."""
        from codex_session_relay import cxc

        payload = self.execution_payload(self.relationship, "blocked_needs_input")
        self.accept(payload)
        event_id = payload["eventId"]
        self.block_reported(event_id, cxc.BLOCKED, 1)
        one = self.obligation(event_id)
        message_id = self.channel.stage(one)["messageId"]

        self.block_reported(event_id, cxc.NEEDS_HUMAN, 2)
        self.assertRefused(RefusalReason.SUPERSEDED_REVISION, self.channel.attempt,
                           message_id, self.adapter)
        self.assertEqual(self.channel.get(message_id)["hold_reason"],
                         channel_module.SUPERSEDED_HOLD)

        self.block_reported(event_id, cxc.BLOCKED, 3)
        self.assertEqual(self.obligation(event_id)["obligationId"], one["obligationId"])
        self.channel.stage(self.obligation(event_id))
        self.assertIsNone(self.channel.get(message_id)["hold_reason"],
                          "the block is owed again, so its hold is released")
        record = self.channel.attempt(message_id, self.adapter)
        self.assertIsNotNone(record, "the block is owed again and nothing sent it")
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.channel.get(message_id)["submission_no"], 3)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_discharge_a_repointed_target_reopens_releases_the_report(self):
        """RED (review 15): discharged, the completion was held for good; the target repointed,
        the obligation stood again and the send answered nothing."""
        one, message_id = self.staged()
        outbox = self.confirmed(one)
        self.assertRefused(RefusalReason.SUPERSEDED_REVISION, self.channel.attempt,
                           message_id, self.adapter)
        outbox.set_target(self.rid, "coordination_document",
                          "https://linear.app/example/document/coordination-111111111111")
        self.assertEqual(supervision.discharge_of(self.store, one)["standing"],
                         supervision.STANDING)

        record = self.channel.attempt(message_id, self.adapter)
        self.assertIsNotNone(record, "the obligation stands again and nothing sent it")
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_an_obligation_discharged_after_staging_is_not_sent(self):
        """RED: the Linear record confirmed the completion after it was staged, and the report
        still woke the supervisor about a fact the record it reads already has."""
        one, message_id = self.staged()
        self.confirmed(one)

        refusal = self.assertRefused(RefusalReason.SUPERSEDED_REVISION, self.channel.attempt,
                                     message_id, self.adapter)
        self.assertIn("Linear record", refusal.detail)
        self.assertEqual(self.channel.get(message_id)["hold_reason"],
                         channel_module.SUPERSEDED_HOLD)
        self.assertEqual(self.adapter.sends, [])

    def test_an_omission_whose_turn_reported_after_staging_is_not_sent(self):
        """RED: the turn's own receipt arrived after its omission was staged, and both the
        omission and the completion that receipt raised would wake the supervisor."""
        turn = self.assigned_turn()
        base = self.observation()
        reading = dict(base, selectors=dict(base["selectors"], turn=turn.turn_id))
        message_id = self.channel.stage(supervision.from_observation(reading),
                                        reading=reading)["messageId"]
        path = self.artifact("out.txt", "the deliverable")
        self.accept(self.ready_payload(self.relationship, [path], turn=turn))

        refusal = self.assertRefused(RefusalReason.SUPERSEDED_REVISION, self.channel.attempt,
                                     message_id, self.adapter)
        self.assertIn("final receipt", refusal.detail)
        self.assertEqual(self.channel.get(message_id)["hold_reason"],
                         channel_module.SUPERSEDED_HOLD)
        self.assertEqual(self.adapter.sends, [])


class WhatTheFourteenthIndependentReviewFound(ChannelTestCase):
    """A push the recipient's policy refuses is not a send, and does not strand the report."""

    staged = WhatTheSecondReviewRoundFound.staged

    class _Services:
        def __init__(self, channel, adapter):
            self.adapter_requested = True
            self.supervisor_channel = channel
            self.adapter = adapter

    def test_a_push_the_recipients_policy_refuses_is_not_sent_and_goes_later(self):
        """RED: the transport answered inbox_only with sendAttempted no and no turn; the command
        said sent, the ladder said transport_accepted, and inbox_only was never claimable, so
        restoring the policy could not send the report."""
        from codex_session_relay import cli

        _one, message_id = self.staged()
        self.adapter.script("approval_policy")
        answer = cli.cmd_supervisor_send(self._Services(self.channel, self.adapter),
                                         type("Args", (), {"message": message_id})())
        self.assertFalse(answer["sent"])
        self.assertEqual((answer["deliveryState"], answer["sendAttempted"], answer["turnId"]),
                         (WITHHELD_PRE_SEND, "no", None))
        self.assertEqual(answer["transportDeliveryState"], INBOX_ONLY)
        self.assertNotEqual(
            self.channel.reach(message_id)[envelope.TRANSPORT_ACCEPTED]["state"], envelope.YES)
        row = self.channel.get(message_id)
        self.assertEqual((row["state"], row["hold_reason"]), (WITHHELD_PRE_SEND, None))
        self.assertGreater(row["next_eligible_at"], self.clock.now())

        record = self.channel.attempt(message_id, self.adapter, now=row["next_eligible_at"])
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(
            self.channel.reach(message_id)[envelope.TRANSPORT_ACCEPTED]["state"], envelope.YES)


class WhatTheSixteenthIndependentReviewFound(ChannelTestCase):
    """The current statement is found by the obligation's key, not by the event a message was
    staged from; and a report cannot change under bytes that already went up about its event."""

    blocked = WhatTheSeventhIndependentReviewFound.blocked
    claimed_then = WhatTheTwelfthIndependentReviewFound.claimed_then
    attempts = WhatTheTwelfthIndependentReviewFound.attempts
    report_naming = WhatTheEighthReviewRoundFound.report_naming

    def test_a_block_stated_again_while_its_first_statement_became_a_decision_goes_up(self):
        """RED (review 16): staged from E1, claimed; E2 states the same block and E1's report is
        corrected into a decision. The check asked E1 first, called the block obsolete and held
        it, though E2 still raised it. By the key, E2 is the current statement and goes up."""
        from codex_session_relay import cxc

        first = self.blocked("the log at /logs/wrong.txt", attempt=1)
        one = self.obligation(first)
        message_id = self.channel.stage(one)["messageId"]
        second = []

        def restated_and_the_first_turned_into_a_decision():
            self.clock.advance(5)
            second.append(self.blocked("the log at /logs/right.txt", attempt=2))
            report_module.record(
                self.store, self.clock, event_id=first,
                repository="thisisjun786/codex-relay-workflow", cxc_status=cxc.NEEDS_HUMAN,
                cxc_reason="the upstream package needs the user's call",
                summary="which upstream package to wait for", next_action="ask the user",
                evidence=["the upstream pull request is still open"], submission_no=2)

        with self.claimed_then(restated_and_the_first_turned_into_a_decision):
            try:
                record = self.channel.attempt(message_id, self.adapter)
            except DeliveryRefused as refused:
                self.fail("the block is still owed through E2 and was refused: " + str(refused))
        self.assertIsNotNone(record, "the block is still owed through E2 and nothing sent it")
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(self.obligation(second[0])["obligationId"], one["obligationId"])
        self.assertIsNone(self.channel.get(message_id)["hold_reason"])
        self.assertIn("show --event " + second[0], self.bytes_of(message_id))
        self.assertEqual(self.attempts(message_id), [(1, "no", 0), (2, "yes", 1)])
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_first_report_after_a_reportless_send_is_refused(self):
        """RED (Devin lGhiL): a completion sent before any report existed left its first report
        free, and the evidence its bytes point at then showed a pull request they never said."""
        from codex_session_relay.errors import ReceiptRefused

        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(self.relationship, [path])
        self.accept(payload)
        event_id = payload["eventId"]
        message_id = self.channel.stage(self.obligation(event_id))["messageId"]
        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)
        with self.assertRaises(ReceiptRefused) as caught:
            self.report_naming(event_id, 42)
        self.assertIn("was sent", caught.exception.detail)
        self.assertIsNone(report_module.read(self.store, event_id))
