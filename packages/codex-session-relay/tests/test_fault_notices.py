"""CRW-205 criterion 7: a fault notification goes up through the supervisor channel, once.

The ledger raises notifications - a broken fault opened, a decision somebody must make, a fault
resolved - and before this nothing delivered them: the only caller of reserve_notifications was a
command a person had to run. These cases run the REAL daemon tick over the same supervisor channel
a parent's reports use, and hold the deliverer to criterion 7 and to that channel's own rules: one
notification is one message and one wake however often the tick runs or restarts, a paused,
archived or unaddressable level above is not woken and the notification is kept, an answer that
may have been sent is never sent again, and a notification nothing reached spends no budget.

Names this change adds are reached through getattr, so on a tree without the deliverer each case
fails on what it asserts rather than on an import.
"""

from unittest import mock

from codex_session_relay import faults, lifecycle
from codex_session_relay import supervisorchannel as channel_module
from codex_session_relay.transport import DISPATCHED, HELD_UNCERTAIN

from .support import ISSUE, PARENT
from .test_supervisor_autosend import DaemonChannelCase
from .test_supervisor_channel import INITIATIVE, PROJECT, SUPERVISOR, _Reader

PRODUCT = "crw"
NOTICE = "fault_notification"
SECRET = "SECRET-TOKEN-4f9c"


def faults_contact_window():
    """How long a recorded observation of the parent counts as current."""
    from codex_session_relay import supervision

    return supervision.CONTACT_FRESH_FOR


class NoticeCase(DaemonChannelCase):
    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.daemon.faults = self.ledger

    def build_daemon(self, channel):
        # The daemon's own sweep reads this fixture's store and records faults of its own (an
        # anchor nobody polls); these cases record exactly the faults they are about.
        daemon = super().build_daemon(channel)
        daemon._sweep_faults = lambda report: None
        return daemon

    def broken(self, key="stuck", *, detail="deliveries to the parent are not moving",
               evidence=(), adopt=None):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"relationship": self.rid, "cause": key}, occurrence_key="test:" + key,
            scope={"projectKey": PROJECT, "issueKey": ISSUE}, detail=detail,
            evidence=list(evidence)), adopt=adopt)
        return answer["faultId"]

    def notification(self, fault_id, kind=faults.BLOCKING):
        found = []
        for state in (faults.PENDING, faults.RESERVED, faults.UNCERTAIN, faults.DELIVERED,
                      faults.WITHDRAWN):
            found += [one for one in self.ledger.notifications(state=state, limit=100)
                      ["notifications"] if one["faultId"] == fault_id and one["kind"] == kind]
        self.assertEqual(len(found), 1, found)
        return found[0]

    def sent_for(self, notification):
        """The sends to the supervisor that carried this notification's one message."""
        row = self.store.one(
            "SELECT message_id, subject FROM supervisor_messages"
            " WHERE obligation_kind = ? AND obligation_id = ?",
            (NOTICE, notification["notificationId"]))
        if row is None:
            return []
        self.assertEqual(row["subject"], notification["deliveryKey"],
                         "the message is keyed by the notification's deliveryKey")
        return [one for one in self.upward() if row["message_id"] in one[2]]

    def notice_rows(self):
        return [dict(row) for row in self.store.all(
            "SELECT * FROM supervisor_messages WHERE obligation_kind = ?", (NOTICE,))]

    def one_notice(self):
        rows = self.notice_rows()
        self.assertEqual(len(rows), 1, "one notification, one staged message")
        return rows[0]

    def cleared(self, key="stuck"):
        """The broken() fault's cause, cleared: nothing about it landed, so it withdraws."""
        self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"relationship": self.rid, "cause": key},
            occurrence_key="test:" + key + ":clear", cleared=True,
            scope={"projectKey": PROJECT, "issueKey": ISSUE}))

    def budget_used(self):
        """Units ever consumed and not given back, whatever window they fell in."""
        return self.store.one("SELECT COUNT(*) AS n FROM fault_budget_uses"
                              " WHERE product = ? AND kind = ?",
                              (PRODUCT, faults.NOTIFICATION))["n"]

    def measure_parent(self):
        """What delivery records about the parent when it observes it, recorded by hand."""
        lifecycle.record(self.store, self.clock, lifecycle.observe(self.adapter, PARENT))


class ABrokenFaultReachesTheLevelAboveOnce(NoticeCase):
    def test_one_notification_is_one_message_across_ticks_and_a_restart(self):
        fault = self.broken()
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(len(self.sent_for(notification)), 1, "the level above was told once")
        self.assertEqual(notification["state"], faults.DELIVERED)
        rows = self.notice_rows()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["state"], DISPATCHED)
        self.assertIn(rows[0]["message_id"], notification["ackRef"])
        for _ in range(5):
            self.tick(advance=3600)
        # A restart: a new daemon and a new channel over the same store and host.
        self.daemon = self.build_daemon(self.build_channel())
        self.daemon.faults = faults.FaultLedger(self.store, self.clock)
        for _ in range(3):
            self.tick(advance=3600)
        self.assertEqual(len(self.sent_for(notification)), 1, "one notification, one wake")
        self.assertEqual(len(self.notice_rows()), 1, "one notification, one message")
        self.assertEqual(self.budget_used(), 1)

    def test_the_bytes_say_what_the_fault_is_and_nothing_it_recorded(self):
        fault = self.broken(detail="transport said token=" + SECRET,
                            evidence=[{"kind": "log", "ref": "tail", "observed": SECRET}],
                            adopt={"externalRef": "REL-9", "scope": {"projectKey": PROJECT}})
        self.tick()
        sent = self.sent_for(self.notification(fault))
        self.assertEqual(len(sent), 1)
        text = sent[0][2]
        for said in ("fault_notice", "delivery_stalled", faults.BROKEN, faults.BLOCKING,
                     "REL-9", fault, "fault-show"):
            self.assertIn(said, text)
        self.assertNotIn(SECRET, text, "no recorded detail or evidence travels upward")

    def test_a_decision_goes_up_as_a_decision(self):
        fault = self.broken()
        self.tick()
        # A caller's own words can hold anything - here a credential - and never travel.
        self.ledger.raise_notification(fault, reason="incident awaiting classification; token="
                                       + SECRET)
        self.tick(advance=3600)
        decision = self.notification(fault, faults.DECISION)
        sent = self.sent_for(decision)
        self.assertEqual(len(sent), 1)
        self.assertIn("fault_decision", sent[0][2])
        self.assertIn("raised by a caller", sent[0][2])
        self.assertIn("fault-show", sent[0][2])
        self.assertNotIn(SECRET, sent[0][2])
        self.assertNotIn("incident awaiting classification", sent[0][2])
        self.assertEqual(decision["state"], faults.DELIVERED)


class TheLevelAboveThatCannotBeToldIsNotWoken(NoticeCase):
    def test_an_archived_supervisor_is_not_woken_and_nothing_is_spent(self):
        self.adapter.threads[SUPERVISOR].archived = True
        fault = self.broken()
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(self.upward(), [])
        self.assertEqual(notification["state"], faults.PENDING, "kept, not dropped")
        self.assertTrue(notification["lastError"])
        self.assertEqual(self.budget_used(), 0, "a notification nothing reached spent nothing")
        self.tick(advance=5)
        self.assertEqual(self.upward(), [], "not retried inside the channel's recheck")
        self.adapter.threads[SUPERVISOR].archived = False
        self.tick(advance=self.channel.policy.lifecycle_recheck_seconds + 1)
        notification = self.notification(fault)
        self.assertEqual(len(self.sent_for(notification)), 1)
        self.assertEqual(notification["state"], faults.DELIVERED)
        self.assertEqual(len(self.notice_rows()), 1)
        self.assertEqual(self.budget_used(), 1)

    def test_no_supervisor_bound_keeps_it_pending_with_the_reason_and_writes_nothing(self):
        self.channel.linkage = _Reader({
            "state": "resolved", "readable": True, "contention": [],
            "gaps": [{"gap": "no_supervisor", "scopeKind": "project", "scopeKey": PROJECT}],
            "levels": [{"scopeKind": "project", "scopeKey": PROJECT,
                        "owner": {"taskId": "01parent-task"}, "depth": 1}]})
        fault = self.broken()
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("nobody to report to", notification["lastError"] or "")
        self.assertEqual(self.notice_rows(), [], "nothing staged for nobody")
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.budget_used(), 0)
        before = (self.store.one("SELECT COUNT(*) AS n FROM journal")["n"],
                  dict(self.store.one("SELECT * FROM fault_notifications")))
        self.tick(advance=60)
        after = (self.store.one("SELECT COUNT(*) AS n FROM journal")["n"],
                 dict(self.store.one("SELECT * FROM fault_notifications")))
        self.assertEqual(before, after, "a notification that is still waiting writes nothing")

    def test_a_paused_assignment_is_never_reserved(self):
        fault = self.broken()
        self.registry.set_status(self.rid, "paused", actor="user")
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertEqual(notification["attempts"], 0)
        self.assertEqual(self.notice_rows(), [])
        self.assertEqual(self.upward(), [])

    def test_a_parent_the_host_reports_archived_withholds_it_without_a_reservation(self):
        # Eligibility reads the parent's deliverability from what the host was last seen to
        # say. Nothing had observed it, so the deliverer measures it the way delivery does,
        # and the rule itself decides: an archived parent withholds the notification.
        self.adapter.threads[PARENT].archived = True
        fault = self.broken()
        self.tick()
        notification = self.notification(fault)
        self.assertEqual((notification["state"], notification["attempts"]), (faults.PENDING, 0))
        self.assertEqual(self.notice_rows(), [])
        self.assertEqual(self.upward(), [])
        self.adapter.threads[PARENT].archived = False
        self.tick(advance=faults_contact_window() + 1)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)


class WhatMayHaveBeenSentIsNeverSentAgain(NoticeCase):
    def test_a_lost_answer_lapses_to_uncertain_and_a_readback_settles_it(self):
        self.adapter.script("transport_unknown")
        fault = self.broken()
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.RESERVED)
        row = self.one_notice()
        self.assertEqual(row["state"], HELD_UNCERTAIN)
        self.tick(advance=faults.LEASE_SECONDS + 1)
        self.assertEqual(self.notification(fault)["state"], faults.UNCERTAIN)
        for _ in range(3):
            self.tick(advance=600)
        self.assertEqual(len(self.sent_for(notification)), 1, "never sent a second time")
        landed = self.adapter.start_turn(SUPERVISOR, status="completed",
                                         text=self.bytes_of(row["message_id"]))
        answer = self.read_back(row["message_id"], landed.turn_id)
        self.assertEqual(answer["verified"], channel_module.HOST_READ)
        self.tick(advance=1)
        settled = self.notification(fault)
        self.assertEqual(settled["state"], faults.DELIVERED)
        self.assertIn(row["message_id"], settled["ackRef"])
        self.assertEqual(len(self.sent_for(notification)), 1)

    def test_a_claim_that_died_before_its_transport_is_sent_once_after_recovery(self):
        fault = self.broken()
        with mock.patch.object(channel_module.SupervisorChannel, "_start_transport",
                               side_effect=RuntimeError("the relay process was killed")):
            self.tick()
        notification = self.notification(fault)
        self.assertEqual(self.upward(), [])
        wait = max(faults.LEASE_SECONDS, self.channel.policy.lease_seconds) + 1
        self.tick(advance=wait)
        self.tick(advance=1)
        notification = self.notification(fault)
        self.assertEqual(len(self.sent_for(notification)), 1)
        self.assertEqual(notification["state"], faults.DELIVERED)
        self.assertEqual(self.budget_used(), 1, "the reservation that sent nothing was refunded")


class ABudgetHoldsANotificationAndNeverDropsIt(NoticeCase):
    def test_a_spent_budget_holds_the_second_until_its_window_passes(self):
        self.ledger.set_limit(PRODUCT, faults.NOTIFICATION, max_count=1, window=3600)
        first, second = self.broken("one"), self.broken("two")
        self.tick()
        states = sorted([self.notification(first)["state"], self.notification(second)["state"]])
        self.assertEqual(states, [faults.DELIVERED, faults.PENDING])
        self.assertEqual(len(self.upward()), 1)
        self.tick(advance=3601)
        self.tick(advance=1)
        self.assertEqual([self.notification(one)["state"] for one in (first, second)],
                         [faults.DELIVERED, faults.DELIVERED])
        self.assertEqual(len(self.upward()), 2)


class ANoticeGoesOnlyWhileItsNotificationIsReserved(NoticeCase):
    def test_a_notice_that_waits_neither_blocks_a_report_nor_goes_without_a_reservation(self):
        self.adapter.set_status(SUPERVISOR, "active")
        fault = self.broken()
        self.tick()
        rows = self.notice_rows()
        self.assertEqual(len(rows), 1, "the notice was staged and is waiting")
        row = rows[0]
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertEqual(self.upward(), [])
        # The supervisor is free again, and the plain send pass is asked for the notice while
        # its notification is not reserved: I-247 refuses it, and nothing is sent.
        self.adapter.set_status(SUPERVISOR, "idle")
        self.clock.advance(3600)
        self.assertIsNone(self.channel.attempt(row["message_id"], self.adapter))
        self.assertEqual(self.upward(), [])
        # A report staged after it is not held back by the waiting notice.
        self.completed()
        self.tick()
        kinds = [one["obligation_kind"] for one in self.messages()
                 if one["state"] == DISPATCHED]
        self.assertIn("completion", kinds)
        self.tick(advance=3600)
        self.assertEqual(len(self.sent_for(self.notification(fault))), 1)


class NothingSentSpendsNothing(NoticeCase):
    def test_a_failed_or_unreached_reservation_gives_its_unit_back(self):
        fault = self.broken()
        self.measure_parent()
        [one] = self.ledger.reserve_notifications(owner="t", limit=5)["reserved"]
        self.assertEqual(self.budget_used(), 1)
        self.ledger.fail_notification(one["notificationId"], token=one["token"], error="refused")
        self.assertEqual(self.budget_used(), 0)
        [one] = self.ledger.reserve_notifications(owner="t", limit=5)["reserved"]
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.ledger.reconcile_notification(one["notificationId"], delivered=False, ref="none")
        self.assertEqual(self.budget_used(), 0)
        [one] = self.ledger.reserve_notifications(owner="t", limit=5)["reserved"]
        self.ledger.ack_notification(one["notificationId"], token=one["token"], ref="sent")
        self.assertEqual(self.budget_used(), 1)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)


class WhatTheArchitectReviewAsked(NoticeCase):
    """Plan audit (architect, round one): the edges the deliverer's seam has to hold."""

    def test_a_capped_notice_waits_named_and_quiet(self):
        import dataclasses

        self.channel.policy = dataclasses.replace(self.channel.policy, busy_max_attempts=1)
        self.adapter.set_status(SUPERVISOR, "active")
        fault = self.broken()
        self.tick()
        row = self.one_notice()
        self.assertEqual(row["hold_reason"], "busy_cap")
        attempts = self.notification(fault)["attempts"]
        for _ in range(3):
            self.tick(advance=3600)
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("busy_cap", notification["lastError"] or "")
        self.assertEqual(notification["attempts"], attempts, "not reserved again while capped")
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.budget_used(), 0)

    def test_a_queued_notice_whose_reservation_lapsed_does_not_hold_a_report_back(self):
        try:
            from codex_session_relay import faultnotice
        except ImportError:
            self.fail("this tree has no fault notification deliverer")

        fault = self.broken()
        # The deliverer dies between staging and settling: the notice is queued and its
        # notification reserved, with nobody holding the token.
        with mock.patch.object(channel_module.SupervisorChannel, "attempt",
                               side_effect=RuntimeError("the relay process was killed")), \
                mock.patch.object(faultnotice.NoticeDeliverer, "_return",
                                  side_effect=RuntimeError("the relay process was killed")):
            self.tick()
        self.assertEqual(self.notification(fault)["state"], faults.RESERVED)
        self.assertEqual(self.upward(), [])
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.completed()
        self.tick()
        self.tick(advance=self.channel.policy.min_send_interval_seconds + 1)
        sent = [one["obligation_kind"] for one in self.messages() if one["state"] == DISPATCHED]
        self.assertEqual(sorted(sent), ["completion", NOTICE])
        self.assertEqual(len(self.sent_for(self.notification(fault))), 1)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)
        self.assertEqual(len(self.upward()), 2, "each once")

    def test_the_channel_readers_take_a_notice_row(self):
        fault = self.broken()
        self.tick()
        row = self.one_notice()
        shown = self.channel.show(row["message_id"])
        self.assertEqual(shown["kind"], NOTICE)
        before = self.store.one("SELECT COUNT(*) AS n FROM supervisor_messages")["n"]
        self.channel.stage_unsent(PROJECT)
        self.assertEqual(self.store.one("SELECT COUNT(*) AS n FROM supervisor_messages")["n"],
                         before, "a project's standing reports never stage a notice")
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)

    def test_a_fault_nothing_places_under_a_project_waits_with_that_reason(self):
        # No relationship, and a scope that names no project (or one nobody owns): there is no
        # level above to tell, and it waits saying which.
        waiting = {}
        for name, scope, said in (
                ("no-project", {"issueKey": "REL-NEW"}, "no level above"),
                ("unowned", {"projectKey": "PRJ-UNBOUND", "issueKey": "REL-NEW"},
                 "no live project owner")):
            answer = self.ledger.record(faults.observation(
                product=PRODUCT, fault_class="managed_start_failed", severity=faults.BROKEN,
                signature={"issueKey": "REL-NEW", "receiptStatus": "failed", "case": name},
                occurrence_key="managed:new:" + name, scope=scope, detail="start failed"))
            waiting[answer["faultId"]] = said
        self.tick()
        for fault, said in waiting.items():
            notification = self.notification(fault)
            self.assertEqual(notification["state"], faults.PENDING)
            self.assertIn(said, notification["lastError"] or "")
        self.assertEqual(self.notice_rows(), [])
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.budget_used(), 0)

    def test_a_blocking_notice_whose_fault_withdrew_before_it_left_never_goes(self):
        # Audit round one: a block that cleared before anybody above was told is not a new
        # serious block (criterion 7), so its blocking notification is withdrawn with it.
        fault = self.broken()
        self.cleared()
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.WITHDRAWN)
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.notice_rows(), [], "nothing was staged for it")
        self.assertEqual(self.budget_used(), 0)


class WhatTheFirstAuditFound(NoticeCase):
    """Plan audit, round one: a notice follows the fault as it is now, a real send is never
    settled as not sent, and a notice withdrawn with its fault comes back with its cause."""

    def reserved_and_staged(self, fault):
        self.measure_parent()
        [one] = [each for each in self.ledger.reserve_notifications(owner="t", limit=5)
                 ["reserved"] if each["faultId"] == fault]
        staged = self.channel.stage_notice(faults.notice_facts(self.store.db,
                                                               one["notificationId"]))
        return one, staged["messageId"]

    def test_a_notice_reserved_when_its_fault_withdraws_is_stopped_where_it_would_start(self):
        fault = self.broken()
        one, message_id = self.reserved_and_staged(fault)
        self.cleared()
        with self.assertRaises(Exception):
            self.channel.attempt(message_id, self.adapter)
        self.assertEqual(self.upward(), [], "withdrawn before its transport started")
        self.assertIsNone(self.channel.may_have_sent(message_id))
        settled = self.ledger.fail_notification(one["notificationId"], token=one["token"],
                                                error="stopped at its transport start")
        self.assertEqual(settled["state"], faults.WITHDRAWN)
        self.assertEqual(self.budget_used(), 0)

    def test_a_cause_that_comes_back_raises_the_withdrawn_notification_again(self):
        fault = self.broken()
        self.cleared()
        self.assertEqual(self.notification(fault)["state"], faults.WITHDRAWN)
        self.broken(key="stuck")
        self.assertEqual(self.notification(fault)["state"], faults.PENDING)
        self.tick()
        self.assertEqual(len(self.sent_for(self.notification(fault))), 1)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)

    def test_a_notice_follows_what_its_fault_is_about_now(self):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"cause": "moving"}, occurrence_key="test:moving",
            scope={"projectKey": PROJECT, "issueKey": ISSUE}, detail="stuck"))
        fault = answer["faultId"]
        self.adapter.threads[SUPERVISOR].archived = True
        self.tick()
        self.one_notice()
        # Moved to an issue no relationship holds, in a project nobody owns: no level above.
        self.ledger.move(fault, scope={"projectKey": "PRJ-UNBOUND", "issueKey": "REL-NO-LINK"})
        self.adapter.threads[SUPERVISOR].archived = False
        for _ in range(2):
            self.tick(advance=3600)
        notification = self.notification(fault)
        self.assertEqual(self.upward(), [], "never to the hierarchy of the issue it left")
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("no live project owner", notification["lastError"] or "")
        self.ledger.move(fault, scope={"projectKey": PROJECT, "issueKey": ISSUE})
        self.tick(advance=3600)
        self.assertEqual(len(self.sent_for(self.notification(fault))), 1)
        self.assertEqual(len(self.notice_rows()), 1, "still one message")

    def test_a_real_send_is_never_settled_as_not_sent(self):
        fault = self.broken()
        one, message_id = self.reserved_and_staged(fault)
        record = self.channel.attempt(message_id, self.adapter)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.fail_notification(one["notificationId"], token=one["token"],
                                          error="a caller that says nothing went")
        self.assertEqual(self.budget_used(), 1, "the unit a real send spent is kept")
        self.assertEqual(self.notification(fault)["state"], faults.RESERVED)
        self.ledger.ack_notification(one["notificationId"], token=one["token"], ref=message_id)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)


class WhatTheSecondAuditFound(NoticeCase):
    """Plan audit, round two: no value a caller typed reaches the level above through an issue
    field - an adoption's reference, or an observation's scope."""

    def test_an_adopted_reference_that_is_not_an_identifier_is_not_sent(self):
        fault = self.broken(adopt={"externalRef": "REL-9 token=" + SECRET,
                                   "scope": {"projectKey": PROJECT}})
        self.tick()
        sent = self.sent_for(self.notification(fault))
        self.assertEqual(len(sent), 1)
        self.assertNotIn(SECRET, sent[0][2])
        self.assertIn("an issue is published", sent[0][2])

    def test_an_observed_issue_key_is_not_what_the_notice_names(self):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"relationship": self.rid, "cause": "scoped"},
            occurrence_key="test:scoped",
            scope={"projectKey": PROJECT, "issueKey": "REL-9 token=" + SECRET}, detail="x"))
        self.tick()
        sent = self.sent_for(self.notification(answer["faultId"]))
        self.assertEqual(len(sent), 1)
        self.assertNotIn(SECRET, sent[0][2])
        self.assertIn("issue: " + ISSUE, sent[0][2], "the relationship's registered issue")


class WhatTheThirdAuditFound(NoticeCase):
    """Plan audit, round three: a value the registered hierarchy supplies - here the project
    key - travels only as a plain identifier; one that is not is never sent, and not echoed."""

    def setUp(self):
        from . import test_supervisor_channel as channel_tests

        with mock.patch.object(channel_tests, "PROJECT", "PRJ-1 token=" + SECRET):
            super().setUp()

    def test_a_project_key_that_is_not_an_identifier_is_never_sent(self):
        # The fault names the project its relationship is registered under, so that
        # relationship addresses it and the linkage's project key is what gets checked.
        fault = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"relationship": self.rid, "cause": "stuck"}, occurrence_key="test:stuck",
            scope={"projectKey": "PRJ-1 token=" + SECRET, "issueKey": ISSUE},
            detail="deliveries to the parent are not moving"))["faultId"]
        self.tick()
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("projectKey", notification["lastError"] or "")
        self.assertNotIn(SECRET, notification["lastError"] or "")
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.notice_rows(), [])
        self.assertEqual(self.budget_used(), 0)
        self.assertFalse(any(SECRET in one[2] for one in self.adapter.sends))


class NoHierarchyValueTravelsAsFreeText(NoticeCase):
    """The same rule for every value the registered hierarchy puts in a notice."""

    def test_each_hierarchy_value_must_be_a_plain_identifier(self):
        fault = self.broken()
        self.measure_parent()
        [one] = self.ledger.reserve_notifications(owner="t", limit=5)["reserved"]
        notice = faults.notice_facts(self.store.db, one["notificationId"])
        live = self.channel.resolve(self.rid)
        self.channel.compose_notice(notice, resolution=live)
        for field in ("projectKey", "sender", "recipient"):
            with self.subTest(field=field):
                with self.assertRaises(Exception) as refused:
                    self.channel.compose_notice(
                        notice, resolution={**live, field: live[field] + " token=" + SECRET})
                self.assertIn(field, str(refused.exception))
                self.assertNotIn(SECRET, str(refused.exception))
        self.assertEqual(self.notification(fault)["state"], faults.RESERVED)


class WhatTheFourthAuditFound(NoticeCase):
    """Plan audit, round four: reports and notices share one per-tick send cap (I-250)."""

    def test_a_notice_takes_only_what_the_reports_left_of_the_tick_cap(self):
        import dataclasses

        self.daemon.policy = dataclasses.replace(self.daemon.policy,
                                                 max_supervisor_sends_per_tick=1)
        self.channel.policy = dataclasses.replace(self.channel.policy,
                                                  min_send_interval_seconds=0.0)
        self.completed()
        fault = self.broken()
        self.tick()
        self.assertEqual(len(self.upward()), 1, "one send in a tick whose cap is one")
        self.assertEqual([one["obligation_kind"] for one in self.messages()
                          if one["state"] == DISPATCHED], ["completion"], "the older report first")
        self.tick(advance=1)
        self.assertEqual(len(self.upward()), 2)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)


class WhatTheFifthAuditFound(NoticeCase):
    """Plan audit, round five: a class name is a value the ledger writes like any other, and an
    attempt that raised after its transport started spends the tick's send cap."""

    def test_a_class_name_is_a_plain_identifier(self):
        with self.assertRaises(ValueError):
            faults.register_class("delivery_stalled token=" + SECRET, component="delivery",
                                  clears="a synthetic clear")

    def test_a_fault_of_a_class_no_notice_may_name_waits_without_echoing_it(self):
        name = "stalled token=" + SECRET
        policy = dict(faults.CLASS_POLICY["delivery_stalled"])
        with mock.patch.dict(faults.CLASS_POLICY, {name: policy}):
            answer = self.ledger.record(faults.observation(
                product=PRODUCT, fault_class=name, severity=faults.BROKEN,
                signature={"relationship": self.rid, "cause": "class"},
                occurrence_key="test:class",
                scope={"projectKey": PROJECT, "issueKey": ISSUE}, detail="x"))
            self.tick()
            notification = self.notification(answer["faultId"])
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("faultClass", notification["lastError"] or "")
        self.assertNotIn(SECRET, notification["lastError"] or "")
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.budget_used(), 0)

    def test_an_attempt_that_raised_after_its_transport_spends_the_tick_cap(self):
        import dataclasses

        self.daemon.policy = dataclasses.replace(self.daemon.policy,
                                                 max_supervisor_sends_per_tick=1)
        self.channel.policy = dataclasses.replace(self.channel.policy,
                                                  min_send_interval_seconds=0.0)
        self.completed()
        fault = self.broken()
        read = self.channel.get
        raised = []

        def read_fails_once_after_the_send(message_id):
            row = read(message_id)
            if (row["state"] == DISPATCHED and row["obligation_kind"] == "completion"
                    and not raised):
                raised.append(message_id)
                raise RuntimeError("a read failed after the transport answered")
            return row
        with mock.patch.object(self.channel, "get", side_effect=read_fails_once_after_the_send):
            self.tick()
        self.assertEqual(raised and len(raised), 1)
        self.assertEqual(len(self.upward()), 1, "the send that raised spent the cap of one")
        self.tick(advance=1)
        self.assertEqual(len(self.upward()), 2)
        self.assertEqual(self.notification(fault)["state"], faults.DELIVERED)


class WhatTheSixthAuditFound(NoticeCase):
    """Plan audit, round six: the user's pause and no-contact are asked again where the
    transport starts, so one committed after the reservation is kept (I-447, I-247)."""

    def ticking_after(self, change):
        original = channel_module.SupervisorChannel.attempt

        def changed_first(channel, message_id, adapter, **kw):
            change()
            return original(channel, message_id, adapter, **kw)
        with mock.patch.object(channel_module.SupervisorChannel, "attempt", changed_first):
            self.tick()

    def assert_kept_back(self, fault):
        notification = self.notification(fault)
        self.assertEqual(self.upward(), [])
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertEqual(self.budget_used(), 0, "the reservation that sent nothing gave it back")
        attempts = notification["attempts"]
        for _ in range(2):
            self.tick(advance=3600)
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.notification(fault)["attempts"], attempts,
                         "not reserved again while the wish stands")

    def test_a_pause_after_the_reservation_is_kept(self):
        fault = self.broken()
        self.ticking_after(lambda: self.registry.set_status(self.rid, "paused", actor="user"))
        self.assert_kept_back(fault)

    def test_a_parent_that_became_uncontactable_after_the_reservation_is_kept(self):
        fault = self.broken()

        def archive_parent():
            self.adapter.threads[PARENT].archived = True
            self.measure_parent()
        self.ticking_after(archive_parent)
        self.assert_kept_back(fault)


class WhatTheSeventhAuditFound(NoticeCase):
    """Plan audit, round seven: a fault about a project and no relationship - a managed start
    that never produced one - goes to that project's supervisor (I-445), and the published
    issue's Linear link travels when it has exactly that shape (I-448)."""

    LINK = "https://linear.app/example/issue/CRW-205/synthetic-fault"

    def project_fault(self, issue="REL-77", key="start"):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="managed_start_failed", severity=faults.BROKEN,
            signature={"issueKey": issue, "receiptStatus": "failed", "case": key},
            occurrence_key="managed:" + key,
            scope={"projectKey": PROJECT, "issueKey": issue}, detail="start failed"))
        return answer["faultId"]

    def test_a_fault_about_a_project_and_no_relationship_reaches_its_supervisor_once(self):
        fault = self.project_fault()
        self.tick()
        notification = self.notification(fault)
        sent = self.sent_for(notification)
        self.assertEqual(len(sent), 1, "the project's supervisor was told")
        self.assertEqual(notification["state"], faults.DELIVERED)
        text = sent[0][2]
        for said in ("managed_start_failed", "REL-77", PROJECT, fault, "fault-show"):
            self.assertIn(said, text)
        row = getattr(self, "one_notice")()
        self.assertEqual(row["relationship_id"],
                         getattr(faults, "PROJECT_ANCHOR", "project:") + PROJECT)
        self.assertEqual(row["recipient_task_id"], SUPERVISOR)
        for _ in range(3):
            self.tick(advance=3600)
        self.assertEqual(len(self.sent_for(notification)), 1, "one notification, one wake")
        self.assertEqual(len(self.notice_rows()), 1)
        self.assertEqual(self.budget_used(), 1)

    def test_an_issue_a_project_fault_names_that_is_not_an_identifier_is_left_out(self):
        fault = self.project_fault(issue="REL-NEW token=" + SECRET, key="odd")
        self.tick()
        sent = self.sent_for(self.notification(fault))
        self.assertEqual(len(sent), 1)
        self.assertNotIn(SECRET, sent[0][2])
        self.assertNotIn("REL-NEW", sent[0][2])

    def test_an_archived_supervisor_of_a_project_fault_is_not_woken(self):
        self.adapter.threads[SUPERVISOR].archived = True
        fault = self.project_fault()
        self.tick()
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.notification(fault)["state"], faults.PENDING, "kept, not dropped")
        self.assertEqual(self.budget_used(), 0)

    def test_a_published_linear_issue_link_travels_with_the_notice(self):
        fault = self.broken(adopt={"externalRef": self.LINK, "scope": {"projectKey": PROJECT}})
        self.tick()
        sent = self.sent_for(self.notification(fault))
        self.assertEqual(len(sent), 1)
        self.assertIn(self.LINK, sent[0][2])
        self.assertNotIn("its reference is on the fault", sent[0][2])

    def test_a_link_of_any_other_shape_is_said_to_exist_and_not_carried(self):
        refs = ("https://linear.app/example/issue/CRW-205/slug?token=" + SECRET,
                "https://elsewhere.example/issue/CRW-205/" + SECRET.lower(),
                "https://linear.app/example/issue/CRW-205/" + "x" * 121)
        found = [self.broken(key="link" + str(n),
                             adopt={"externalRef": ref, "scope": {"projectKey": PROJECT}})
                 for n, ref in enumerate(refs)]
        for _ in range(3):
            self.tick(advance=3600)
        for fault, ref in zip(found, refs):
            sent = self.sent_for(self.notification(fault))
            self.assertEqual(len(sent), 1)
            self.assertNotIn(ref, sent[0][2])
            self.assertNotIn(SECRET, sent[0][2])
            self.assertNotIn(SECRET.lower(), sent[0][2])
            self.assertIn("an issue is published", sent[0][2])


class WhatDevinFoundOnTheFirstHead(NoticeCase):
    """Devin review of PR #151 at 70f81a60: a bound about a former supervisor does not keep a
    notice from the one there now (the channel's own re-address rule), and a parent the host
    cannot observe does not stall every other notification."""

    def test_a_capped_notice_goes_to_the_supervisor_who_took_over(self):
        import dataclasses

        from codex_session_relay.models import Endpoint

        from .support import HOST
        from .test_supervisor_channel import SUCCESSOR

        self.channel.policy = dataclasses.replace(self.channel.policy, busy_max_attempts=1)
        self.adapter.set_status(SUPERVISOR, "active")
        fault = self.broken()
        self.tick()
        self.assertEqual(self.one_notice()["hold_reason"], "busy_cap")
        self.adapter.add_thread(SUCCESSOR)
        self.linkage.handover(
            role="supervisor", scope_key=INITIATIVE, expect_task_id=SUPERVISOR,
            endpoint=Endpoint(SUCCESSOR, HOST, cwd="/successor", cxc_session="cxc-next"),
            acknowledged=[], evidence="the initiative changed hands", actor="a test")
        for _ in range(2):
            self.tick(advance=3600)
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.DELIVERED, notification["lastError"])
        told = [thread for _r, thread, _m, _o in self.adapter.sends
                if thread in (SUPERVISOR, SUCCESSOR)]
        self.assertEqual(told, [SUCCESSOR], "the successor, once; the former supervisor never")
        row = self.one_notice()
        self.assertEqual((row["recipient_task_id"], row["hold_reason"]), (SUCCESSOR, None))
        self.assertEqual(self.budget_used(), 1)

    def test_a_parent_the_host_cannot_observe_does_not_hold_other_notices_back(self):
        original = lifecycle.observe

        def observe(adapter, task, **kw):
            if task == PARENT:
                raise ConnectionError("the host did not answer")
            return original(adapter, task, **kw)

        stuck = self.broken()
        other = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="managed_start_failed", severity=faults.BROKEN,
            signature={"issueKey": "REL-77", "receiptStatus": "failed"},
            occurrence_key="managed:other",
            scope={"projectKey": PROJECT, "issueKey": "REL-77"}, detail="start failed"))
        with mock.patch.object(lifecycle, "observe", side_effect=observe):
            self.tick()
        self.assertEqual(self.notification(other["faultId"])["state"], faults.DELIVERED)
        waiting = self.notification(stuck)
        self.assertEqual(waiting["state"], faults.PENDING)
        self.assertIn("could not be observed", waiting["lastError"] or "")
        self.assertNotIn(SECRET, waiting["lastError"] or "")
        self.assertEqual(self.budget_used(), 1, "only what went spent anything")
        self.tick(advance=3600)
        self.assertEqual(self.notification(stuck)["state"], faults.DELIVERED,
                         "measured and sent once the host answers")


class WhatDevinFoundOnTheSecondHead(NoticeCase):
    """Devin review of PR #151 at d76c7a4b: a relationship registered under another project
    than the one the fault's scope names never addresses its notice (never sideways)."""

    def test_an_issue_s_relationship_in_another_project_does_not_address_the_notice(self):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"cause": "elsewhere"}, occurrence_key="test:elsewhere",
            scope={"projectKey": "PRJ-OTHER", "issueKey": ISSUE}, detail="stuck"))
        for _ in range(2):
            self.tick(advance=3600)
        notification = self.notification(answer["faultId"])
        self.assertEqual(self.upward(), [], "never to another project's supervisor")
        self.assertEqual(self.notice_rows(), [])
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("no live project owner", notification["lastError"] or "")
        self.assertEqual(self.budget_used(), 0)

    def test_a_fault_moved_to_another_project_is_not_told_to_the_one_it_left(self):
        fault = self.broken()
        self.ledger.move(fault, scope={"projectKey": "PRJ-OTHER", "issueKey": ISSUE})
        for _ in range(2):
            self.tick(advance=3600)
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.notification(fault)["state"], faults.PENDING)
        self.ledger.move(fault, scope={"projectKey": PROJECT, "issueKey": ISSUE})
        self.tick(advance=3600)
        self.assertEqual(len(self.sent_for(self.notification(fault))), 1,
                         "back under the relationship's project, it goes there once")

    def test_an_archived_parent_holds_back_only_its_own_notices(self):
        # Devin's second finding on d76c7a4b, answered: the parent measured by the pre-pass is
        # recorded before the reservation, which reads eligibility from the store in its own
        # transaction, so the same tick sees the new answer - and nothing else waits on it.
        self.adapter.threads[PARENT].archived = True
        held = self.broken()
        other = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="managed_start_failed", severity=faults.BROKEN,
            signature={"issueKey": "REL-77", "receiptStatus": "failed"},
            occurrence_key="managed:other",
            scope={"projectKey": PROJECT, "issueKey": "REL-77"}, detail="start failed"))
        self.tick()
        self.assertEqual(self.notification(other["faultId"])["state"], faults.DELIVERED)
        waiting = self.notification(held)
        self.assertEqual(waiting["state"], faults.PENDING)
        self.assertIn("recipient_archived", waiting["eligibility"]["reason"])
        for _ in range(3):
            self.tick(advance=3600)
        self.assertEqual(self.notification(held)["attempts"], 0, "never reserved while archived")
        self.assertEqual(len(self.upward()), 1)
        self.assertEqual(self.budget_used(), 1)


class WhatTheFinalReviewFound(NoticeCase):
    """Final review of 06ed9543: the relationship that addresses a notice and the one whose
    wishes apply are one answer (anchor_relationship), so a relationship of another project
    neither addresses the notice nor holds it back."""

    def other_project(self):
        from codex_session_relay.models import Endpoint
        from codex_session_relay.registry import record_settings

        from .support import HOST, task_settings

        parent, supervisor = "01other-parent", "01other-supervisor"
        for task, cwd in ((parent, "/other-parent"), (supervisor, "/other-supervisor")):
            self.adapter.add_thread(task)
            record_settings(self.store, self.clock, task, task_settings(cwd),
                            source="creation_result")
        self.linkage.register_supervision(
            initiative_key="INI-2", project_key="PRJ-2",
            supervisor=Endpoint(supervisor, HOST, cwd="/other-supervisor",
                                cxc_session="cxc-other-supervisor"),
            parent=Endpoint(parent, HOST, cwd="/other-parent", cxc_session="cxc-other-parent"))
        return supervisor

    def test_another_project_s_archived_parent_does_not_hold_the_notice_back(self):
        supervisor = self.other_project()
        self.adapter.threads[PARENT].archived = True
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="managed_start_failed", severity=faults.BROKEN,
            signature={"cause": "start failed"}, occurrence_key="start:failed",
            scope={"projectKey": "PRJ-2", "issueKey": ISSUE}, detail="start failed"))
        self.tick()
        notification = self.notification(answer["faultId"])
        self.assertEqual(notification["state"], faults.DELIVERED, notification.get("eligibility"))
        told = [one for one in self.adapter.sends if one[1] == supervisor]
        self.assertEqual(len(told), 1, "PRJ-2's supervisor, once")
        self.assertEqual(self.upward(), [], "never the supervisor of the project it is not in")
        for _ in range(2):
            self.tick(advance=3600)
        self.assertEqual(len([one for one in self.adapter.sends if one[1] == supervisor]), 1)

    def test_the_relationship_the_fault_s_project_holds_still_carries_its_wishes(self):
        self.other_project()
        self.registry.set_status(self.rid, "paused", actor="user")
        fault = self.broken()
        for _ in range(2):
            self.tick(advance=3600)
        notification = self.notification(fault)
        self.assertEqual(notification["state"], faults.PENDING)
        self.assertIn("paused", notification["eligibility"]["reason"])
        self.assertEqual([one for one in self.adapter.sends if one[1] != PARENT], [])
