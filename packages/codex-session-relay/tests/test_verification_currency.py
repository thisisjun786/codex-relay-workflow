"""Verification currency, canonical criteria, declared lineage and the deferred acknowledgement.

The three cases at the top are the ones the coordinator reproduced against the delivered core:
a verdict of verified was accepted for an already acknowledged OLD event after the generation
advanced, after a different ready revision was accepted in the same generation, and after the
relationship was paused. Each of them is now a refusal, and each test says which one it is.
"""

import inspect
import unittest

from codex_session_relay import identity
from codex_session_relay.criteria import CriteriaService, set_digest
from codex_session_relay.currency import (
    CHAIN,
    CYCLE,
    FORK,
    SOLE,
    UNKNOWN_PREDECESSOR,
    head_revision,
)
from codex_session_relay.errors import RefusalReason
from codex_session_relay.transport import ACKNOWLEDGED

from .support import CHILD, PARENT, DeliveryTestCase


class VerificationTestCase(DeliveryTestCase):
    def acknowledged(self, *, recipients=None, turn_id="ack-turn", text="the deliverable"):
        recipients = recipients if recipients is not None else [PARENT, CHILD]
        _relationship, event_id = self.queued_event(recipients=recipients, text=text)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id=turn_id, status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        return event_id

    def advance_generation(self, *, request="newer-execution", turn="newer-turn"):
        return self.registry.open_generation(
            self._rid, dispatch_request_id=request, reason="needs_changes_revision",
            dispatch_turn_id=turn,
        )

    def second_revision(self, text="a different revision", **kwargs):
        _relationship, event_id = self.ready_event(register=False, text=text, **kwargs)
        return event_id


class VerdictCurrency(VerificationTestCase):
    def test_generation_advanced_refuses_verified(self):
        """Baseline case 1. The old event is no longer what the assignment stands on."""
        event_id = self.acknowledged()
        self.advance_generation()
        self.assertRefused(
            RefusalReason.STALE_GENERATION,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_new_revision_same_generation_refuses_verified(self):
        """Baseline case 2. Two revisions, neither declared, so neither is the head."""
        event_id = self.acknowledged()
        self.second_revision()
        self.assertRefused(
            RefusalReason.REVISION_AMBIGUOUS,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_superseded_revision_refuses_verified(self):
        """The declared case: the child said what replaced it, so the reason is precise."""
        event_id = self.acknowledged()
        first = self.intake.row(event_id)["revision_hash"]
        path = self.artifact("out.txt", "the corrected revision")
        payload = self.ready_payload(self.registry.get(self._rid), [path])
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn(), supersedes_revision=first
        )
        self.assertRefused(
            RefusalReason.SUPERSEDED_REVISION,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_paused_relationship_refuses_every_verdict(self):
        """Baseline case 3. A paused assignment is not a thing to rule on at all."""
        event_id = self.acknowledged()
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        for verdict in ("verified", "needs_changes", "unverified", "aborted"):
            self.assertRefused(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                lambda v=verdict: self.ack.record_verdict(
                    event_id, verdict=v, verdict_turn_id="v1", reason="stopping"
                ),
            )

    def test_generation_advanced_after_a_callers_preflight_read_refuses(self):
        """A decision read before the write can be raced; this one is read inside it."""
        event_id = self.acknowledged()
        # A caller reads the world and concludes this event is current.
        self.assertEqual(self.registry.get(self._rid)["executionGeneration"], 1)
        # The world moves before the caller writes.
        self.advance_generation()
        self.assertRefused(
            RefusalReason.STALE_GENERATION,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_needs_changes_on_a_stale_event_is_refused(self):
        """A correction about an outdated artifact is not a correction anyone can use."""
        event_id = self.acknowledged()
        self.advance_generation()
        self.assertRefused(
            RefusalReason.STALE_GENERATION,
            lambda: self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v1",
                findings=[{"id": "c1", "verdict": "needs_changes", "note": "still wrong"}],
            ),
        )

    def test_unverified_on_a_stale_event_records_the_currency_it_was_decided_under(self):
        """unverified completes nothing and advances nothing, so it is not currency-gated."""
        event_id = self.acknowledged()
        self.advance_generation()
        record = self.ack.record_verdict(
            event_id, verdict="unverified", verdict_turn_id="v1", reason="could not reach it"
        )
        self.assertEqual(record["verdict"], "unverified")
        context = self.store.one(
            "SELECT * FROM verdict_context WHERE event_id = ?", (event_id,)
        )
        self.assertEqual(context["currency"], "stale_generation")

    def test_replay_returns_the_historical_verdict_marked_replay(self):
        event_id = self.acknowledged()
        first = self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v1",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix it"}],
        )
        again = self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v2")
        self.assertFalse(first.get("_replay"))
        self.assertTrue(again.get("_replay"))
        # The replay returns what was DECIDED, not what was asked for a second time.
        self.assertEqual(again["verdict"], "needs_changes")
        self.assertEqual(again["verdictTurnId"], "v1")

    def test_no_parameter_can_bypass_the_currency_check(self):
        """An override on the public API writes the exact stale completion this guard prevents."""
        parameters = set(inspect.signature(self.ack.record_verdict).parameters)
        self.assertNotIn("require_currency", parameters)
        self.assertFalse(
            [p for p in parameters if "bypass" in p or "force" in p or "skip" in p]
        )


class LineageGraph(VerificationTestCase):
    def head(self, generation=1):
        return head_revision(self.store.db, self._rid, generation)

    def test_single_revision_is_sole(self):
        self.acknowledged()
        self.assertEqual(self.head()["evidence"], SOLE)

    def test_declared_chain_resolves_to_its_tip(self):
        first_event = self.acknowledged()
        first = self.intake.row(first_event)["revision_hash"]
        path = self.artifact("out.txt", "second revision")
        payload = self.ready_payload(self.registry.get(self._rid), [path])
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn(), supersedes_revision=first
        )
        head = self.head()
        self.assertEqual(head["evidence"], CHAIN)
        self.assertEqual(head["eventId"], payload["eventId"])

    def test_two_undeclared_revisions_are_a_fork(self):
        self.acknowledged()
        self.second_revision()
        head = self.head()
        self.assertEqual(head["evidence"], FORK)
        self.assertIsNone(head["eventId"])
        self.assertEqual(len(head["competitors"]), 2)

    def test_two_revisions_declaring_one_predecessor_are_a_fork(self):
        first_event = self.acknowledged()
        first = self.intake.row(first_event)["revision_hash"]
        for text in ("branch one", "branch two"):
            path = self.artifact("out.txt", text)
            payload = self.ready_payload(self.registry.get(self._rid), [path])
            self.intake.accept_child_receipt(
                payload, observation=self.assigned_turn(), supersedes_revision=first
            )
        self.assertEqual(self.head()["evidence"], FORK)

    def test_unknown_predecessor_is_ambiguous_not_a_root(self):
        """Reading an unknown predecessor as no predecessor is how a late arrival becomes root."""
        self.acknowledged()
        path = self.artifact("out.txt", "claims to replace something we never saw")
        payload = self.ready_payload(self.registry.get(self._rid), [path])
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn(), supersedes_revision="f" * 64
        )
        self.assertEqual(self.head()["evidence"], UNKNOWN_PREDECESSOR)

    def test_a_revision_cannot_declare_itself(self):
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        path = self.artifact("out.txt", "self referential")
        payload = self.ready_payload(relationship, [path])
        self.assertRefused(
            RefusalReason.REVISION_LINEAGE_INVALID,
            lambda: self.intake.accept_child_receipt(
                payload, observation=self.assigned_turn(),
                supersedes_revision=payload["revisionHash"],
            ),
        )

    def test_a_cycle_is_ambiguous(self):
        first_event = self.acknowledged()
        first_hash = self.intake.row(first_event)["revision_hash"]
        path = self.artifact("out.txt", "second revision")
        second = self.ready_payload(self.registry.get(self._rid), [path])
        self.intake.accept_child_receipt(
            second, observation=self.assigned_turn(), supersedes_revision=first_hash
        )
        # Close the loop by hand: the first revision declares the second as its predecessor.
        with self.store.transaction() as db:
            db.execute(
                "UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?",
                (second["revisionHash"], first_event),
            )
        self.assertEqual(self.head()["evidence"], CYCLE)

    def test_arrival_order_changes_no_outcome(self):
        """The same declared graph must answer the same way whichever side arrived first."""
        self.acknowledged()
        self.second_revision()
        forward = self.head()

        self.doCleanups()
        self.setUp()
        self.acknowledged(text="a different revision")
        self.second_revision(text="the deliverable")
        reversed_order = self.head()

        self.assertEqual(forward["evidence"], reversed_order["evidence"])
        self.assertEqual(forward["eventId"], reversed_order["eventId"])


class CriteriaCoverage(VerificationTestCase):
    SET = [
        {"id": "c1", "title": "the endpoint returns the agreed shape"},
        {"id": "c2", "title": "a malformed request is refused"},
    ]

    def register_criteria(self, entries=None):
        return self.criteria_service().register(
            self._rid, entries or self.SET, source_ref="https://linear.app/doc/1"
        )

    def reviewed(self, text="the deliverable"):
        """The ordinary managed flow: criteria registered, then the review claimed."""
        event_id = self.acknowledged(text=text)
        self.register_criteria()
        self.ack.claim_verification(event_id, turn_id="ack-turn")
        return event_id

    def criteria_service(self):
        return CriteriaService(self.store, self.clock)

    def test_managed_assignment_without_criteria_refuses_verified(self):
        event_id = self.acknowledged()
        self.criteria_service().set_mode(self._rid, "managed")
        self.assertRefused(
            RefusalReason.CRITERIA_UNREGISTERED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_legacy_relationship_keeps_the_delivered_behaviour(self):
        event_id = self.acknowledged()
        record = self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v1")
        self.assertEqual(record["verdict"], "verified")

    def test_verified_requires_every_required_criterion(self):
        event_id = self.reviewed()
        self.assertRefused(
            RefusalReason.CRITERIA_NOT_COVERED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1",
                findings=[{"id": "c1", "verdict": "verified"}],
            ),
        )

    def test_verified_passes_when_every_required_criterion_is_covered(self):
        event_id = self.reviewed()
        record = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v1",
            findings=[
                {"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"},
            ],
        )
        self.assertEqual(record["verdict"], "verified")

    def test_needs_changes_requires_a_finding_with_a_note(self):
        event_id = self.reviewed()
        self.assertRefused(
            RefusalReason.FINDINGS_REQUIRED,
            lambda: self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v1",
                findings=[{"id": "c1", "verdict": "needs_changes"}],
            ),
        )

    def test_unknown_criterion_is_refused(self):
        event_id = self.reviewed()
        self.assertRefused(
            RefusalReason.UNKNOWN_CRITERION,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1",
                findings=[{"id": "nope", "verdict": "verified"}],
            ),
        )

    def test_a_disposition_outside_the_frozen_enum_is_refused(self):
        event_id = self.reviewed()
        self.assertRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1",
                findings=[{"id": "c1", "verdict": "regressed"}],
            ),
        )

    def test_editing_a_criterion_after_the_claim_invalidates_the_review(self):
        """Findings made against one wording cannot certify a different wording."""
        event_id = self.acknowledged()
        self.register_criteria()
        self.ack.claim_verification(event_id, turn_id="ack-turn")
        edited = [
            {"id": "c1", "title": "the endpoint returns a COMPLETELY different shape"},
            {"id": "c2", "title": "a malformed request is refused"},
        ]
        self.register_criteria(edited)
        self.assertRefused(
            RefusalReason.CRITERIA_SET_CHANGED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1",
                findings=[
                    {"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"},
                ],
            ),
        )

    def test_expected_digest_mismatch_is_refused(self):
        event_id = self.reviewed()
        self.assertRefused(
            RefusalReason.CRITERIA_SET_CHANGED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1",
                findings=[
                    {"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"},
                ],
                expect_criteria_digest="0" * 64,
            ),
        )

    def test_the_digest_survives_delimiter_injection(self):
        """Joined text would collide here; canonical JSON cannot."""
        first = set_digest([{"id": "a", "title": "b|c", "required": True}])
        second = set_digest([{"id": "a|b", "title": "c", "required": True}])
        self.assertNotEqual(first, second)

    def test_a_managed_verdict_without_a_bound_review_is_refused(self):
        """Skipping the claim would otherwise skip the criteria currency protection entirely."""
        event_id = self.acknowledged()
        self.register_criteria()
        self.assertRefused(
            RefusalReason.REVIEW_NOT_BOUND,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1",
                findings=[
                    {"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"},
                ],
            ),
        )

    def test_an_explicit_reviewed_digest_is_the_alternative_to_claiming(self):
        event_id = self.acknowledged()
        registered = self.register_criteria()
        record = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v1",
            findings=[
                {"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"},
            ],
            expect_criteria_digest=registered["setDigest"],
        )
        self.assertEqual(record["verdict"], "verified")


class OfflineAcknowledgement(VerificationTestCase):
    def dispatched(self):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        return event_id

    def test_ack_without_an_adapter_records_unverified_intent(self):
        event_id = self.dispatched()
        record = self.ack.acknowledge(
            event_id, ack_turn_id="parent-own-turn",
            ack_proof=identity.ack_proof(event_id, "parent-own-turn"), accepted=True,
            adapter=None,
        )
        self.assertEqual(record["_verified"], "unverified_turn")
        self.assertTrue(record["accepted"])
        row = self.store.one("SELECT * FROM ack_evidence WHERE event_id = ?", (event_id,))
        self.assertEqual(row["tier"], "unverified")

    def test_an_unverified_ack_cannot_produce_a_verdict(self):
        event_id = self.dispatched()
        self.ack.acknowledge(
            event_id, ack_turn_id="parent-own-turn",
            ack_proof=identity.ack_proof(event_id, "parent-own-turn"), accepted=True,
            adapter=None,
        )
        self.assertRefused(
            RefusalReason.NOT_ACKNOWLEDGED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_a_dispatch_turn_alone_never_verifies_an_ack(self):
        """A stored accepted dispatch proves a send; it never proves the parent observed it."""
        event_id = self.dispatched()
        dispatch_turn = self.delivery.find(event_id)["dispatch_turn_id"]
        self.assertTrue(dispatch_turn)
        record = self.ack.acknowledge(
            event_id, ack_turn_id=dispatch_turn,
            ack_proof=identity.ack_proof(event_id, dispatch_turn), accepted=True,
            adapter=None,
        )
        self.assertEqual(record["_verified"], "unverified_turn")

    def test_verify_pending_acks_upgrades_once_a_host_is_available(self):
        event_id = self.dispatched()
        turn = self.adapter.start_turn(PARENT, turn_id="parent-own-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True, adapter=None,
        )
        results = self.ack.verify_pending_acks(self.adapter)
        self.assertEqual([r["outcome"] for r in results], ["verified"])
        self.assertEqual(self.delivery.find(event_id)["state"], ACKNOWLEDGED)


class DeferredAckDisposition(VerificationTestCase):
    def pending(self, turn_id="parent-own-turn"):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        self.adapter.start_turn(PARENT, turn_id=turn_id, status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn_id,
            ack_proof=identity.ack_proof(event_id, turn_id), accepted=True, adapter=None,
        )
        return event_id

    def test_generation_advance_before_confirmation_blocks_promotion(self):
        event_id = self.pending()
        self.advance_generation()
        results = self.ack.verify_pending_acks(self.adapter)
        self.assertEqual(results[0]["outcome"], "withheld")
        self.assertEqual(results[0]["reason"], "stale_generation")
        self.assertNotEqual(self.delivery.find(event_id)["state"], ACKNOWLEDGED)

    def test_relationship_paused_before_confirmation_blocks_promotion(self):
        event_id = self.pending()
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        results = self.ack.verify_pending_acks(self.adapter)
        self.assertEqual(results[0]["outcome"], "withheld")
        self.assertEqual(results[0]["reason"], "relationship_not_active")
        self.assertNotEqual(self.delivery.find(event_id)["state"], ACKNOWLEDGED)

    def test_blocked_promotion_preserves_the_authored_intent(self):
        """The relay never writes a rejection the parent did not author."""
        event_id = self.pending()
        before = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
        self.advance_generation()
        self.ack.verify_pending_acks(self.adapter)
        after = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
        self.assertEqual(after["accepted"], before["accepted"])
        self.assertEqual(after["rejection_reason"], before["rejection_reason"])
        self.assertEqual(after["record"], before["record"])
        self.assertEqual(after["verified"], "unverified_turn")

    def test_an_unchanged_terminal_refusal_is_not_rejournalled(self):
        event_id = self.pending()
        self.advance_generation()
        self.ack.verify_pending_acks(self.adapter, now=self.clock.now())
        first = self._withheld_entries(event_id)
        # Same world, later pass: nothing new happened, so nothing new is written.
        self.ack.verify_pending_acks(self.adapter, now=self.clock.now() + 10_000)
        self.assertEqual(self._withheld_entries(event_id), first)

    def test_a_pending_pass_respects_next_check_at(self):
        event_id = self.pending()
        self.advance_generation()
        self.ack.verify_pending_acks(self.adapter, now=100.0)
        row = self.store.one("SELECT * FROM ack_evidence WHERE event_id = ?", (event_id,))
        self.assertGreater(row["next_check_at"], 100.0)
        self.assertEqual(self.ack.verify_pending_acks(self.adapter, now=101.0), [])

    def test_promotion_works_once_the_blocker_clears(self):
        event_id = self.pending()
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        self.ack.verify_pending_acks(self.adapter, now=100.0)
        relationship = self.registry.get(self._rid)
        self.registry.resume(
            self._rid, expect_generation=relationship["executionGeneration"],
            expect_artifact_roots=relationship["authorizedScope"]["artifactRoots"],
            expect_allowed_recipients=relationship["authorizedScope"]["allowedRecipients"],
            actor=PARENT,
        )
        results = self.ack.verify_pending_acks(self.adapter, now=100_000.0)
        self.assertEqual(results[0]["outcome"], "verified")
        self.assertEqual(self.delivery.find(event_id)["state"], ACKNOWLEDGED)

    def _withheld_entries(self, event_id):
        return [
            dict(row) for row in self.store.all(
                "SELECT kind, subject, detail FROM journal WHERE subject = ?"
                "   AND kind = 'ack_verification_withheld'",
                (event_id,),
            )
        ]


class ServicesLifecycle(unittest.TestCase):
    """Closing the store alone left the adapter's transport thread running."""

    class _Args:
        socket = None
        state = None

        def __init__(self, state, socket=None):
            self.state = state
            self.socket = socket

    def _services(self, socket=None):
        import tempfile

        from codex_session_relay.cli import Services

        directory = tempfile.mkdtemp(prefix="relay-lifecycle-")
        self.addCleanup(lambda: None)
        return Services(self._Args(directory, socket))

    def test_close_closes_an_owned_adapter(self):
        closed = []

        class FakeAdapter:
            def close(self):
                closed.append(True)

        services = self._services()
        services._adapter = FakeAdapter()
        services.close()
        self.assertEqual(closed, [True])

    def test_close_does_not_build_an_adapter(self):
        services = self._services()
        services.close()
        self.assertIsNone(services._adapter)
