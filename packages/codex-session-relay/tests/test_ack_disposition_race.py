"""Two processes acknowledging one event, and which of them is allowed to win.

acknowledge() reads the existing acknowledgement BEFORE its write transaction and returns early
only when that read already shows a verified one. Two processes therefore both pass that read.
Whichever commits second used to overwrite the first, and the taxonomy made the rejection the
one that overwrote: evaluate() reports duplicate_event for a settled event, the conflict guard
refused it only for accepted=True, and the rejection path carried on into the upsert. A verified
acceptance became a verified rejection, decided by scheduling.

The interleaving here is forced, not raced. Each process gets its own connection to the same
store, and the winning one commits inside the window acknowledge() documents as its
pre-transaction host read. Nothing sleeps and nothing depends on an ordering the test does not
itself impose, so a failure is the defect rather than a slow machine. The transport's
per-recipient concurrency moved when acknowledgements arrive; it did not move where this window
is.
"""

import json

from codex_session_relay import identity
from codex_session_relay.transport import ACKNOWLEDGED

from .support import CHILD, PARENT, DeliveryTestCase


class CompetingAcknowledgements(DeliveryTestCase):
    def dispatched(self):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        return event_id

    def rival(self):
        """A second process: its own services over its own connection to the same store."""
        from codex_session_relay.ack import AckService
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.receipts import ReceiptIntake
        from codex_session_relay.registry import Registry
        from codex_session_relay.store import Store

        store = Store(self.store.path)
        self.addCleanup(store.close)
        registry = Registry(store, self.clock)
        intake = ReceiptIntake(store, registry, self.clock)
        delivery = DeliveryService(store, registry, intake, self.clock)
        return AckService(store, registry, intake, delivery, self.clock)

    def acknowledge(self, service, event_id, turn_id, **kwargs):
        return service.acknowledge(
            event_id, ack_turn_id=turn_id,
            ack_proof=identity.ack_proof(event_id, turn_id), adapter=self.adapter, **kwargs
        )

    def racing(self, event_id, winner, turn_id, **kwargs):
        """Commit the winning acknowledgement inside the loser's pre-transaction host read.

        The flag is set before the nested call so the winner's own read_turn takes the plain
        path; without it the hook would re-enter itself.
        """
        original = self.adapter.read_turn
        state = {"raced": False, "won": None}

        def commit_then_read(thread_id, read_turn_id):
            if not state["raced"]:
                state["raced"] = True
                state["won"] = self.acknowledge(winner, event_id, turn_id, **kwargs)
            return original(thread_id, read_turn_id)

        self.adapter.read_turn = commit_then_read
        self.addCleanup(setattr, self.adapter, "read_turn", original)
        return state

    def stored(self, event_id):
        row = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
        return row, json.loads(row["record"])

    def test_a_losing_rejection_does_not_overwrite_a_verified_acceptance(self):
        event_id = self.dispatched()
        accepting = self.adapter.start_turn(
            PARENT, turn_id="accepting-turn", status="inProgress"
        )
        rejecting = self.adapter.start_turn(
            PARENT, turn_id="rejecting-turn", status="inProgress"
        )
        race = self.racing(event_id, self.rival(), accepting.turn_id, accepted=True)

        result = self.acknowledge(
            self.ack, event_id, rejecting.turn_id, accepted=False,
            rejection_reason="revision_mismatch",
        )

        self.assertTrue(race["raced"], "the acceptance never committed, so nothing was raced")
        self.assertEqual(race["won"]["_verified"], "verified")
        row, record = self.stored(event_id)
        self.assertTrue(record["accepted"])
        self.assertIsNone(record["rejectionReason"])
        self.assertEqual(row["ack_turn_id"], accepting.turn_id)
        self.assertEqual(row["verified"], "verified")
        self.assertEqual(row["accepted"], 1)
        # The loser is told what actually stands rather than what it asked for.
        self.assertTrue(result["accepted"])
        self.assertEqual(result["ackTurnId"], accepting.turn_id)
        self.assertTrue(result["_replay"], "the loser must be able to tell it did not win")
        self.assertEqual(result["_verified"], "verified")
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_a_losing_acceptance_does_not_overwrite_a_verified_rejection(self):
        """The same guard from the other side: settled is settled, whoever arrives second."""
        event_id = self.dispatched()
        rejecting = self.adapter.start_turn(
            PARENT, turn_id="rejecting-turn", status="inProgress"
        )
        accepting = self.adapter.start_turn(
            PARENT, turn_id="accepting-turn", status="inProgress"
        )
        race = self.racing(
            event_id, self.rival(), rejecting.turn_id, accepted=False,
            rejection_reason="revision_mismatch",
        )

        result = self.acknowledge(self.ack, event_id, accepting.turn_id, accepted=True)

        self.assertTrue(race["raced"])
        row, record = self.stored(event_id)
        self.assertFalse(record["accepted"])
        self.assertEqual(record["rejectionReason"], "revision_mismatch")
        self.assertEqual(row["ack_turn_id"], rejecting.turn_id)
        self.assertEqual(row["verified"], "verified")
        self.assertFalse(result["accepted"])
        self.assertEqual(result["ackTurnId"], rejecting.turn_id)
        self.assertTrue(result["_replay"])

    def test_the_losing_rejection_leaves_the_verdict_path_open(self):
        """What the overwrite actually cost: a verdict needs a verified ACCEPTANCE."""
        event_id = self.dispatched()
        accepting = self.adapter.start_turn(
            PARENT, turn_id="accepting-turn", status="inProgress"
        )
        rejecting = self.adapter.start_turn(
            PARENT, turn_id="rejecting-turn", status="inProgress"
        )
        self.racing(event_id, self.rival(), accepting.turn_id, accepted=True)
        self.acknowledge(
            self.ack, event_id, rejecting.turn_id, accepted=False,
            rejection_reason="revision_mismatch",
        )
        record = self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v1")
        self.assertEqual(record["verdict"], "verified")

    def test_a_sequential_second_disposition_is_unchanged(self):
        """No race at all: the pre-transaction read already answered this, and still does."""
        event_id = self.dispatched()
        accepting = self.adapter.start_turn(
            PARENT, turn_id="accepting-turn", status="inProgress"
        )
        rejecting = self.adapter.start_turn(
            PARENT, turn_id="rejecting-turn", status="inProgress"
        )
        self.acknowledge(self.ack, event_id, accepting.turn_id, accepted=True)
        result = self.acknowledge(
            self.ack, event_id, rejecting.turn_id, accepted=False,
            rejection_reason="revision_mismatch",
        )
        self.assertTrue(result["accepted"])
        _row, record = self.stored(event_id)
        self.assertTrue(record["accepted"])
        self.assertTrue(result["_replay"])
        self.assertEqual(result["_verified"], "verified")

    def test_an_unverified_acknowledgement_is_still_upgradable(self):
        """Only a VERIFIED acknowledgement is settled; a withheld one stays completable."""
        event_id = self.dispatched()
        turn = self.adapter.start_turn(
            PARENT, turn_id="parent-own-turn", status="inProgress"
        )
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True, adapter=None,
        )
        row, _record = self.stored(event_id)
        self.assertEqual(row["verified"], "unverified_turn")
        results = self.ack.verify_pending_acks(self.adapter)
        self.assertEqual([r["outcome"] for r in results], ["verified"])
