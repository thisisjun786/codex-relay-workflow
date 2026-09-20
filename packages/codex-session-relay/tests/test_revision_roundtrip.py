"""Correction lineage across the generation opened by a real needs_changes ruling."""

import json

from codex_session_relay.currency import CHAIN, FORK, UNKNOWN_PREDECESSOR, head_revision
from codex_session_relay.errors import RefusalReason
from codex_session_relay.identity import ack_proof

from .support import CHILD, PARENT
from .test_verification_currency import VerificationTestCase


class RevisionRoundtrip(VerificationTestCase):
    def request_correction(self, event_id):
        result = self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id=f"review-{event_id}",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix the output"}],
        )
        generation = result["nextExecutionGeneration"]
        request = self.store.one(
            "SELECT * FROM events WHERE relationship_id = ? AND execution_generation = ?"
            " AND outcome = 'revision_request'", (self._rid, generation),
        )
        self.attempt(request["event_id"])
        bound = self.ack.bind_dispatched_revision(request["event_id"])
        self.assertEqual(bound["anchorState"], "bound")
        self.assertEqual(self.adapter.sends[-1][1], CHILD)
        self.adapter.finish_turn(CHILD, bound["dispatchTurnId"])
        return json.loads(request["receipt"])

    def emit_correction(self, predecessor, text="corrected output"):
        relationship = self.registry.get(self._rid)
        generation = self.registry.generation(self._rid, relationship["executionGeneration"])
        turn = self.assigned_turn(turn=generation["dispatchTurnId"])
        payload = self.ready_payload(
            relationship, [self.artifact("out.txt", text)], turn=turn,
        )
        self.intake.accept_child_receipt(
            payload, observation=turn, supersedes_revision=predecessor,
        )
        return payload

    def acknowledge_correction(self, payload):
        event_id = payload["eventId"]
        self.delivery.enqueue(event_id)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id, ack_proof=ack_proof(event_id, turn.turn_id),
            accepted=True, adapter=self.adapter,
        )
        self.assertEqual(self.store.one(
            "SELECT verified FROM acks WHERE event_id = ?", (event_id,),
        )["verified"], "verified")

    def test_needs_changes_same_child_supersedes_then_verified_and_replay(self):
        first = self.acknowledged()
        request = self.request_correction(first)
        corrected = self.emit_correction(request["supersedesRevisionHash"])
        self.acknowledge_correction(corrected)
        final = self.ack.record_verdict(
            corrected["eventId"], verdict="verified", verdict_turn_id="review-corrected",
            findings=[{"id": "c1", "verdict": "verified"}],
        )
        self.assertEqual(final["verdict"], "verified")
        self.assertEqual(final["executionGeneration"], 2)
        self.assertEqual(head_revision(self.store.db, self._rid, 2)["evidence"], CHAIN)
        replay = self.ack.record_verdict(
            first, verdict="needs_changes", verdict_turn_id="replayed-review",
        )
        self.assertTrue(replay["_replay"])
        self.assertEqual(self.registry.get(self._rid)["executionGeneration"], 2)
        self.assertEqual(self.store.one(
            "SELECT COUNT(*) AS n FROM events WHERE outcome = 'revision_request'"
        )["n"], 1)

    def test_repeated_corrections_link_only_the_immediate_requested_result(self):
        first = self.acknowledged()
        request = self.request_correction(first)
        second = self.emit_correction(request["supersedesRevisionHash"], "second output")
        self.acknowledge_correction(second)
        request2 = self.request_correction(second["eventId"])
        third = self.emit_correction(request2["supersedesRevisionHash"], "third output")
        self.acknowledge_correction(third)
        result = self.ack.record_verdict(
            third["eventId"], verdict="verified", verdict_turn_id="review-third",
            findings=[{"id": "c1", "verdict": "verified"}],
        )
        self.assertEqual(result["executionGeneration"], 3)

    def test_manual_generation_does_not_authorize_a_historical_predecessor(self):
        first = self.acknowledged()
        self.advance_generation()
        self.emit_correction(self.intake.row(first)["revision_hash"])
        self.assertEqual(head_revision(self.store.db, self._rid, 2)["evidence"],
                         UNKNOWN_PREDECESSOR)

    def test_unknown_hash_remains_ambiguous_after_correction_request(self):
        self.request_correction(self.acknowledged())
        self.emit_correction("f" * 64)
        self.assertEqual(head_revision(self.store.db, self._rid, 2)["evidence"],
                         UNKNOWN_PREDECESSOR)

    def test_two_corrections_of_the_requested_result_are_still_a_fork(self):
        request = self.request_correction(self.acknowledged())
        one = self.emit_correction(request["supersedesRevisionHash"], "one")
        self.emit_correction(request["supersedesRevisionHash"], "two")
        head = head_revision(self.store.db, self._rid, 2)
        self.assertEqual(head["evidence"], FORK)
        self.assertIsNone(head["eventId"])
        self.assertIn(one["eventId"], head["competitors"])

    def test_old_unruled_event_is_not_made_current_by_correction(self):
        first = self.acknowledged()
        # Same-generation successor is the result the parent actually asks to fix.
        one_hash = self.intake.row(first)["revision_hash"]
        second = self.emit_correction(one_hash, "successor")
        self.acknowledge_correction(second)
        request = self.request_correction(second["eventId"])
        self.emit_correction(request["supersedesRevisionHash"])
        self.assertRefused(RefusalReason.STALE_GENERATION, lambda: self.ack.record_verdict(
            first, verdict="verified", verdict_turn_id="late-review",
        ))

    def test_earlier_unrequested_result_is_not_an_anchor(self):
        first = self.acknowledged()
        first_hash = self.intake.row(first)["revision_hash"]
        second = self.emit_correction(first_hash, "successor")
        self.acknowledge_correction(second)
        self.request_correction(second["eventId"])
        self.emit_correction(first_hash)
        self.assertEqual(head_revision(self.store.db, self._rid, 2)["evidence"],
                         UNKNOWN_PREDECESSOR)

    def test_other_relationships_requested_result_is_not_an_anchor(self):
        first = self.acknowledged(text="first relationship output")
        foreign_hash = self.intake.row(first)["revision_hash"]
        self.request_correction(first)
        other = self.register(issue_key="REL-OTHER", dispatch_request_id="other-assignment",
                              recipients=[PARENT, CHILD])
        self._rid = other["relationshipId"]
        initial = self.emit_correction(None, "other relationship output")
        self.acknowledge_correction(initial)
        self.request_correction(initial["eventId"])
        self.emit_correction(foreign_hash)
        self.assertEqual(head_revision(self.store.db, self._rid, 2)["evidence"],
                         UNKNOWN_PREDECESSOR)

    def test_current_generation_chain_can_extend_the_correction(self):
        request = self.request_correction(self.acknowledged())
        corrected = self.emit_correction(request["supersedesRevisionHash"], "first fix")
        latest = self.emit_correction(corrected["revisionHash"], "refined fix")
        self.acknowledge_correction(latest)
        final = self.ack.record_verdict(
            latest["eventId"], verdict="verified", verdict_turn_id="final-review",
            findings=[{"id": "c1", "verdict": "verified"}],
        )
        self.assertEqual(final["verdict"], "verified")

    def test_suppressed_request_cannot_authorize_a_predecessor(self):
        request = self.request_correction(self.acknowledged())
        # Exercise a withdrawn request without erasing its durable audit history.
        with self.store.transaction() as db:
            db.execute("UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?",
                       (request["eventId"],))
        self.emit_correction(request["supersedesRevisionHash"])
        self.assertEqual(head_revision(self.store.db, self._rid, 2)["evidence"],
                         UNKNOWN_PREDECESSOR)


class TheCorrectionIsVisibleWhileItIsQueued(RevisionRoundtrip):
    """The queued correction is what everyone is waiting for, and it was invisible.

    head_revision only ever names a ready_for_review event, so between a needs_changes verdict
    and the child's re-emit the new generation has no head. A projection anchored only on that
    head would report the PREVIOUS generation's acknowledgement beside nothing at all, and an
    operator reading it could not see that a correction had been queued to the child.
    """

    def projection(self):
        from codex_session_relay.assignment import AssignmentView

        return AssignmentView(self.store, self.registry, self.clock).state(
            self._rid
        )["projection"]

    def test_the_correction_gets_its_own_anchor_in_the_new_generation(self):
        first = self.acknowledged()
        request = self.request_correction(first)

        projection = self.projection()
        correction = projection["correction"]
        self.assertEqual(correction["eventId"], request["eventId"])
        self.assertEqual(correction["executionGeneration"], 2)
        self.assertEqual(
            correction["delivery"]["state"], "dispatched",
            "request_correction attempts the send, so the correction has really gone out",
        )

    def test_the_old_acknowledgement_is_not_reported_beside_the_new_delivery(self):
        """The mixture this anchoring exists to prevent."""
        first = self.acknowledged()
        request = self.request_correction(first)

        projection = self.projection()
        # The completion anchor has no event in generation 2: the child has not re-emitted.
        self.assertIsNone(projection["completion"]["eventId"])
        self.assertIsNone(projection["completion"]["ack"])
        self.assertIn("no such event", projection["completion"]["detail"])
        # And the correction carries generation 2's own delivery, not generation 1's.
        self.assertEqual(projection["correction"]["eventId"], request["eventId"])
        self.assertNotEqual(projection["correction"]["eventId"], first)

    def test_once_the_child_re_emits_both_anchors_are_in_the_same_generation(self):
        first = self.acknowledged()
        request = self.request_correction(first)
        corrected = self.emit_correction(request["supersedesRevisionHash"])

        projection = self.projection()
        self.assertEqual(projection["completion"]["eventId"], corrected["eventId"])
        self.assertEqual(projection["completion"]["executionGeneration"], 2)
        self.assertEqual(projection["correction"]["executionGeneration"], 2)

