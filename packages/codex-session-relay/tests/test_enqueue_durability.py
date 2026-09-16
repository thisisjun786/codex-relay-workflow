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
        # Past the refusal's backoff. A retry that fired immediately would hammer a refusal
        # that is usually still true a moment later.
        self.clock.advance(3600)
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

    def test_an_event_nobody_asked_to_send_is_not_resurrected(self):
        """Absence of a delivery row is not evidence that delivery was wanted and failed.

        An event emitted with --no-enqueue looks exactly like one whose queuing was refused,
        so recovery reads the recorded intent rather than guessing from what is missing.
        """
        _relationship, event_id = self.staged_completion()
        self.intake.resolve_staged(TurnRef(CHILD, "turn-dispatch-1", "completed"))
        self.assertEqual(self.intake.row(event_id)["stage"], "final")
        self.assertIsNone(self.delivery.find(event_id))

        report = self.daemon().tick(now=self.clock.now())

        self.assertIsNone(
            self.delivery.find(event_id),
            "recovery must not send something nobody asked it to send",
        )
        self.assertEqual(report.requeued, 0)

    def test_a_refused_event_records_an_intent_that_recovery_reads(self):
        _relationship, event_id = self.staged_completion()
        self.refuse_enqueue_once()
        self.daemon().tick(now=self.clock.now())
        intent = self.store.one(
            "SELECT * FROM delivery_intent WHERE event_id = ?", (event_id,),
        )
        self.assertIsNotNone(intent, "the refusal recorded that delivery was wanted")
        self.assertEqual(intent["attempts"], 1)
        self.assertGreater(intent["next_retry_at"], self.clock.now())

    def test_a_permanently_refused_intent_backs_off_instead_of_holding_its_slot(self):
        """Otherwise four unqueueable events keep every recovery slot forever."""
        relationship, event_id = self.staged_completion()
        self.refuse_enqueue_once()
        self.daemon().tick(now=self.clock.now())
        first = self.store.one(
            "SELECT * FROM delivery_intent WHERE event_id = ?", (event_id,),
        )
        # The relationship now excludes its own parent, so every retry is refused for good.
        self.registry.set_status(relationship["relationshipId"], "paused", actor="test")
        self.clock.advance(3600)
        self.daemon().tick(now=self.clock.now())
        second = self.store.one(
            "SELECT * FROM delivery_intent WHERE event_id = ?", (event_id,),
        )
        self.assertGreater(second["attempts"], first["attempts"])
        self.assertGreater(second["next_retry_at"], first["next_retry_at"])

    def test_the_backoff_survives_an_intent_that_is_refused_indefinitely(self):
        """base * 2 ** (attempts - 1) was computed and only then clamped.

        A relationship that stays paused has no cap on its attempt count, and around the
        1025th refusal the product is an integer too large to convert to a float. The
        OverflowError escaped the handler meant to absorb the refusal, the transaction
        rolled back with the intent still due, and every later tick failed the same way.
        """
        ceiling = self.delivery.policy.presend_max_seconds
        for attempts in (1, 2, 10, 1024, 1025, 5000, 10 ** 6):
            delay = self.delivery._backoff(attempts)
            self.assertIsInstance(delay, (int, float))
            self.assertLessEqual(delay, ceiling)
            self.assertGreaterEqual(delay, 0)
        self.assertEqual(self.delivery._backoff(10 ** 6), ceiling)
        self.assertLess(
            self.delivery._backoff(1), self.delivery._backoff(4),
            "and it still backs off before the ceiling",
        )

    def test_an_intent_refused_past_the_overflow_point_still_records_its_retry(self):
        """The end-to-end shape: the write must survive, not just the arithmetic."""
        relationship, event_id = self.staged_completion()
        self.refuse_enqueue_once()
        self.daemon().tick(now=self.clock.now())
        self.registry.set_status(relationship["relationshipId"], "paused", actor="test")
        with self.store.transaction() as db:
            db.execute(
                "UPDATE delivery_intent SET attempts = 1024, next_retry_at = 0"
                " WHERE event_id = ?", (event_id,),
            )

        self.clock.advance(3600)
        report = self.daemon().tick(now=self.clock.now())

        intent = self.store.one(
            "SELECT * FROM delivery_intent WHERE event_id = ?", (event_id,),
        )
        self.assertEqual(intent["attempts"], 1025)
        self.assertEqual(
            intent["next_retry_at"],
            self.clock.now() + self.delivery.policy.presend_max_seconds,
        )
        self.assertIsNotNone(report)
