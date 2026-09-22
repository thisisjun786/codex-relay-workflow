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
from codex_session_relay.transport import DEFERRED_BUSY, DISPATCHED, QUEUED, WITHHELD_PRE_SEND

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
