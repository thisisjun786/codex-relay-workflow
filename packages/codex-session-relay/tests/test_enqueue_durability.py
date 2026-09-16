"""JUN-167 P1: a terminal observation must not outlive the queuing it implies.

_settle_turn recorded the observation first and queued after, catching enqueue errors. If the
relationship was paused at that moment, or the database was briefly busy, the event was final
with no delivery row - and the next tick skipped the turn through _already_observed, so
resuming the relationship never helped. The event simply never reached anyone.
"""

from codex_session_relay.errors import DeliveryRefused, RefusalReason
from codex_session_relay.models import TurnRef

from .support import CHILD, DeliveryTestCase


class EnqueueDurability(DeliveryTestCase):
    def daemon(self):
        from codex_session_relay.daemon import RelayDaemon

        return RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, self.adapter, clock=self.clock,
        )

    def staged_completion(self):
        """A child claim emitted from inside its own live turn, which is therefore staged."""
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        turn = self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(
            relationship, [path],
            turn=TurnRef(CHILD, turn.turn_id, "inProgress"),
        )
        self.accept(payload)
        self.assertEqual(self.intake.row(payload["eventId"])["stage"], "staged")
        # The turn ends normally, which is what makes the staged claim deliverable.
        self.adapter.finish_turn(CHILD, turn.turn_id, status="completed")
        return relationship, payload["eventId"]

    def refuse_enqueue_once(self):
        """Exactly the shape a pause committed between selection and enqueue produces."""
        calls = []
        original = self.delivery.enqueue_in

        def refusing(db, event_id, **kwargs):
            calls.append(event_id)
            raise DeliveryRefused(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE, "paused between selection and enqueue",
            )

        self.delivery.enqueue_in = refusing
        return calls, original

    def test_an_event_refused_at_enqueue_is_still_delivered_later(self):
        _relationship, event_id = self.staged_completion()
        calls, original = self.refuse_enqueue_once()

        self.daemon().tick(now=self.clock.now())

        # The defect state, asserted explicitly rather than inferred.
        self.assertTrue(calls, "the tick must have reached the enqueue it then refused")
        self.assertEqual(self.intake.row(event_id)["stage"], "final")
        self.assertIsNotNone(
            self.store.one(
                "SELECT 1 FROM observations WHERE thread_id = ? AND turn_id = ?",
                (CHILD, "turn-dispatch-1"),
            ),
            "the observation was recorded",
        )
        self.assertIsNone(self.delivery.find(event_id), "and nothing was queued for it")

        # The refusal is over. Nothing about the turn has changed, so a loop that only
        # reconsiders unobserved turns will never look at this event again.
        self.delivery.enqueue_in = original
        self.daemon().tick(now=self.clock.now())

        queued = self.delivery.find(event_id)
        self.assertIsNotNone(
            queued, "a final event with no delivery row must be recoverable, not lost",
        )
        self.assertEqual(queued["event_id"], event_id, "the original event, not a new one")

    def test_a_transient_failure_leaves_no_observation_behind(self):
        """Durable refusal and transient failure are not the same and must not settle alike."""
        import sqlite3

        _relationship, event_id = self.staged_completion()

        def blow_up(db, event_id_, **kwargs):
            raise sqlite3.OperationalError("database is locked")

        self.delivery.enqueue_in = blow_up
        self.daemon().tick(now=self.clock.now())
        self.assertIsNone(
            self.store.one(
                "SELECT 1 FROM observations WHERE thread_id = ? AND turn_id = ?",
                (CHILD, "turn-dispatch-1"),
            ),
            "a transient failure rolls the observation back so the next tick retries cleanly",
        )

    def test_a_final_event_with_no_delivery_row_is_recovered_on_its_own(self):
        """Covers rows already stranded before this existed, not only new ones."""
        _relationship, event_id = self.staged_completion()
        self.intake.resolve_staged(TurnRef(CHILD, "turn-dispatch-1", "completed"))
        self.assertEqual(self.intake.row(event_id)["stage"], "final")
        self.assertIsNone(self.delivery.find(event_id))

        report = self.daemon().tick(now=self.clock.now())

        self.assertIsNotNone(self.delivery.find(event_id))
        self.assertGreaterEqual(report.requeued, 1)
