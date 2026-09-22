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
import unittest

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
)

from .support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, RelayTestCase, task_settings

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
            "issue": ISSUE, "generation": 1})
        self.assertEqual(agreeing["disposition"], packets.ACCEPTED)
        wrong = packets.reception(packet, {
            "relationId": self.rid, "parentTaskId": PARENT,
            "supervisorTaskId": "01another-supervisor", "issue": ISSUE, "generation": 1})
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
        self.assertEqual(answer["delivered"]["token"], record["requestId"])
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
        state is for the process to stop existing between the two.
        """
        _one, message_id = self.staged()
        self.channel._claim(message_id, now=self.clock.now(), owner="a worker that died",
                            recipient=SUPERVISOR)
        row = self.channel.get(message_id)
        self.assertEqual(row["state"], "sending")
        self.assertEqual(self.channel.stranded(now=row["lease_until"] + 1)[0]["message_id"],
                         message_id)

        recovered = self.channel.attempt(message_id, self.adapter,
                                         now=row["lease_until"] + 1)
        self.assertIsNone(recovered, "an expired lease authorises no resend")
        self.assertEqual(self.channel.get(message_id)["state"], HELD_UNCERTAIN)
        self.assertEqual(
            len(self.store.all("SELECT request_id FROM supervisor_attempts")), 1,
            "what that send did is unknown, and a second one is a second wake for one fact")
        stranded = [json.loads(row["detail"]) for row in self.store.all(
            "SELECT detail FROM journal WHERE kind = 'supervisor_message_stranded'")]
        self.assertEqual(len(stranded), 1)
        self.assertIn("unknown", stranded[0]["reason"])

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
    def test_a_delivered_attempt_with_no_turn_id_leaves_the_origin_unknown(self):
        """inbox_only is a delivered state and carries no turn id, so unknown is reachable."""
        _one, message_id, record = self.delivered()
        self.store.db.execute(
            "UPDATE supervisor_attempts SET state = ?, turn_id = NULL WHERE message_id = ?",
            (INBOX_ONLY, message_id))
        self.store.db.execute(
            "UPDATE supervisor_messages SET state = ? WHERE message_id = ?",
            (INBOX_ONLY, message_id))
        self.store.db.commit()
        answer = self.read_back(message_id, record["turnId"])
        self.assertEqual(answer["turnOrigin"], channel_module.ORIGIN_UNKNOWN)
