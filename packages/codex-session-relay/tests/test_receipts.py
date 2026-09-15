"""Completion receipts: five endings, and what a claim has to survive to be stored."""

import os
import unittest

from codex_session_relay import NO_DELIVERABLE, identity, manifest
from codex_session_relay.errors import RefusalReason
from codex_session_relay.models import TurnRef
from codex_session_relay.receipts import (
    ObservationOutcome,
    ReceiptIntake,
    classify_observation,
    contract_record,
)
from codex_session_relay.scope import PathBindingMode

from .support import PARENT, RelayTestCase


class Observation(unittest.TestCase):
    def test_completed_without_a_child_receipt_is_an_ordinary_turn_end(self):
        self.assertEqual(
            classify_observation("completed", None), ObservationOutcome.ORDINARY_TURN_END
        )

    def test_a_completed_turn_is_never_success_on_its_own(self):
        outcome = classify_observation("completed", None)
        self.assertNotEqual(outcome, ObservationOutcome.READY_FOR_REVIEW)

    def test_failed_and_interrupted_turns_are_observable_without_a_receipt(self):
        self.assertEqual(classify_observation("failed", None), ObservationOutcome.FAILED)
        self.assertEqual(
            classify_observation("interrupted", None), ObservationOutcome.INTERRUPTED
        )

    def test_a_turn_still_running_is_not_terminal(self):
        self.assertEqual(classify_observation("inProgress", None), ObservationOutcome.IN_PROGRESS)

    def test_a_child_receipt_supplies_the_outcome(self):
        for outcome in ("ready_for_review", "blocked_needs_input", "failed"):
            self.assertEqual(
                classify_observation("completed", {"outcome": outcome, "producer": "child"}),
                ObservationOutcome(outcome),
            )

    def test_an_observed_failure_is_never_promoted_to_reviewable(self):
        for status in ("failed", "interrupted"):
            for claim in ("ready_for_review", "blocked_needs_input"):
                self.assertEqual(
                    classify_observation(status, {"outcome": claim, "producer": "child"}),
                    ObservationOutcome.CONTRADICTORY,
                    f"{status} turn must not carry a {claim} claim",
                )

    def test_a_child_may_report_its_own_failure_from_a_normal_turn(self):
        self.assertEqual(
            classify_observation("completed", {"outcome": "failed", "producer": "child"}),
            ObservationOutcome.FAILED,
        )

    def test_a_receipt_that_is_not_child_produced_cannot_assert_an_outcome(self):
        with self.assertRaises(Exception):
            classify_observation(
                "completed", {"outcome": "ready_for_review", "producer": "daemon_observation"}
            )


class ReadyForReview(RelayTestCase):
    def test_a_valid_receipt_over_real_bytes_is_accepted_and_stored(self):
        relationship = self.register()
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        stored = self.accept(payload)
        self.assertFalse(stored["_duplicate"])
        self.assertEqual(stored["outcome"], "ready_for_review")
        self.assertIsNotNone(self.intake.get(payload["eventId"]))
        self.assertEqual(
            self.intake.row(payload["eventId"])["path_binding_mode"],
            PathBindingMode.BEST_EFFORT_DETECTION.value,
        )

    def test_a_completion_phrase_with_no_matching_bytes_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("something else entirely")
        self.assertRefused(
            RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload
        )

    def test_a_truncated_artifact_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "the full deliverable contents")
        payload = self.ready_payload(relationship, [path])
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("the full")
        self.assertRefused(
            RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload
        )

    def test_a_missing_artifact_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        os.remove(path)
        self.assertRefused(
            RefusalReason.MANIFEST_UNVERIFIED, self.accept, payload
        )

    def test_a_reviewable_receipt_with_no_manifest_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        payload["manifest"] = None
        self.assertRefused(
            RefusalReason.MANIFEST_REQUIRED, self.accept, payload
        )

    def test_a_reviewable_receipt_carrying_the_sentinel_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        payload["revisionHash"] = NO_DELIVERABLE
        self.assertRefused(
            RefusalReason.OUTCOME_INCONSISTENT, self.accept, payload
        )

    def test_a_forged_digest_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        payload["revisionHash"] = "f" * 64
        payload["eventId"] = identity.event_id(
            payload["relationshipId"], payload["executionGeneration"], "f" * 64,
            "ready_for_review",
        )
        self.assertRefused(
            RefusalReason.REVISION_MISMATCH, self.accept, payload
        )

    def test_a_forged_event_id_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        payload["eventId"] = "0" * 32
        self.assertRefused(
            RefusalReason.EVENT_ID_MISMATCH, self.accept, payload
        )

    def test_a_turnref_that_disagrees_with_the_observation_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        other = TurnRef("01child-task", "a-different-turn", "completed")
        self.assertRefused(
            RefusalReason.TURNREF_MISMATCH,
            self.accept, payload, observation=other,
        )

    def test_every_refusal_is_recorded_for_an_operator(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        payload["eventId"] = "0" * 32
        with self.assertRaises(Exception):
            self.accept(payload)
        refusals = self.intake.refusals(relationship["relationshipId"])
        self.assertEqual(len(refusals), 1)
        self.assertEqual(refusals[0]["reason"], RefusalReason.EVENT_ID_MISMATCH.value)


class ExecutionOnly(RelayTestCase):
    def test_a_child_failure_is_accepted_with_the_sentinel(self):
        relationship = self.register()
        payload = self.execution_payload(relationship, "failed")
        stored = self.accept(payload)
        self.assertEqual(stored["revisionHash"], NO_DELIVERABLE)

    def test_a_child_failure_carrying_a_manifest_is_refused(self):
        """The frozen schema only constrains a daemon observation here; the prose binds both."""
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        entries, _ = manifest.build([path], [self.root])
        payload = self.execution_payload(relationship, "failed")
        payload["manifest"] = [e.to_record() for e in entries]
        self.assertRefused(
            RefusalReason.MANIFEST_FORBIDDEN, self.accept, payload
        )

    def test_a_child_interruption_carrying_a_digest_is_refused(self):
        relationship = self.register()
        payload = self.execution_payload(
            relationship, "interrupted",
            turn=self.assigned_turn("interrupted"),
        )
        payload["revisionHash"] = "a" * 64
        payload["eventId"] = identity.event_id(
            payload["relationshipId"], payload["executionGeneration"], "a" * 64,
            "interrupted", turn_id="turn-dispatch-1", attempt=1,
        )
        self.assertRefused(
            RefusalReason.OUTCOME_INCONSISTENT, self.accept, payload
        )

    def test_a_child_receipt_without_its_rerun_counter_is_refused(self):
        relationship = self.register()
        payload = self.execution_payload(relationship, "failed")
        payload["attempt"] = None
        payload["eventId"] = identity.event_id(
            payload["relationshipId"], 1, NO_DELIVERABLE, "failed",
            turn_id=payload["turnRef"]["turnId"], attempt=None,
        )
        self.assertRefused(
            RefusalReason.OUTCOME_INCONSISTENT, self.accept, payload
        )


class DaemonObservation(RelayTestCase):
    def test_a_daemon_may_synthesize_failure_and_interruption(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        for status in ("failed", "interrupted"):
            stored = self.intake.daemon_observation(
                rid, self.assigned_turn(status)
            )
            self.assertEqual(stored["producer"], "daemon_observation")
            self.assertEqual(stored["outcome"], status)
            self.assertIsNone(stored["attempt"])
            self.assertEqual(stored["revisionHash"], NO_DELIVERABLE)
            self.assertIsNone(stored["manifest"])

    def test_a_daemon_cannot_synthesize_a_reviewable_result(self):
        relationship = self.register()
        self.assertRefused(
            RefusalReason.PRODUCER_NOT_PERMITTED,
            self.intake.daemon_observation,
            relationship["relationshipId"], self.assigned_turn("completed"),
        )

    def test_a_daemon_cannot_claim_a_task_is_waiting_for_approval(self):
        relationship = self.register()
        payload = self.execution_payload(relationship, "blocked_needs_input")
        payload["producer"] = "daemon_observation"
        payload["attempt"] = None
        payload["eventId"] = identity.event_id(
            payload["relationshipId"], 1, NO_DELIVERABLE, "blocked_needs_input",
            turn_id=payload["turnRef"]["turnId"], attempt=None,
        )
        self.assertRefused(
            RefusalReason.PRODUCER_NOT_PERMITTED, self.accept, payload
        )

    def test_the_daemon_observation_key_deduplicates_its_own_stream(self):
        relationship = self.register()
        turn = self.assigned_turn("failed")
        for _ in range(3):
            self.intake.record_observation(
                turn, ObservationOutcome.FAILED,
                relationship_id=relationship["relationshipId"],
            )
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM observations")["c"], 1)


class DuplicateAndRevision(RelayTestCase):
    def test_re_observing_one_revision_collapses(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        first = self.accept(payload)
        second = self.accept(dict(payload))
        self.assertFalse(first["_duplicate"])
        self.assertTrue(second["_duplicate"])
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM events")["c"], 1)
        self.assertEqual(self.intake.row(payload["eventId"])["observation_count"], 2)

    def test_a_new_revision_is_a_separate_retained_verification_target(self):
        relationship = self.register()
        path = self.artifact("out.txt", "first revision")
        first_payload = self.ready_payload(relationship, [path])
        self.accept(first_payload)
        reference = os.path.join(self.tmp, "frozen-first")
        entries = [manifest.Entry.from_record(r) for r in first_payload["manifest"]]
        manifest.freeze(entries, reference)

        with open(path, "w", encoding="utf-8") as handle:
            handle.write("second revision")
        second_payload = self.ready_payload(relationship, [path], attempt=2)
        self.accept(second_payload)

        self.assertNotEqual(first_payload["eventId"], second_payload["eventId"])
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM events")["c"], 2)
        # The older revision is still verifiable through its frozen copy.
        digest, problems = manifest.verify_frozen(reference, entries)
        self.assertEqual(problems, [])
        self.assertEqual(digest, first_payload["revisionHash"])


class GenerationAndScopeRefusals(RelayTestCase):
    def test_an_unregistered_relationship_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        payload["relationshipId"] = "rel-ffffffffffffffff"
        self.assertRefused(
            RefusalReason.UNREGISTERED_RELATIONSHIP, self.accept, payload
        )

    def test_a_stale_generation_is_refused(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        path = self.artifact("out.txt", "payload")
        stale = self.ready_payload(relationship, [path], generation=1)
        self.registry.open_generation(
            rid, dispatch_request_id="d2", reason="needs_changes_revision",
            dispatch_turn_id="t2",
        )
        self.assertRefused(
            RefusalReason.STALE_GENERATION, self.accept, stale
        )

    def test_an_unknown_generation_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path], generation=7)
        self.assertRefused(
            RefusalReason.UNKNOWN_GENERATION, self.accept, payload
        )

    def test_an_unbound_generation_is_refused_and_stays_reportable(self):
        relationship = self.register(dispatch_turn_id=None)
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        self.assertRefused(
            RefusalReason.UNBOUND_GENERATION, self.accept, payload
        )
        self.assertEqual(
            self.registry.generation(relationship["relationshipId"], 1)["anchorState"],
            "anchor_pending",
        )

    def test_an_inactive_relationship_is_refused(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        self.registry.set_status(relationship["relationshipId"], "paused", actor="user")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE, self.accept, payload
        )

    def test_a_manifest_path_outside_the_artifact_roots_is_refused(self):
        relationship = self.register()
        outside = os.path.join(self.tmp, "outside.txt")
        with open(outside, "w", encoding="utf-8") as handle:
            handle.write("not yours")
        inside = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [inside])
        entries = [manifest.Entry.from_record(r) for r in payload["manifest"]]
        entries.append(manifest.Entry(outside, "a" * 64, 9))
        payload["manifest"] = [e.to_record() for e in entries]
        self.assertRefused(
            RefusalReason.SCOPE_ESCAPE, self.accept, payload
        )


class PathBindingMinimum(RelayTestCase):
    def test_a_store_requiring_an_enforced_binding_refuses_a_best_effort_receipt(self):
        strict = ReceiptIntake(
            self.store, self.registry, self.clock,
            minimum_path_binding=PathBindingMode.LEASE_ENFORCED,
        )
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        # A pre-existing writable open makes a read lease unobtainable, so the read can only
        # reach the best-effort tier and a store demanding the enforced tier must refuse it.
        writable = os.open(path, os.O_RDWR)
        self.addCleanup(os.close, writable)
        self.assertRefused(
            RefusalReason.INSUFFICIENT_PATH_BINDING,
            lambda pl: self.accept(pl, intake=strict), payload,
        )

    def test_the_default_accepts_a_best_effort_binding_and_records_it(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        stored = self.accept(payload)
        self.assertEqual(
            stored["_pathBindingMode"], PathBindingMode.BEST_EFFORT_DETECTION.value
        )


class Artifacts(RelayTestCase):
    def test_artifacts_are_never_opened_for_writing(self):
        relationship = self.register()
        path = self.artifact("out.txt", "original bytes")
        before = os.stat(path)
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        after = os.stat(path)
        self.assertEqual(
            (before.st_size, before.st_mtime_ns, before.st_ino),
            (after.st_size, after.st_mtime_ns, after.st_ino),
        )
        with open(path, encoding="utf-8") as handle:
            self.assertEqual(handle.read(), "original bytes")

    def test_the_contract_record_carries_no_internal_annotations(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        stored = self.accept(self.ready_payload(relationship, [path]))
        record = contract_record(stored)
        self.assertFalse([k for k in record if k.startswith("_")])


if __name__ == "__main__":
    unittest.main()
