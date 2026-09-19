"""Regressions for bugs found by independent review of wp1.

Three were reproduced by the parent against a frozen copy of this source; the rest came from
an independent code review that probed the modules directly. Each test here failed before its
fix, so none of them is decoration.
"""

import fcntl
import os
import subprocess
import sys
import unittest

from codex_session_relay import NO_DELIVERABLE, identity, manifest
from codex_session_relay.errors import RefusalReason, ScopeError
from codex_session_relay.models import TurnRef
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.scope import PathBindingMode, open_authorized

from .support import CHILD, DISPATCH_TURN, PARENT, RelayTestCase

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class TurnIdentityIsCheckedAgainstTheRegistry(RelayTestCase):
    """Payload and observation agreeing proves only that a claimant is self-consistent."""

    def _payload_for(self, relationship, path, *, thread, turn):
        entries, _ = manifest.build([path], [self.root])
        digest = manifest.revision_hash(entries)
        return {
            "eventId": identity.event_id(
                relationship["relationshipId"], 1, digest, "ready_for_review",
                turn_id=turn, attempt=1,
            ),
            "relationshipId": relationship["relationshipId"],
            "executionGeneration": 1,
            "attempt": 1,
            "revisionHash": digest,
            "outcome": "ready_for_review",
            "producer": "child",
            "turnRef": {"threadId": thread, "turnId": turn, "turnStatus": "completed"},
            "manifest": [e.to_record() for e in entries],
            "emittedAt": self.clock.iso(),
        }

    def test_the_registered_child_on_its_assigned_turn_is_accepted(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self._payload_for(relationship, path, thread=CHILD, turn=DISPATCH_TURN)
        stored = self.accept(payload)
        self.assertFalse(stored["_duplicate"])

    def test_an_unregistered_child_is_refused_even_when_it_is_self_consistent(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self._payload_for(
            relationship, path, thread="unregistered-task", turn=DISPATCH_TURN
        )
        error = self.assertRefused(RefusalReason.UNASSIGNED_TURN, self.accept, payload)
        self.assertIn("registered child", error.detail)

    def test_an_unassigned_turn_is_refused_even_for_the_right_child(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self._payload_for(relationship, path, thread=CHILD, turn="unassigned-turn")
        error = self.assertRefused(RefusalReason.UNASSIGNED_TURN, self.accept, payload)
        self.assertIn("explicit continuation admission", error.detail)

    def test_a_daemon_observation_of_a_foreign_turn_is_refused(self):
        relationship = self.register()
        self.assertRefused(
            RefusalReason.UNASSIGNED_TURN,
            self.intake.daemon_observation,
            relationship["relationshipId"], TurnRef(CHILD, "some-other-turn", "failed"),
        )


class ObservedFailureIsNeverPromoted(RelayTestCase):
    def test_a_ready_claim_on_a_failed_turn_is_refused_at_intake(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn("failed")
        )
        self.assertRefused(RefusalReason.CONTRADICTORY_OBSERVATION, self.accept, payload)

    def test_a_ready_claim_on_an_interrupted_turn_is_refused_at_intake(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn("interrupted")
        )
        self.assertRefused(RefusalReason.CONTRADICTORY_OBSERVATION, self.accept, payload)

    def test_a_daemon_receipt_may_only_restate_what_it_observed(self):
        relationship = self.register()
        payload = self.execution_payload(relationship, "failed", turn=self.assigned_turn("completed"))
        payload["producer"] = "daemon_observation"
        payload["attempt"] = None
        payload["eventId"] = identity.event_id(
            payload["relationshipId"], 1, NO_DELIVERABLE, "failed",
            turn_id=DISPATCH_TURN, attempt=None,
        )
        self.assertRefused(RefusalReason.CONTRADICTORY_OBSERVATION, self.accept, payload)


class NonRegularArtifacts(RelayTestCase):
    def test_a_named_pipe_is_refused_promptly_rather_than_blocking(self):
        """Run in a subprocess with a hard timeout: a regression here would hang, not fail."""
        fifo = os.path.join(self.root, "artifact.fifo")
        os.mkfifo(fifo)
        code = (
            "import sys;sys.path.insert(0, sys.argv[1]);"
            "from codex_session_relay.scope import open_authorized;"
            "from codex_session_relay.errors import ScopeError\n"
            "try:\n"
            "    with open_authorized(sys.argv[2], [sys.argv[3]]):\n"
            "        print('OPENED')\n"
            "except ScopeError as error:\n"
            "    print(error.reason.value)\n"
        )
        completed = subprocess.run(
            [sys.executable, "-c", code, os.path.join(REPO, "src"), fifo, self.root],
            capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(completed.stdout.strip(), RefusalReason.NOT_A_REGULAR_FILE.value)

    def test_a_directory_is_refused(self):
        self.assertRefused(
            RefusalReason.NOT_A_REGULAR_FILE, manifest.hash_path, self.root, [self.root]
        )


class ActivationRequiresResume(RelayTestCase):
    def test_set_status_cannot_reactivate_a_paused_relationship(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.set_status(rid, "paused", actor="user")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE, self.registry.set_status, rid, "active",
            actor="sneaky",
        )
        self.assertEqual(self.registry.get(rid)["status"], "paused")

    def test_set_status_cannot_reactivate_a_superseded_relationship(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.supersede(rid, new_relationship_id="rel-aaaaaaaaaaaaaaaa")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE, self.registry.set_status, rid, "active",
            actor="sneaky",
        )


class FrozenVerificationRespectsTheMinimum(RelayTestCase):
    def test_a_frozen_fallback_cannot_satisfy_an_enforced_minimum(self):
        """A frozen copy establishes no live path binding, so it is best-effort at most."""
        strict = ReceiptIntake(
            self.store, self.registry, self.clock,
            minimum_path_binding=PathBindingMode.LEASE_ENFORCED,
        )
        relationship = self.register()
        path = self.artifact("out.txt", "first revision")
        payload = self.ready_payload(relationship, [path])
        reference = os.path.join(self.tmp, "frozen")
        entries = [manifest.Entry.from_record(r) for r in payload["manifest"]]
        manifest.freeze(entries, reference)
        payload["manifestRef"] = reference
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("a later revision")
        self.assertRefused(
            RefusalReason.INSUFFICIENT_PATH_BINDING,
            lambda pl: self.accept(pl, intake=strict), payload,
        )


class FrozenBlobNaming(RelayTestCase):
    def test_a_non_digest_blob_name_is_refused_before_any_path_is_built(self):
        entry = manifest.Entry("/etc/hostname", "/etc/passwd", 1)
        with self.assertRaises(ScopeError) as caught:
            manifest.freeze([entry], os.path.join(self.tmp, "evil"))
        self.assertEqual(caught.exception.reason, RefusalReason.MANIFEST_UNVERIFIED)

    def test_canonicalization_rejects_a_non_hex_digest(self):
        with self.assertRaises(ScopeError):
            manifest.canonical_payload([manifest.Entry("/a", "z" * 64)])

    def test_frozen_verification_checks_byte_counts(self):
        path = self.artifact("sized.txt", "exactly this")
        entries, _ = manifest.build([path], [self.root])
        reference = os.path.join(self.tmp, "frozen-size")
        manifest.freeze(entries, reference)
        wrong = [manifest.Entry(entries[0].path, entries[0].sha256, entries[0].bytes + 5)]
        _digest, problems = manifest.verify_frozen(reference, wrong)
        self.assertTrue(problems)


class MalformedReceipts(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.relationship = self.register()
        self.path = self.artifact("out.txt", "payload")

    def _mangled(self, **changes):
        payload = self.ready_payload(self.relationship, [self.path])
        payload.update(changes)
        return payload

    def test_an_unknown_field_is_refused(self):
        self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT, self.accept, self._mangled(surprise=1)
        )

    def test_a_missing_required_field_is_refused(self):
        payload = self._mangled()
        del payload["emittedAt"]
        self.assertRefused(RefusalReason.MALFORMED_RECEIPT, self.accept, payload)

    def test_an_unknown_producer_is_refused(self):
        self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT, self.accept, self._mangled(producer="somebody")
        )

    def test_an_unknown_turn_status_is_refused(self):
        payload = self._mangled()
        payload["turnRef"] = dict(payload["turnRef"], turnStatus="vanished")
        self.assertRefused(RefusalReason.MALFORMED_RECEIPT, self.accept, payload)

    def test_a_manifest_entry_missing_its_digest_is_refused_not_crashed(self):
        payload = self._mangled()
        payload["manifest"] = [{"path": self.path}]
        self.assertRefused(RefusalReason.MALFORMED_RECEIPT, self.accept, payload)

    def test_a_malformed_receipt_is_recorded_as_a_refusal(self):
        with self.assertRaises(Exception):
            self.accept(self._mangled(surprise=1))
        self.assertTrue(self.intake.refusals())

    def test_a_daemon_receipt_carrying_a_rerun_counter_is_refused(self):
        payload = self.execution_payload(self.relationship, "failed", turn=self.assigned_turn("failed"))
        payload["producer"] = "daemon_observation"
        self.assertRefused(RefusalReason.OUTCOME_INCONSISTENT, self.accept, payload)

    def test_a_non_object_receipt_is_refused(self):
        self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT,
            self.intake.accept_child_receipt, "not a receipt",
            observation=self.assigned_turn(),
        )


class TransactionRecovery(RelayTestCase):
    def test_a_failing_commit_leaves_the_store_usable(self):
        real = self.store.db

        class RefusesCommit:
            """A connection proxy that rejects COMMIT, which sqlite3 will not let us patch."""

            in_transaction = property(lambda self: real.in_transaction)

            def execute(self, sql, *args, **kwargs):
                if isinstance(sql, str) and sql.strip().upper().startswith("COMMIT"):
                    raise RuntimeError("commit rejected")
                return real.execute(sql, *args, **kwargs)

            def __getattr__(self, name):
                return getattr(real, name)

        self.store.db = RefusesCommit()
        try:
            with self.assertRaises(RuntimeError):
                with self.store.transaction() as db:
                    db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k','s','d')")
        finally:
            self.store.db = real
        # The next transaction must still work, and the rejected row must not be visible.
        with self.store.transaction() as db:
            db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k2','s','d')")
        kinds = [r["kind"] for r in self.store.all("SELECT kind FROM journal")]
        self.assertEqual(kinds, ["k2"])


class GenerationReadsEnforceTheInvariant(RelayTestCase):
    def test_a_corrupted_current_pointer_is_refused_on_every_public_read(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        with self.store.transaction() as db:
            db.execute(
                "UPDATE relationships SET execution_generation = 99 WHERE relationship_id = ?",
                (rid,),
            )
        self.assertRefused(RefusalReason.UNKNOWN_GENERATION, self.registry.get, rid)
        self.assertRefused(RefusalReason.UNKNOWN_GENERATION, self.registry.generation, rid, 1)


class AnchorValidationIsShared(RelayTestCase):
    def test_a_blank_dispatch_turn_cannot_bind_during_registration(self):
        self.assertRefused(
            RefusalReason.UNBOUND_GENERATION, self.register, dispatch_turn_id="   "
        )

    def test_a_blank_dispatch_turn_cannot_bind_when_opening_a_generation(self):
        relationship = self.register()
        self.assertRefused(
            RefusalReason.UNBOUND_GENERATION,
            lambda: self.registry.open_generation(
                relationship["relationshipId"], dispatch_request_id="d2",
                reason="needs_changes_revision", dispatch_turn_id="  ",
            ),
        )


if __name__ == "__main__":
    unittest.main()


class ActiveTurnSelfEmission(RelayTestCase):
    """A child emitting from inside its own turn can only observe inProgress.

    It must not hold its turn open waiting for a parent, and it must not claim its current
    turn completed. So the claim is staged, and only an independent observation of that turn
    ending normally makes it deliverable.
    """

    def _staged(self):
        relationship = self.register()
        path = self.artifact("out.txt", "work in progress")
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn("inProgress")
        )
        return relationship, self.accept(payload), payload

    def test_a_claim_from_a_live_turn_is_accepted_but_staged(self):
        _relationship, stored, payload = self._staged()
        self.assertEqual(stored["_stage"], "staged")
        self.assertFalse(self.intake.deliverable(payload["eventId"]))
        self.assertEqual(self.intake.row(payload["eventId"])["turn_status"], "inProgress")

    def test_normal_completion_finalizes_the_staged_claim_exactly_once(self):
        _relationship, _stored, payload = self._staged()
        result = self.intake.resolve_staged(self.assigned_turn("completed"))
        self.assertEqual(result["finalized"], [payload["eventId"]])
        self.assertEqual(result["suppressed"], [])
        self.assertTrue(self.intake.deliverable(payload["eventId"]))
        # The same event became deliverable; no second event was minted for the parent.
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM events")["c"], 1)
        # Settling again is a no-op, so a repeated observation cannot produce a second request.
        again = self.intake.resolve_staged(self.assigned_turn("completed"))
        self.assertEqual(again["finalized"], [])

    def test_a_failed_ending_suppresses_the_staged_claim(self):
        _relationship, _stored, payload = self._staged()
        result = self.intake.resolve_staged(self.assigned_turn("failed"))
        self.assertEqual(result["suppressed"], [payload["eventId"]])
        self.assertFalse(self.intake.deliverable(payload["eventId"]))
        row = self.intake.row(payload["eventId"])
        self.assertEqual(row["stage"], "suppressed")
        self.assertIn("not\n                        promoted".replace("\n                        ", " "), row["suppressed_reason"])

    def test_an_interrupted_ending_suppresses_the_staged_claim(self):
        _relationship, _stored, payload = self._staged()
        self.intake.resolve_staged(self.assigned_turn("interrupted"))
        self.assertFalse(self.intake.deliverable(payload["eventId"]))
        # Named separately because deliverable() is bool(row) and row['stage'] == FINAL, so it
        # is equally false for a claim that was never recorded at all. Alone among the cases
        # here this one had nothing to tell those two apart. Found by the summary inventory in
        # test_regression_map.py rather than by review.
        self.assertEqual(self.intake.row(payload["eventId"])["stage"], "suppressed")

    def test_a_still_running_turn_leaves_the_claim_staged(self):
        _relationship, _stored, payload = self._staged()
        result = self.intake.resolve_staged(self.assigned_turn("inProgress"))
        self.assertTrue(result["pending"])
        self.assertFalse(self.intake.deliverable(payload["eventId"]))
        # resolve_staged answers pending before it reads the stored claim, so the line above is
        # true for a suppressed row too, and deliverable() is false for a row that is absent.
        # Between them they named nothing; the stage does.
        self.assertEqual(self.intake.row(payload["eventId"])["stage"], "staged")

    def test_a_receipt_from_a_completed_turn_is_final_immediately(self):
        relationship = self.register()
        path = self.artifact("out.txt", "done")
        payload = self.ready_payload(relationship, [path])
        stored = self.accept(payload)
        self.assertEqual(stored["_stage"], "final")
        self.assertTrue(self.intake.deliverable(payload["eventId"]))

    def test_staging_does_not_relax_the_registry_identity_checks(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(
            relationship, [path],
            turn=self.assigned_turn("inProgress", turn="unassigned-turn"),
        )
        self.assertRefused(RefusalReason.UNASSIGNED_TURN, self.accept, payload)
