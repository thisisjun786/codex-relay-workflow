"""The assignment ledger: one issue, one responsible child, and where that assignment stands."""

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
