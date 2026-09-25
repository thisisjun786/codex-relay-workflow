"""The assignment ledger: one issue, one responsible child, and where that assignment stands."""

import os
import threading

from codex_session_relay import identity
from codex_session_relay.assignment import (
    AMBIGUOUS_STATE,
    AssignmentView,
    CORRECTED,
    MERGED,
    NEEDS_CHANGES,
    PAUSED,
    RECEIVED,
    REQUESTED,
    VERIFIED,
    VERIFYING,
)
from codex_session_relay.errors import RefusalReason
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store

from .support import CHILD, HOST, ISSUE, PARENT, DeliveryTestCase

OTHER_CHILD = "01other-child"


class AssignmentTestCase(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        self.assignments = AssignmentView(self.store, self.registry, self.clock)

    def state(self):
        return self.assignments.state(self._rid)["state"]

    def verified_head(self, text="the deliverable"):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD], text=text)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v1")
        return event_id


class AssignmentStates(AssignmentTestCase):
    def test_registered_only_is_requested(self):
        self._rid = self.register()["relationshipId"]
        self.assertEqual(self.state(), REQUESTED)
        self.assertEqual(
            self.assignments.state(self._rid)["nextExpectedAction"], "child_emits"
        )

    def test_a_receipt_is_received(self):
        self.ready_event()
        self.assertEqual(self.state(), RECEIVED)

    def test_a_claimed_head_without_a_verdict_is_verifying(self):
        _relationship, event_id = self.ready_event()
        self.ack.claim_verification(event_id, turn_id="ack-turn")
        self.assertEqual(self.state(), VERIFYING)

    def test_a_verified_head_is_verified(self):
        self.verified_head()
        self.assertEqual(self.state(), VERIFIED)
        self.assertEqual(
            self.assignments.state(self._rid)["nextExpectedAction"], "coordinator_integrates"
        )

    def test_needs_changes_then_a_new_receipt_is_corrected(self):
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
            event_id, verdict="needs_changes", verdict_turn_id="v1",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix the shape"}],
        )
        self.assertEqual(self.state(), NEEDS_CHANGES)
        # The verdict queued the correction in its own transaction and nothing has sent it, so
        # what happens next is the relay delivering it, not the child correcting.
        self.assertEqual(
            self.assignments.state(self._rid)["nextExpectedAction"], "daemon_delivers_correction"
        )
        correction = self.assignments.state(self._rid)["projection"]["correction"]["eventId"]
        self.clock.advance(1)
        self.attempt(correction)
        self.assertEqual(
            self.assignments.state(self._rid)["nextExpectedAction"], "child_corrects"
        )

        relationship = self.registry.get(self._rid)
        generation = relationship["executionGeneration"]
        self.registry.bind_anchor(
            self._rid, generation, dispatch_turn_id="revision-turn",
            source="dispatch_receipt",
        )
        path = self.artifact("out.txt", "the corrected revision")
        payload = self.ready_payload(
            relationship, [path], generation=generation,
            turn=self.assigned_turn(turn="revision-turn"),
        )
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn(turn="revision-turn")
        )
        self.assertEqual(self.state(), CORRECTED)

    def test_a_paused_assignment_is_paused_not_verified(self):
        """An older verified must never hide a pause the operator needs to see."""
        self.verified_head()
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        self.assertEqual(self.state(), PAUSED)

    def test_two_undeclared_revisions_are_ambiguous_not_verified(self):
        self.verified_head()
        self.ready_event(register=False, text="a competing revision")
        self.assertEqual(self.state(), AMBIGUOUS_STATE)

    def test_an_explicit_mark_is_merged(self):
        event_id = self.verified_head()
        record = self.assignments.mark(
            self._rid, "merged", evidence="merged into dev as abc1234", actor=PARENT,
            expected_event=event_id,
        )
        self.assertEqual(record["state"], MERGED)
        self.assertEqual(record["mark"]["revisionHash"], record["head"]["revisionHash"])

    def test_an_unverified_head_cannot_be_marked_merged(self):
        _relationship, event_id = self.ready_event()
        self.assertRefused(
            RefusalReason.NOT_ACKNOWLEDGED,
            lambda: self.assignments.mark(
                self._rid, "merged", evidence="merged anyway", actor=PARENT,
                expected_event=event_id,
            ),
        )

    def test_a_mark_does_not_survive_a_new_generation(self):
        """One old merge must not label every later generation merged."""
        event_id = self.verified_head()
        self.assignments.mark(
            self._rid, "merged", evidence="merged as abc1234", actor=PARENT,
            expected_event=event_id,
        )
        self.registry.open_generation(
            self._rid, dispatch_request_id="newer", reason="needs_changes_revision",
            dispatch_turn_id="newer-turn",
        )
        record = self.assignments.state(self._rid)
        self.assertNotEqual(record["state"], MERGED)
        self.assertIsNone(record["mark"])
        self.assertEqual(len(record["markHistory"]), 1)

    def test_a_mark_does_not_survive_a_new_revision(self):
        event_id = self.verified_head()
        self.assignments.mark(
            self._rid, "merged", evidence="merged as abc1234", actor=PARENT,
            expected_event=event_id,
        )
        superseded = self.intake.row(event_id)["revision_hash"]
        path = self.artifact("out.txt", "a newer revision after the merge")
        payload = self.ready_payload(self.registry.get(self._rid), [path])
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn(), supersedes_revision=superseded
        )
        record = self.assignments.state(self._rid)
        self.assertNotEqual(record["state"], MERGED)
        self.assertEqual(record["head"]["eventId"], payload["eventId"])
        self.assertEqual(len(record["markHistory"]), 1)

    def test_a_pause_after_verification_is_observable(self):
        event_id = self.verified_head()
        self.assignments.mark(
            self._rid, "merged", evidence="merged as abc1234", actor=PARENT,
            expected_event=event_id,
        )
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        self.assertEqual(self.state(), PAUSED)


class MarkContext(AssignmentTestCase):
    """An integrator states what it integrated, and stale context is refused."""

    def test_marking_a_revision_that_is_no_longer_current_is_refused(self):
        first = self.verified_head()
        superseded = self.intake.row(first)["revision_hash"]
        path = self.artifact("out.txt", "a newer revision")
        payload = self.ready_payload(self.registry.get(self._rid), [path])
        self.intake.accept_child_receipt(
            payload, observation=self.assigned_turn(), supersedes_revision=superseded
        )
        self.delivery.enqueue(payload["eventId"])
        self.attempt(payload["eventId"])
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn-2", status="inProgress")
        self.ack.acknowledge(
            payload["eventId"], ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(payload["eventId"], turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(payload["eventId"], verdict="verified", verdict_turn_id="v2")

        # The operator integrated the FIRST revision and says so. Silently binding the merge to
        # whatever became current in between is the opposite of a record.
        self.assertRefused(
            RefusalReason.STALE_MARK_CONTEXT,
            lambda: self.assignments.mark(
                self._rid, "merged", evidence="integrated the first revision", actor=PARENT,
                expected_event=first,
            ),
        )

    def test_a_mark_without_an_expected_event_is_refused(self):
        self.verified_head()
        self.assertRefused(
            RefusalReason.STALE_MARK_CONTEXT,
            lambda: self.assignments.mark(
                self._rid, "merged", evidence="merged", actor=PARENT, expected_event="",
            ),
        )


class CriteriaCurrency(AssignmentTestCase):
    """A verdict certifies the wording it actually ruled on, and only that wording."""

    SET = [
        {"id": "c1", "title": "the endpoint returns the agreed shape"},
        {"id": "c2", "title": "a malformed request is refused"},
    ]

    def managed_verified(self):
        from codex_session_relay.criteria import CriteriaService

        self.criteria = CriteriaService(self.store, self.clock)
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.criteria.register(self._rid, self.SET, source_ref="https://linear.app/doc/1")
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.claim_verification(event_id, turn_id=turn.turn_id)
        self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v1",
            findings=[
                {"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"},
            ],
        )
        return event_id

    def edit_criteria(self):
        self.criteria.register(
            self._rid,
            [
                {"id": "c1", "title": "the endpoint returns a COMPLETELY different shape"},
                {"id": "c2", "title": "a malformed request is refused"},
            ],
            source_ref="https://linear.app/doc/1",
        )

    def test_editing_the_criteria_after_a_verdict_stops_it_reading_as_verified(self):
        self.managed_verified()
        self.assertEqual(self.state(), VERIFIED)
        self.edit_criteria()
        record = self.assignments.state(self._rid)
        self.assertEqual(record["state"], "re_review_needed")
        self.assertEqual(record["nextExpectedAction"], "parent_verifies")
        self.assertFalse(record["criteria"]["current"])
        self.assertNotEqual(
            record["criteria"]["reviewedSetDigest"], record["criteria"]["setDigest"]
        )
        # The verdict itself is retained: it is history, not a deletion.
        self.assertEqual(record["lastVerdict"]["verdict"], "verified")

    def test_editing_the_criteria_after_a_verdict_blocks_the_merge_mark(self):
        event_id = self.managed_verified()
        self.edit_criteria()
        self.assertRefused(
            RefusalReason.CRITERIA_SET_CHANGED,
            lambda: self.assignments.mark(
                self._rid, "merged", evidence="merged as abc1234", actor=PARENT,
                expected_event=event_id,
            ),
        )

    def test_an_existing_mark_stops_being_state_when_the_criteria_change(self):
        event_id = self.managed_verified()
        self.assignments.mark(
            self._rid, "merged", evidence="merged as abc1234", actor=PARENT,
            expected_event=event_id,
        )
        self.assertEqual(self.state(), MERGED)
        self.edit_criteria()
        record = self.assignments.state(self._rid)
        self.assertEqual(record["state"], "re_review_needed")
        self.assertIsNone(record["mark"])
        self.assertEqual(len(record["markHistory"]), 1)


class DuplicateAssignment(AssignmentTestCase):
    def rival(self, child=OTHER_CHILD, **kw):
        return self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint(child, HOST, cwd=self.root),
            issue_key=kw.pop("issue_key", ISSUE),
            artifact_roots=[self.root],
            allowed_recipients=[PARENT],
            dispatch_request_id=kw.pop("dispatch_request_id", "rival-dispatch"),
            dispatch_turn_id="rival-turn",
            **kw,
        )

    def test_a_second_child_for_the_same_issue_is_refused(self):
        self.register()
        self.assertRefused(RefusalReason.DUPLICATE_ASSIGNMENT, self.rival)

    def test_a_paused_assignment_still_owns_its_issue(self):
        """A pause is a temporary state, never permission to open a second assignment."""
        relationship = self.register()
        self.registry.set_status(relationship["relationshipId"], "paused", actor=PARENT)
        self.assertRefused(RefusalReason.DUPLICATE_ASSIGNMENT, self.rival)

    def test_an_archived_assignment_releases_the_issue(self):
        relationship = self.register()
        self.registry.set_status(relationship["relationshipId"], "archived", actor=PARENT)
        self.assertEqual(self.rival()["child"]["taskId"], OTHER_CHILD)

    def test_re_registering_the_same_pair_stays_idempotent(self):
        first = self.register()
        again = self.register()
        self.assertEqual(first["relationshipId"], again["relationshipId"])

    def test_supersedes_allows_a_deliberate_replacement(self):
        relationship = self.register()
        replacement = self.rival(supersedes=relationship["relationshipId"])
        self.assertEqual(replacement["child"]["taskId"], OTHER_CHILD)
        self.assertEqual(
            self.registry.get(relationship["relationshipId"])["status"], "archived"
        )

    def test_two_concurrent_registrations_produce_exactly_one_assignment(self):
        """Checked outside the write transaction, both writers would have seen no rival."""
        barrier = threading.Barrier(2)
        results, errors = [], []

        def run(child):
            store = Store(self.store.path)
            try:
                registry = Registry(store, self.clock)
                barrier.wait(timeout=10)
                results.append(registry.register(
                    parent=Endpoint(PARENT, HOST, cwd="/parent"),
                    child=Endpoint(child, HOST, cwd=self.root),
                    issue_key=ISSUE,
                    artifact_roots=[self.root],
                    allowed_recipients=[PARENT],
                    dispatch_request_id=f"dispatch-{child}",
                    dispatch_turn_id=f"turn-{child}",
                ))
            except Exception as error:  # noqa: BLE001 - asserted below
                errors.append(error)
            finally:
                store.close()

        threads = [
            threading.Thread(target=run, args=(child,))
            for child in (CHILD, OTHER_CHILD)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)

        self.assertEqual(len(results), 1, f"exactly one should win: {results}")
        self.assertEqual(len(errors), 1, f"exactly one should be refused: {errors}")
        self.assertEqual(errors[0].reason, RefusalReason.DUPLICATE_ASSIGNMENT)
        owning = self.store.all(
            "SELECT * FROM relationships WHERE issue_key = ? AND status = 'active'", (ISSUE,)
        )
        self.assertEqual(len(owning), 1)


class AssignmentLookup(AssignmentTestCase):
    def test_for_issue_names_the_child_to_reuse(self):
        relationship = self.register()
        found = self.assignments.for_issue(ISSUE)
        self.assertEqual(found["responsibleChild"], CHILD)
        self.assertEqual(found["responsibleRelationship"], relationship["relationshipId"])

    def test_for_issue_still_names_a_paused_owner(self):
        self.register()
        self.registry.set_status(self._rid_for(ISSUE), "paused", actor=PARENT)
        self.assertEqual(self.assignments.for_issue(ISSUE)["responsibleChild"], CHILD)

    def test_for_issue_is_empty_when_nothing_is_assigned(self):
        found = self.assignments.for_issue("NOT-AN-ISSUE")
        self.assertIsNone(found["responsibleChild"])
        self.assertEqual(found["assignments"], [])

    def _rid_for(self, issue_key):
        return self.store.one(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?", (issue_key,)
        )["relationship_id"]


class TheAnswerNamesTheStoreItCameFrom(AssignmentTestCase):
    """A lookup that cannot say which store it read cannot be compared with the packet.

    OPS-3.4 makes the proof a conjunction: doctor reporting the packet's state directory AND
    this lookup naming the expected relationship. A mistyped state directory creates an empty
    store, and an empty store answers "nothing is assigned" perfectly honestly - after which a
    coordinator opens a duplicate writer. These tests pin the provenance that lets the caller
    notice it asked the wrong file.
    """

    def test_holds_is_false_before_registration_and_true_after(self):
        self.assertFalse(self.assignments.for_issue(ISSUE)["relay"]["holds"])
        self.register()
        self.assertTrue(self.assignments.for_issue(ISSUE)["relay"]["holds"])

    def test_holds_agrees_with_the_responsible_relationship_it_is_derived_from(self):
        for issue in (ISSUE, "NOT-AN-ISSUE"):
            found = self.assignments.for_issue(issue)
            self.assertEqual(
                found["relay"]["holds"], found["responsibleRelationship"] is not None
            )

    def test_a_paused_owner_still_holds_the_issue(self):
        self.register()
        rid = self.store.one(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?", (ISSUE,)
        )["relationship_id"]
        self.registry.set_status(rid, "paused", actor=PARENT)
        self.assertTrue(self.assignments.for_issue(ISSUE)["relay"]["holds"])

    def test_the_empty_store_answers_with_its_own_identity_not_silence(self):
        """The dangerous answer is an honest 'no assignment' from the wrong file."""
        found = self.assignments.for_issue("NOT-AN-ISSUE")
        self.assertFalse(found["relay"]["holds"])
        self.assertEqual(found["relay"]["store"]["storeId"], self.store.identity)
        self.assertEqual(found["relay"]["store"]["dbPath"], str(self.store.path))

    def test_two_stores_are_distinguishable_by_the_reading_alone(self):
        other = Store(os.path.join(self.tmp, "elsewhere", "relay.sqlite3"))
        self.addCleanup(other.close)
        mine = self.assignments.for_issue(ISSUE)["relay"]["store"]
        theirs = AssignmentView(
            other, Registry(other, self.clock), self.clock
        ).for_issue(ISSUE)["relay"]["store"]
        self.assertNotEqual(mine["storeId"], theirs["storeId"])
        self.assertNotEqual(mine["inode"], theirs["inode"])

    def test_the_recorded_socket_is_provenance_and_does_not_follow_a_later_process(self):
        """schema_meta records the socket that CREATED the store, first write wins.

        That is the point: a participant pointing at a different socket still reads this
        value, so the two can be seen to disagree instead of the later one quietly winning.
        """
        path = os.path.join(self.tmp, "socketed", "relay.sqlite3")
        first = Store(path, socket_path="/tmp/crw125-first.sock")
        self.addCleanup(first.close)
        view = AssignmentView(first, Registry(first, self.clock), self.clock)
        recorded = view.for_issue(ISSUE)["relay"]["store"]["recordedSocket"]
        self.assertTrue(recorded.endswith("crw125-first.sock"))

        second = Store(path, socket_path="/tmp/crw125-second.sock")
        self.addCleanup(second.close)
        again = AssignmentView(
            second, Registry(second, self.clock), self.clock
        ).for_issue(ISSUE)["relay"]["store"]["recordedSocket"]
        self.assertEqual(again, recorded)

    def test_a_store_opened_without_a_socket_records_none(self):
        self.assertIsNone(
            self.assignments.for_issue(ISSUE)["relay"]["store"]["recordedSocket"]
        )


class TheAxesStayApart(AssignmentTestCase):
    """Five vocabularies answer five different questions and none of them substitutes.

    The status names CRW-125 asks to be told apart come from different tables: staged is an
    EVENT stage, queued and dispatched and acknowledged are DELIVERY states, and an
    acknowledgement settles on one axis while carrying its evidence on another. Reporting any
    of them as another would let a bridge receipt read as receipt, application or verification.
    """

    def projection(self):
        return self.assignments.state(self._rid)["projection"]

    def test_a_staged_event_has_no_delivery_at_all(self):
        """Staged is real recorded progress and it is NOT a delivery state."""
        relationship = self.register(recipients=[PARENT, CHILD])
        self._rid = relationship["relationshipId"]
        payload = self.ready_payload(
            relationship, [self.artifact("out.txt", "staged work")],
            turn=self.assigned_turn(status="inProgress"),
        )
        self.accept(payload, observation=self.assigned_turn(status="inProgress"))
        completion = self.projection()["completion"]
        self.assertEqual(completion["event"]["stage"], "staged")
        self.assertIsNone(
            completion["delivery"],
            "a staged receipt has no delivery row, so it cannot carry a delivery state",
        )

    def test_the_axes_keep_their_own_vocabulary_through_the_lifecycle(self):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        queued = self.projection()["completion"]
        self.assertEqual(queued["event"]["stage"], "final")
        self.assertEqual(queued["delivery"]["state"], "queued")
        self.assertIsNone(queued["ack"]["settlement"])
        self.assertEqual(queued["ack"]["evidenceTier"], "unrecorded")

        self.attempt(event_id)
        dispatched = self.projection()["completion"]
        self.assertEqual(dispatched["delivery"]["state"], "dispatched")
        # The event stage did not move because delivery is a different question.
        self.assertEqual(dispatched["event"]["stage"], "final")

    def test_the_request_id_names_the_current_attempt_not_the_first(self):
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        delivery = self.projection()["completion"]["delivery"]
        rows = self.attempts_for(event_id)
        current = self.store.one(
            "SELECT attempt_count FROM deliveries WHERE event_id = ?", (event_id,)
        )["attempt_count"]
        self.assertEqual(delivery["attemptNo"], current)
        self.assertEqual(delivery["requestId"], rows[-1]["request_id"])

    def test_a_generation_with_no_correction_answers_null_not_a_borrowed_row(self):
        self.queued_event(recipients=[PARENT, CHILD])
        correction = self.projection()["correction"]
        self.assertIsNone(correction["eventId"])
        self.assertIsNone(correction["delivery"])
        self.assertIn("no such event", correction["detail"])

    def test_the_verdict_and_state_are_referenced_rather_than_recomputed(self):
        """A second derivation of a fact state() already derived is a second source of truth."""
        self.queued_event(recipients=[PARENT, CHILD])
        record = self.assignments.state(self._rid)
        self.assertIs(record["projection"]["verdict"], record["lastVerdict"])
        self.assertEqual(record["projection"]["assignment"]["state"], record["state"])

    def test_a_verified_rejection_does_not_read_like_a_verified_acceptance(self):
        """acks records the disposition and the turn verification independently.

        Both settle as verified. Reading only that axis reports a parent who REFUSED the
        completion exactly like one who accepted it, which is the collapse this whole
        projection exists to prevent.
        """
        from codex_session_relay import identity

        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id),
            accepted=False, rejection_reason="revision_mismatch", adapter=self.adapter,
        )
        ack = self.projection()["completion"]["ack"]
        self.assertFalse(ack["accepted"])
        self.assertEqual(ack["rejectionReason"], "revision_mismatch")
        # The verification axis is unchanged by the refusal, which is exactly why it cannot
        # stand in for the disposition.
        self.assertEqual(ack["settlement"], "verified")

    def test_a_refusal_stops_being_the_reason_once_a_delivery_exists(self):
        """Refusals are durable history; the same event can be accepted after its cause is fixed.

        Returning the old refusal once a delivery has dispatched would have the projection
        contradict itself.
        """
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.intake.record_refusal(
            "unassigned_turn", relationship_id=self._rid, event=event_id,
            detail="the earlier attempt named a turn it did not own",
        )
        self.attempt(event_id)
        completion = self.projection()["completion"]
        self.assertEqual(completion["delivery"]["state"], "dispatched")
        self.assertIsNone(
            completion["undeliveredReason"],
            "a dispatched delivery was reported with a historical refusal as its reason",
        )

    def test_provenance_says_whether_it_identified_one_file(self):
        relay = self.assignments.for_issue(ISSUE)["relay"]
        self.assertTrue(relay["store"]["identified"])
        self.assertIsNone(relay["store"]["detail"])
        self.assertEqual(relay["store"]["storeId"], self.store.identity)

    def test_one_anchor_reads_its_whole_lifecycle_in_a_single_statement(self):
        """Five statements are five snapshots, and a delivery worker commits between them.

        SQLite gives every autocommit SELECT its own snapshot, so reading the event, the
        delivery, the attempt and the acknowledgement separately can pair a delivery state
        with an acknowledgement that never coexisted with it - the combination this whole
        projection exists to make impossible. Counting the reads is how that stays true.
        """
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        head = {"eventId": event_id}

        calls = []
        original = self.assignments.store.one

        def counting(sql, params=()):
            calls.append(sql)
            return original(sql, params)

        self.assignments.store.one = counting
        try:
            anchored = self.assignments._anchored(event_id, 1)
        finally:
            self.assignments.store.one = original

        self.assertEqual(
            len(calls), 1,
            f"the lifecycle came from {len(calls)} snapshots, not one: {calls}",
        )
        # And it really did read all of it, rather than reading one thing cheaply.
        self.assertEqual(anchored["delivery"]["state"], "dispatched")
        self.assertEqual(anchored["event"]["stage"], "final")
        self.assertIsNotNone(anchored["ack"])



class UnsentCorrection(AssignmentTestCase):
    """CRW-222: a correction the child never received is not reported as the child's to make.

    In CRW-5 c6 the relay withheld the correction every minute as lifecycle_unknown while
    assignment-show said child_corrects with undeliveredReason null. The clock moves between
    transitions throughout, as a real clock does between two transactions.
    """

    LIFECYCLE_SOURCE = "failed_operations.lifecycle_read"

    def setUp(self):
        super().setUp()
        self._completion, self.correction = self.correction_after_needs_changes()

    def read(self):
        record = self.assignments.state(self._rid)
        return record, record["projection"]["correction"]

    def withhold(self):
        self.assertIsNone(self.attempt(self.correction))
        self.clock.advance(1)

    def test_a_correction_withheld_for_an_archived_child_names_the_withhold(self):
        from codex_session_relay.lifecycle import ARCHIVED

        self.adapter.threads[CHILD].archived = True
        self.withhold()
        record, correction = self.read()
        self.assertEqual(record["state"], NEEDS_CHANGES)
        self.assertEqual(record["nextExpectedAction"], "daemon_delivers_correction")
        self.assertEqual(correction["delivery"]["state"], "withheld_pre_send")
        reason = correction["undeliveredReason"]
        self.assertEqual(reason["source"], self.LIFECYCLE_SOURCE)
        self.assertEqual(reason["value"], ARCHIVED)
        delivery = self.delivery_row(self.correction)
        self.assertEqual(reason["recordedAt"], delivery["updated_at"])
        self.assertEqual(reason["nextRetryAt"], delivery["next_eligible_at"])

    def test_an_archive_state_that_cannot_be_read_is_named_lifecycle_unknown(self):
        from codex_session_relay.lifecycle import UNKNOWN

        self.adapter.fail_reads("is_archived")
        self.withhold()
        record, correction = self.read()
        self.assertEqual(record["nextExpectedAction"], "daemon_delivers_correction")
        self.assertEqual(correction["undeliveredReason"]["value"], UNKNOWN)

    def test_a_freshly_queued_correction_waits_on_the_relay_with_no_reason_yet(self):
        record, correction = self.read()
        self.assertEqual(correction["delivery"]["state"], "queued")
        self.assertEqual(record["nextExpectedAction"], "daemon_delivers_correction")
        self.assertIsNone(correction["undeliveredReason"])

    def test_a_dispatched_correction_is_the_childs_to_act_on(self):
        self.adapter.threads[CHILD].archived = True
        self.withhold()
        self.adapter.threads[CHILD].archived = False
        self.clock.advance(self.delivery.policy.lifecycle_recheck_seconds + 1)
        record = self.attempt(self.correction, now=self.clock.now())
        self.assertEqual(record["deliveryState"], "dispatched")
        record, correction = self.read()
        self.assertEqual(record["nextExpectedAction"], "child_corrects")
        self.assertIsNone(correction["undeliveredReason"])

    def test_a_later_settings_withhold_is_not_reported_as_the_lifecycle(self):
        self.adapter.threads[CHILD].archived = True
        self.withhold()
        self.adapter.threads[CHILD].archived = False
        self.store.db.execute("DELETE FROM authorized_settings WHERE task_id = ?", (CHILD,))
        self.store.db.commit()
        self.clock.advance(self.delivery.policy.lifecycle_recheck_seconds + 1)
        self.assertIsNone(self.attempt(self.correction, now=self.clock.now()))
        record, correction = self.read()
        self.assertEqual(correction["delivery"]["state"], "withheld_pre_send")
        # The child's record is gone, a settings code only a person resolves, so the operator
        # restores it, as for a completion (CRW-235); before that no action named settings.
        self.assertEqual(record["nextExpectedAction"], "operator_restores_recipient_settings")
        self.assertIsNone(correction["undeliveredReason"])

    def test_a_pause_names_the_relationship_and_a_resume_waits_for_the_next_reading(self):
        from codex_session_relay.lifecycle import ARCHIVED

        self.adapter.threads[CHILD].archived = True
        self.withhold()
        self.registry.set_status(self._rid, "paused", actor="user")
        self.clock.advance(1)
        self.attempt(self.correction)
        record, correction = self.read()
        self.assertEqual(record["state"], PAUSED)
        self.assertEqual(correction["undeliveredReason"]["value"], "relationship_not_active")
        self.assertEqual(correction["undeliveredReason"]["relationshipStatus"], "paused")

        self.clock.advance(1)
        current = self.registry.get(self._rid)
        self.registry.resume(
            self._rid, expect_generation=current["executionGeneration"],
            expect_artifact_roots=current["authorizedScope"]["artifactRoots"],
            expect_allowed_recipients=current["authorizedScope"]["allowedRecipients"],
            actor="user",
        )
        _record, correction = self.read()
        self.assertIsNone(correction["undeliveredReason"],
                          "the pause set the delivery's current state, not the old reading")

        self.clock.advance(self.delivery.policy.lifecycle_recheck_seconds + 1)
        self.assertIsNone(self.attempt(self.correction, now=self.clock.now()))
        _record, correction = self.read()
        self.assertEqual(correction["undeliveredReason"]["value"], ARCHIVED)

    def test_a_held_correction_is_the_parents_to_recover(self):
        self.store.db.execute(
            "UPDATE deliveries SET state = 'withheld_pre_send', hold_reason = 'attempt_cap'"
            " WHERE event_id = ?",
            (self.correction,),
        )
        self.store.db.commit()
        record, correction = self.read()
        self.assertEqual(record["nextExpectedAction"], "parent_recovers_held_correction")
        self.assertEqual(correction["undeliveredReason"],
                         {"source": "deliveries.hold_reason", "value": "attempt_cap"})

    def test_a_correction_stored_without_waking_the_child_is_the_parents_to_recover(self):
        """inbox_only is stored where the child reads it, no turn woken, held and never retried."""
        self.adapter.threads[CHILD].approval_policy = "on-request"
        self.adapter.script("approval_policy")
        self.assertEqual(self.attempt(self.correction)["deliveryState"], "inbox_only")
        record, correction = self.read()
        self.assertEqual(record["nextExpectedAction"], "parent_recovers_held_correction")
        self.assertEqual(correction["undeliveredReason"],
                         {"source": "deliveries.hold_reason", "value": "push_channel_closed"})

    def test_a_correction_whose_send_is_uncertain_waits_on_the_relay_to_confirm_it(self):
        self.adapter.script("transport_unknown")
        self.assertEqual(self.attempt(self.correction)["deliveryState"], "held_uncertain")
        record, _correction = self.read()
        self.assertEqual(record["nextExpectedAction"], "daemon_confirms_correction")

    def test_a_correction_claimed_and_not_yet_answered_waits_on_the_relay_to_confirm_it(self):
        # What a claim leaves while its send is in flight, or after the process stopped mid-send.
        self.store.db.execute(
            "UPDATE deliveries SET state = 'sending', attempt_count = attempt_count + 1,"
            " lease_owner = 'relay', lease_until = ? WHERE event_id = ?",
            (self.clock.now() + 60, self.correction),
        )
        self.store.db.commit()
        record, correction = self.read()
        self.assertEqual(correction["delivery"]["state"], "sending")
        self.assertEqual(record["nextExpectedAction"], "daemon_confirms_correction")

    def test_another_reading_of_the_same_task_does_not_move_this_events_reason(self):
        from codex_session_relay.lifecycle import ARCHIVED, Lifecycle, record

        self.adapter.threads[CHILD].archived = True
        self.withhold()
        # What an observation made for some other delivery to the same task would leave.
        record(self.store, self.clock,
               Lifecycle(CHILD, "idle", False, None, True, "yes", None))
        _record, correction = self.read()
        self.assertEqual(correction["undeliveredReason"]["value"], ARCHIVED)

    def test_a_correction_the_child_already_answered_is_not_awaiting_delivery(self):
        """A final child event in the correction's generation supersedes the correction.

        The supersession is noted on the queued delivery, whose state is left alone, and the
        next attempt suppresses it rather than sending. The relay is not going to deliver it,
        and the child has already answered, so what is owed next is the parent reading that
        answer - before the suppression is applied and after it.
        """
        failed_event = self._child_answers_with_failure()
        record, correction = self.read()
        self.assertEqual(correction["delivery"]["state"], "queued")
        self.assertIsNotNone(correction["supersession"])
        self.assertEqual(record["state"], NEEDS_CHANGES)
        self.assertEqual(record["nextExpectedAction"], "parent_reads_child_disposition")

        self.clock.advance(1)
        self.attempt(self.correction)
        record, correction = self.read()
        self.assertEqual(correction["delivery"]["state"], "superseded")
        self.assertEqual(record["nextExpectedAction"], "parent_reads_child_disposition")
        self.assertEqual(self.adapter.sends[-1][1], PARENT,
                         "the suppressed correction was sent to the child")
        self.assertIsNotNone(failed_event)

    def _child_answers_with_failure(self):
        relationship = self.registry.get(self._rid)
        generation = relationship["executionGeneration"]
        self.registry.bind_anchor(
            self._rid, generation, dispatch_turn_id="revision-turn", source="dispatch_receipt",
        )
        payload = self.execution_payload(
            relationship, "failed", generation=generation,
            turn=self.assigned_turn("failed", turn="revision-turn"),
        )
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        return payload["eventId"]

    def test_an_identical_stamp_on_another_operation_is_not_guessed(self):
        self.adapter.threads[CHILD].archived = True
        self.withhold()
        lifecycle = self.store.one(
            "SELECT occurred_at FROM failed_operations WHERE scope_key = ?"
            " AND operation = 'lifecycle_read'", (self.correction,))
        self.store.db.execute(
            "INSERT INTO failed_operations (scope_key, operation, detail, occurred_at,"
            " next_retry_at) VALUES (?, 'settings_check', 'raw', ?, 1)",
            (self.correction, lifecycle["occurred_at"]),
        )
        self.store.db.commit()
        _record, correction = self.read()
        self.assertIsNone(correction["undeliveredReason"])

    def test_a_late_record_of_an_older_attempt_does_not_hide_the_reason(self):
        """A send attempt's record can commit long after it was claimed, even after a recovery.

        Without a later delivery transition the lifecycle withhold still set the current state.
        If that old attempt then settles, its transition does, and the reason is not guessed.
        """
        from codex_session_relay.lifecycle import ARCHIVED

        self.adapter.threads[CHILD].archived = True
        self.withhold()
        self.store.db.execute(
            "INSERT INTO failed_operations (scope_key, operation, detail, occurred_at)"
            " VALUES (?, 'thread/resume', 'late', ?)",
            (self.correction, self.clock.iso()),
        )
        self.store.db.commit()
        _record, correction = self.read()
        self.assertEqual(correction["undeliveredReason"]["value"], ARCHIVED)

        self.clock.advance(1)
        self.store.db.execute(
            "UPDATE deliveries SET next_eligible_at = ?, updated_at = ? WHERE event_id = ?",
            (self.clock.now() + 60, self.clock.iso(), self.correction),
        )
        self.store.db.commit()
        _record, correction = self.read()
        self.assertIsNone(correction["undeliveredReason"])

    def test_the_reason_needs_the_withhold_and_its_record_to_share_one_stamp(self):
        """Two clock reads differ in production; this clock moves on every read to show it."""
        from codex_session_relay.lifecycle import ARCHIVED

        self.adapter.threads[CHILD].archived = True
        original = self.clock.iso

        def ticking():
            self.clock.advance(0.000001)
            return original()

        self.clock.iso = ticking
        self.withhold()
        _record, correction = self.read()
        self.assertEqual(correction["undeliveredReason"]["value"], ARCHIVED)

    def test_the_correction_is_still_read_in_one_statement(self):
        self.adapter.threads[CHILD].archived = True
        self.withhold()
        calls = []
        original = self.assignments.store.one

        def counting(sql, params=()):
            calls.append(sql)
            return original(sql, params)

        self.assignments.store.one = counting
        try:
            anchored = self.assignments._anchored(self.correction, 2)
        finally:
            self.assignments.store.one = original
        self.assertEqual(len(calls), 1, calls)
        self.assertIsNotNone(anchored["undeliveredReason"])
