"""The bounded daemon: automatic invocation, and silence when nothing happened."""

import unittest

from codex_session_relay.daemon import RelayDaemon, SingleInstance
from codex_session_relay.transport import DISPATCHED

from .support import CHILD, DISPATCH_TURN, PARENT, DeliveryTestCase


class DaemonTestCase(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        self.daemon = RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, self.adapter, clock=self.clock,
        )

    def journal_rows(self) -> int:
        return self.store.one("SELECT COUNT(*) AS c FROM journal")["c"]


class Bounds(DaemonTestCase):
    def test_a_stop_callable_alone_is_not_a_bound(self):
        with self.assertRaises(ValueError) as caught:
            self.daemon.run(stop=lambda: False)
        self.assertIn("not a bound", str(caught.exception))

    def test_a_tick_count_bounds_the_run(self):
        reports = self.daemon.run(max_ticks=3)
        self.assertEqual(len(reports), 3)

    def test_a_deadline_bounds_the_run(self):
        reports = self.daemon.run(deadline=self.clock.now() - 1)
        self.assertEqual(reports, [])

    def test_stop_can_end_a_bounded_run_early(self):
        calls = {"n": 0}

        def stop():
            calls["n"] += 1
            return calls["n"] > 2

        reports = self.daemon.run(max_ticks=10, stop=stop)
        self.assertEqual(len(reports), 2)


class QuietTicks(DaemonTestCase):
    def test_two_consecutive_unchanged_ticks_write_no_journal_rows(self):
        """The assertion is on the table, so an unanticipated journal source fails it too."""
        self.register()
        self.daemon.tick()
        after_first = self.journal_rows()
        report = self.daemon.tick()
        self.assertEqual(self.journal_rows(), after_first, "an unchanged tick wrote nothing")
        self.assertTrue(report.quiet)
        # quiet folds seven counters and leaves these two out, so a tick that suppressed a send
        # or wrote a note is quiet and writes no journal row either. Neither assertion above
        # covers them; they are named here rather than implied.
        self.assertEqual(report.skipped, 0, "a suppressed send is not an unchanged tick")
        self.assertEqual(report.notes, [], f"a tick that had something to say: {report.notes}")

    def test_a_tick_that_changed_something_is_not_quiet(self):
        _relationship, event_id = self.queued_event()
        report = self.daemon.tick()
        self.assertFalse(report.quiet)
        self.assertEqual(report.delivered, 1)

    def test_an_already_observed_terminal_turn_is_not_reobserved(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="failed")
        self.daemon.tick()
        rows = self.journal_rows()
        self.daemon.tick()
        self.assertEqual(self.journal_rows(), rows)


class ReconcileGate(DaemonTestCase):
    def _uncertain(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("turn_start_fail")
        record = self.attempt(event_id)
        return record["requestId"]

    def test_a_failed_read_forces_reconciliation_on_a_later_unchanged_tick(self):
        """The three-tick sequence: succeed, fail, then succeed with the SAME fingerprint.

        Preserving the fingerprint alone would skip the third tick and silently drop the
        reconciliation the failed tick owed. The retry_required flag records the debt.
        """
        request_id = self._uncertain()
        self.daemon.tick()
        gate = self.store.one(
            "SELECT * FROM reconcile_gate WHERE request_id = ?", (request_id,)
        )
        self.assertEqual(gate["retry_required"], 0, "a clean pass clears the debt")

        # Tick two: both the gate read and reconciliation's own reads fail, so no progress is
        # made and the debt must survive.
        self.adapter.fail_reads("recipient_fingerprint", "get_operation", "find_token")
        self.daemon.tick()
        gate = self.store.one(
            "SELECT * FROM reconcile_gate WHERE request_id = ?", (request_id,)
        )
        self.assertEqual(gate["retry_required"], 1, "a failed read records that work is owed")

        self.adapter.read_failures.clear()
        before = self.store.one(
            "SELECT reconciled_at FROM attempts WHERE request_id = ?", (request_id,)
        )["reconciled_at"]
        self.clock.advance(1)
        report = self.daemon.tick()
        after = self.store.one(
            "SELECT reconciled_at FROM attempts WHERE request_id = ?", (request_id,)
        )["reconciled_at"]
        self.assertEqual(report.reconciled, 1, "the owed reconciliation ran")
        self.assertNotEqual(before, after)

    def test_an_unchanged_attempt_is_skipped_rather_than_reconciled(self):
        self._uncertain()
        self.daemon.tick()
        report = self.daemon.tick()
        self.assertEqual(report.reconciled, 0)
        self.assertEqual(report.skipped, 1)

    def test_a_failed_fingerprint_read_reconciles_instead_of_skipping(self):
        """A read that failed is not a reading, so it can never justify doing nothing."""
        self._uncertain()
        self.daemon.tick()
        self.adapter.fail_reads("recipient_fingerprint")
        report = self.daemon.tick()
        self.assertEqual(report.skipped, 0)
        self.assertEqual(report.reconciled, 1)


class AutomaticInvocation(DaemonTestCase):
    """The end-to-end shape: a child emits from its own live turn and the parent is woken once."""

    def test_a_staged_claim_is_finalized_and_delivered_exactly_once(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="inProgress")
        path = self.artifact("out.txt", "work in progress")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn("inProgress")
        )
        stored = self.accept(payload)
        self.assertEqual(stored["_stage"], "staged")

        # Nothing goes out while the child's turn is still running.
        first = self.daemon.tick()
        self.assertEqual(first.delivered, 0)
        self.assertEqual(self.adapter.sends, [])

        # The child's turn ends normally. The daemon observes it independently.
        self.adapter.finish_turn(CHILD, DISPATCH_TURN, "completed")
        second = self.daemon.tick()
        self.assertEqual(second.observed, 1)
        self.assertEqual(second.delivered, 1)
        self.assertEqual(len(self.adapter.sends), 1, "exactly one parent request")
        self.assertEqual(self.delivery_row(payload["eventId"])["state"], DISPATCHED)

        # Further ticks add nothing.
        third = self.daemon.tick()
        self.assertEqual(third.delivered, 0)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_a_failed_turn_suppresses_the_staged_claim_instead_of_delivering_it(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="inProgress")
        path = self.artifact("out.txt", "never finished")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn("inProgress")
        )
        self.accept(payload)
        self.adapter.finish_turn(CHILD, DISPATCH_TURN, "failed")
        self.daemon.tick()

        # The staged readiness claim is suppressed: a failed execution is never promoted.
        self.assertFalse(self.intake.deliverable(payload["eventId"]))
        row = self.intake.row(payload["eventId"])
        self.assertEqual(row["stage"], "suppressed")

        # What the parent IS told is that the child failed. Storing that without delivering it
        # would leave a parent waiting on a verdict it can never reach.
        sent = [message for _request, _thread, message, _outcome in self.adapter.sends]
        self.assertEqual(len(sent), 1)
        self.assertNotIn(payload["eventId"], sent[0], "the suppressed claim was not delivered")
        failure = self.store.one(
            "SELECT * FROM events WHERE producer = 'daemon_observation'"
        )
        self.assertEqual(failure["outcome"], "failed")
        self.assertIn(failure["event_id"], sent[0])

    def test_an_absent_anchor_is_not_counted_as_a_healthy_poll(self):
        """observation_health reads poll freshness and settlement, nothing else.

        Recording an absent turn as a successful poll refreshed last_polled_at on every tick,
        so an anchor the host exhaustively reports gone - one that can never settle - stayed
        healthy indefinitely. Process alive is not the same as work progressing, and neither
        is a poll that found nothing.
        """
        self.register()  # the anchor turn is never created on the host

        self.daemon.tick()

        row = self.store.one(
            "SELECT last_status, last_polled_at, last_error FROM poll_observations"
            "  WHERE turn_id = ?", (DISPATCH_TURN,),
        )
        self.assertIsNotNone(row, "the anchor was never polled at all")
        self.assertEqual(row["last_status"], "absent")
        self.assertIsNone(
            row["last_polled_at"],
            "an absent anchor was recorded as a successful poll, so health never decays",
        )
        self.assertIsNotNone(row["last_error"])

    def test_a_pause_during_the_host_read_does_not_retire_a_failed_turn(self):
        """The scheduler selects active assignments and then reads the host.

        A pause landing in between says nothing about what the turn did. Recording a
        settlement anyway retires it - _worth_polling drops it from every later tick - while
        the synthesized receipt that is the only carrier of that outcome was never written. A
        resume then finds nothing left to observe and the parent waits for a verdict forever.
        """
        from unittest import mock

        from codex_session_relay.errors import RefusalReason, RegistrationError

        self.register()
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="failed")

        def paused(*_args, **_kwargs):
            raise RegistrationError(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                "paused while the host read was in flight",
            )

        with mock.patch.object(self.intake, "daemon_observation", side_effect=paused):
            report = self.daemon.tick()

        self.assertEqual(report.observed, 0)
        self.assertIsNone(
            self.store.one(
                "SELECT 1 FROM assignment_settlements WHERE turn_id = ?", (DISPATCH_TURN,),
            ),
            "the turn was retired while its outcome was never written",
        )

        # And once the assignment is active again, the very next tick produces the receipt.
        resumed = self.daemon.tick()

        self.assertEqual(resumed.observed, 1)
        self.assertIsNotNone(
            self.store.one("SELECT 1 FROM events WHERE producer = 'daemon_observation'"),
            "the outcome never reached the parent after the assignment resumed",
        )

    def test_a_synthesized_failure_is_persisted_and_delivered_as_separate_outcomes(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="failed")
        report = self.daemon.tick()
        self.assertEqual(report.observed, 1)
        stored = self.store.one("SELECT * FROM events WHERE producer = 'daemon_observation'")
        self.assertIsNotNone(stored, "persisted")
        self.assertEqual(report.delivered, 1, "and separately delivered")
        self.assertIsNone(stored["attempt"])
        self.assertEqual(stored["revision_hash"], "0" * 64)

    def test_a_claim_staged_on_a_later_admitted_turn_is_still_resolved(self):
        """Polling only the anchor would leave a multi-turn loop's claim staged forever."""
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="completed")
        self.adapter.start_turn(CHILD, turn_id="turn-loop-4", status="inProgress")
        path = self.artifact("out.txt", "finished on a later turn")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn("inProgress", turn="turn-loop-4")
        )
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn("inProgress", turn="turn-loop-4"),
            continuation={"anchorTurnId": DISPATCH_TURN, "actor": "child-loop",
                          "reason": "cycle 4 of this execution"},
        )
        self.adapter.finish_turn(CHILD, "turn-loop-4", "completed")
        report = self.daemon.tick()
        self.assertEqual(report.delivered, 1)
        self.assertEqual(len(self.adapter.sends), 1)

    def test_every_send_goes_through_the_delivery_claim(self):
        _relationship, event_id = self.queued_event()
        calls = []
        original = self.delivery.attempt

        def watched(*args, **kwargs):
            calls.append(args[0])
            return original(*args, **kwargs)

        self.delivery.attempt = watched
        self.daemon.tick()
        self.assertEqual(calls, [event_id])
        self.assertEqual(len(self.adapter.sends), 1)


class Instance(DaemonTestCase):
    def test_a_second_daemon_refuses_rather_than_racing(self):
        with SingleInstance(self.tmp):
            with self.assertRaises(RuntimeError):
                with SingleInstance(self.tmp):
                    pass


if __name__ == "__main__":
    unittest.main()
