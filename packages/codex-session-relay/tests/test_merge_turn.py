"""Whose turn it is to merge, and what no amount of waiting changes.

The cases are the contended ones. A single parent claiming a free target is not where this
goes wrong; two parents claiming at once, a holder that vanishes mid-merge, and a message that
was accepted but never acted on are.

Every case moves an injected clock and never a real one, because the property under test is
that nothing here advances because time passed. A suite that waited on a wall clock to prove
that would be testing the opposite of what it claims.
"""

import json
import pathlib
import threading
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.coordination import DOMAIN_MERGE_TARGET, Conflicts
from codex_session_relay.errors import CoordinationError, RefusalReason
from codex_session_relay.linkage import Linkage, PARENT, PROJECT
from codex_session_relay.mergeturn import MergeTurn, grant_id, target_key
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

from .support import FakeTarget, RelayTestCase

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
        # Where the target's base branch points (CRW-229). The currency check and every
        # recorded base read it; a case that moves the branch says so.
        self.target = FakeTarget()
        self.target.set(REPO, BASE, "base-0")
        self.turns = MergeTurn(self.store, self.clock, self.linkage, target_reader=self.target)
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

    def set_status(self, project, status):
        """A binding that is live but not running, or running again."""
        self.store.db.execute(
            "UPDATE scope_bindings SET status = ? WHERE scope_key = ? AND role = ?",
            (status, project, "parent"))

    def answer_grant(self, turn, actor):
        """What a parent does between being given the turn and using it.

        A turn that has no grant - one that reached holding before grants were recorded - has
        nothing to answer, and answering is not required of it.
        """
        grant = self.turns.turn(turn)["grant"]
        if grant is None:
            return None
        return self.turns.acknowledge_grant(
            turn, actor=actor, grant=grant["grantId"],
            evidence="read the grant and re-checked the record")


class APausedParentKeepsItsClaimAndCannotAct(MergeTurnTestCase):
    """Owning a project and running are two facts, and a pause separates them.

    The linkage treats paused as LIVE ownership on purpose, so a paused parent must not lose
    its claim or its place. It must also not hold a target it is not running to use: that is
    a branch occupied against every peer until a human notices, which is the failure this
    whole issue is about, reached by a different door.
    """

    def test_a_paused_owner_queues_even_when_the_target_is_free(self):
        self.set_status(PROJECT_A, "paused")
        claimed = self.claim(self.alpha, PROJECT_A, "head-a")
        self.assertEqual(claimed["state"], "waiting")
        self.assertFalse(self.turns.target(REPO, BASE)["occupied"])

    def test_a_paused_waiter_does_not_take_a_free_target(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b", ready=False)
        self.set_status(PROJECT_B, "paused")
        self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="deferring")
        answer = self.turns.declare_ready(
            waiter["turnId"], actor=self.beta.task_id, ready=True)
        self.assertEqual(answer["state"], "waiting")
        self.assertEqual(answer["blockedBy"]["state"], "owner_paused")
        self.assertFalse(self.turns.target(REPO, BASE)["occupied"])

    def test_a_promotion_skips_a_paused_waiter_and_keeps_its_claim(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.set_status(PROJECT_B, "paused")
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="not ready")
        self.assertIsNone(released["promoted"])
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")
        self.assertIsNone(self.turns.turn(waiter["turnId"])["grant"])
        self.assertFalse(self.turns.target(REPO, BASE)["occupied"])

    def test_a_paused_holder_cannot_begin_a_merge(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.set_status(PROJECT_A, "paused")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                base_sha="base-0", checks=run_checks("head-a"), review=dict(GREEN),
                required=["dev-gate"])
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")

    def test_the_next_ready_waiter_is_the_one_the_promotion_would_actually_take(self):
        """nextReady named the oldest waiter that declared readiness, and the promotion takes
        the oldest one it can actually promote. With a paused first waiter those differ."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        first = self.claim(self.beta, PROJECT_B, "head-b")
        gamma = Endpoint("task-gamma", "host-g", cwd="/gamma")
        self.linkage.bind_scope(role=PARENT, scope_key="PRJ-C", endpoint=gamma)
        self.linkage.register_supervision(
            initiative_key="INIT-1", project_key="PRJ-C",
            supervisor=self.supervisor, parent=gamma)
        second = self.claim(gamma, "PRJ-C", "head-c")
        self.set_status(PROJECT_B, "paused")

        answer = self.turns.target(REPO, BASE)
        self.assertEqual(answer["nextReady"]["turnId"], second["turnId"])
        self.assertEqual([peer["turnId"] for peer in answer["blocked"]["withheldPeers"]],
                         [first["turnId"]])

        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned", reason="done")
        self.assertEqual(released["promoted"]["turnId"], second["turnId"])

    def test_a_paused_holder_cannot_act_on_its_grant(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        grant = self.turns.turn(held["turnId"])["grant"]["grantId"]
        self.set_status(PROJECT_A, "paused")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.acknowledge_grant(
                held["turnId"], actor=self.alpha.task_id, grant=grant, evidence="back now")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_resumed_parent_reads_its_claim_and_takes_the_free_target(self):
        """B4's recovery, end to end.

        Nothing promotes on a status change, and re-requesting replays the waiting claim
        rather than acquiring. So the whole route back is: read your own claims, see the
        target is free, declare readiness. One read and one call, no trigger and no wake.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.set_status(PROJECT_B, "paused")
        self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned", reason="done")

        self.set_status(PROJECT_B, "active")
        mine = self.turns.outstanding(self.beta.task_id)
        self.assertEqual([record["turnId"] for record in mine], [waiter["turnId"]])
        self.assertTrue(mine[0]["targetFree"])

        answer = self.turns.declare_ready(
            waiter["turnId"], actor=self.beta.task_id, ready=True)
        self.assertEqual(answer["state"], "holding")
        self.assertEqual(answer["grant"]["recipientTaskId"], self.beta.task_id)


class ReadinessThatDiedLeavesARecord(MergeTurnTestCase):
    def test_a_withdrawn_readiness_is_recorded_with_the_cause_the_caller_states(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        answer = self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=False,
            cause="a new finding arrived on the pull request")
        entry = next(e for e in answer["ledger"]
                     if e["evidenceKind"] == "readiness_withdrawn")
        self.assertEqual(entry["evidence"], "a new finding arrived on the pull request")
        self.assertFalse(answer["declaredReady"])

    def test_a_holder_that_lost_its_readiness_returns_and_the_ready_peer_proceeds(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=False,
            cause="the base moved to base-7")
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="my readiness died; handing it on")
        self.assertEqual(released["promoted"]["turnId"], waiter["turnId"])
        self.assertEqual(released["promoted"]["state"], "holding")

    def test_a_restated_head_records_the_readiness_it_reset(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        answer = self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-a2")
        kinds = [e["evidenceKind"] for e in answer["ledger"]]
        self.assertIn("candidate_head_changed", kinds)
        self.assertIn("readiness_withdrawn", kinds)
        self.assertFalse(answer["declaredReady"])


class WhatTheTargetIsWaitingOn(MergeTurnTestCase):
    """The state the 2026-09-21 observation could not name.

    A holder that is MERGING and a holder restating the same candidate against checks that
    have not finished both read as "a parent has the turn". They need different answers, and
    neither answer is a timeout.
    """

    def refused_check(self, turn):
        with self.assertRaises(CoordinationError):
            self.turns.begin_merge(
                turn, actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
                checks=run_checks("head-a", conclusion="failure"), review=dict(GREEN),
                required=["dev-gate"])

    def test_a_refused_check_with_a_ready_peer_behind_it_names_both(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.answer_grant(waiter["turnId"], self.beta.task_id)
        self.refused_check(held["turnId"])
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "required_evidence_not_current")
        self.assertEqual(blocked["candidateHead"], "head-a")
        self.assertEqual(blocked["lastResult"], "refused")
        self.assertEqual(blocked["lastRefusal"], "merge_currency_stale")
        self.assertEqual([peer["turnId"] for peer in blocked["readyPeers"]],
                         [waiter["turnId"]])

    def test_an_unfinished_review_is_not_reported_as_an_unfinished_check(self):
        """Every refusal writes a check row, so one word for all of them is a wrong word."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        with self.assertRaises(CoordinationError):
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                base_sha="base-0", checks=run_checks("head-a"), required=["dev-gate"],
                review={"hasNextPage": True, "pagesRead": 1, "totalCount": 4,
                        "threadsSeen": ["thread-1"], "unresolved": 0})
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "review_not_finished")
        self.assertEqual(blocked["lastRefusal"], "merge_review_incomplete")

    def test_a_holder_that_never_declared_readiness_is_not_reported_as_checking(self):
        self.claim(self.alpha, PROJECT_A, "head-a", ready=False)
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "candidate_not_ready")
        self.assertEqual(blocked["checkSnapshots"], 0)

    def test_a_merging_holder_is_told_apart_from_one_restating_its_candidate(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        self.assertEqual(self.turns.target(REPO, BASE)["blocked"]["cause"],
                         "merge_in_flight")

    def test_an_unoccupied_target_has_nothing_to_be_waiting_on(self):
        self.assertIsNone(self.turns.target(REPO, BASE)["blocked"])

    def test_a_holder_that_is_not_running_is_reported_as_the_blocker_it_is(self):
        self.claim(self.alpha, PROJECT_A, "head-a")
        self.claim(self.beta, PROJECT_B, "head-b")
        self.set_status(PROJECT_A, "paused")
        self.assertEqual(self.turns.target(REPO, BASE)["blocked"]["cause"], "holder_paused")

    def test_a_holder_whose_project_changed_hands_is_not_reported_as_merely_unrestated(self):
        """Asking whether the OWNER can act, without first asking whether the holder IS the
        owner, answered about the replacement - so a stale holder that can neither restate nor
        merge read as one that simply had not restated yet."""
        self.claim(self.alpha, PROJECT_A, "head-a")
        self.claim(self.beta, PROJECT_B, "head-b")
        successor = Endpoint("task-alpha-2", "host-a2", cwd="/alpha2")
        self.store.db.execute(
            "UPDATE scope_bindings SET task_id = ? WHERE scope_key = ? AND role = ?",
            (successor.task_id, PROJECT_A, "parent"))
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "holder_no_longer_owns_the_project")
        self.assertEqual(blocked["holderTaskId"], self.alpha.task_id)

    def test_two_restatements_in_one_instant_that_disagree_decide_nothing(self):
        """recorded_at is a second and check_id is a digest, so ordering by it is a coin that
        lands the same way every time - which looks like an answer and is not one."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.refused_check(held["turnId"])
        with self.assertRaises(CoordinationError):
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                base_sha="base-0", checks=run_checks("head-a"), required=["dev-gate"],
                review={"hasNextPage": True, "pagesRead": 1, "totalCount": 4,
                        "threadsSeen": ["thread-1"], "unresolved": 0})
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "restatements_disagree")
        self.assertEqual(len(blocked["ambiguousRestatements"]), 2)
        self.assertIsNone(blocked["lastRefusal"])
        self.assertEqual(blocked["checkSnapshots"], 2)

    def test_a_peer_the_promotion_would_skip_is_not_reported_as_ready(self):
        """Returning the turn for a peer that cannot take it frees the target and moves nobody."""
        self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.set_status(PROJECT_B, "paused")
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["readyPeers"], [])
        self.assertEqual([peer["turnId"] for peer in blocked["withheldPeers"]],
                         [waiter["turnId"]])
        self.assertEqual(blocked["withheldPeers"][0]["reason"], "owner_paused")

    def test_a_refusal_about_a_head_this_turn_left_behind_does_not_decide_the_cause(self):
        """Check rows outlive a head change, so the newest one can be about another candidate."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.refused_check(held["turnId"])
        self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-b")
        self.turns.declare_ready(held["turnId"], actor=self.alpha.task_id, ready=True)
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "candidate_not_restated")
        self.assertEqual(blocked["candidateHead"], "head-b")
        self.assertEqual(blocked["checkSnapshots"], 0)
        self.assertIsNone(blocked["lastRefusal"])

    def test_restating_a_head_this_turn_does_not_hold_is_reported_as_the_movement_it_is(self):
        """A check row records what its CALLER restated, not the candidate it was judged
        against, so filtering on the candidate dropped exactly the mismatch it describes."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-b",
                base_sha="base-0", checks=run_checks("head-b"), review=dict(GREEN),
                required=["dev-gate"])
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CANDIDATE_MOVED)
        blocked = self.turns.target(REPO, BASE)["blocked"]
        self.assertEqual(blocked["cause"], "candidate_moved")
        self.assertEqual(blocked["candidateHead"], "head-a")
        self.assertEqual(blocked["lastCheckedHead"], "head-b")
        self.assertEqual(blocked["checkSnapshots"], 0)

    def test_restating_the_same_evidence_does_not_look_like_a_second_restatement(self):
        """checkSnapshots counts distinct evidence, and says so rather than counting polls."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.refused_check(held["turnId"])
        self.refused_check(held["turnId"])
        self.assertEqual(self.turns.target(REPO, BASE)["blocked"]["checkSnapshots"], 1)


class AParentThatCameBackFindsItsOwnClaims(MergeTurnTestCase):
    def test_outstanding_answers_from_a_task_id_alone(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        mine = self.turns.outstanding(self.alpha.task_id)
        self.assertEqual([record["turnId"] for record in mine], [held["turnId"]])
        self.assertEqual(mine[0]["state"], "holding")
        self.assertFalse(mine[0]["targetFree"])
        self.assertEqual(mine[0]["grant"]["recipientTaskId"], self.alpha.task_id)

    def test_an_unresolved_outcome_is_reported_as_unresolved_and_not_as_free(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="the host stopped answering")
        self.clock.advance(1_000_000)
        mine = self.turns.outstanding(self.alpha.task_id)
        self.assertEqual(mine[0]["state"], "unknown")
        self.assertFalse(mine[0]["targetFree"])

    def test_a_second_store_on_the_same_path_reads_the_same_claims(self):
        """A restart is a different process reading one durable record, and nothing else."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        store = Store(self.store.path)
        self.addCleanup(store.close)
        clock = FakeClock()
        after = MergeTurn(store, clock, Linkage(store, clock))
        mine = after.outstanding(self.alpha.task_id)
        self.assertEqual([record["turnId"] for record in mine], [held["turnId"]])
        self.assertEqual(mine[0]["grant"]["grantId"],
                         grant_id(held["turnId"], mine[0]["tenure"], 1))

    def test_a_free_target_a_paused_owner_cannot_take_is_not_reported_free(self):
        """targetFree answers whether to try, so occupancy alone was the wrong question."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.set_status(PROJECT_B, "paused")
        self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned", reason="done")
        mine = self.turns.outstanding(self.beta.task_id)
        self.assertEqual([record["turnId"] for record in mine], [waiter["turnId"]])
        self.assertFalse(mine[0]["targetFree"])
        self.assertEqual(mine[0]["heldBackBy"], "owner_paused")

    def test_a_claim_that_already_holds_replays_with_the_grant_it_was_given(self):
        """A lost response is retried, and the retry has to carry what the first one did."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        again = self.claim(self.alpha, PROJECT_A, "head-a")
        self.assertTrue(again["alreadyClaimed"])
        self.assertEqual(again["turnId"], held["turnId"])
        self.assertEqual(again["grant"]["grantId"], held["grant"]["grantId"])
        self.turns.acknowledge_grant(
            again["turnId"], actor=self.alpha.task_id, grant=again["grant"]["grantId"],
            evidence="acting on the retried response")


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
        self.answer_grant(held["turnId"], self.alpha.task_id)
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
        self.target.set(REPO, BASE, "base-9")
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
        self.target.set(REPO, BASE, "base-1")
        answer = self.turns.land(
            held["turnId"], actor=self.alpha.task_id, landed_sha="merge-1",
            observed_base_sha="base-1", evidence="the merge commit is on the base")
        self.assertEqual(answer["released"]["state"], "landed")
        self.assertIsNotNone(answer["released"]["closedAt"])
        self.assertEqual(answer["promoted"]["turnId"], waiter["turnId"])

class TheCurrencyCheckImmediatelyBeforeMerging(MergeTurnTestCase):
    def held(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        return held

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

    def test_a_review_record_that_states_nothing_is_not_a_complete_review(self):
        """Absent read as satisfied.

        hasNextPage missing was falsy, totalCount missing was zero and matched an empty
        threadsSeen, and unresolved missing was zero, so a record saying nothing at all passed
        every check.
        """
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(held, review={"pagesRead": 1})
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_REVIEW_INCOMPLETE)
        self.assertIn("does not state", caught.exception.detail)

    def test_every_part_of_the_review_record_has_to_be_stated(self):
        held = self.held()
        complete = dict(GREEN)
        for field in complete:
            partial = {k: v for k, v in complete.items() if k != field}
            with self.assertRaises(CoordinationError) as caught:
                self.begin(held, review=partial)
            self.assertEqual(
                caught.exception.reason, RefusalReason.MERGE_REVIEW_INCOMPLETE, field)


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

    def test_an_optional_check_failing_does_not_block_a_green_required_set(self):
        """The declared set decides. Refusing on an optional failure made it mean nothing."""
        held = self.held()
        checks = run_checks("head-a", name="dev-gate", run="run-1")
        checks += run_checks("head-a", conclusion="failure", name="lint", run="run-2")
        answer = self.begin(held, checks=checks, required=["dev-gate"])
        self.assertEqual(answer["state"], "merging")

    def test_with_nothing_declared_required_something_still_has_to_be_green(self):
        held = self.held()
        with self.assertRaises(CoordinationError) as caught:
            self.begin(
                held, required=[],
                checks=run_checks("head-a", conclusion="failure", name="lint", run="run-2"))
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)


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
        self.target.set(REPO, BASE, "base-1")
        self.turns.land(
            first["turnId"], actor=self.alpha.task_id, landed_sha="merge-1",
            observed_base_sha="base-1", evidence="landed")
        second = self.claim(self.alpha, PROJECT_A, "head-c")
        self.answer_grant(second["turnId"], self.alpha.task_id)
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

    def test_a_former_parent_cannot_begin_a_merge_after_a_handover(self):
        """Ownership again, at the last moment it can still matter.

        A handover between the claim and the merge leaves the former parent holding a turn for
        a project it no longer owns, and this is the write that lands work.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.store.db.execute(
            "UPDATE scope_bindings SET status = 'archived'"
            "  WHERE scope_key = ? AND task_id = ?", (PROJECT_A, self.alpha.task_id))
        with self.assertRaises(CoordinationError) as caught:
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                base_sha="base-0", checks=run_checks("head-a"), review=dict(GREEN))
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_check_with_no_identity_is_not_evidence(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        for entry in ({"runId": "", "name": "dev-gate"}, {"runId": "run-1", "name": ""}):
            check = dict(entry, headSha="head-a", conclusion="success", attempt=1)
            with self.assertRaises(CoordinationError) as caught:
                self.turns.begin_merge(
                    held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                    base_sha="base-0", checks=[check], review=dict(GREEN))
            self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)

    def test_an_unready_holder_cannot_begin_merging(self):
        """Holding a free target is not saying the candidate is ready.

        A request on a free target becomes holding whatever its ready argument said, and a
        head rewrite deliberately resets readiness, so without this the reset could be walked
        straight past.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a", ready=False)
        self.answer_grant(held["turnId"], self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a",
                base_sha="base-0", checks=run_checks("head-a"), review=dict(GREEN))
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CANDIDATE_MOVED)

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
        self.answer_grant(held["turnId"], self.alpha.task_id)
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
        self.answer_grant(held["turnId"], self.alpha.task_id)
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

    def test_a_moved_base_does_not_land_a_pull_request_that_reads_open(self):
        """Any unrelated commit moves a base, so movement is not evidence THIS one landed.

        Reading it as one released an open candidate's target and promoted somebody behind an
        unmerged predecessor, which is the failure the whole module exists to prevent.
        """
        held = self.unknown_turn()
        self.target.set(REPO, BASE, "base-9")
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-9",
            pr_state="open", evidence="somebody else pushed; this one is still open")
        self.assertEqual(answer["outcome"], "returned")
        self.assertIsNone(self.turns.turn(held["turnId"])["landedSha"])

    def test_a_moved_base_with_an_unreadable_state_asks_for_a_clearer_observation(self):
        held = self.unknown_turn()
        with self.assertRaises(CoordinationError) as caught:
            self.turns.resolve_unknown(
                held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-9",
                pr_state="unknown", evidence="could not read the pull request")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "unknown")

    def test_an_unrecognised_pull_request_state_establishes_nothing(self):
        """A typo used to close the turn and promote a waiter.

        The base being unchanged said nothing either: an unmerged candidate leaves it
        unchanged too, so no reading of that pair established an outcome.
        """
        held = self.unknown_turn()
        for observed in ("base-0", "base-9"):
            with self.assertRaises(CoordinationError) as caught:
                self.turns.resolve_unknown(
                    held["turnId"], actor=self.supervisor.task_id,
                    observed_base_sha=observed, pr_state="mergd",
                    evidence="I think it merged")
            self.assertEqual(
                caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "unknown")


    def test_a_merged_pull_request_is_landed_whatever_the_base_reads(self):
        held = self.unknown_turn()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-0",
            pr_state="merged", evidence="the pull request reads merged")
        self.assertEqual(answer["outcome"], "landed")


class ReviewEvidenceIsCountedByDistinctThread(MergeTurnTestCase):
    def test_a_repeated_thread_identifier_does_not_stand_for_an_unread_one(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
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
        self.answer_grant(held["turnId"], self.alpha.task_id)
        answer = self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"),
            review={"hasNextPage": False, "pagesRead": 2, "totalCount": 2,
                    "threadsSeen": ["thread-1", "thread-2"], "unresolved": 0})
        self.assertEqual(answer["state"], "merging")


class AGrantIsAddressedAndConverges(MergeTurnTestCase):
    """Who was told they have the turn, and what a second telling does.

    Before this, a promotion moved a row and said nothing to anybody. The parent behind it
    learned it had the turn only if it happened to look, which is how four parents ended up
    waiting for a human to reassign a window none of them could see had moved.
    """

    def test_a_claim_on_a_free_target_is_granted_to_its_owner(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        grant = self.turns.turn(held["turnId"])["grant"]
        self.assertEqual(grant["recipientTaskId"], self.alpha.task_id)
        self.assertEqual(grant["grantedFrom"], "claim")
        self.assertEqual(grant["candidateHead"], "head-a")
        self.assertIsNone(grant["acknowledgedAt"])

    def test_the_grant_is_keyed_by_what_defines_it_and_not_by_a_clock(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        record = self.turns.turn(held["turnId"])
        key = "grant:" + grant_id(held["turnId"], record["tenure"], 1)
        keys = [entry["idempotencyKey"] for entry in record["ledger"]
                if entry["evidenceKind"] == "grant"]
        self.assertEqual(keys, [key])
        self.assertNotIn(self.clock.iso(), key)

    def test_a_candidate_this_turn_used_before_can_be_restored(self):
        """A to B and back to A. Keyed on the candidate, the third grant collided with the
        first, and the ledger's own consistency check refused the restatement."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        first = self.turns.turn(held["turnId"])["grant"]["grantId"]
        self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-b")
        back = self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-a")
        self.assertEqual(back["candidateHead"], "head-a")
        self.assertEqual(back["grant"]["candidateHead"], "head-a")
        self.assertEqual(back["grant"]["sequence"], 3)
        self.assertNotEqual(back["grant"]["grantId"], first)
        answered = self.turns.acknowledge_grant(
            held["turnId"], actor=self.alpha.task_id, grant=back["grant"]["grantId"],
            evidence="re-read the record for the restored candidate")
        self.assertEqual(answered["grant"]["acknowledgedBy"], self.alpha.task_id)

    def test_a_promotion_grants_the_next_ready_candidate(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        released = self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="checks are not green yet")
        grant = released["promoted"]["grant"]
        self.assertEqual(grant["recipientTaskId"], self.beta.task_id)
        self.assertEqual(grant["grantedFrom"], "promotion")
        self.assertEqual(grant["turnId"], waiter["turnId"])

    def test_a_late_ready_waiter_is_granted_when_it_takes_the_target(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b", ready=False)
        self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="deferring")
        answer = self.turns.declare_ready(
            waiter["turnId"], actor=self.beta.task_id, ready=True)
        self.assertEqual(answer["grant"]["grantedFrom"], "late_ready")

    def test_a_duplicate_acknowledgement_records_one_fact(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        grant = self.turns.turn(held["turnId"])["grant"]["grantId"]
        for _ in range(3):
            answer = self.turns.acknowledge_grant(
                held["turnId"], actor=self.alpha.task_id, grant=grant,
                evidence="read the grant and re-checked the head")
        entries = [entry for entry in answer["ledger"]
                   if entry["evidenceKind"] == "grant_acknowledged"]
        self.assertEqual(len(entries), 1)
        self.assertEqual(answer["grant"]["acknowledgedBy"], self.alpha.task_id)

    def test_a_grant_from_a_closed_tenure_acknowledges_nothing(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        stale = self.turns.turn(held["turnId"])["grant"]["grantId"]
        self.turns.release(
            held["turnId"], actor=self.alpha.task_id, disposition="returned",
            reason="handing it back")
        again = self.claim(self.alpha, PROJECT_A, "head-a2")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.acknowledge_grant(
                again["turnId"], actor=self.alpha.task_id, grant=stale,
                evidence="I still had the old one")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_NOT_HELD)

    def test_a_restated_candidate_gets_its_own_grant_to_answer(self):
        """The wedge a head change used to create, from both sides.

        Keyed on the tenure alone, the only grant named a head that no longer existed. A
        holder that had not answered it could never answer it again, and one that HAD answered
        the old head could merge the new candidate having acknowledged nothing about it.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        first = self.turns.turn(held["turnId"])["grant"]["grantId"]
        moved = self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-a2")

        second = moved["grant"]
        self.assertEqual(second["candidateHead"], "head-a2")
        self.assertEqual(second["grantedFrom"], "candidate_restated")
        self.assertNotEqual(second["grantId"], first)
        self.assertIsNone(second["acknowledgedAt"])

        with self.assertRaises(CoordinationError) as caught:
            self.turns.acknowledge_grant(
                held["turnId"], actor=self.alpha.task_id, grant=first,
                evidence="acting on what I read before")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_NOT_HELD)

        answered = self.turns.acknowledge_grant(
            held["turnId"], actor=self.alpha.task_id, grant=second["grantId"],
            evidence="re-read the record for the new head")
        self.assertEqual(answered["grant"]["acknowledgedBy"], self.alpha.task_id)

    def test_answering_the_old_head_does_not_let_the_new_one_merge(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        self.turns.declare_ready(
            held["turnId"], actor=self.alpha.task_id, ready=True, candidate_head="head-a2")
        # A restatement resets readiness, so declare it again for the head it now means.
        self.turns.declare_ready(held["turnId"], actor=self.alpha.task_id, ready=True)
        with self.assertRaises(CoordinationError) as caught:
            self.turns.begin_merge(
                held["turnId"], actor=self.alpha.task_id, head_sha="head-a2",
                base_sha="base-0", checks=run_checks("head-a2"), review=dict(GREEN),
                required=["dev-gate"])
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_NOT_HELD)
        self.assertIn("acknowledged it", caught.exception.detail)

    def test_a_bare_acknowledgement_is_refused(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        grant = self.turns.turn(held["turnId"])["grant"]["grantId"]
        with self.assertRaises(CoordinationError) as caught:
            self.turns.acknowledge_grant(
                held["turnId"], actor=self.alpha.task_id, grant=grant, evidence="  ")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)

    def test_a_stranger_cannot_acknowledge_somebody_elses_grant(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        grant = self.turns.turn(held["turnId"])["grant"]["grantId"]
        with self.assertRaises(CoordinationError) as caught:
            self.turns.acknowledge_grant(
                held["turnId"], actor=self.beta.task_id, grant=grant, evidence="mine now")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_TURN_NOT_HELD)

    def test_no_grant_is_written_while_an_outcome_is_unknown(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.answer_grant(held["turnId"], self.alpha.task_id)
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        self.answer_grant(waiter["turnId"], self.beta.task_id)
        self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        self.turns.report_unknown(
            held["turnId"], actor=self.alpha.task_id, reason="the host stopped answering")
        self.clock.advance(1_000_000)
        self.assertIsNone(self.turns.turn(waiter["turnId"])["grant"])
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")


class TheNamesThisModuleWritesAreItsOwn(MergeTurnTestCase):
    """A ledger that converges on conflict can be made to converge on somebody else's row.

    attest() takes any kind and any key, and the write is ON CONFLICT DO NOTHING. Reaching a
    key first would not overwrite the engine's record; it would make the engine's record
    silently not happen, which is the one thing an append-only ledger must not produce.
    """

    def test_attesting_a_reserved_evidence_kind_is_refused(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.attest(
                held["turnId"], evidence_kind="grant", idempotency_key="mine-1",
                actor=self.beta.task_id, evidence="I say it is granted")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)

    def test_attesting_into_a_reserved_key_namespace_is_refused(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        with self.assertRaises(CoordinationError) as caught:
            self.turns.attest(
                held["turnId"], evidence_kind="transport_accepted",
                idempotency_key="close:landed", actor=self.beta.task_id, evidence="accepted")
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_EVIDENCE_REQUIRED)

    def test_a_transport_acceptance_is_still_ordinary_and_allowed(self):
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        answer = self.turns.attest(
            held["turnId"], evidence_kind="transport_accepted", idempotency_key="delivery-9",
            actor=self.beta.task_id, evidence="the relay accepted the message")
        kinds = {entry["evidenceKind"] for entry in answer["ledger"]}
        self.assertIn("transport_accepted", kinds)

    def test_a_squatted_reserved_key_rolls_the_whole_operation_back(self):
        """The read-back, measured where attest() can no longer reach.

        A store can already hold such a row - written before the namespace was reserved, or by
        something that is not this module. The engine must not treat the discarded insert as a
        successful one, and must not leave half of a release behind either.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.claim(self.beta, PROJECT_B, "head-b")
        tenure = self.turns.turn(waiter["turnId"])["tenure"]
        self.store.db.execute(
            "INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state,"
            " evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?)",
            ("squatted-1", waiter["turnId"], "attestation", None, None, "transport_accepted",
             self.beta.task_id, "not a promotion", "promote:" + str(tenure),
             "2026-01-01T00:00:00Z"))
        with self.assertRaises(CoordinationError) as caught:
            self.turns.release(
                held["turnId"], actor=self.alpha.task_id, disposition="returned",
                reason="handing it on")
        self.assertIn("promote:" + str(tenure), caught.exception.detail)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")
        self.assertTrue(self.turns.target(REPO, BASE)["occupied"])

    def write_row(self, turn, *, kind, key, evidence):
        """A ledger row this module did not write, which a real store can already hold."""
        self.store.db.execute(
            "INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state,"
            " evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?)",
            ("legacy-" + key, turn, "attestation", None, None, kind, self.beta.task_id,
             evidence, key, "2026-01-01T00:00:00Z"))

    def test_a_ledger_holding_a_legacy_grant_attestation_still_reads(self):
        """attest() took any kind and any text until this change reserved the namespace.

        A valid existing store therefore holds rows whose kind is grant and whose evidence is
        a sentence somebody typed. Reading one as an envelope raised out of turn(), which
        every mutator calls AFTER its transaction commits - so the write landed and the caller
        was handed a host fault for an operation that had already succeeded.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        mine = self.turns.turn(held["turnId"])["grant"]["grantId"]
        self.write_row(held["turnId"], kind="grant", key="chat-note-1",
                       evidence="approved in chat")
        self.write_row(held["turnId"], kind="grant", key="grant:not-json",
                       evidence="{oops")
        self.write_row(held["turnId"], kind="grant", key="grant:no-sequence",
                       evidence='{"grantId": "mtg-forged"}')

        record = self.turns.turn(held["turnId"])
        self.assertEqual(record["grant"]["grantId"], mine)
        self.assertEqual(sorted(record["unreadableGrants"]),
                         ["chat-note-1", "grant:no-sequence", "grant:not-json"])
        self.assertEqual(
            self.turns.target(REPO, BASE)["holder"]["grant"]["grantId"], mine)
        self.assertEqual(self.turns.outstanding(self.alpha.task_id)[0]["grant"]["grantId"],
                         mine)

        # And the mutators that resolve the current grant inside their own transaction.
        self.turns.acknowledge_grant(
            held["turnId"], actor=self.alpha.task_id, grant=mine,
            evidence="read the grant past the rows nobody can read")
        answer = self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        self.assertEqual(answer["state"], "merging")

    def test_a_turn_whose_only_grant_row_is_unreadable_has_no_grant_and_no_gate(self):
        """Not this module's grant, so not a grant - and nothing to acknowledge either."""
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        self.store.db.execute(
            "DELETE FROM merge_turn_ledger WHERE turn_id = ? AND evidence_kind = ?",
            (held["turnId"], "grant"))
        self.write_row(held["turnId"], kind="grant", key="chat-note-1",
                       evidence="approved in chat")
        record = self.turns.turn(held["turnId"])
        self.assertIsNone(record["grant"])
        self.assertEqual(record["unreadableGrants"], ["chat-note-1"])
        answer = self.turns.begin_merge(
            held["turnId"], actor=self.alpha.task_id, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        self.assertEqual(answer["state"], "merging")

    def test_a_well_formed_row_that_is_not_this_turns_grant_cannot_become_it(self):
        """Shape is not identity.

        A legacy row whose evidence happened to be well-formed JSON with a large sequence
        became the CURRENT grant, and the engine's own grant was then refused as stale. The
        envelope now has to BE the one this module would have written for this turn: naming
        this turn and tenure, with the id derived from those and its own sequence, under that
        id's key.
        """
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        turn = held["turnId"]
        mine = self.turns.turn(turn)["grant"]["grantId"]
        tenure = self.turns.turn(turn)["tenure"]

        forged = [
            ("grant:mtg-forged", {"grantId": "mtg-forged", "sequence": 9999,
                                  "turnId": turn, "tenure": tenure,
                                  "recipientTaskId": self.beta.task_id,
                                  "candidateHead": "head-x"}),
            ("grant:" + grant_id(turn, tenure, 9998),
             {"grantId": grant_id(turn, tenure, 9998), "sequence": 9998,
              "turnId": "some-other-turn", "tenure": tenure,
              "recipientTaskId": self.beta.task_id, "candidateHead": "head-x"}),
            ("wrong-key", {"grantId": grant_id(turn, tenure, 9997), "sequence": 9997,
                           "turnId": turn, "tenure": tenure,
                           "recipientTaskId": self.beta.task_id,
                           "candidateHead": "head-x"}),
        ]
        for key, envelope in forged:
            self.write_row(turn, kind="grant", key=key, evidence=json.dumps(envelope))

        record = self.turns.turn(turn)
        self.assertEqual(record["grant"]["grantId"], mine)
        self.assertEqual(len(record["unreadableGrants"]), 3)
        # And the in-transaction resolver agrees, so the real grant still acknowledges.
        self.turns.acknowledge_grant(
            turn, actor=self.alpha.task_id, grant=mine,
            evidence="the impersonating rows are not this turn's grant")
        self.assertEqual(self.turns.turn(turn)["grant"]["acknowledgedBy"],
                         self.alpha.task_id)


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


CROSSED = json.loads(
    (pathlib.Path(__file__).parent / "fixtures" / "merge_turn_crossed_handoff.json")
    .read_text(encoding="utf-8"))


class TheCrossedHandoffOf20260921(MergeTurnTestCase):
    """The sequence this issue started from, replayed as an input rather than as a claim.

    What this is: four parents on one base ref, one of them holding while its required CI was
    still running, one of them asking for the turn back, and a peer reporting that the window
    had been handed over. It is reconstructed from those parents' own reports and replayed
    against this store.

    What it is NOT, said here because the distinction is the whole point of keeping it. It was
    never observed inside this store. It was not rerun on any host. It does not reverse the
    landing it describes, and a green run here is not evidence that the original defect was
    reproduced - it is evidence that the SHAPE is refused now.
    """

    def setUp(self):
        super().setUp()
        self.target.set(CROSSED["repository"], CROSSED["baseRef"], "base-0")
        self.crossed = {}
        for entry in CROSSED["parents"]:
            endpoint = Endpoint(entry["task"], entry["host"], cwd="/" + entry["task"])
            self.linkage.bind_scope(
                role=PARENT, scope_key=entry["project"], endpoint=endpoint)
            self.linkage.register_supervision(
                initiative_key="INIT-1", project_key=entry["project"],
                supervisor=self.supervisor, parent=endpoint)
            self.crossed[entry["task"]] = (endpoint, entry)

    def claim_fixture(self, task):
        endpoint, entry = self.crossed[task]
        return self.turns.request(
            repository=CROSSED["repository"], base_ref=CROSSED["baseRef"],
            project_key=entry["project"], holder=endpoint,
            candidate_head=entry["head"], pr_number=entry["pr"], ready=True)

    def test_the_recorded_sequence_is_refused_at_every_step_it_should_be(self):
        repository, base = CROSSED["repository"], CROSSED["baseRef"]
        held = self.claim_fixture("task-hierarchy")
        waiting = [self.claim_fixture(task)
                   for task in ("task-plugin", "task-docs", "task-status")]
        self.assertEqual(held["state"], "holding")
        self.assertEqual([turn["state"] for turn in waiting], ["waiting"] * 3)
        self.answer_grant(held["turnId"], "task-hierarchy")

        # Its required CI has not finished. The restatement is refused, the refusal is kept,
        # and the turn stays exactly where it was.
        with self.assertRaises(CoordinationError):
            self.turns.begin_merge(
                held["turnId"], actor="task-hierarchy", head_sha="head-73",
                base_sha="base-0", required=["dev-gate"],
                checks=run_checks("head-73", conclusion=None), review=dict(GREEN))
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")

        blocked = self.turns.target(repository, base)["blocked"]
        self.assertEqual(blocked["cause"], "required_evidence_not_current")
        self.assertEqual(blocked["prNumber"], 73)
        self.assertEqual(len(blocked["readyPeers"]), 3)

        # The crossing itself. Asking is recorded, the transport accepting is recorded, and a
        # peer's account of having been given the window is not in the store at all.
        self.turns.request_return(
            held["turnId"], actor="task-status", evidence="my candidate is green")
        self.turns.attest(
            held["turnId"], evidence_kind="transport_accepted", idempotency_key="msg-69",
            actor="task-status", evidence="the relay accepted the message")
        answer = self.turns.target(repository, base)
        self.assertIsNotNone(answer["returnRequestedAt"])
        self.assertIsNotNone(answer["transportAcceptedAt"])
        self.assertIsNone(answer["releasedAt"])
        self.assertEqual(answer["holder"]["holderTaskId"], "task-hierarchy")

        # Only the holder's own write returns it, and exactly one waiter is granted the turn.
        released = self.turns.release(
            held["turnId"], actor="task-hierarchy", disposition="returned",
            reason="required CI has not finished and a peer is ready")
        promoted = released["promoted"]
        self.assertEqual(promoted["turnId"], waiting[0]["turnId"])
        self.assertEqual(promoted["grant"]["recipientTaskId"], "task-plugin")
        still = [self.turns.turn(turn["turnId"])["state"] for turn in waiting[1:]]
        self.assertEqual(still, ["waiting", "waiting"])
        self.assertEqual(
            [self.turns.turn(turn["turnId"])["grant"] for turn in waiting[1:]], [None, None])

    def test_the_fixture_says_what_it_is_and_what_it_is_not(self):
        """A fixture that lost its provenance becomes a claim about a host it never touched."""
        self.assertIn("never observed inside this store", CROSSED["provenance"])
        self.assertIn("not rerun on any host", CROSSED["provenance"])
        self.assertEqual(len(CROSSED["parents"]), 4)
        for step in CROSSED["steps"]:
            self.assertTrue(step["invariant"].strip())


class TheBaseTheTargetActuallyReads(MergeTurnTestCase):
    """CRW-229: a recorded base is what the branch reads, never what a caller typed.

    The CRW-124 G1 trial, on an installed relay: both parents landed their first candidate by
    fast-forward and passed --observed-base-sha the base they had CHECKED against rather than
    the one the branch pointed at afterwards. The next candidate, verified and built on the
    real post-landing base, was then refused merge_currency_stale, and nothing could correct a
    landed turn. The relay now reads the target when it records a base, refuses a statement
    that disagrees with that reading, and a recorded base can be read again and restated.
    """

    POST = "base-1"

    def held(self, endpoint=None, project=PROJECT_A, head="head-a"):
        endpoint = endpoint or self.alpha
        held = self.claim(endpoint, project, head)
        self.answer_grant(held["turnId"], endpoint.task_id)
        return held

    def check(self, held, *, head="head-a", base="base-0", actor=None):
        return self.turns.begin_merge(
            held["turnId"], actor=actor or self.alpha.task_id, head_sha=head, base_sha=base,
            checks=run_checks(head), review=dict(GREEN), required=["dev-gate"])

    def merging(self, head="head-a", base="base-0"):
        held = self.held(head=head)
        self.check(held, head=head, base=base)
        return held

    def merged(self, tip=POST):
        """The merge happened: the branch now reads tip."""
        self.target.set(REPO, BASE, tip)

    def land(self, held, *, landed="merge-1", observed=None, actor=None):
        arguments = {"actor": actor or self.alpha.task_id, "landed_sha": landed,
                     "evidence": "merged; the base branch read afterwards"}
        if observed is not None:
            arguments["observed_base_sha"] = observed
        return self.turns.land(held["turnId"], **arguments)

    def landed(self, *, head="head-a", tip=POST):
        held = self.merging(head=head)
        self.merged(tip)
        self.land(held)
        return held

    def restate(self, turn, *, actor=None, observed=None, evidence="read the branch again"):
        arguments = {"actor": actor or self.alpha.task_id, "evidence": evidence}
        if observed is not None:
            arguments["observed_base_sha"] = observed
        return self.turns.restate_base(turn, **arguments)

    def refused(self, call, reason):
        with self.assertRaises(CoordinationError) as caught:
            call()
        self.assertEqual(caught.exception.reason, reason)
        return caught.exception

    def ledger_rows(self, turn):
        return self.store.all(
            "SELECT * FROM merge_turn_ledger WHERE turn_id = ? ORDER BY recorded_at, entry_id",
            (turn,))

    def restatement_rows(self, turn):
        return [row for row in self.ledger_rows(turn)
                if row["evidence_kind"] == "landing_base_restated"]

    def legacy_row(self, turn, *, kind, key, evidence_kind, evidence, states=("landed", None)):
        """A ledger row as a store written before this change could hold it."""
        self.store.db.execute(
            "INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state,"
            " evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?)",
            ("legacy-" + key, turn, kind, states[0], states[1], evidence_kind,
             self.alpha.task_id, evidence, key, self.clock.iso()))

    def r3_landing(self, held):
        """What an R3 store holds after the trial: the landing recorded its checked base."""
        self.store.db.execute(
            "UPDATE merge_turns SET observed_base_sha = checked_base_sha WHERE turn_id = ?",
            (held["turnId"],))

    # ------------------------------------------------------------- the check reads

    def test_the_check_stores_what_the_branch_reads(self):
        held = self.held()
        self.target.set(REPO, BASE, "c" * 40)
        answer = self.check(held, base="C" * 40)
        self.assertEqual(answer["state"], "merging")
        self.assertEqual(answer["checkedBaseSha"], "c" * 40)

    def test_a_restated_base_the_branch_does_not_read_is_refused(self):
        held = self.held()
        error = self.refused(lambda: self.check(held, base="base-x"),
                             RefusalReason.MERGE_CURRENCY_STALE)
        self.assertIn("reads 'base-0'", error.detail)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")

    def test_a_first_turn_restating_its_own_head_as_base_never_merges(self):
        """Review round 3: a first check has no landing to compare, so its base was trusted."""
        held = self.held()
        self.refused(lambda: self.check(held, base="head-a"),
                     RefusalReason.MERGE_CURRENCY_STALE)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "holding")

    def test_an_unreadable_target_is_a_refused_check_with_its_own_cause(self):
        held = self.held()
        self.target.forget(REPO, BASE)
        self.refused(lambda: self.check(held), RefusalReason.MERGE_TARGET_UNREADABLE)
        rows = self.store.all("SELECT * FROM merge_turn_checks WHERE turn_id = ?",
                              (held["turnId"],))
        self.assertEqual([row["refusal_reason"] for row in rows], ["merge_target_unreadable"])
        self.assertEqual(self.turns.target(REPO, BASE)["blocked"]["cause"], "target_unreadable")

    def test_a_check_refused_before_the_comparison_never_reads_the_target(self):
        held = self.held()
        before = len(self.target.reads)
        self.refused(lambda: self.check(held, head="head-z"),
                     RefusalReason.MERGE_CANDIDATE_MOVED)
        self.refused(lambda: self.check(held, actor=self.beta.task_id),
                     RefusalReason.MERGE_TURN_NOT_HELD)
        self.assertEqual(len(self.target.reads), before)

    def test_the_check_leaves_its_reading_on_the_engines_own_transition(self):
        held = self.merging()
        entry = next(row for row in self.ledger_rows(held["turnId"])
                     if row["evidence_kind"] == "currency_confirmed")
        self.assertEqual(entry["kind"], "transition")
        self.assertEqual(json.loads(entry["evidence"])["baseRead"], "base-0")

    # ------------------------------------------------------------- the landing reads

    def test_a_landing_records_what_the_branch_reads(self):
        held = self.merging()
        self.merged()
        answer = self.land(held)
        self.assertEqual(answer["released"]["state"], "landed")
        self.assertEqual(answer["released"]["observedBaseSha"], self.POST)
        self.assertEqual(answer["released"]["landedSha"], "merge-1")
        self.assertEqual(answer["baseObservation"]["sha"], self.POST)

    def test_a_stated_base_that_agrees_with_the_reading_is_accepted(self):
        held = self.merging()
        self.merged("d" * 40)
        answer = self.land(held, observed="D" * 40)
        self.assertEqual(answer["released"]["observedBaseSha"], "d" * 40)

    def test_the_trial_mistake_is_refused_where_it_is_written(self):
        """landed = the fast-forwarded head, observed = the base it was checked against."""
        held = self.merging()
        self.merged("head-a")
        self.refused(lambda: self.land(held, landed="head-a", observed="base-0"),
                     RefusalReason.MERGE_BASE_MISMATCH)
        record = self.turns.turn(held["turnId"])
        self.assertEqual(record["state"], "merging")
        self.assertIsNone(record["observedBaseSha"])
        self.assertIn("merge_base_mismatch", [c["reason"] for c in self.contests_for()])

    def test_a_landing_before_the_branch_moved_is_refused(self):
        held = self.merging()
        self.refused(lambda: self.land(held), RefusalReason.MERGE_BASE_NOT_ADVANCED)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "merging")
        self.merged()
        self.assertEqual(self.land(held)["released"]["state"], "landed")

    def test_an_abbreviation_is_refused_rather_than_trusted(self):
        """Final review 1: two commits can share a prefix, and nothing here can tell."""
        self.target.set(REPO, BASE, "c" * 40)
        held = self.held()
        self.refused(lambda: self.check(held, base="c" * 8), RefusalReason.MERGE_CURRENCY_STALE)
        self.check(held, base="c" * 40)
        self.merged("d" * 40)
        self.refused(lambda: self.land(held, observed="d" * 8), RefusalReason.MERGE_BASE_MISMATCH)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "merging")

    def test_a_candidate_that_already_was_the_base_is_not_wedged(self):
        held = self.held()
        self.target.set(REPO, BASE, "head-a")
        self.check(held, base="head-a")
        answer = self.land(held, landed="head-a")
        self.assertEqual(answer["released"]["state"], "landed")
        self.assertEqual(answer["released"]["observedBaseSha"], "head-a")

    def test_an_unreadable_target_keeps_the_turn_merging(self):
        held = self.merging()
        self.target.forget(REPO, BASE)
        self.refused(lambda: self.land(held), RefusalReason.MERGE_TARGET_UNREADABLE)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "merging")

    def test_a_relay_with_no_reader_records_no_base(self):
        held = self.merging()
        blind = MergeTurn(self.store, self.clock, self.linkage)
        self.refused(lambda: blind.land(held["turnId"], actor=self.alpha.task_id,
                                        landed_sha="merge-1", evidence="merged"),
                     RefusalReason.MERGE_TARGET_UNREADABLE)

    def test_a_stranger_landing_never_reads_the_target(self):
        held = self.merging()
        before = len(self.target.reads)
        self.refused(lambda: self.land(held, actor="task-stranger"),
                     RefusalReason.MERGE_TURN_NOT_HELD)
        self.assertEqual(len(self.target.reads), before)

    def test_a_turn_checked_before_the_relay_read_its_base_lands_through_resolve(self):
        """Review round 4: an R3 row's checked base is typed, so the branch proves nothing."""
        held = self.merging()
        self.store.db.execute(
            "UPDATE merge_turns SET checked_base_sha = 'base-y' WHERE turn_id = ?",
            (held["turnId"],))
        self.store.db.execute(
            "UPDATE merge_turn_ledger SET evidence = 'chk-legacy'"
            "  WHERE turn_id = ? AND evidence_kind = 'currency_confirmed'", (held["turnId"],))
        self.refused(lambda: self.land(held), RefusalReason.MERGE_EVIDENCE_REQUIRED)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "merging")
        self.turns.report_unknown(held["turnId"], actor=self.alpha.task_id,
                                  reason="checked before the relay read its base")
        self.merged()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.alpha.task_id, observed_base_sha=self.POST,
            pr_state="merged", evidence="the pull request reads merged")
        self.assertEqual(answer["outcome"], "landed")
        self.assertEqual(answer["released"]["observedBaseSha"], self.POST)

    def test_a_forged_reading_mark_written_as_an_attestation_is_not_a_mark(self):
        held = self.merging()
        self.store.db.execute(
            "UPDATE merge_turn_ledger SET kind = 'attestation'"
            "  WHERE turn_id = ? AND evidence_kind = 'currency_confirmed'", (held["turnId"],))
        self.merged()
        self.refused(lambda: self.land(held), RefusalReason.MERGE_EVIDENCE_REQUIRED)

    # ------------------------------------------------------------- resolution reads

    def unknown(self):
        held = self.merging()
        self.turns.report_unknown(held["turnId"], actor=self.alpha.task_id,
                                  reason="lost the connection")
        return held

    def test_a_merged_resolution_records_the_reading(self):
        held = self.unknown()
        self.merged()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha=self.POST,
            pr_state="merged", evidence="merged")
        self.assertEqual(answer["released"]["observedBaseSha"], self.POST)

    def test_a_candidate_the_base_already_contained_resolves_as_merged(self):
        """Stacked pull requests: the merge changed nothing, and the forge says merged."""
        held = self.unknown()
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-0",
            pr_state="merged", evidence="merged; the base already contained it")
        self.assertEqual(answer["outcome"], "landed")

    def test_a_resolution_statement_that_disagrees_with_the_reading_is_refused(self):
        held = self.unknown()
        self.refused(lambda: self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-9",
            pr_state="open", evidence="still open"), RefusalReason.MERGE_BASE_MISMATCH)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "unknown")

    def test_an_unreadable_target_returns_an_open_candidate_without_a_base(self):
        held = self.unknown()
        self.target.forget(REPO, BASE)
        answer = self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-0",
            pr_state="open", evidence="still open")
        self.assertEqual(answer["outcome"], "returned")
        self.assertIsNone(answer["released"]["observedBaseSha"])

    def test_an_unreadable_target_keeps_a_merged_outcome_unknown(self):
        held = self.unknown()
        self.target.forget(REPO, BASE)
        self.refused(lambda: self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha=self.POST,
            pr_state="merged", evidence="merged"), RefusalReason.MERGE_TARGET_UNREADABLE)
        self.assertEqual(self.turns.turn(held["turnId"])["state"], "unknown")

    def test_a_base_missing_after_an_unreadable_return_does_not_let_a_merge_through(self):
        held = self.unknown()
        self.target.forget(REPO, BASE)
        self.turns.resolve_unknown(
            held["turnId"], actor=self.supervisor.task_id, observed_base_sha="base-0",
            pr_state="closed", evidence="closed unmerged")
        second = self.held(head="head-c")
        self.refused(lambda: self.check(second, head="head-c"),
                     RefusalReason.MERGE_TARGET_UNREADABLE)
        self.target.set(REPO, BASE, "base-0")
        self.assertEqual(self.check(second, head="head-c")["state"], "merging")

    # ------------------------------------------------------------- restating it

    def test_the_trial_shape_is_recovered_by_restating_the_landing(self):
        first = self.landed()
        self.r3_landing(first)
        second = self.held(head="head-c")
        self.target.set(REPO, BASE, self.POST)
        error = self.refused(lambda: self.check(second, head="head-c", base=self.POST),
                             RefusalReason.MERGE_CURRENCY_STALE)
        self.assertIn(first["turnId"], error.detail)
        self.assertIn("merge-turn-restate-base", error.detail)
        self.assertIn(self.alpha.task_id, error.detail)

        restated = self.restate(first["turnId"], observed=self.POST)
        self.assertTrue(restated["restated"])
        self.assertEqual(restated["observedBaseSha"], self.POST)
        self.assertEqual([(r["from"], r["to"]) for r in restated["baseRestatements"]],
                         [("base-0", self.POST)])
        self.assertEqual(restated["closeReason"], "merged; the base branch read afterwards")

        self.assertEqual(self.check(second, head="head-c", base=self.POST)["state"], "merging")
        self.merged("merge-2")
        self.assertEqual(self.land(second, landed="merge-2")["released"]["state"], "landed")

    def test_a_restatement_keeps_the_original_beside_the_correction(self):
        first = self.landed()
        self.r3_landing(first)
        self.restate(first["turnId"], evidence="git rev-parse main reads base-1")
        rows = self.restatement_rows(first["turnId"])
        self.assertEqual(len(rows), 1)
        self.assertEqual((rows[0]["kind"], rows[0]["from_state"], rows[0]["to_state"]),
                         ("transition", "landed", "landed"))
        body = json.loads(rows[0]["evidence"])
        self.assertEqual((body["from"], body["to"]), ("base-0", self.POST))
        self.assertEqual(body["evidence"], "git rev-parse main reads base-1")
        self.assertEqual(body["source"], "fake")
        self.assertEqual(rows[0]["idempotency_key"], "restate-base:1")

    def test_restating_what_is_already_recorded_writes_nothing(self):
        first = self.landed()
        answer = self.restate(first["turnId"])
        self.assertFalse(answer["restated"])
        self.assertEqual(self.restatement_rows(first["turnId"]), [])
        self.r3_landing(first)
        self.restate(first["turnId"])
        self.restate(first["turnId"])
        self.assertEqual(len(self.restatement_rows(first["turnId"])), 1)

    def test_a_branch_that_moved_twice_keeps_every_step(self):
        first = self.landed()
        self.target.set(REPO, BASE, "base-2")
        self.restate(first["turnId"])
        self.target.set(REPO, BASE, self.POST)
        answer = self.restate(first["turnId"])
        self.assertEqual([(r["sequence"], r["from"], r["to"]) for r in answer["baseRestatements"]],
                         [(1, self.POST, "base-2"), (2, "base-2", self.POST)])

    def test_the_supervisor_may_restate_and_a_stranger_learns_nothing(self):
        first = self.landed()
        self.r3_landing(first)
        before = len(self.target.reads)
        self.refused(lambda: self.restate(first["turnId"], actor="task-stranger"),
                     RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(len(self.target.reads), before)
        answer = self.restate(first["turnId"], actor=self.supervisor.task_id)
        self.assertEqual(answer["observedBaseSha"], self.POST)

    def test_a_restated_value_the_branch_does_not_read_is_refused(self):
        first = self.landed()
        self.r3_landing(first)
        self.refused(lambda: self.restate(first["turnId"], observed="base-0"),
                     RefusalReason.MERGE_BASE_MISMATCH)
        self.assertEqual(self.turns.turn(first["turnId"])["observedBaseSha"], "base-0")

    def test_only_a_landed_turn_is_restated(self):
        held = self.merging()
        self.refused(lambda: self.restate(held["turnId"]), RefusalReason.MERGE_TURN_NOT_HELD)

    def test_only_the_landing_the_currency_check_reads_is_restated(self):
        first = self.landed()
        second = self.held(head="head-c")
        self.check(second, head="head-c", base=self.POST)
        self.merged("base-2")
        self.land(second)
        error = self.refused(lambda: self.restate(first["turnId"]),
                             RefusalReason.MERGE_TURN_NOT_HELD)
        self.assertIn(second["turnId"], error.detail)

    def test_two_landings_in_one_instant_are_ordered_by_when_they_were_written(self):
        """Final review 1: closed_at ties were broken by a digest, so by chance."""
        first = self.landed()
        second = self.held(head="head-c")
        self.check(second, head="head-c", base=self.POST)
        self.merged("base-2")
        self.land(second)
        self.assertEqual(self.turns.turn(first["turnId"])["closedAt"],
                         self.turns.turn(second["turnId"])["closedAt"])
        third = self.held(head="head-d")
        self.assertEqual(self.check(third, head="head-d", base="base-2")["state"], "merging")
        error = self.refused(lambda: self.restate(first["turnId"]),
                             RefusalReason.MERGE_TURN_NOT_HELD)
        self.assertIn(second["turnId"], error.detail)

    def test_the_latest_of_two_landings_in_one_instant_is_the_one_restated(self):
        first = self.landed()
        second = self.held(head="head-c")
        self.check(second, head="head-c", base=self.POST)
        self.merged("base-2")
        self.land(second)
        self.r3_landing(second)
        self.merged("base-2")
        answer = self.restate(second["turnId"])
        self.assertEqual(answer["observedBaseSha"], "base-2")
        self.refused(lambda: self.restate(first["turnId"]), RefusalReason.MERGE_TURN_NOT_HELD)

    def test_a_merge_in_flight_holds_the_restatement(self):
        first = self.landed()
        second = self.held(head="head-c")
        self.check(second, head="head-c", base=self.POST)
        self.target.set(REPO, BASE, "base-2")
        self.refused(lambda: self.restate(first["turnId"]),
                     RefusalReason.MERGE_TURN_UNRESOLVED)
        self.assertEqual(self.turns.turn(first["turnId"])["observedBaseSha"], self.POST)

    def test_a_returned_turn_does_not_gate_the_target(self):
        """Architect 011 W1: a resolved-open row carried a base nobody could restate."""
        self.landed()
        second = self.held(head="head-c")
        self.check(second, head="head-c", base=self.POST)
        self.turns.report_unknown(second["turnId"], actor=self.alpha.task_id, reason="lost")
        self.turns.resolve_unknown(
            second["turnId"], actor=self.alpha.task_id, observed_base_sha=self.POST,
            pr_state="open", evidence="still open")
        self.target.set(REPO, BASE, "base-3")
        third = self.held(head="head-d")
        error = self.refused(lambda: self.check(third, head="head-d", base="base-3"),
                             RefusalReason.MERGE_CURRENCY_STALE)
        self.assertNotIn(second["turnId"], error.detail)

    def test_a_restatement_states_why(self):
        first = self.landed()
        self.refused(lambda: self.restate(first["turnId"], evidence=" "),
                     RefusalReason.MERGE_EVIDENCE_REQUIRED)

    def test_attesting_cannot_squat_the_restatement_record(self):
        first = self.landed()
        for kind, key in (("landing_base_restated", "mine-1"),
                          ("transport_accepted", "restate-base:1")):
            self.refused(lambda: self.turns.attest(
                first["turnId"], evidence_kind=kind, idempotency_key=key,
                actor=self.alpha.task_id, evidence="{}"), RefusalReason.MERGE_EVIDENCE_REQUIRED)

    def test_a_key_too_long_to_be_a_number_does_not_stop_a_restatement(self):
        """Final review 3: an older caller could write restate-base:<4301 digits>."""
        first = self.landed()
        turn = first["turnId"]
        self.r3_landing(first)
        self.legacy_row(turn, kind="attestation", key="restate-base:" + "9" * 4301,
                        evidence_kind="transport_accepted", evidence="old caller row")
        self.legacy_row(turn, kind="attestation", key="restate-base:" + "9" * 18,
                        evidence_kind="transport_accepted", evidence="old caller row")
        self.legacy_row(turn, kind="attestation", key="restate-base:1" + "0" * 18,
                        evidence_kind="transport_accepted", evidence="old caller row")
        answer = self.restate(turn)
        self.assertTrue(answer["restated"])
        self.assertEqual([r["sequence"] for r in answer["baseRestatements"]],
                         [10 ** 18 + 1])

    def test_rows_an_older_store_may_hold_neither_block_nor_pass_as_restatements(self):
        first = self.landed()
        turn = first["turnId"]
        self.r3_landing(first)
        self.legacy_row(turn, kind="attestation", key="restate-base:1",
                        evidence_kind="transport_accepted", evidence="squatted")
        self.legacy_row(turn, kind="attestation", key="mine",
                        evidence_kind="landing_base_restated", evidence="free text")
        self.legacy_row(turn, kind="attestation", key="restate-base:7",
                        evidence_kind="landing_base_restated",
                        evidence=json.dumps({"turnId": turn, "sequence": 7, "from": "x",
                                             "to": "y", "evidence": "forged"}))
        answer = self.restate(turn)
        self.assertEqual([r["sequence"] for r in answer["baseRestatements"]], [8])
        self.assertEqual(sorted(answer["unreadableRestatements"]), ["mine", "restate-base:7"])


if __name__ == "__main__":
    unittest.main()
