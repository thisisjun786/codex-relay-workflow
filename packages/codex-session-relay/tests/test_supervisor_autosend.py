"""The supervisor channel with nobody asking: the daemon tick stages and sends what is owed.

CRW-215's reporting duty rested on the project parent running supervisor-stage and
supervisor-send. These cases run the REAL daemon tick - and, once, the real daemon command - over
the same channel a parent drives by hand, and hold it to the channel's own rules: one obligation
is one message and one wake, a paused or archived supervisor is not woken and keeps the
obligation, and a parent that also stages or sends by hand converges on the same ids.

The channel is attached to the daemon by attribute rather than by the constructor keyword, so on
a daemon that has no supervisor pass each case fails on what it asserts rather than on a
signature.
"""

import json
import os

from codex_session_relay import cxc, supervision
from codex_session_relay import report as report_module
from codex_session_relay.daemon import RelayDaemon
from codex_session_relay.supervisorchannel import SupervisorChannel
from codex_session_relay.transport import DISPATCHED, SENDING, WITHHELD_PRE_SEND

from . import test_supervisor_channel as channel_tests
from .test_supervisor_channel import PROJECT, SUPERVISOR, ChannelTestCase


class DaemonChannelCase(ChannelTestCase):
    def setUp(self):
        super().setUp()
        from codex_session_relay.ack import AckService
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.reconcile import Reconciler

        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.ack = AckService(self.store, self.registry, self.intake, self.delivery, self.clock)
        self.reconciler = Reconciler(self.store, self.registry, self.delivery, self.clock)
        self.daemon = self.build_daemon(self.channel)

    def build_daemon(self, channel):
        daemon = RelayDaemon(self.store, self.registry, self.intake, self.delivery, self.ack,
                             self.reconciler, self.adapter, clock=self.clock)
        daemon.supervisor_channel = channel
        return daemon

    def upward(self):
        """What reached the supervisor's thread, and nothing the parent-child path sent."""
        return [one for one in self.adapter.sends if one[1] == SUPERVISOR]

    def messages(self):
        return [dict(row) for row in self.store.all(
            "SELECT message_id, obligation_id, obligation_kind, state, hold_reason"
            "  FROM supervisor_messages ORDER BY staged_at")]

    @staticmethod
    def counts(report):
        """(staged, sent) this tick; zero on a daemon that has no supervisor pass at all."""
        return (getattr(report, "supervisorStaged", 0), getattr(report, "supervisorSent", 0))

    def only_message(self):
        messages = self.messages()
        self.assertEqual(len(messages), 1, "exactly one message for the one fact")
        return messages[0]

    def tick(self, *, advance=0):
        if advance:
            self.clock.advance(advance)
        return self.daemon.tick()


class AParentThatNeverReports(DaemonChannelCase):
    """The negative control CRW-148 requires: nobody runs supervisor-stage or supervisor-send."""

    blocked = channel_tests.WhatTheSeventhIndependentReviewFound.blocked

    def test_a_completion_goes_up_once_with_nobody_asking(self):
        self.completed()
        first = self.tick()
        self.assertEqual(self.counts(first), (1, 1))
        self.assertEqual(len(self.upward()), 1)
        message = self.only_message()
        self.assertEqual((message["obligation_kind"], message["state"]),
                         (supervision.COMPLETION, DISPATCHED))

        again = self.tick(advance=3600)
        self.assertEqual(self.counts(again), (0, 0))
        self.assertEqual(len(self.upward()), 1, "one fact, one wake")

    def test_a_block_goes_up_with_nobody_asking(self):
        self.blocked("the upstream pull request is still open", attempt=1)
        self.tick()
        self.assertEqual(len(self.upward()), 1)
        self.assertEqual([one["obligation_kind"] for one in self.messages()],
                         [supervision.BLOCKED])

    def test_a_decision_goes_up_with_nobody_asking(self):
        payload = self.execution_payload(self.relationship, "blocked_needs_input")
        self.accept(payload)
        report_module.record(
            self.store, self.clock, event_id=payload["eventId"],
            repository="thisisjun786/codex-relay-workflow", cxc_status=cxc.NEEDS_HUMAN,
            cxc_reason="two readings of the criterion are defensible",
            summary="which reading of the criterion is the agreed one",
            next_action="ask the user", evidence=["both readings are in the review thread"])
        self.tick()
        self.assertEqual(len(self.upward()), 1)
        self.assertEqual([one["obligation_kind"] for one in self.messages()],
                         [supervision.DECISION])

    def test_a_quiet_store_ticks_without_touching_the_channel(self):
        self.completed()
        self.tick()
        journal = self.store.one("SELECT COUNT(*) AS c FROM journal"
                                 " WHERE kind LIKE 'supervisor%'")["c"]
        again = self.tick(advance=60)
        self.assertEqual(self.counts(again), (0, 0))
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM journal"
                                        " WHERE kind LIKE 'supervisor%'")["c"], journal)


class OneLogicalIdAcrossRestartsAndRedelivery(DaemonChannelCase):
    def test_a_restarted_daemon_converges_on_the_message_already_sent(self):
        self.completed()
        self.tick()
        # A new process: a new daemon over a new channel instance, the same store.
        self.daemon = self.build_daemon(SupervisorChannel(
            self.store, self.registry, self.linkage, self.clock,
            settings=lambda task, runtime=None: {"authorized": task}))
        after = self.tick(advance=3600)
        self.assertEqual(self.counts(after), (0, 0))
        self.assertEqual(len(self.messages()), 1)
        self.assertEqual(len(self.upward()), 1)

    def test_a_redelivered_receipt_is_the_same_fact(self):
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(self.relationship, [path])
        self.accept(payload)
        self.tick()
        self.accept(payload)
        self.tick(advance=3600)
        self.assertEqual(len(self.messages()), 1)
        self.assertEqual(len(self.upward()), 1)

    def test_a_restart_between_staging_and_sending_sends_the_staged_message(self):
        one = self.obligation(self.completed())
        staged = self.channel.stage(one)["messageId"]
        self.tick()
        self.assertEqual([row["message_id"] for row in self.messages()], [staged])
        self.assertEqual(self.messages()[0]["state"], DISPATCHED)
        self.assertEqual(len(self.upward()), 1)


class ASupervisorWhoCannotBeWoken(DaemonChannelCase):
    def test_an_archived_supervisor_is_not_woken_and_the_report_waits_for_it(self):
        self.adapter.threads[SUPERVISOR].archived = True
        one = self.obligation(self.completed())
        self.tick()
        self.assertEqual(self.upward(), [])
        message = self.only_message()
        self.assertEqual((message["state"], message["hold_reason"]), (WITHHELD_PRE_SEND, None))
        standing = supervision.standing_for(self.store, self.linkage, PROJECT)["standing"]
        self.assertIn(one["obligationId"], [each["obligationId"] for each in standing],
                      "the obligation is kept, not dropped")

        self.tick(advance=5)
        self.assertEqual(self.upward(), [], "not retried inside its recheck interval")

        self.adapter.threads[SUPERVISOR].archived = False
        self.tick(advance=self.channel.policy.lifecycle_recheck_seconds + 1)
        self.assertEqual(len(self.upward()), 1)
        self.assertEqual(self.messages()[0]["state"], DISPATCHED)


class AParentWhoAlsoSendsByHand(DaemonChannelCase):
    def test_a_parent_who_sent_first_leaves_the_daemon_nothing_to_send(self):
        self.completed()
        message_id = self.channel.stage_standing(PROJECT)["staged"][0]["messageId"]
        self.assertEqual(self.channel.attempt(message_id, self.adapter)["deliveryState"],
                         DISPATCHED)
        report = self.tick(advance=3600)
        self.assertEqual(self.counts(report), (0, 0))
        self.assertEqual(len(self.upward()), 1)

    def test_the_daemon_sending_first_leaves_the_parent_nothing_to_send(self):
        self.completed()
        self.tick()
        message = self.only_message()
        again = self.channel.stage_standing(PROJECT)
        self.assertEqual([one["messageId"] for one in again["staged"]],
                         [message["message_id"]], "the parent's staging converges on it")
        self.assertIsNone(self.channel.attempt(message["message_id"], self.adapter))
        self.assertEqual(len(self.upward()), 1)

    def test_a_claim_the_parent_holds_is_not_taken_by_the_daemon(self):
        one = self.obligation(self.completed())
        message_id = self.channel.stage(one)["messageId"]
        self.channel._claim(message_id, now=self.clock.now(), owner="the parent by hand",
                            recipient=SUPERVISOR, resolution=self.channel.resolve(self.rid))
        report = self.tick()
        self.assertEqual(self.counts(report)[1], 0)
        self.assertEqual(self.upward(), [])
        self.assertEqual(self.channel.get(message_id)["state"], SENDING)


class TheDaemonCommandRunsThePass(DaemonChannelCase):
    """Through the command the installed service runs, not only through RelayDaemon."""

    def test_the_daemon_command_sends_what_a_silent_parent_owes(self):
        from .test_daemon_cadence import _Args, build_services
        from codex_session_relay.cli import cmd_daemon

        self.completed()
        services, _args = build_services(self, max_ticks=1)
        args = _Args(os.path.dirname(str(self.store.path)),
                     socket="/nonexistent-for-this-test", max_ticks=1)
        services = type(services)(args)
        self.addCleanup(services.close)
        services._adapter = self.adapter
        # The suite's one deliberate stub: the role policy is a file this suite does not write.
        services.supervisor_channel._settings = lambda task, runtime=None: {"authorized": task}
        result = cmd_daemon(services, args)
        self.assertEqual(len(result["ticks"]), 1)
        self.assertEqual(len(self.upward()), 1, "the daemon command sent the owed report")
        self.assertEqual(result["ticks"][0].get("supervisorSent"), 1)

class AReportWithNoAddresseeDoesNotHoldTheQueue(DaemonChannelCase):
    """Review 1 on 460d3bae: an older message nobody can be addressed with blocked the rest.

    The claim keeps per-recipient order: a message waits while an older one to the same task
    can go. An archived assignment releases the edge resolve() walks, so its unsent message
    was refused on every attempt and stayed claimable - the oldest message to its supervisor
    for ever, with every later report to that supervisor waiting behind it.
    """

    def test_a_report_from_an_archived_assignment_does_not_block_a_live_one(self):
        from codex_session_relay.models import TurnRef

        stale = self.channel.stage(self.obligation(self.completed()))["messageId"]
        self.registry.set_status(self.rid, "archived", actor="the parent archived it")

        other = self.register(issue_key="REL-2", dispatch_request_id="dispatch-2",
                              dispatch_turn_id="turn-dispatch-2")
        self.linkage.attach_issue(other["relationshipId"], PROJECT)
        path = self.artifact("second.txt", "the other deliverable")
        payload = self.ready_payload(other, [path],
                                     turn=TurnRef(channel_tests.CHILD, "turn-dispatch-2",
                                                  "completed"))
        self.accept(payload)
        report_module.record(
            self.store, self.clock, event_id=payload["eventId"],
            repository="thisisjun786/codex-relay-workflow", cxc_status=cxc.DONE,
            cxc_reason="every recorded criterion was proved", summary="the other work is done",
            next_action="merge", evidence=["pytest passed"])

        for _ in range(3):
            self.tick(advance=60)
        sent = [one for one in self.messages() if one["state"] == DISPATCHED]
        self.assertEqual(len(sent), 1, "the live assignment's report went up")
        self.assertEqual(len(self.upward()), 1)
        held = self.channel.get(stale)
        self.assertNotEqual(held["state"], DISPATCHED, "the archived one has nobody to go to")
        self.assertEqual(held["hold_reason"], "hierarchy_unresolved")

    def test_the_hold_is_released_when_the_hierarchy_names_the_message_again(self):
        stale = self.channel.stage(self.obligation(self.completed()))["messageId"]
        refusing = self.linkage.up

        def unresolvable(**kwargs):
            answer = refusing(**kwargs)
            return {**answer, "state": "unresolved", "levels": []}

        self.linkage.up = unresolvable
        self.tick()
        self.assertEqual(self.channel.get(stale)["hold_reason"], "hierarchy_unresolved")
        self.assertEqual(self.upward(), [])

        del self.linkage.up
        self.tick(advance=60)
        self.assertEqual(self.channel.get(stale)["state"], DISPATCHED)
        self.assertEqual(len(self.upward()), 1)
