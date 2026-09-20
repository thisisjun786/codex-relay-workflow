"""Whose turn it is to merge, and what no amount of waiting changes.

The cases are the contended ones. A single parent claiming a free target is not where this
goes wrong; two parents claiming at once, a holder that vanishes mid-merge, and a message that
was accepted but never acted on are.

Every case moves an injected clock and never a real one, because the property under test is
that nothing here advances because time passed. A suite that waited on a wall clock to prove
that would be testing the opposite of what it claims.
"""

import threading
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.coordination import DOMAIN_MERGE_TARGET, Conflicts
from codex_session_relay.errors import CoordinationError, RefusalReason
from codex_session_relay.linkage import Linkage, PARENT, PROJECT
from codex_session_relay.mergeturn import MergeTurn, target_key
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

from .support import RelayTestCase

PROJECT_A = "PRJ-A"
PROJECT_B = "PRJ-B"
REPO = "owner/repo"
BASE = "dev"
GREEN = {"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
         "threadsSeen": ["thread-1"], "unresolved": 0}


def run_checks(head, conclusion="success", attempt=1, name="dev-gate", run="run-1"):
    return [{"runId": run, "name": name, "headSha": head,
             "conclusion": conclusion, "attempt": attempt}]


class MergeTurnTestCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.linkage = Linkage(self.store, self.clock)
        self.turns = MergeTurn(self.store, self.clock, self.linkage)
        self.contests = Conflicts(self.store)
        self.alpha = Endpoint("task-alpha", "host-a", cwd="/alpha")
        self.beta = Endpoint("task-beta", "host-b", cwd="/beta")
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_A, endpoint=self.alpha)
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_B, endpoint=self.beta)
        # A registered supervisor, because taking a turn away or resolving an unknown outcome
        # is its authority and nobody else's. An unrelated caller with the same evidence is
        # refused, which the cases below check from both sides.
        self.supervisor = Endpoint("task-supervisor", "host-s", cwd="/sup")
        for project, parent in ((PROJECT_A, self.alpha), (PROJECT_B, self.beta)):
            self.linkage.register_supervision(
                initiative_key="INIT-1", project_key=project,
                supervisor=self.supervisor, parent=parent)

    def claim(self, endpoint, project, head, *, repository=REPO, base=BASE, ready=True):
        return self.turns.request(
            repository=repository, base_ref=base, project_key=project, holder=endpoint,
            candidate_head=head, ready=ready)

    def contests_for(self, repository=REPO, base=BASE):
        return self.contests.all(DOMAIN_MERGE_TARGET, target_key(repository, base))


class OneParentHoldsTheTargetAtATime(MergeTurnTestCase):
    def test_two_parents_claiming_at_once_produce_one_holder_and_one_waiter(self):
        first = self.claim(self.alpha, PROJECT_A, "head-a")
        second = self.claim(self.beta, PROJECT_B, "head-b")
        self.assertEqual(first["state"], "holding")
        self.assertEqual(second["state"], "waiting")
        answer = self.turns.target(REPO, BASE)
        self.assertEqual(answer["holder"]["holderTaskId"], self.alpha.task_id)
        self.assertEqual([w["holderTaskId"] for w in answer["waiters"]], [self.beta.task_id])

    def test_saying_it_is_your_turn_does_not_make_it_your_turn(self):
        self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.turns.attest(
            waiter["turnId"], evidence_kind="claimed_turn", idempotency_key="chat-1",
            actor=self.beta.task_id, evidence="I said in chat that it is my turn")
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")
        self.assertEqual(
            self.turns.target(REPO, BASE)["holder"]["holderTaskId"], self.alpha.task_id)

    def test_a_parent_claiming_twice_replays_instead_of_colliding(self):
        first = self.claim(self.alpha, PROJECT_A, "head-a")
        again = self.claim(self.alpha, PROJECT_A, "head-a")
        self.assertEqual(again["turnId"], first["turnId"])
        self.assertTrue(again["alreadyClaimed"])
        self.assertEqual(len(self.store.all(
            "SELECT * FROM merge_turns WHERE target_key = ?",
            (target_key(REPO, BASE),))), 1)

    def test_a_task_that_does_not_own_the_project_cannot_claim_for_it(self):
        with self.assertRaises(CoordinationError) as caught:
            self.claim(self.beta, PROJECT_A, "head-x")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(len(self.contests_for()), 1)

    def test_a_project_with_no_registered_parent_has_nobody_to_claim_for_it(self):
        with self.assertRaises(CoordinationError) as caught:
            self.claim(self.alpha, "PRJ-UNKNOWN", "head-x")
        self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_SCOPE)

    def test_work_on_another_target_is_not_serialised_against_this_one(self):
        here = self.claim(self.alpha, PROJECT_A, "head-a")
        there = self.claim(self.beta, PROJECT_B, "head-b", repository="owner/other")
        self.assertEqual(here["state"], "holding")
        self.assertEqual(there["state"], "holding")


class ATransportAcceptingAMessageIsNotTheHolderActing(MergeTurnTestCase):
    def test_an_accepted_return_request_leaves_the_target_occupied(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.turns.request_return(
            held["turnId"], actor=self.beta.task_id, evidence="I have a ready candidate")
        self.turns.attest(
            held["turnId"], evidence_kind="transport_accepted", idempotency_key="delivery-1",
            actor=self.beta.task_id, evidence="the relay accepted the message")
        answer = self.turns.target(REPO, BASE)
        self.assertIsNotNone(answer["returnRequestedAt"])
        self.assertIsNotNone(answer["transportAcceptedAt"])
        self.assertIsNone(answer["releasedAt"])
        self.assertTrue(answer["occupied"])
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")

    def test_a_replayed_notification_records_one_fact(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        for _ in range(3):
            self.turns.attest(
                held["turnId"], evidence_kind="transport_accepted",
                idempotency_key="delivery-1", actor=self.beta.task_id, evidence="accepted")
        entries = [e for e in self.turns.ledger(held["turnId"])
                   if e["idempotencyKey"] == "delivery-1"]
        self.assertEqual(len(entries), 1)

    def test_a_release_reports_the_control_messages_recorded_under_its_tenure(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.turns.request_return(
            held["turnId"], actor=self.beta.task_id, evidence="please hand it back")
        self.turns.attest(
            held["turnId"], evidence_kind="transport_accepted", idempotency_key="delivery-1",
            actor=self.beta.task_id, evidence="accepted")
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="candidate is not ready")
        kinds = {e["evidenceKind"] for e in released["ledger"]}
        self.assertIn("return_requested", kinds)
        self.assertIn("transport_accepted", kinds)

class AnUnreadyCandidateGetsOutOfTheWay(MergeTurnTestCase):
    def test_a_safe_return_promotes_the_next_ready_candidate_in_one_transaction(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="checks are not green yet")
        self.assertEqual(released["released"]["state"], "returned")
        self.assertEqual(released["promoted"]["turnId"], waiter["turnId"])
        self.assertEqual(released["promoted"]["state"], "holding")

    def test_an_unready_waiter_is_not_promoted_and_the_target_is_left_free(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.claim(self.beta, PROJECT_B, "head-b", ready=False)
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned", reason="deferring")
        self.assertIsNone(released["promoted"])
        self.assertFalse(self.turns.target(REPO, BASE)["occupied"])

    def test_a_waiter_that_becomes_ready_takes_a_free_target(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b", ready=False)
        self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned", reason="deferring")
        answer = self.turns.declare_ready(
            waiter["turnId"], actor=self.beta.task_id, ready=True)
        self.assertEqual(answer["state"], "holding")
        self.assertIsNone(answer["blockedBy"])

    def test_declaring_readiness_against_an_occupied_target_reports_rather_than_refuses(self):
        self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b", ready=False)
        answer = self.turns.declare_ready(
            waiter["turnId"], actor=self.beta.task_id, ready=True)
        self.assertEqual(answer["state"], "waiting")
        self.assertEqual(answer["blockedBy"]["state"], "holding")

    def test_a_promotion_re_checks_ownership_and_skips_a_parent_that_lost_its_project(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.store.db.execute(
            "UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", (PROJECT_B,))
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned", reason="done")
        self.assertIsNone(released["promoted"])
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "withdrawn")
        self.assertTrue(self.contests_for())

    def test_a_late_return_after_a_cancellation_does_not_reopen_the_turn(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.turns.release(
            held["turnId"], actor=self.supervisor.task_id, disposition="cancelled",
            reason="parent stopped answering", evidence="no turn for two hours, host checked")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.release(
                held["turnId"], actor=self.alpha.task_id, disposition="returned",
                reason="I am back")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_NOT_HELD)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "cancelled")

    def test_taking_a_turn_away_from_its_holder_requires_evidence(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.release(
                held["turnId"], actor=self.supervisor.task_id, disposition="cancelled",
                reason="it stopped", evidence="")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)


class NothingIsReleasedBecauseTimePassed(MergeTurnTestCase):
    def merging(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        return held

    def test_a_merging_turn_cannot_be_cancelled_and_the_refusal_says_what_can(self):
        held = self.merging()
        with self.assertRaises(CoordinationError) as caught:
            self.turns.release(
                held["turnId"], actor=self.supervisor.task_id, disposition="cancelled",
                reason="the parent died", evidence="its host is gone")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_UNRESOLVED)
        self.assertIn("report_unknown", caught.exception.detail)

    def test_an_unknown_outcome_survives_any_amount_of_elapsed_time(self):
        held = self.merging()
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="the connection dropped mid-merge")
        before = self.turns.target(REPO, BASE)
        self.clock.advance(1_000_000)
        after = self.turns.target(REPO, BASE)
        self.assertEqual(before, after)
        self.assertEqual(after["holder"]["state"], "unknown")
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")

    def test_an_observation_is_what_releases_an_unknown_outcome(self):
        held = self.merging()
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="lost the connection")
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-9",
            pr_state="merged", evidence="the pull request reads merged and the base moved")
        self.assertEqual(answer["outcome"], "landed")
        self.assertEqual(answer["promoted"]["turnId"], waiter["turnId"])

    def test_resolving_an_unknown_outcome_without_an_observation_is_refused(self):
        held = self.merging()
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="lost the connection")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.resolve_unknown(
                held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-9",
                pr_state="open", evidence="")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)

    def test_a_confirmed_landing_closes_the_tenure_and_frees_the_target(self):
        held = self.merging()
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        answer = self.turns.land(
            held["turnId"], actor=self.alpha.task_id, landed_sha="merge-1",
            observed_base_sha="base-1", evidence="the merge commit is on the base")
        self.assertEqual(answer["released"]["state"], "landed")
        self.assertIsNotNone(answer["released"]["closedAt"])
        self.assertEqual(answer["promoted"]["turnId"], waiter["turnId"])

class TheCurrencyCheckImmediatelyBeforeMerging(MergeTurnTestCase):
    def held(self):
        return self.claim(self.alpha, PROJECT_A, "head-a")

    def begin(self, held, **overrides):
        arguments = {
            "actor": self.alpha.task_id, "head_sha": "head-a", "base_sha": "base-0",
            "checks": run_checks("head-a"), "review": dict(GREEN), "required": ["dev-gate"],
        }
        arguments.update(overrides)
        return self.turns.begin_merge(held["turnId"], **arguments)

    def stored_checks(self, held):
        return self.store.all(
            "SELECT * FROM merge_turn_checks WHERE turn_id = ? ORDER BY recorded_at",
            (held["turnId"],))

    def test_a_candidate_whose_head_moved_is_refused(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, head_sha="head-z", checks=run_checks("head-z"))
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CANDIDATE_MOVED)

    def test_a_refused_check_is_retained_and_the_turn_stays_holding(self):
        held = self.held()
        with self.assertRaises(CoordinationError):
            self.begin(held, review={"hasNextPage": True, "pagesRead": 1, "totalCount": 2,
                                     "threadsSeen": ["one"], "unresolved": 0})
        rows = self.stored_checks(held)
        self.assertEqual([row["result"] for row in rows], ["refused"])
        self.assertEqual(rows[0]["refusal_reason"], "merge_review_incomplete")
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")

    def test_unfinished_review_pagination_is_refused(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, review={"hasNextPage": True, "pagesRead": 1, "totalCount": 1,
                                     "threadsSeen": ["one"], "unresolved": 0})
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_REVIEW_INCOMPLETE)

    def test_an_unresolved_review_thread_is_refused(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, review={"hasNextPage": False, "pagesRead": 2, "totalCount": 1,
                                     "threadsSeen": ["one"], "unresolved": 1})
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_REVIEW_INCOMPLETE)

    def test_a_declared_required_check_missing_from_the_restated_set_is_refused(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, required=["dev-gate", "devin"])
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)
        self.assertIn("devin", caught.exception.detail)

    def test_a_failing_newest_attempt_beats_an_older_passing_one(self):
        held = self.held()
        checks = run_checks("head-a", conclusion="success", attempt=1)
        checks += run_checks("head-a", conclusion="failure", attempt=2)
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, checks=checks)
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)

    def test_a_passing_newest_attempt_after_a_failed_one_is_accepted(self):
        held = self.held()
        checks = run_checks("head-a", conclusion="failure", attempt=1)
        checks += run_checks("head-a", conclusion="success", attempt=2)
        answer = self.begin(held, checks=checks)
        self.assertEqual(answer["state"], "merging")
        self.assertEqual(answer["requiredDeclared"], ["dev-gate"])

    def test_a_check_reporting_another_head_is_refused(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, checks=run_checks("head-other"))
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)

    def test_restating_no_checks_at_all_is_refused(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, checks=[], required=[])
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)

    def test_a_base_that_moved_since_the_last_landing_is_refused(self):
        first = self.held()
        self.begin(first)
        self.turns.land(
            first["turnId"], actor=self.alpha.task_id, landed_sha="merge-1",
            observed_base_sha="base-1", evidence="landed")
        second = self.claim(self.alpha, PROJECT_A, "head-c")
        with self.assertRaises(CoordinationError) as caught:
            self.begin(second, head_sha="head-c", checks=run_checks("head-c"),
                       base_sha="base-0")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)

    def test_rewriting_the_head_resets_readiness_so_the_check_cannot_be_side_stepped(self):
        held = self.held()
        answer = self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-new")
        self.assertFalse(answer["declaredReady"])
        self.assertEqual(answer["candidateHead"], "head-new")
        kinds = {e["evidenceKind"] for e in self.turns.ledger(held["turnId"])}
        self.assertIn("candidate_head_changed", kinds)

    def test_a_task_that_does_not_hold_the_turn_cannot_begin_a_merge(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, actor=self.beta.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_NOT_HELD)


class ActingOnATurnYouDoNotHoldNeedsAuthority(MergeTurnTestCase):
    """Evidence is not authority. Every claim path already verified the caller; these did not.

    A stranger who could cancel a held turn, wedge a merging one, or resolve an unknown one
    could promote a different claim behind an unmerged predecessor, which is the whole failure
    the module exists to prevent, reached through the recovery door instead of the front one.
    """

    def merging(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        return held

    def test_a_stranger_cannot_take_a_held_turn_away(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.release(
                held["turnId"], actor="task-stranger", disposition="cancelled",
                reason="I want it", evidence="none of my business")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")

    def test_a_stranger_cannot_wedge_a_merging_turn(self):
        held = self.merging()
        with self.assertRaises(CoordinationError) as caught:
            self.turns.report_unknown(
                held["turnId"], actor="task-stranger", reason="I say it is unknown")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "merging")

    def test_a_stranger_cannot_resolve_an_unknown_outcome(self):
        held = self.merging()
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="lost the connection")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.resolve_unknown(
                held["turnId"], actor="task-stranger", observed_base_sha="base-9",
                pr_state="merged", evidence="I looked")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "unknown")

    def test_the_holder_may_resolve_its_own_unknown_outcome(self):
        held = self.merging()
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="lost the connection")
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.alpha.task_id, observed_base_sha="base-0",
            pr_state="open", evidence="the pull request is still open on the same base")
        self.assertEqual(answer["outcome"], "returned")


class AnObservationDecidesTheOutcomeRatherThanTheCaller(MergeTurnTestCase):
    def unknown_turn(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="lost the connection")
        return held

    def test_an_open_pull_request_on_the_checked_base_is_returned_not_landed(self):
        """The case the null comparison got backwards.

        observed_base_sha is null until this very call, so comparing against it made every
        observation look like a movement and landed an open pull request, releasing its target
        behind an unmerged predecessor. The comparison is against the base the currency check
        confirmed.
        """
        held = self.unknown_turn()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-0",
            pr_state="open", evidence="still open, base unchanged")
        self.assertEqual(answer["outcome"], "returned")
        self.assertIsNone(self.turns.turn(held["turnId"])["landedSha"])

    def test_a_moved_base_is_landed_even_when_the_pull_request_reads_open(self):
        held = self.unknown_turn()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-9",
            pr_state="open", evidence="the base moved past the candidate")
        self.assertEqual(answer["outcome"], "landed")

    def test_a_merged_pull_request_is_landed_whatever_the_base_reads(self):
        held = self.unknown_turn()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-0",
            pr_state="merged", evidence="the pull request reads merged")
        self.assertEqual(answer["outcome"], "landed")


class ReviewEvidenceIsCountedByDistinctThread(MergeTurnTestCase):
    def test_a_repeated_thread_identifier_does_not_stand_for_an_unread_one(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                base_sha="base-0", checks=run_checks("head-a"),
                review={"hasNextPage": False, "pagesRead": 2, "totalCount": 2,
                        "threadsSeen": ["thread-1", "thread-1"], "unresolved": 0})
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_REVIEW_INCOMPLETE)
        self.assertIn("repeats an identifier", caught.exception.detail)

    def test_two_distinct_threads_satisfy_a_total_of_two(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        answer = self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"),
            review={"hasNextPage": False, "pagesRead": 2, "totalCount": 2,
                    "threadsSeen": ["thread-1", "thread-2"], "unresolved": 0})
        self.assertEqual(answer["state"], "merging")


class TwoParentsRacingForOneTarget(MergeTurnTestCase):
    """One independent Store per thread, a barrier, bounded joins, errors collected.

    The construction test_registration_contention.py established, and its honesty applies here
    too: a barrier aligns the two starts and the scheduler may still run either call to
    completion first. So the assertions hold under EVERY interleaving, including both serial
    ones - exactly one claim holds the target and the other waits, whichever arrives first.
    """

    def claim_in_thread(self, endpoint, project, results, errors, barrier):
        def run():
            store = Store(self.store.path)
            try:
                barrier.wait(timeout=20)
                turns = MergeTurn(store, FakeClock(), Linkage(store, FakeClock()))
                results[endpoint.task_id] = turns.request(
                    repository=REPO, base_ref=BASE, project_key=project, holder=endpoint,
                    candidate_head="head-" + endpoint.task_id, ready=True)
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors[endpoint.task_id] = error
            finally:
                store.close()

        return threading.Thread(target=run)

    def test_exactly_one_claim_holds_the_target_under_every_interleaving(self):
        results, errors = {}, {}
        barrier = threading.Barrier(2)
        threads = [
            self.claim_in_thread(self.alpha, PROJECT_A, results, errors, barrier),
            self.claim_in_thread(self.beta, PROJECT_B, results, errors, barrier),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        for thread in threads:
            self.assertFalse(thread.is_alive(), "a thread never finished")
        self.assertEqual(errors, {}, "neither claim should raise: one holds and one waits")
        states = sorted(record["state"] for record in results.values())
        self.assertEqual(states, ["holding", "waiting"])
        rows = self.store.all(
            "SELECT * FROM merge_turns WHERE target_key = ? AND state = 'holding'",
            (target_key(REPO, BASE),))
        self.assertEqual(len(rows), 1)


if __name__ == "__main__":
    unittest.main()
