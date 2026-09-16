"""A stale event must be stopped before the send, not rejected after it.

Reproduced 2026-09-16: JUN-119 generation 2 events delivered after generation 3 started, and
JUN-100 generation 5 delivered after generation 7, every one rejected downstream as
disposition_conflict: stale_generation. They had been waiting on a busy parent, and the wait
ending was treated as permission to send.
"""

from codex_session_relay import identity
from codex_session_relay.currency import STALE_GENERATION, SUPERSEDED as SUPERSEDED_REVISION
from codex_session_relay.models import TurnRef
from codex_session_relay.transport import DEFERRED_BUSY, DISPATCHED, SUPERSEDED

from .support import CHILD, PARENT, DeliveryTestCase


class PreSendSupersession(DeliveryTestCase):
    def advance_generation(self, relationship, number):
        """Open the next generation, as a needs_changes verdict would."""
        turn_id = f"turn-dispatch-{number}"
        self.adapter.start_turn(CHILD, turn_id=turn_id, status="inProgress")
        return self.registry.open_generation(
            relationship["relationshipId"], dispatch_request_id=f"dispatch-{number}",
            reason="needs_changes_revision", dispatch_turn_id=turn_id,
        )

    def queued_outcome(self, outcome):
        """A queued completion carrying the given outcome, on generation 1."""
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        if outcome == "ready_for_review":
            path = self.artifact("out.txt", "the deliverable")
            payload = self.ready_payload(relationship, [path])
        else:
            payload = self.execution_payload(relationship, outcome)
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        return relationship, payload["eventId"]

    def test_an_outstanding_send_is_annotated_when_the_generation_advances(self):
        """attempt() rejects a non-claimable state, so nothing reached the pre-send check.

        A delivery that was sending or held_uncertain as a newer generation opened therefore
        got no delivery_supersession row at all. It could reconcile to dispatched and be
        presented as an ordinary current delivery rather than as history.
        """
        relationship, event_id = self.queued_outcome("ready_for_review")
        self.adapter.script("transport_unknown")
        self.attempt(event_id)
        self.assertEqual(self.delivery.get(event_id)["state"], "held_uncertain")

        self.advance_generation(relationship, 2)

        note = self.store.one(
            "SELECT * FROM delivery_supersession WHERE event_id = ?", (event_id,),
        )
        self.assertIsNotNone(note, "the outstanding send is no longer current and says so")
        self.assertEqual(note["reason"], STALE_GENERATION)
        self.assertEqual(
            self.delivery.get(event_id)["state"], "held_uncertain",
            "annotated, never rewritten: a lost response still has to be reconcilable",
        )
        item = [d for d in self.delivery.snapshot()["deliveries"]
                if d["eventId"] == event_id][0]
        self.assertIsNotNone(item["supersededNote"])

    def test_a_current_generations_delivery_is_left_alone(self):
        """The annotation must not mark the generation that is actually running."""
        relationship, event_id = self.queued_outcome("ready_for_review")
        self.adapter.script("transport_unknown")
        self.attempt(event_id)
        self.advance_generation(relationship, 2)
        self.store.db.execute(
            "DELETE FROM delivery_supersession WHERE event_id = ?", (event_id,),
        )

        self.advance_generation(relationship, 3)

        rows = self.store.all("SELECT event_id FROM delivery_supersession")
        self.assertEqual([r["event_id"] for r in rows], [event_id],
                         "only the older generation's outstanding send is annotated")
    def test_a_new_generation_with_no_revision_still_suppresses_the_old_outcome(self):
        """The exact reproduction: g3 opened, g3 empty, and a g2 event went out anyway."""
        relationship, event_id = self.queued_outcome("ready_for_review")
        self.advance_generation(relationship, 2)
        self.clock.advance(3600)

        record = self.attempt(event_id)

        self.assertEqual(self.adapter.sends, [], "a stale event must not reach the host")
        self.assertEqual(record["deliveryState"], SUPERSEDED)
        self.assertEqual(record["supersededReason"], STALE_GENERATION)
        self.assertEqual(self.delivery.get(event_id)["hold_reason"], STALE_GENERATION)

    def test_every_outcome_is_suppressed_not_only_a_reviewable_one(self):
        """blocked_needs_input carries no manifest, and failed carries none either."""
        for outcome in ("blocked_needs_input", "failed"):
            with self.subTest(outcome=outcome):
                self.setUp()
                relationship, event_id = self.queued_outcome(outcome)
                self.advance_generation(relationship, 2)
                self.clock.advance(3600)
                record = self.attempt(event_id)
                self.assertEqual(record["deliveryState"], SUPERSEDED)
                self.assertEqual(record["supersededReason"], STALE_GENERATION)
                self.assertEqual(self.adapter.sends, [])

    def test_a_busy_release_re_checks_the_generation_before_sending(self):
        """The waiting half of the reproduction: the parent was busy, then it was not."""
        relationship, event_id = self.queued_outcome("ready_for_review")
        self.adapter.set_status(PARENT, "active")
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.delivery.get(event_id)["state"], DEFERRED_BUSY)

        # The assignment moves on while the event waits.
        self.advance_generation(relationship, 2)
        self.adapter.set_status(PARENT, "idle")
        self.clock.advance(3600)

        record = self.attempt(event_id)

        self.assertEqual(record["deliveryState"], SUPERSEDED)
        self.assertEqual(self.adapter.sends, [], "the wait ending is not permission to send")

    def test_suppression_opens_no_generation_and_makes_no_transport_call(self):
        relationship, event_id = self.queued_outcome("ready_for_review")
        self.advance_generation(relationship, 2)
        before = len(self.registry.get(self._rid)["generations"])
        self.clock.advance(3600)

        self.attempt(event_id)

        self.assertEqual(len(self.registry.get(self._rid)["generations"]), before)
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.registry.get(self._rid)["executionGeneration"], 2)

    def test_a_newer_final_revision_supersedes_the_older_one_in_its_generation(self):
        relationship, older = self.queued_outcome("ready_for_review")
        newer_path = self.artifact("newer.txt", "the corrected deliverable")
        newer = self.ready_payload(relationship, [newer_path], attempt=2)
        older_hash = self.intake.row(older)["revision_hash"]
        self.accept(newer, )
        with self.store.transaction() as db:
            db.execute(
                "UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?",
                (older_hash, newer["eventId"]),
            )
        self.delivery.enqueue(newer["eventId"])
        self.clock.advance(3600)

        record = self.attempt(older)

        self.assertEqual(record["deliveryState"], SUPERSEDED)
        self.assertEqual(record["supersededReason"], SUPERSEDED_REVISION)
        # And the newest still goes.
        self.clock.advance(3600)
        sent = self.attempt(newer["eventId"])
        self.assertEqual(sent["deliveryState"], DISPATCHED)

    def test_a_staged_successor_does_not_destroy_the_older_events_chance(self):
        """A claim is not a replacement; if it fails, the older one is all there is."""
        relationship, older = self.queued_outcome("ready_for_review")
        path = self.artifact("staged.txt", "still being written")
        staged = self.ready_payload(
            relationship, [path], attempt=2,
            # On the anchor turn: a claim from a turn the generation never admitted is a
            # different refusal and would not exercise this rule.
            turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        )
        self.accept(staged)
        self.assertEqual(self.intake.row(staged["eventId"])["stage"], "staged")
        self.clock.advance(3600)

        record = self.attempt(older)

        self.assertEqual(
            record["deliveryState"], DISPATCHED,
            "a staged claim must not suppress the only finished revision there is",
        )

    def test_an_outstanding_send_is_annotated_rather_than_rewritten(self):
        """Rewriting it would make a lost response permanently unresolvable."""
        relationship, event_id = self.queued_outcome("ready_for_review")
        self.adapter.script("transport_unknown")
        self.clock.advance(3600)
        self.attempt(event_id)
        state = self.delivery.get(event_id)["state"]

        self.advance_generation(relationship, 2)
        self.delivery.mark_superseded(event_id, reason=STALE_GENERATION)

        self.assertEqual(
            self.delivery.get(event_id)["state"], state,
            "an unresolved send keeps its state so reconciliation can still settle it",
        )
        noted = self.store.one(
            "SELECT * FROM delivery_supersession WHERE event_id = ?", (event_id,),
        )
        self.assertIsNotNone(noted)
        self.assertEqual(noted["reason"], STALE_GENERATION)
