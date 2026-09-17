"""A criteria re-review has to be recordable, or the assignment is stuck for good.

Editing the canonical criteria after a verified verdict is already REPORTED correctly:
AssignmentView drops out of verified into re_review_needed and asks the parent to verify again.
What was missing was any way for the parent to do it. record_verdict returned the historical
verdict before it ever looked at the new set, the review could not be claimed again onto the new
digest, and an event id is derived from the artifact revision, so re-emitting unchanged bytes
produces no new event to rule on either. The assignment stayed in re_review_needed until the
child happened to change an unrelated byte.

The protections that made the old state correct are not relaxed here. A review still certifies
only the wording it was actually claimed against, and an unchanged set still replays.
"""

import json

from codex_session_relay import identity
from codex_session_relay.assignment import AssignmentView, MERGED, REREVIEW_NEEDED, VERIFIED
from codex_session_relay.criteria import CriteriaService
from codex_session_relay.errors import RefusalReason

from .support import CHILD, PARENT, DeliveryTestCase

SET = [
    {"id": "c1", "title": "the endpoint returns the agreed shape"},
    {"id": "c2", "title": "a malformed request is refused"},
]
# Same ids, one different wording. Identical ids are the whole point: findings recorded against
# the old text would otherwise certify the new text without anybody reading it.
EDITED = [
    {"id": "c1", "title": "the endpoint returns a COMPLETELY different shape"},
    {"id": "c2", "title": "a malformed request is refused"},
]
PASSING = [{"id": "c1", "verdict": "verified"}, {"id": "c2", "verdict": "verified"}]


class ReReviewTestCase(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        self.criteria = CriteriaService(self.store, self.clock)
        self.assignments = AssignmentView(self.store, self.registry, self.clock)

    def claimed(self, *, register=True):
        """The ordinary managed flow up to the point the parent holds the review."""
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        if register:
            self.criteria.register(self._rid, SET, source_ref="https://linear.app/doc/1")
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.claim_verification(event_id, turn_id=turn.turn_id)
        return event_id

    def managed_verified(self):
        """The ordinary managed flow, all the way to a verified head."""
        event_id = self.claimed()
        self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v1", findings=PASSING,
        )
        return event_id

    def edit_criteria(self):
        self.criteria.register(self._rid, EDITED, source_ref="https://linear.app/doc/1")

    def re_review(self, event_id, *, verdict="verified", turn="v2", findings=None):
        self.ack.claim_verification(event_id, turn_id="re-review-turn")
        return self.ack.record_verdict(
            event_id, verdict=verdict, verdict_turn_id=turn,
            findings=PASSING if findings is None else findings,
        )

    def state(self):
        return self.assignments.state(self._rid)


class ReReviewIsReachable(ReReviewTestCase):
    def test_the_starting_point_is_the_state_the_view_already_reports(self):
        """Not a new claim: this is the delivered behaviour the deadlock sits behind."""
        self.managed_verified()
        self.assertEqual(self.state()["state"], VERIFIED)
        self.edit_criteria()
        record = self.state()
        self.assertEqual(record["state"], REREVIEW_NEEDED)
        self.assertEqual(record["nextExpectedAction"], "parent_verifies")

    def test_the_review_can_be_claimed_again_onto_the_current_set(self):
        event_id = self.managed_verified()
        self.edit_criteria()
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="re-review-turn"), "proceed"
        )
        self.assertEqual(
            self.criteria.bound_digest(event_id), self.criteria.get(self._rid)["setDigest"]
        )

    def test_a_re_review_is_decided_rather_than_replayed(self):
        event_id = self.managed_verified()
        self.edit_criteria()
        record = self.re_review(event_id)
        self.assertFalse(record.get("_replay"))
        self.assertEqual(record["verdictTurnId"], "v2")

    def test_a_re_review_returns_the_assignment_to_verified(self):
        event_id = self.managed_verified()
        self.edit_criteria()
        self.re_review(event_id)
        record = self.state()
        self.assertEqual(record["state"], VERIFIED)
        self.assertTrue(record["criteria"]["current"])
        self.assertEqual(
            record["criteria"]["reviewedSetDigest"], record["criteria"]["setDigest"]
        )

    def test_the_child_never_has_to_touch_an_unrelated_byte(self):
        """The deliverable did not change, so no new event should be needed to unblock it."""
        event_id = self.managed_verified()
        before = self.store.one("SELECT COUNT(*) AS c FROM events")["c"]
        self.edit_criteria()
        record = self.re_review(event_id)
        self.assertFalse(record.get("_replay"))
        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM events")["c"], before)
        self.assertEqual(self.state()["head"]["eventId"], event_id)

    def test_a_re_review_can_also_ask_for_changes(self):
        event_id = self.managed_verified()
        self.edit_criteria()
        record = self.re_review(
            event_id, verdict="needs_changes",
            findings=[
                {"id": "c1", "verdict": "needs_changes", "note": "the new shape is missing"},
            ],
        )
        self.assertEqual(record["nextExecutionGeneration"], 2)
        self.assertEqual(self.registry.get(self._rid)["executionGeneration"], 2)

    def test_the_superseded_verdict_stays_on_the_record(self):
        """A re-review replaces what the assignment STANDS ON, never the history of deciding it."""
        event_id = self.managed_verified()
        self.edit_criteria()
        self.re_review(event_id)
        entry = self.store.one(
            "SELECT detail FROM journal WHERE kind = 'verdict_superseded' AND subject = ?",
            (event_id,),
        )
        self.assertIsNotNone(entry)
        superseded = json.loads(entry["detail"])["supersededVerdict"]
        self.assertEqual(superseded["verdictTurnId"], "v1")
        self.assertEqual(superseded["verdict"], "verified")

    def test_an_earlier_merge_mark_is_state_again_once_the_re_review_lands(self):
        """Integration is a fact about the revision, not about the wording it was judged by."""
        event_id = self.managed_verified()
        self.assignments.mark(
            self._rid, "merged", evidence="merged as abc1234", actor=PARENT,
            expected_event=event_id,
        )
        self.assertEqual(self.state()["state"], MERGED)
        self.edit_criteria()
        self.assertEqual(self.state()["state"], REREVIEW_NEEDED)
        self.re_review(event_id)
        record = self.state()
        self.assertEqual(record["state"], MERGED)
        self.assertEqual(record["mark"]["eventId"], event_id)


class ReviewClaimedBeforeTheEdit(ReReviewTestCase):
    """The same deadlock one step earlier, where no verdict was ever recorded.

    This is the commoner shape: the parent claims the review, somebody edits a criterion while
    it is being read, and the ruling is refused with "claim the review again". Re-claiming was
    the impossible half.
    """

    def test_the_ruling_is_still_refused_until_the_review_is_claimed_again(self):
        event_id = self.claimed()
        self.edit_criteria()
        self.assertRefused(
            RefusalReason.CRITERIA_SET_CHANGED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v1", findings=PASSING,
            ),
        )

    def test_claiming_it_again_lets_the_review_finish(self):
        event_id = self.claimed()
        self.edit_criteria()
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="re-review-turn"), "proceed"
        )
        record = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v1", findings=PASSING,
        )
        self.assertFalse(record.get("_replay"))
        self.assertEqual(self.state()["state"], VERIFIED)

    def test_registering_criteria_after_a_legacy_verdict_opens_a_re_review(self):
        """A legacy ruling certified no wording at all, so registering a set moves it too."""
        event_id = self.claimed(register=False)
        self.ack.record_verdict(event_id, verdict="verified", verdict_turn_id="v1")
        self.assertEqual(self.state()["state"], VERIFIED)
        self.criteria.register(self._rid, SET, source_ref="https://linear.app/doc/1")
        self.assertEqual(self.state()["state"], REREVIEW_NEEDED)
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="re-review-turn"), "proceed"
        )
        record = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v2", findings=PASSING,
        )
        self.assertFalse(record.get("_replay"))
        self.assertEqual(self.state()["state"], VERIFIED)


class ProtectionsThatMustSurvive(ReReviewTestCase):
    def test_findings_made_against_the_old_wording_are_still_refused(self):
        """Reaching the new set is the fix; being certified by old findings is not."""
        event_id = self.managed_verified()
        self.edit_criteria()
        self.assertRefused(
            RefusalReason.CRITERIA_SET_CHANGED,
            lambda: self.ack.record_verdict(
                event_id, verdict="verified", verdict_turn_id="v2", findings=PASSING,
            ),
        )

    def test_an_unchanged_criteria_set_still_replays(self):
        event_id = self.managed_verified()
        again = self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v2",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "on reflection, no"}],
        )
        self.assertTrue(again.get("_replay"))
        self.assertEqual(again["verdictTurnId"], "v1")
        self.assertEqual(again["verdict"], "verified")

    def test_a_second_verified_ruling_on_an_unchanged_set_still_replays(self):
        """The bypass is keyed on the digest pair, so an equal pair must not open it."""
        event_id = self.managed_verified()
        again = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v2", findings=PASSING,
        )
        self.assertTrue(again.get("_replay"))
        self.assertEqual(again["verdictTurnId"], "v1")

    def test_a_criteria_edit_does_not_reopen_a_verdict_the_generation_left_behind(self):
        """A re-review is only ever about the revision the assignment still stands on."""
        event_id = self.managed_verified()
        self.registry.open_generation(
            self._rid, dispatch_request_id="later", reason="needs_changes_revision",
            dispatch_turn_id="later-turn",
        )
        self.edit_criteria()
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="re-review-turn"), "already_claimed"
        )
        again = self.ack.record_verdict(
            event_id, verdict="unverified", verdict_turn_id="v2",
            findings=[{"id": "c1", "verdict": "unverified", "note": "could not reach it"}],
        )
        self.assertTrue(again.get("_replay"))
        self.assertEqual(again["verdict"], "verified")

    def test_a_paused_assignment_is_not_re_reviewable(self):
        event_id = self.managed_verified()
        self.edit_criteria()
        self.registry.set_status(self._rid, "paused", actor=PARENT)
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="re-review-turn"), "already_claimed"
        )

    def test_an_unchanged_criteria_set_still_refuses_a_second_claim(self):
        """I-54: a duplicate delivery cannot cause a second verification."""
        event_id = self.managed_verified()
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="another-turn"), "already_claimed"
        )
        self.assertEqual(
            self.store.one("SELECT COUNT(*) AS c FROM verification_claims")["c"], 1
        )

    def test_a_criteria_edit_after_needs_changes_does_not_reopen_the_old_event(self):
        """needs_changes already moved the assignment on; the old event is not the head."""
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.criteria.register(self._rid, SET, source_ref="https://linear.app/doc/1")
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
            event_id, verdict="needs_changes", verdict_turn_id="v1",
            findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix it"}],
        )
        self.edit_criteria()
        self.assertEqual(
            self.ack.claim_verification(event_id, turn_id="re-review-turn"), "already_claimed"
        )
        again = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v2", findings=PASSING,
        )
        self.assertTrue(again.get("_replay"))
