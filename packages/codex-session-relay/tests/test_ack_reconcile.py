"""Acknowledgement, verdicts, reconciliation and restart recovery."""

import json
import unittest

from codex_session_relay import identity
from codex_session_relay.ack import certainly_before
from codex_session_relay.delivery import REVISION
from codex_session_relay.errors import RefusalReason
from codex_session_relay.lifecycle import UNKNOWN, observe
from codex_session_relay.reconcile import Evidence
from codex_session_relay.transport import ACKNOWLEDGED, DISPATCHED, HELD_UNCERTAIN

from .support import CHILD, PARENT, DeliveryTestCase


def _contract(record):
    """The verdict as the frozen schema defines it, without the relay's internal markers."""
    return {k: v for k, v in record.items() if not k.startswith("_")}


class TurnStartPrecision(unittest.TestCase):
    """The host reports whole seconds; we record microseconds. Do not confuse the two."""

    SENT = "2026-09-14T21:22:09.360483+00:00"

    def test_a_turn_starting_in_the_same_second_as_its_send_is_not_refused(self):
        # These are the real values from the observed JUN-89 acknowledgement: the send was at
        # ...929.360483 and the recipient's turn reported startedAt 1789420929. Treating the
        # rounded value as proof the turn predates its own delivery would reject a genuine ack.
        from datetime import datetime, timezone

        sent = datetime.fromtimestamp(1789420929.360483, timezone.utc).isoformat()
        self.assertFalse(certainly_before(1789420929, sent))

    def test_a_genuinely_earlier_whole_second_turn_is_still_refused(self):
        from datetime import datetime, timezone

        sent = datetime.fromtimestamp(1789420929.360483, timezone.utc).isoformat()
        self.assertTrue(certainly_before(1789420920, sent))
        self.assertTrue(certainly_before(1789420928, sent))

    def test_a_later_turn_is_never_refused(self):
        from datetime import datetime, timezone

        sent = datetime.fromtimestamp(1789420929.360483, timezone.utc).isoformat()
        self.assertFalse(certainly_before(1789420930, sent))


class UnknownArchiveState(DeliveryTestCase):
    def test_an_unknown_archive_observation_is_not_evidence_of_being_unarchived(self):
        class NoArchiveInfo:
            def __init__(self, inner):
                self._inner = inner

            def __getattr__(self, name):
                return getattr(self._inner, name)

            def is_archived(self, thread_id, *, cwd=None):
                return None

        observation = observe(NoArchiveInfo(self.adapter), PARENT)
        self.assertEqual(observation.deliverable, "unknown")
        self.assertEqual(observation.withhold_reason, UNKNOWN)
        self.assertFalse(observation.may_send)

    def test_a_confirmed_unarchived_recipient_stays_deliverable(self):
        self.adapter.threads[PARENT].archived = False
        self.assertTrue(observe(self.adapter, PARENT).may_send)

    def test_a_confirmed_archived_recipient_stays_blocked(self):
        self.adapter.threads[PARENT].archived = True
        observation = observe(self.adapter, PARENT)
        self.assertFalse(observation.may_send)
        self.assertEqual(observation.withhold_reason, "recipient_archived")

    def test_relaxing_the_evidence_requirement_is_explicit(self):
        class NoArchiveInfo:
            def __init__(self, inner):
                self._inner = inner

            def __getattr__(self, name):
                return getattr(self._inner, name)

            def is_archived(self, thread_id, *, cwd=None):
                return None

        relaxed = observe(NoArchiveInfo(self.adapter), PARENT, require_evidence=False)
        self.assertTrue(relaxed.may_send)


class Acknowledgement(DeliveryTestCase):
    def _dispatched(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="parent-ack-turn", status="inProgress")
        return event_id, record, turn

    def test_a_correct_proof_from_a_real_later_turn_closes_the_attempt(self):
        event_id, _record, turn = self._dispatched()
        proof = identity.ack_proof(event_id, turn.turn_id)
        result = self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id, ack_proof=proof, accepted=True,
            adapter=self.adapter,
        )
        self.assertTrue(result["accepted"])
        self.assertIsNone(result["rejectionReason"])
        self.assertEqual(result["_verified"], "verified")
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_an_echo_without_the_proof_never_closes_the_attempt(self):
        event_id, _record, turn = self._dispatched()
        self.assertRefused(
            RefusalReason.ACK_PROOF_MISMATCH,
            lambda: self.ack.acknowledge(
                event_id, ack_turn_id=turn.turn_id, ack_proof="0" * 64, accepted=True,
                adapter=self.adapter,
            ),
        )
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_a_proof_computed_over_someone_else_turn_is_refused(self):
        event_id, _record, turn = self._dispatched()
        wrong = identity.ack_proof(event_id, "a-different-turn")
        self.assertRefused(
            RefusalReason.ACK_PROOF_MISMATCH,
            lambda: self.ack.acknowledge(
                event_id, ack_turn_id=turn.turn_id, ack_proof=wrong, accepted=True,
                adapter=self.adapter,
            ),
        )

    def test_a_turn_that_does_not_exist_does_not_close_the_attempt(self):
        event_id, _record, _turn = self._dispatched()
        proof = identity.ack_proof(event_id, "invented-turn")
        result = self.ack.acknowledge(
            event_id, ack_turn_id="invented-turn", ack_proof=proof, accepted=True,
            adapter=self.adapter,
        )
        self.assertEqual(result["_verified"], "unverified_turn")
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_a_turn_that_started_before_the_delivery_is_refused(self):
        _relationship, event_id = self.queued_event()
        old = self.adapter.start_turn(PARENT, turn_id="older-turn", status="completed")
        self.clock.advance(120)
        self.attempt(event_id, now=self.clock.now())
        proof = identity.ack_proof(event_id, old.turn_id)
        self.assertRefused(
            RefusalReason.ACK_TURN_UNVERIFIED,
            lambda: self.ack.acknowledge(
                event_id, ack_turn_id=old.turn_id, ack_proof=proof, accepted=True,
                adapter=self.adapter,
            ),
        )

    def test_a_generation_that_advanced_after_dispatch_cannot_be_accepted(self):
        event_id, _record, turn = self._dispatched()
        self.registry.open_generation(
            self._rid, dispatch_request_id="later", reason="needs_changes_revision",
            dispatch_turn_id="later-turn",
        )
        proof = identity.ack_proof(event_id, turn.turn_id)
        self.assertRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            lambda: self.ack.acknowledge(
                event_id, ack_turn_id=turn.turn_id, ack_proof=proof, accepted=True,
                adapter=self.adapter,
            ),
        )

    def test_a_generation_advancing_during_the_acknowledgement_is_caught(self):
        """The disposition is decided inside the write transaction, not before it."""
        event_id, _record, turn = self._dispatched()
        original = self.adapter.read_turn

        def advance_then_read(thread_id, turn_id):
            self.registry.open_generation(
                self._rid, dispatch_request_id="racy", reason="needs_changes_revision",
                dispatch_turn_id="racy-turn",
            )
            return original(thread_id, turn_id)

        self.adapter.read_turn = advance_then_read
        proof = identity.ack_proof(event_id, turn.turn_id)
        self.assertRefused(
            RefusalReason.DISPOSITION_CONFLICT,
            lambda: self.ack.acknowledge(
                event_id, ack_turn_id=turn.turn_id, ack_proof=proof, accepted=True,
                adapter=self.adapter,
            ),
        )
        self.assertIsNone(self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,)))

    def test_one_event_is_verified_only_once(self):
        _relationship, event_id = self.queued_event()
        self.assertEqual(self.ack.claim_verification(event_id, turn_id="t1"), "proceed")
        self.assertEqual(self.ack.claim_verification(event_id, turn_id="t2"), "already_claimed")

    def test_a_duplicate_delivery_cannot_cause_a_second_verification(self):
        _relationship, event_id = self.queued_event()
        first = self.ack.claim_verification(event_id, turn_id="t1")
        second = self.ack.claim_verification(event_id, turn_id="t1")
        self.assertEqual((first, second), ("proceed", "already_claimed"))
        self.assertEqual(
            self.store.one("SELECT COUNT(*) AS c FROM verification_claims")["c"], 1
        )


class Verdicts(DeliveryTestCase):
    def _acknowledged(self, *, recipients=None):
        _relationship, event_id = self.queued_event(recipients=recipients)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        return event_id

    def test_a_verdict_requires_a_verified_acceptance(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        self.assertRefused(
            RefusalReason.NOT_ACKNOWLEDGED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1"
            ),
        )

    def test_needs_changes_opens_a_generation_and_queues_a_revision_to_the_same_child(self):
        event_id = self._acknowledged(recipients=[PARENT, CHILD])
        record = self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="verdict-1"
        )
        self.assertEqual(record["nextExecutionGeneration"], 2)
        revision = self.store.one(
            "SELECT * FROM deliveries WHERE kind = ?", (REVISION,)
        )
        self.assertIsNotNone(revision)
        self.assertEqual(revision["recipient_task_id"], CHILD)

    def test_a_revision_cannot_be_routed_to_an_unauthorized_child(self):
        event_id = self._acknowledged(recipients=[PARENT])
        self.assertRefused(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            lambda: self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="verdict-1"
            ),
        )

    def test_a_revision_request_is_never_acknowledged_by_the_parent_path(self):
        event_id = self._acknowledged(recipients=[PARENT, CHILD])
        self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="verdict-1")
        revision = self.store.one("SELECT * FROM deliveries WHERE kind = ?", (REVISION,))
        self.assertRefused(
            RefusalReason.WRONG_DELIVERY_KIND,
            lambda: self.ack.evaluate(revision["event_id"]),
        )

    def test_a_dispatched_revision_binds_the_new_generation_anchor(self):
        event_id = self._acknowledged(recipients=[PARENT, CHILD])
        self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="verdict-1")
        revision = self.store.one("SELECT * FROM deliveries WHERE kind = ?", (REVISION,))
        self.delivery.attempt(revision["event_id"], self.adapter, now=self.clock.now())
        bound = self.ack.bind_dispatched_revision(revision["event_id"])
        self.assertEqual(bound["anchorState"], "bound")
        self.assertTrue(bound["dispatchTurnId"])


class Reconciliation(DeliveryTestCase):
    def _uncertain(self, outcome="turn_start_fail"):
        _relationship, event_id = self.queued_event()
        self.adapter.script(outcome)
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], HELD_UNCERTAIN)
        return event_id, record["requestId"]

    def test_the_operation_receipt_is_checked_before_the_recipient_turns(self):
        event_id, request_id = self._uncertain()
        order = []
        original_op, original_scan = self.adapter.get_operation, self.adapter.find_token
        self.adapter.get_operation = lambda rid: (order.append("operation"), original_op(rid))[1]
        self.adapter.find_token = lambda *a, **k: (order.append("scan"), original_scan(*a, **k))[1]
        self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(order[:2], ["operation", "scan"])

    def test_no_affirmative_evidence_keeps_the_attempt_held_and_says_what_is_missing(self):
        event_id, request_id = self._uncertain()
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(outcome["evidence"], Evidence.NONE.value)
        self.assertEqual(outcome["state"], HELD_UNCERTAIN)
        self.assertIn("no confirmed pre-send rejection", outcome["missing"])
        self.assertIn("exhausted", outcome["recipientScan"])

    def test_elapsed_time_never_changes_the_outcome(self):
        event_id, request_id = self._uncertain()
        first = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.clock.advance(86400 * 30)
        second = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(first["evidence"], second["evidence"])
        self.assertEqual(second["state"], HELD_UNCERTAIN)
        self.assertEqual(len(self.attempts_for(event_id)), 1)

    def test_a_token_found_in_the_recipient_items_advances_the_delivery_honestly(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        record = self.attempt(event_id)
        request_id = record["requestId"]
        # The message did land in a real turn even though the transport never confirmed.
        turn = self.adapter.start_turn(PARENT, status="completed", text=f"...{request_id}...")
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(outcome["evidence"], Evidence.TURN_FOUND.value)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(self.delivery_row(event_id)["dispatch_evidence"], "turn_found")
        stored = json.loads(self.attempts_for(event_id)[0]["record"])
        # The transport snapshot stays honest: it never claims an acceptance that never happened.
        self.assertEqual(stored["transportReceiptStatus"], "in_progress_or_unknown")
        self.assertEqual(stored["sendAttempted"], "unknown")
        self.assertEqual(stored["deliveryState"], HELD_UNCERTAIN)
        self.assertEqual(stored["reconciliation"]["affirmativeEvidence"], "turn_found")

    def test_a_truncated_scan_is_inconclusive_rather_than_absent(self):
        event_id, request_id = self._uncertain()
        for index in range(30):
            self.adapter.start_turn(PARENT, status="completed", text=f"noise {index}")
        self.adapter.scan_limit = 5
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(outcome["evidence"], Evidence.NONE.value)
        self.assertIn("exhausted=False", outcome["recipientScan"])

    def test_a_missing_ledger_row_is_an_observation_not_a_licence_to_resend(self):
        event_id, request_id = self._uncertain()
        self.adapter.ledger.pop(request_id)
        outcome = self.reconciler.reconcile_attempt(request_id, self.adapter)
        self.assertEqual(outcome["operationObservation"], "missing")
        self.assertEqual(outcome["evidence"], Evidence.NONE.value)
        self.clock.advance(86400)
        self.assertEqual(self.delivery.eligible(now=self.clock.now()), [])

    def test_a_confirmed_pre_send_rejection_permits_a_new_attempt(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        record = self.attempt(event_id)
        # The transport later settles as an attributable pre-send refusal.
        self.adapter.ledger[record["requestId"]] = {
            "requestId": record["requestId"], "status": "failed",
            "error": "thread/read: transport refused",
            "rpcError": {"code": "internal", "message": "refused"},
        }
        outcome = self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        self.assertEqual(outcome["evidence"], Evidence.CONFIRMED_PRE_SEND_REJECTION.value)
        self.clock.advance(100000)
        second = self.attempt(event_id, now=self.clock.now())
        self.assertEqual(second["attemptNo"], 2)


class RestartRecovery(DeliveryTestCase):
    def test_a_crash_before_the_transport_ledger_row_recovers_without_resending(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("process_death")
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], HELD_UNCERTAIN)
        self.adapter.ledger.pop(record["requestId"], None)
        report = self.reconciler.recover_on_start(self.adapter)
        self.assertIn(record["requestId"], report["heldUncertain"])
        self.assertEqual(report["resent"], [])

    def test_a_crash_during_the_send_recovers_as_uncertain(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        record = self.attempt(event_id)
        report = self.reconciler.recover_on_start(self.adapter)
        self.assertIn(record["requestId"], report["heldUncertain"])

    def test_a_crash_after_acceptance_recovers_from_the_ledger(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        record = self.attempt(event_id)
        # The transport actually succeeded; only our settle write was lost.
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.adapter.ledger[record["requestId"]] = {
            "requestId": record["requestId"], "status": "accepted",
            "resumed": {"approvalPolicy": "never"}, "turnId": turn.turn_id,
        }
        report = self.reconciler.recover_on_start(self.adapter)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.assertEqual(report["heldUncertain"], [])

    def test_an_app_restart_recovers_pending_work_without_duplicating_it(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        record = self.attempt(event_id)
        self.adapter.restart()
        before = len(self.adapter.sends)
        self.reconciler.recover_on_start(self.adapter)
        self.assertEqual(len(self.adapter.sends), before, "recovery never sends")
        self.assertEqual(len(self.attempts_for(event_id)), 1)

    def test_a_dispatched_delivery_awaiting_acknowledgement_is_reported_not_resent(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        report = self.reconciler.recover_on_start(self.adapter)
        self.assertEqual(report["awaitingAck"], [event_id])
        self.assertEqual(report["resent"], [])

    def test_a_dispatched_correction_is_not_reported_as_awaiting_acknowledgement(self):
        """A revision request goes parent to child, and no record acknowledges that direction.

        Reporting one as awaiting an acknowledgement states an obligation nothing can ever meet.
        Both deliveries below are genuinely dispatched; no child ACK is invented for either.
        """
        _relationship, first = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(first)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            first, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(first, turn.turn_id),
            accepted=True, adapter=self.adapter,
        )
        self.ack.record_verdict(
            first, verdict="needs_changes", verdict_turn_id="v1",
            findings=[{"id": "tie", "verdict": "needs_changes", "note": "equal timestamps"}],
        )
        correction = self.store.one(
            "SELECT event_id FROM deliveries WHERE kind = ?", (REVISION,),
        )["event_id"]
        self.delivery.attempt(correction, self.adapter)

        # The child's corrected deliverable, dispatched to the parent and not yet acknowledged.
        self.clock.advance(5)
        relationship = self.registry.get(self._rid)
        generation = relationship["executionGeneration"]
        self.registry.bind_anchor(
            self._rid, generation, dispatch_turn_id="revision-turn",
            source="dispatch_receipt",
        )
        payload = self.ready_payload(
            relationship, [self.artifact("revised.txt", "the corrected deliverable")],
            generation=generation, turn=self.assigned_turn(turn="revision-turn"),
        )
        self.accept(payload, observation=self.assigned_turn(turn="revision-turn"))
        self.delivery.enqueue(payload["eventId"])
        self.attempt(payload["eventId"])

        before = len(self.adapter.sends)
        report = self.reconciler.recover_on_start(self.adapter)
        self.assertEqual(report["awaitingAck"], [payload["eventId"]])
        self.assertNotIn(correction, report["awaitingAck"])
        self.assertEqual(report["resent"], [])
        # Report only: the correction is still dispatched and recovery sent nothing.
        self.assertEqual(self.delivery_row(correction)["state"], DISPATCHED)
        self.assertEqual(self.delivery_row(payload["eventId"])["state"], DISPATCHED)
        self.assertEqual(len(self.adapter.sends), before)


if __name__ == "__main__":
    unittest.main()


class VerdictAtomicity(DeliveryTestCase):
    """The verdict, the generation it opens and the correction it queues are one operation."""

    def _acknowledged(self):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        from codex_session_relay import identity

        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        return event_id

    def _counts(self):
        return (
            self.store.one("SELECT COUNT(*) AS c FROM generations")["c"],
            self.store.one("SELECT COUNT(*) AS c FROM verdicts")["c"],
            self.store.one("SELECT COUNT(*) AS c FROM deliveries WHERE kind = 'revision_request'")["c"],
        )

    def test_the_normal_path_produces_a_generation_a_verdict_and_a_revision(self):
        event_id = self._acknowledged()
        self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        self.assertEqual(self._counts(), (2, 1, 1))

    def test_a_storage_failure_while_queueing_the_correction_rolls_everything_back(self):
        """Reproduces the parent's injection: the generation must not survive alone.

        Before this was one transaction, the injected failure left two generations with no
        verdict and no revision delivery, so the execution had advanced with nothing telling
        the child what to correct.
        """
        event_id = self._acknowledged()
        before = self._counts()

        def explode(*_args, **_kwargs):
            raise RuntimeError("storage failed while queueing the correction")

        original = self.delivery.enqueue_in
        self.delivery.enqueue_in = explode
        try:
            with self.assertRaises(RuntimeError):
                self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        finally:
            self.delivery.enqueue_in = original
        self.assertEqual(self._counts(), before, "no generation, verdict or delivery survives")
        self.assertEqual(before[0], 1)

    def test_a_storage_failure_leaves_the_verdict_retryable(self):
        event_id = self._acknowledged()

        def explode(*_args, **_kwargs):
            raise RuntimeError("storage failed")

        original = self.delivery.enqueue_in
        self.delivery.enqueue_in = explode
        try:
            with self.assertRaises(RuntimeError):
                self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        finally:
            self.delivery.enqueue_in = original
        # The same call now succeeds: nothing was half-done that would block it.
        record = self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        self.assertEqual(record["nextExecutionGeneration"], 2)
        self.assertEqual(self._counts(), (2, 1, 1))

    def test_a_replayed_verdict_allocates_nothing_further(self):
        event_id = self._acknowledged()
        first = self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        again = self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        # The contract record is identical; the replay is additionally MARKED as historical,
        # so a caller can tell a fresh decision from a re-read of an old one.
        self.assertEqual(_contract(first), _contract(again))
        self.assertFalse(first.get("_replay"))
        self.assertTrue(again.get("_replay"))
        self.assertEqual(self._counts(), (2, 1, 1))

    def test_a_fault_at_commit_time_also_rolls_back(self):
        event_id = self._acknowledged()
        before = self._counts()
        self.store.fault_hook = lambda: (_ for _ in ()).throw(RuntimeError("commit-time fault"))
        try:
            with self.assertRaises(RuntimeError):
                self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
        finally:
            self.store.fault_hook = None
        self.assertEqual(self._counts(), before)

    def test_concurrent_replay_allocates_one_generation_and_one_revision(self):
        """Two writers racing the same verdict must not both allocate.

        BEGIN IMMEDIATE serialises them, and the loser re-reads the winner's verdict inside its
        own transaction, so the second call returns the first result instead of opening another
        generation.
        """
        import threading

        event_id = self._acknowledged()
        results, errors = [], []
        barrier = threading.Barrier(2)

        def run():
            from codex_session_relay.ack import AckService
            from codex_session_relay.delivery import DeliveryService
            from codex_session_relay.receipts import ReceiptIntake
            from codex_session_relay.registry import Registry
            from codex_session_relay.store import Store

            store = Store(self.store.path)
            try:
                registry = Registry(store, self.clock)
                intake = ReceiptIntake(store, registry, self.clock)
                delivery = DeliveryService(store, registry, intake, self.clock)
                ack = AckService(store, registry, intake, delivery, self.clock)
                barrier.wait(timeout=10)
                results.append(
                    ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1")
                )
            except Exception as error:  # noqa: BLE001 - recorded and asserted below
                errors.append(error)
            finally:
                store.close()

        threads = [threading.Thread(target=run) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)
        self.assertEqual(errors, [], f"both writers should succeed: {errors}")
        self.assertEqual(len(results), 2)
        self.assertEqual(_contract(results[0]), _contract(results[1]))
        # Exactly one of them allocated and exactly one re-read the other's verdict. Comparing
        # the dicts alone could not tell those apart, which is the thing this test is named for.
        self.assertEqual(
            sorted(bool(result.get("_replay")) for result in results), [False, True]
        )
        self.assertEqual(self._counts(), (2, 1, 1))
