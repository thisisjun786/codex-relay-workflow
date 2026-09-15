"""Identity derivations against the frozen contract's canonical rendering."""

import hashlib
import unittest

from codex_session_relay import NO_DELIVERABLE, identity

REL = "rel-0123456789abcdef"


class Derivations(unittest.TestCase):
    def test_relationship_id_is_deterministic_over_task_ids(self):
        first = identity.relationship_id("p", "c", "JUN-1")
        self.assertEqual(first, identity.relationship_id("p", "c", "JUN-1"))
        self.assertRegex(first, r"^rel-[0-9a-f]{16}$")
        expected = "rel-" + hashlib.sha256(b"p|c|JUN-1").hexdigest()[:16]
        self.assertEqual(first, expected)

    def test_swapping_parent_and_child_changes_identity(self):
        self.assertNotEqual(
            identity.relationship_id("p", "c", "JUN-1"),
            identity.relationship_id("c", "p", "JUN-1"),
        )

    def test_separator_cannot_be_smuggled_into_a_field(self):
        # Every other field is valid, so only the embedded separator can cause the refusal.
        with self.assertRaises(ValueError):
            identity.relationship_id("p|c", "child", "JUN-1")
        with self.assertRaises(ValueError):
            identity.relationship_id("parent", "c|d", "JUN-1")
        with self.assertRaises(ValueError):
            identity.relationship_id("parent", "child", "JUN|1")

    def test_ready_branch_is_product_level(self):
        digest = "a" * 64
        value = identity.event_id(REL, 2, digest, "ready_for_review")
        self.assertEqual(value, hashlib.sha256(f"{REL}|2|{digest}|ready_for_review".encode()).hexdigest()[:32])
        # The turn and the attempt are deliberately absent, so one revision collapses.
        self.assertEqual(
            value,
            identity.event_id(REL, 2, digest, "ready_for_review", turn_id="other", attempt=7),
        )

    def test_execution_branch_keeps_two_interruptions_apart(self):
        first = identity.event_id(REL, 1, NO_DELIVERABLE, "interrupted", turn_id="t1", attempt=1)
        second = identity.event_id(REL, 1, NO_DELIVERABLE, "interrupted", turn_id="t2", attempt=1)
        self.assertNotEqual(first, second)

    def test_null_attempt_renders_as_the_literal_null(self):
        self.assertEqual(identity.render_attempt(None), "null")
        self.assertEqual(identity.render_attempt(3), "3")
        expected = hashlib.sha256(
            f"{REL}|1|failed|t1|null".encode()
        ).hexdigest()[:32]
        self.assertEqual(
            identity.event_id(REL, 1, NO_DELIVERABLE, "failed", turn_id="t1", attempt=None),
            expected,
        )

    def test_zero_attempt_renders_as_zero_not_null(self):
        # Zero is invalid per the schema; the derivation must not quietly treat it as null.
        self.assertEqual(identity.render_attempt(0), "0")

    def test_execution_branch_requires_the_turn_it_was_observed_on(self):
        with self.assertRaises(ValueError):
            identity.event_id(REL, 1, NO_DELIVERABLE, "failed")

    def test_ready_branch_refuses_the_no_deliverable_sentinel(self):
        with self.assertRaises(ValueError):
            identity.event_id(REL, 1, NO_DELIVERABLE, "ready_for_review")

    def test_request_id_shape_and_round_trip(self):
        event = "b" * 32
        value = identity.request_id(event, 2)
        self.assertEqual(value, "del-bbbbbbbbbbbb-a2")
        self.assertEqual(identity.parse_request_id(value), ("bbbbbbbbbbbb", 2))
        with self.assertRaises(ValueError):
            identity.request_id(event, 0)

    def test_ack_proof_uses_the_recipient_own_turn_id(self):
        event = "c" * 32
        proof = identity.ack_proof(event, "parent-turn-77")
        self.assertEqual(proof, hashlib.sha256(f"{event}|parent-turn-77".encode()).hexdigest())
        self.assertNotEqual(proof, identity.ack_proof(event, "parent-turn-78"))

    def test_revision_request_id_cannot_collide_with_a_completion_id(self):
        # The discriminator is not a contract outcome, so no completion derivation reaches it.
        revision = identity.revision_request_event_id(REL, "e" * 32, "verdict-turn-1")
        for outcome in identity.OUTCOMES:
            if outcome == "ready_for_review":
                continue
            for generation in (1, 2, 3):
                self.assertNotEqual(
                    revision,
                    identity.event_id(
                        REL, generation, NO_DELIVERABLE, outcome, turn_id="verdict-turn-1"
                    ),
                )

    def test_a_revision_id_is_stable_across_a_replayed_verdict(self):
        # Keyed on the corrected event, not on a generation counter, so replaying one verdict
        # resolves to the same revision instead of allocating another generation.
        first = identity.revision_request_event_id(REL, "a" * 32, "verdict-turn-1")
        again = identity.revision_request_event_id(REL, "a" * 32, "verdict-turn-1")
        self.assertEqual(first, again)
        self.assertNotEqual(
            first, identity.revision_request_event_id(REL, "b" * 32, "verdict-turn-1")
        )


if __name__ == "__main__":
    unittest.main()
