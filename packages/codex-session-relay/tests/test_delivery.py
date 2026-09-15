"""JUN-91 delivery: busy handling, dispatch, bounds, and honest reporting."""

import unittest

from codex_session_relay.delivery import COMPLETION, REVISION
from codex_session_relay.errors import RefusalReason
from codex_session_relay.lifecycle import ARCHIVED, BUDGET_LIMITED, CANNOT_ACCEPT, PAUSED, UNKNOWN
from codex_session_relay.transport import (
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    QUEUED,
    WITHHELD_PRE_SEND,
)

from .support import CHILD, PARENT, DeliveryTestCase


class Queueing(DeliveryTestCase):
    def test_an_accepted_final_event_becomes_eligible(self):
        _relationship, event_id = self.queued_event()
        row = self.delivery_row(event_id)
        self.assertEqual(row["state"], QUEUED)
        self.assertEqual(row["recipient_task_id"], PARENT)
        eligible = self.delivery.eligible(now=self.clock.now())
        self.assertEqual([r["event_id"] for r in eligible], [event_id])

    def test_enqueue_is_idempotent(self):
        _relationship, event_id = self.queued_event()
        self.delivery.enqueue(event_id)
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM deliveries")["c"], 1)

    def test_a_staged_event_cannot_be_queued(self):
        relationship = self.register()
        path = self.artifact("out.txt", "still working")
        payload = self.ready_payload(relationship, [path], turn=self.assigned_turn("inProgress"))
        self.accept(payload)
        self.assertRefused(
            RefusalReason.NOT_CLAIMABLE, self.delivery.enqueue, payload["eventId"]
        )

    def test_a_recipient_outside_the_authorized_scope_is_refused_before_any_transport(self):
        _relationship, event_id = self.ready_event()
        self.assertRefused(
            RefusalReason.RECIPIENT_NOT_AUTHORIZED,
            lambda: self.delivery.enqueue(event_id, recipient_task_id="somebody-else"),
        )
        self.assertEqual(self.adapter.sends, [])

    def test_the_message_never_carries_a_recipient_turn_id(self):
        _relationship, event_id = self.queued_event()
        message = self.delivery.render_message(event_id)
        self.assertIn(event_id, message)
        self.assertNotIn("turn-", message.replace("your own turn id", ""))
        self.assertEqual(message, self.delivery.render_message(event_id))


class Busy(DeliveryTestCase):
    def test_an_active_recipient_is_deferred_without_opening_an_attempt(self):
        _relationship, event_id = self.queued_event()
        self.adapter.set_status(PARENT, "active")
        self.assertIsNone(self.attempt(event_id))
        row = self.delivery_row(event_id)
        self.assertEqual(row["state"], DEFERRED_BUSY)
        self.assertEqual(self.attempts_for(event_id), [])
        self.assertEqual(self.adapter.sends, [], "the transport was never called")

    def test_a_transport_busy_refusal_produces_a_real_deferred_attempt(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DEFERRED_BUSY)
        self.assertTrue(record["retrySafe"])
        self.assertEqual(record["sendAttempted"], "no")
        self.assertEqual(record["failedOperation"], "thread/read")

    def test_the_running_turn_is_not_interrupted(self):
        _relationship, event_id = self.queued_event()
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.adapter.set_status(PARENT, "active")
        self.attempt(event_id)
        still = self.adapter.read_turn(PARENT, turn.turn_id)
        self.assertEqual(still.status, "inProgress")

    def test_no_model_effort_sandbox_or_policy_override_is_ever_sent(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        request_id, thread_id, message, _outcome = self.adapter.sends[0]
        self.assertEqual(thread_id, PARENT)
        for forbidden in ("model", "effort", "sandbox", "approvalPolicy", "reasoning"):
            self.assertNotIn(forbidden, message)


class Dispatch(DeliveryTestCase):
    def test_an_idle_recipient_receives_a_push_with_a_real_turn_id(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertTrue(record["turnId"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_a_steered_existing_turn_is_recorded_not_assumed_fresh(self):
        _relationship, event_id = self.queued_event()
        self.adapter.start_turn(PARENT, turn_id="already-running", status="inProgress")
        self.adapter.script("steer_existing")
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(record["turnId"], "already-running")
        self.assertTrue(record["_turnPreviouslyObserved"])
        self.assertEqual(record["_turnOrigin"], "steered_observed_turn")

    def test_dispatched_is_not_delivered(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        snapshot = self.delivery.snapshot()["deliveries"][0]
        self.assertEqual(snapshot["state"], DISPATCHED)
        self.assertEqual(snapshot["reported"], "dispatched_awaiting_ack")
        self.assertFalse(snapshot["acknowledged"])

    def test_event_id_and_request_id_are_distinct(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.assertNotEqual(record["requestId"], record["eventId"])
        self.assertTrue(record["requestId"].startswith("del-"))
        self.assertIn(event_id[:12], record["requestId"])


class InboxOnly(DeliveryTestCase):
    def test_an_unsupported_approval_policy_is_stored_not_woken(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].approval_policy = "on-request"
        self.adapter.script("approval_policy")
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], INBOX_ONLY)
        self.assertEqual(record["recipientApprovalPolicy"], "on-request")
        self.assertFalse(record["retrySafe"])
        snapshot = self.delivery.snapshot()["deliveries"][0]
        self.assertEqual(snapshot["reported"], "stored_not_woken")
        self.assertEqual(snapshot["holdReason"], "push_channel_closed")

    def test_an_inbox_only_delivery_is_held_and_not_retried(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].approval_policy = "untrusted"
        self.adapter.script("approval_policy")
        self.attempt(event_id)
        self.clock.advance(100000)
        self.assertEqual(self.delivery.eligible(now=self.clock.now()), [])


class Retry(DeliveryTestCase):
    def test_a_retry_opens_a_new_attempt_number(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        first = self.attempt(event_id)
        self.clock.advance(3600)
        second = self.attempt(event_id, now=self.clock.now())
        self.assertEqual(first["attemptNo"], 1)
        self.assertEqual(second["attemptNo"], 2)
        self.assertNotEqual(first["requestId"], second["requestId"])

    def test_a_cached_busy_failure_is_never_replayed_as_the_outcome(self):
        """The transport answers a settled request id from its receipt, forever.

        So a retry that reused the id would read the old busy failure as a fresh result and
        loop on it. A new attempt number is what makes the second attempt a real send.
        """
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        first = self.attempt(event_id)
        self.clock.advance(3600)
        second = self.attempt(event_id, now=self.clock.now())
        self.assertEqual(second["deliveryState"], DISPATCHED)
        replayed = [s for s in self.adapter.sends if s[0] == first["requestId"]]
        self.assertEqual(len(replayed), 1, "the first request id was never sent twice")

    def test_a_dispatched_delivery_is_not_claimable_again(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        self.clock.advance(100000)
        self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual(len(self.attempts_for(event_id)), 1)

    def test_an_uncertain_attempt_is_never_retried_by_elapsed_time(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("turn_start_fail")
        self.attempt(event_id)
        self.assertEqual(self.delivery_row(event_id)["state"], HELD_UNCERTAIN)
        for _ in range(5):
            self.clock.advance(86400)
            self.assertEqual(self.delivery.eligible(now=self.clock.now()), [])
            self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual(len(self.attempts_for(event_id)), 1)


class Bounds(DeliveryTestCase):
    def test_repeated_pre_send_failure_reaches_a_cap_and_holds(self):
        _relationship, event_id = self.queued_event()
        for _ in range(self.delivery.policy.max_attempts):
            self.adapter.script("read_fail")
            self.clock.advance(100000)
            self.attempt(event_id, now=self.clock.now())
        row = self.delivery_row(event_id)
        self.assertEqual(row["hold_reason"], "attempt_cap")
        self.clock.advance(100000)
        self.assertEqual(self.delivery.eligible(now=self.clock.now()), [])

    def test_a_minimum_interval_prevents_a_flood(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("read_fail")
        self.attempt(event_id)
        # Immediately eligible by backoff would still be refused by the rate limiter.
        self.delivery._reschedule(
            event_id, WITHHELD_PRE_SEND, self.clock.now(),
            attempts=self.delivery_row(event_id)["attempt_count"],
        )
        self.assertIsNone(self.attempt(event_id, now=self.clock.now() + 1))
        self.assertEqual(len(self.attempts_for(event_id)), 1)

    def test_an_hourly_cap_prevents_a_flood(self):
        policy = self.delivery.policy
        _relationship, event_id = self.queued_event()
        now = self.clock.now()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"
                " VALUES (?,?,?,?)",
                (PARENT, int(now // 3600) * 3600, policy.max_sends_per_recipient_per_hour, 0),
            )
        self.assertIsNone(self.attempt(event_id, now=now))
        self.assertEqual(self.adapter.sends, [])


class HostLifecycle(DeliveryTestCase):
    """The relationship says what we may do; the host says what is possible."""

    def _withheld(self, event_id, reason):
        """Withheld is a DEFERRAL, not a permanent hold.

        A recipient that is archived or paused today may not be tomorrow, so the delivery keeps
        no hold_reason and simply becomes eligible again after the recheck interval. What was
        observed is recorded, so an operator can see why nothing went out.
        """
        self.assertIsNone(self.attempt(event_id))
        row = self.delivery_row(event_id)
        self.assertIsNone(row["hold_reason"], "a host state is never a permanent hold")
        self.assertIsNotNone(row["next_eligible_at"])
        observed = self.store.one(
            "SELECT * FROM recipient_lifecycle WHERE task_id = ?", (PARENT,)
        )
        self.assertEqual(observed["withhold_reason"], reason)
        self.assertEqual(self.adapter.sends, [])

    def test_a_host_archived_recipient_is_withheld_while_the_relationship_is_active(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].archived = True
        self.assertEqual(self.registry.get(self._rid)["status"], "active")
        self._withheld(event_id, ARCHIVED)

    def test_a_host_paused_goal_is_withheld(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].goal_status = "paused"
        self._withheld(event_id, PAUSED)

    def test_a_budget_limited_goal_is_withheld(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].goal_status = "budgetLimited"
        self._withheld(event_id, BUDGET_LIMITED)

    def test_a_recipient_that_cannot_accept_direct_input_is_withheld(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].can_accept_input = False
        self._withheld(event_id, CANNOT_ACCEPT)

    def test_an_unreadable_lifecycle_withholds_rather_than_guessing(self):
        _relationship, event_id = self.queued_event()
        self.adapter.fail_reads("read_goal_status")
        self.assertIsNone(self.attempt(event_id))
        observed = self.store.one(
            "SELECT * FROM recipient_lifecycle WHERE task_id = ?", (PARENT,)
        )
        self.assertEqual(observed["deliverable"], "unknown")
        self.assertEqual(observed["withhold_reason"], UNKNOWN)
        self.assertEqual(self.adapter.sends, [])

    def test_a_later_successful_observation_releases_the_withheld_delivery(self):
        """No SQL, no operator step: the next observation after the interval decides again."""
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].goal_status = "paused"
        self.attempt(event_id)
        self.adapter.threads[PARENT].goal_status = "active"
        self.clock.advance(self.delivery.policy.lifecycle_recheck_seconds + 1)
        self.assertTrue(self.delivery.eligible(now=self.clock.now()))
        record = self.attempt(event_id, now=self.clock.now())
        self.assertEqual(record["deliveryState"], DISPATCHED)

    def test_an_archived_recipient_that_is_restored_is_delivered_without_intervention(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].archived = True
        self.attempt(event_id)
        self.adapter.threads[PARENT].archived = False
        self.clock.advance(self.delivery.policy.lifecycle_recheck_seconds + 1)
        record = self.attempt(event_id, now=self.clock.now())
        self.assertEqual(record["deliveryState"], DISPATCHED)

    def test_an_idle_status_alone_is_not_treated_as_deliverable(self):
        _relationship, event_id = self.queued_event()
        self.adapter.threads[PARENT].archived = True
        self.adapter.threads[PARENT].status = "idle"
        self._withheld(event_id, ARCHIVED)


class AuthorizationRace(DeliveryTestCase):
    """A pause committed while we were reading the host must not be overtaken by a send."""

    def _pause_during_reads(self, status="paused"):
        original = self.adapter.read_goal_status

        def pause_then_read(thread_id):
            self.registry.set_status(self._rid, status, actor="user")
            return original(thread_id)

        self.adapter.read_goal_status = pause_then_read

    def test_a_pause_between_the_precheck_and_the_claim_blocks_the_send(self):
        _relationship, event_id = self.queued_event()
        self._pause_during_reads("paused")
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])
        self.assertEqual(self.attempts_for(event_id), [])

    def test_an_archive_in_the_same_window_blocks_the_send(self):
        _relationship, event_id = self.queued_event()
        self._pause_during_reads("archived")
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])

    def test_a_supersession_in_the_same_window_blocks_the_send(self):
        _relationship, event_id = self.queued_event()
        original = self.adapter.read_goal_status

        def supersede_then_read(thread_id):
            self.registry.supersede(self._rid, new_relationship_id="rel-bbbbbbbbbbbbbbbb")
            return original(thread_id)

        self.adapter.read_goal_status = supersede_then_read
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])

    def test_a_paused_relationship_is_never_eligible(self):
        _relationship, event_id = self.queued_event()
        self.registry.set_status(self._rid, "paused", actor="user")
        self.assertEqual(self.delivery.eligible(now=self.clock.now()), [])


if __name__ == "__main__":
    unittest.main()


class DuplicateSendGuards(DeliveryTestCase):
    """The two reproduced ways one event could be sent twice."""

    def test_a_stale_busy_observation_cannot_overwrite_a_successful_dispatch(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        # A caller that read "active" before the dispatch landed now tries to defer it.
        row = self.delivery_row(event_id)
        self.delivery._defer_busy(event_id, dict(row, state=QUEUED, attempt_count=0), self.clock.now())
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.clock.advance(100000)
        self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual(len(self.adapter.sends), 1)

    def test_reconciling_an_older_attempt_cannot_reopen_a_dispatched_delivery(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("busy")
        first = self.attempt(event_id)
        self.clock.advance(3600)
        second = self.attempt(event_id, now=self.clock.now())
        self.assertEqual(second["deliveryState"], DISPATCHED)
        # Now reconcile the OLD busy attempt. It is retry-safe, but it is not current.
        self.reconciler.reconcile_attempt(first["requestId"], self.adapter)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.clock.advance(100000)
        self.assertIsNone(self.attempt(event_id, now=self.clock.now()))
        self.assertEqual(len(self.adapter.sends), 2)

    def test_a_reconciled_pre_send_failure_still_obeys_the_attempt_cap(self):
        _relationship, event_id = self.queued_event()
        for _ in range(self.delivery.policy.max_attempts):
            self.adapter.script("in_progress")
            self.clock.advance(100000)
            record = self.attempt(event_id, now=self.clock.now())
            if record is None:
                break
            self.adapter.ledger[record["requestId"]] = {
                "requestId": record["requestId"], "status": "failed",
                "error": "thread/read: refused",
                "rpcError": {"code": "internal", "message": "refused"},
            }
            self.reconciler.reconcile_attempt(record["requestId"], self.adapter, now=self.clock.now())
        self.assertEqual(self.delivery_row(event_id)["hold_reason"], "attempt_cap")
        self.assertLessEqual(len(self.attempts_for(event_id)), self.delivery.policy.max_attempts)

    def test_receipt_recovery_keeps_the_dispatch_turn_and_provenance(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("in_progress")
        record = self.attempt(event_id)
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.adapter.ledger[record["requestId"]] = {
            "requestId": record["requestId"], "status": "accepted",
            "resumed": {"approvalPolicy": "never"}, "turnId": turn.turn_id,
        }
        self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        row = self.delivery_row(event_id)
        self.assertEqual(row["state"], DISPATCHED)
        self.assertEqual(row["dispatch_turn_id"], turn.turn_id)
        self.assertEqual(row["dispatch_evidence"], "transport_accepted")


class ContinuationAdmission(DeliveryTestCase):
    """A loop spans many turns, and a later turn needs a real admission, not an inference."""

    def _later_turn_payload(self, relationship, turn_id="turn-loop-3"):
        path = self.artifact("out.txt", "finished after several turns")
        return self.ready_payload(
            relationship, [path], turn=self.assigned_turn("completed", turn=turn_id)
        )

    def test_a_later_turn_without_an_admission_is_refused(self):
        relationship = self.register()
        payload = self._later_turn_payload(relationship)
        error = self.assertRefused(RefusalReason.UNASSIGNED_TURN, self.accept, payload)
        self.assertIn("explicit continuation admission", error.detail)

    def test_a_later_turn_with_an_explicit_admission_is_accepted(self):
        relationship = self.register()
        payload = self._later_turn_payload(relationship)
        stored = self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn("completed", turn="turn-loop-3"),
            continuation={
                "anchorTurnId": "turn-dispatch-1",
                "actor": "child-loop",
                "reason": "PABCD cycle 3 completed this generation",
            },
        )
        self.assertEqual(stored["outcome"], "ready_for_review")
        admitted = self.store.one(
            "SELECT * FROM generation_turns WHERE turn_id = ?", ("turn-loop-3",)
        )
        self.assertEqual(admitted["evidence"], "explicit_admission")
        self.assertIn("PABCD cycle 3", admitted["detail"])

    def test_an_admission_naming_the_wrong_anchor_is_refused(self):
        relationship = self.register()
        payload = self._later_turn_payload(relationship)
        error = self.assertRefused(
            RefusalReason.UNASSIGNED_TURN,
            lambda: self.intake.accept_child_receipt(
                payload, observation=self.assigned_turn("completed", turn="turn-loop-3"),
                continuation={
                    "anchorTurnId": "some-other-execution", "actor": "a", "reason": "b",
                },
            ),
        )
        self.assertIn("is anchored to", error.detail)

    def test_an_admission_still_requires_the_registered_child_thread(self):
        relationship = self.register()
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(
            relationship, [path],
            turn=self.assigned_turn("completed", thread="someone-else", turn="turn-loop-3"),
        )
        self.assertRefused(
            RefusalReason.UNASSIGNED_TURN,
            lambda: self.intake.accept_child_receipt(
                payload, observation=self.assigned_turn(
                    "completed", thread="someone-else", turn="turn-loop-3"
                ),
                continuation={
                    "anchorTurnId": "turn-dispatch-1", "actor": "a", "reason": "b",
                },
            ),
        )

    def test_a_malformed_admission_is_refused(self):
        relationship = self.register()
        payload = self._later_turn_payload(relationship)
        self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT,
            lambda: self.intake.accept_child_receipt(
                payload, observation=self.assigned_turn("completed", turn="turn-loop-3"),
                continuation={"anchorTurnId": "turn-dispatch-1"},
            ),
        )

    def test_once_admitted_a_turn_stays_admitted(self):
        relationship = self.register()
        payload = self._later_turn_payload(relationship)
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn("completed", turn="turn-loop-3"),
            continuation={"anchorTurnId": "turn-dispatch-1", "actor": "a", "reason": "b"},
        )
        again = self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn("completed", turn="turn-loop-3")
        )
        self.assertTrue(again["_duplicate"])


class DirectionalMessages(DeliveryTestCase):
    """Each direction is answered differently, so each must be instructed differently."""

    def _revision(self):
        from codex_session_relay import identity

        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="verdict-1",
            criteria=[{"id": "c-1", "verdict": "needs_changes",
                       "note": "the manifest omits the migration script"}],
        )
        row = self.store.one("SELECT * FROM deliveries WHERE kind = 'revision_request'")
        return event_id, row["event_id"]

    def test_the_completion_message_carries_the_deliverables_and_the_ack_instruction(self):
        _relationship, event_id = self.queued_event()
        message = self.delivery.render_message(event_id)
        receipt = self.intake.get(event_id)
        self.assertIn("verification request", message)
        self.assertIn(receipt["revisionHash"], message)
        self.assertIn(receipt["manifest"][0]["path"], message)
        self.assertIn(receipt["manifest"][0]["sha256"], message)
        self.assertIn("ack-proof", message)
        self.assertIn("show --event", message)
        self.assertNotIn("None", message)

    def test_the_revision_message_never_asks_the_child_for_an_acknowledgement(self):
        _source, revision_event = self._revision()
        message = self.delivery.render_message(revision_event)
        self.assertIn("revision request", message)
        # No acknowledgement INSTRUCTION of any form, since the relay would refuse it.
        self.assertNotIn("ack-proof --event", message)
        self.assertNotIn("--ack-proof", message)
        self.assertNotIn("--ack-turn", message)
        self.assertIn("nothing to acknowledge", message)
        self.assertIn("emit --relationship", message)

    def test_the_revision_message_says_what_to_change(self):
        source, revision_event = self._revision()
        message = self.delivery.render_message(revision_event)
        self.assertIn("the manifest omits the migration script", message)
        self.assertIn(source, message, "it names the event being superseded")
        self.assertIn("--generation 2", message)
        self.assertNotIn("None", message)

    def test_the_instruction_each_side_is_given_is_the_one_that_actually_works(self):
        from codex_session_relay import identity
        from codex_session_relay.errors import RefusalReason

        source, revision_event = self._revision()
        # The parent's instruction worked: the acknowledgement above was accepted.
        self.assertIsNotNone(self.store.one("SELECT 1 FROM acks WHERE event_id = ?", (source,)))
        # The child's would-be acknowledgement is refused by kind, which is exactly why the
        # revision message must not ask for one.
        self.assertRefused(
            RefusalReason.WRONG_DELIVERY_KIND,
            lambda: self.ack.acknowledge(
                revision_event, ack_turn_id="child-turn",
                ack_proof=identity.ack_proof(revision_event, "child-turn"), accepted=True,
                adapter=self.adapter,
            ),
        )
        # What the message DOES tell the child to do is available: emit under generation 2.
        relationship = self.registry.get(self._rid)
        self.assertEqual(relationship["executionGeneration"], 2)
