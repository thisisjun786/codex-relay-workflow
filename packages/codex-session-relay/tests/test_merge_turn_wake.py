"""A freed merge target has to reach the parent it was handed to, not just be written down.

CRW-164 made the grant durable and addressed, and said so in its own docstring: it is not a
push, and nothing in it wakes a parent that is not running. That is fine for a parent that
comes back - it re-reads its own claims - and a promoted parent's last act was to WAIT, which
is what an idle task looks like. These cases are about the difference.

Everything here moves an injected clock and a fake host. No case asserts delivery from a
receipt it wrote itself: the evidence that a parent was woken is a turn on the parent's own
thread carrying the grant, produced by the same atomic claim every other delivery goes
through.
"""

import json
import os
import pathlib

from codex_session_relay import NO_DELIVERABLE
from codex_session_relay import rolepolicy
from codex_session_relay.delivery import MERGE_TURN_GRANT, REVISION
from codex_session_relay.linkage import Linkage, PARENT as PARENT_ROLE, PROJECT
from codex_session_relay.mergeturn import (
    MERGE_TURN_CLOSED, MERGE_TURN_GRANT_ANSWERED, MERGE_TURN_REGRANTED, MergeTurn,
)
from codex_session_relay.models import Endpoint
from codex_session_relay.transport import DISPATCHED

from .support import CHILD, HOST, PARENT, DeliveryTestCase

PROJECT_A = "PRJ-A"
PROJECT_B = "PRJ-B"
REPO = "owner/repo"
BASE = "dev"
GREEN = {"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
         "threadsSeen": ["thread-1"], "unresolved": 0}


def run_checks(head):
    return [{"runId": "run-1", "name": "dev-gate", "headSha": head,
             "conclusion": "success", "attempt": 1}]


# The parent here is BOUND as a parent, which is what makes it a project owner and therefore
# promotable at all - and a role-bound recipient's settings are checked against the role
# policy before anything is sent. The pair is the fixture's own recorded pair, so these cases
# exercise the wake rather than a settings mismatch; test_rolepolicy owns the mismatch.
POLICY = {
    "roles": {
        "supervisor": {"expectation": "record"},
        "parent": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
        "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
    }
}


class MergeTurnWakeTestCase(DeliveryTestCase):
    """One registered assignment, its parent owning a project, and a rival owning another.

    The turn's parent IS the registered assignment's parent on purpose. That is the only
    channel this queue authorizes for a merge-turn grant, and a fixture that used some other
    task would be testing an address the delivery contract refuses.
    """

    def setUp(self):
        super().setUp()
        # Both ends, because one case drives a correction down to the child over the same
        # assignment to prove a grant does not answer it.
        self.relationship = self.register(recipients=[PARENT, CHILD])
        self.rid = self.relationship["relationshipId"]
        self._install_policy()
        self.linkage = Linkage(self.store, self.clock)
        self.turns = MergeTurn(self.store, self.clock, self.linkage, delivery=self.delivery)
        self.alpha = Endpoint(PARENT, HOST, cwd="/parent")
        self.beta = Endpoint("01rival-task", "host-b", cwd="/rival")
        self.supervisor = Endpoint("01supervisor-task", "host-s", cwd="/sup")
        self.adapter.add_thread(self.beta.task_id)
        self.linkage.bind_scope(role=PARENT_ROLE, scope_key=PROJECT_A, endpoint=self.alpha)
        self.linkage.bind_scope(role=PARENT_ROLE, scope_key=PROJECT_B, endpoint=self.beta)
        for project, parent in ((PROJECT_A, self.alpha), (PROJECT_B, self.beta)):
            self.linkage.register_supervision(
                initiative_key="INIT-1", project_key=project,
                supervisor=self.supervisor, parent=parent)

    def _install_policy(self):
        path = pathlib.Path(self.tmp) / "execution-policy.json"
        path.write_text(json.dumps(POLICY), encoding="utf-8")
        previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = str(path)
        rolepolicy.reset()

        def restore():
            if previous is None:
                os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
            else:
                os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = previous
            rolepolicy.reset()

        self.addCleanup(restore)

    # ------------------------------------------------------------- fixtures

    def claim(self, endpoint, project, head, *, ready=True, relationship=True):
        return self.turns.request(
            repository=REPO, base_ref=BASE, project_key=project, holder=endpoint,
            candidate_head=head, ready=ready,
            relationship_id=self.rid if relationship else None)

    def answer_grant(self, turn, actor):
        grant = self.turns.turn(turn)["grant"]
        return self.turns.acknowledge_grant(
            turn, actor=actor, grant=grant["grantId"], evidence="read it and re-checked")

    def wakes(self):
        """Every queued grant notice in this store, oldest first."""
        return self.store.all(
            "SELECT d.event_id, d.recipient_task_id, d.state, e.receipt FROM deliveries d"
            "  JOIN events e ON e.event_id = d.event_id"
            " WHERE d.kind = ? ORDER BY d.created_at, d.event_id", (MERGE_TURN_GRANT,))

    def promoted(self):
        """The rival takes the target, our parent queues behind it, the rival hands it back."""
        held = self.claim(self.beta, PROJECT_B, "head-b")
        waiter = self.claim(self.alpha, PROJECT_A, "head-a")
        self.assertEqual(waiter["state"], "waiting")
        self.turns.release(
            held["turnId"], actor=self.beta.task_id, disposition="returned",
            reason="not ready after all")
        return waiter["turnId"]

    def grant_of(self, turn):
        return self.turns.turn(turn)["grant"]


class APromotionReachesTheParentItNames(MergeTurnWakeTestCase):
    def test_a_promotion_queues_exactly_one_wake(self):
        turn = self.promoted()
        self.assertEqual(self.turns.turn(turn)["state"], "holding")
        queued = self.wakes()
        self.assertEqual(len(queued), 1)
        self.assertEqual(queued[0]["recipient_task_id"], PARENT)
        envelope = json.loads(queued[0]["receipt"])
        self.assertEqual(envelope["grantId"], self.grant_of(turn)["grantId"])
        self.assertEqual(envelope["grantedFrom"], "promotion")

    def test_the_grant_itself_names_the_event_it_was_pushed_through(self):
        turn = self.promoted()
        self.assertEqual(self.grant_of(turn)["wake"]["eventId"],
                         self.wakes()[0]["event_id"])

    def test_a_second_release_on_the_same_target_adds_no_second_wake(self):
        turn = self.promoted()
        self.answer_grant(turn, PARENT)
        self.turns.release(
            turn, actor=PARENT, disposition="returned", reason="handing it back")
        # Nothing was waiting, so nothing was promoted and nothing new was queued.
        self.assertEqual(len(self.wakes()), 1)

    def test_the_event_id_is_derived_from_the_grant_and_not_from_the_clock(self):
        turn = self.promoted()
        first = self.wakes()[0]["event_id"]
        self.clock.advance(3600)
        # The same grant read again resolves to the same notice, so a replay converges.
        self.assertEqual(self.grant_of(turn)["wake"]["eventId"], first)


class NobodyIsWokenWithoutBothConditions(MergeTurnWakeTestCase):
    def test_a_held_target_wakes_nobody(self):
        self.claim(self.beta, PROJECT_B, "head-b")
        self.claim(self.alpha, PROJECT_A, "head-a")
        self.assertEqual(self.wakes(), [])

    def test_an_unready_waiter_is_not_woken_when_the_target_frees(self):
        held = self.claim(self.beta, PROJECT_B, "head-b")
        waiter = self.claim(self.alpha, PROJECT_A, "head-a", ready=False)
        self.turns.release(
            held["turnId"], actor=self.beta.task_id, disposition="returned", reason="done")
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")
        self.assertEqual(self.wakes(), [])

    def test_a_parent_that_took_the_free_target_itself_is_not_messaged(self):
        # It just called this package. Telling it what it has only now done is the heartbeat
        # the CRW-148/149 contract family refuses.
        claimed = self.claim(self.alpha, PROJECT_A, "head-a")
        self.assertEqual(claimed["state"], "holding")
        self.assertEqual(self.wakes(), [])

    def test_a_waiter_that_declares_ready_into_a_free_target_is_not_messaged(self):
        held = self.claim(self.beta, PROJECT_B, "head-b")
        waiter = self.claim(self.alpha, PROJECT_A, "head-a", ready=False)
        self.turns.release(
            held["turnId"], actor=self.beta.task_id, disposition="returned", reason="done")
        answer = self.turns.declare_ready(waiter["turnId"], actor=PARENT, ready=True)
        self.assertEqual(answer["state"], "holding")
        self.assertEqual(self.wakes(), [])

    def test_an_unknown_outcome_is_not_stolen_by_the_clock(self):
        held = self.claim(self.beta, PROJECT_B, "head-b")
        self.answer_grant(held["turnId"], self.beta.task_id)
        self.turns.begin_merge(
            held["turnId"], actor=self.beta.task_id, head_sha="head-b", base_sha="base-0",
            checks=run_checks("head-b"), review=dict(GREEN), required=["dev-gate"])
        waiter = self.claim(self.alpha, PROJECT_A, "head-a")
        self.turns.report_unknown(
            held["turnId"], actor=self.beta.task_id, reason="the host went away mid-merge")
        self.clock.advance(86400 * 7)
        self.assertEqual(self.turns.turn(waiter["turnId"])["state"], "waiting")
        self.assertEqual(self.wakes(), [])


class TheIdleParentActuallyReceivesATurn(MergeTurnWakeTestCase):
    def test_the_wake_produces_a_turn_on_the_parents_own_thread(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        before = len(self.adapter.threads[PARENT].turns)
        record = self.attempt(event)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(len(self.adapter.threads[PARENT].turns), before + 1)
        _turn_id, text = self.adapter.threads[PARENT].items[-1]
        self.assertIn(self.grant_of(turn)["grantId"], text)
        self.assertIn("merge turn granted", text)
        self.assertIn(REPO, text)

    def test_the_message_does_not_ask_for_an_acknowledgement_it_cannot_receive(self):
        self.promoted()
        self.attempt(self.wakes()[0]["event_id"])
        _turn_id, text = self.adapter.threads[PARENT].items[-1]
        self.assertNotIn("ack-proof", text)
        self.assertIn("merge-turn-acknowledge", text)

    def test_a_delivered_wake_reports_the_acknowledgement_the_turn_is_waiting_for(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.attempt(event)
        phases = {item["eventId"]: item["phase"]
                  for item in self.delivery.snapshot()["deliveries"]}
        self.assertEqual(phases[event], "awaiting_grant_acknowledgement")
        self.answer_grant(turn, PARENT)
        phases = {item["eventId"]: item["phase"]
                  for item in self.delivery.snapshot()["deliveries"]}
        self.assertEqual(phases[event], "grant_acknowledged")

    def test_sending_it_twice_is_not_possible_from_one_grant(self):
        self.promoted()
        event = self.wakes()[0]["event_id"]
        self.attempt(event)
        sends = len(self.adapter.sends)
        # Dispatched is not claimable, so a second pass is a no-op rather than a second turn.
        self.attempt(event)
        self.assertEqual(len(self.adapter.sends), sends)


class AGrantsCurrencyIsItsOwnTurn(MergeTurnWakeTestCase):
    def test_an_acknowledged_grant_is_not_sent_afterwards(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.answer_grant(turn, PARENT)
        record = self.attempt(event)
        self.assertEqual(record["sendAttempted"], "no")
        self.assertEqual(record["supersededReason"], MERGE_TURN_GRANT_ANSWERED)
        self.assertEqual(self.adapter.sends, [])

    def test_a_returned_turn_suppresses_its_queued_wake(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.answer_grant(turn, PARENT)
        self.turns.release(
            turn, actor=PARENT, disposition="returned", reason="cannot land it today")
        record = self.attempt(event)
        self.assertEqual(record["sendAttempted"], "no")
        self.assertIn(record["supersededReason"],
                      (MERGE_TURN_CLOSED, MERGE_TURN_GRANT_ANSWERED))
        self.assertEqual(self.adapter.sends, [])

    def test_a_restated_candidate_supersedes_the_notice_it_replaced(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.turns.declare_ready(turn, actor=PARENT, ready=True, candidate_head="head-a2")
        record = self.attempt(event)
        self.assertEqual(record["sendAttempted"], "no")
        self.assertEqual(record["supersededReason"], MERGE_TURN_REGRANTED)

    def test_a_generation_that_advances_does_not_lose_the_wake(self):
        # The assignment's generation is bookkeeping about a child's work. It neither gives
        # nor takes the right to land on a shared branch, and a promotion does not come round
        # again for a parent that already holds the target, so suppressing here loses the
        # wake for good.
        self.promoted()
        event = self.wakes()[0]["event_id"]
        with self.store.transaction() as db:
            self.registry.open_generation_in(
                db, self.rid, dispatch_request_id="revision-1",
                reason="needs_changes_revision")
        record = self.attempt(event)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        phases = {item["eventId"]: item["phase"]
                  for item in self.delivery.snapshot()["deliveries"]}
        self.assertEqual(phases[event], "awaiting_grant_acknowledgement")


class AGrantAnswersNothingTheChildWasAskedFor(MergeTurnWakeTestCase):
    def staged_revision(self):
        """A relay-owned revision request, queued the way ack.py queues one.

        Written directly rather than driven through claim, ack-proof and verdict, because what
        is under test is the supersession rule this row is measured by, not the route that
        produces it.
        """
        event = "f" * 32
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO events (event_id, relationship_id, execution_generation,"
                " revision_hash, outcome, producer, attempt, turn_thread_id, turn_id,"
                " turn_status, receipt, stage, first_seen_at, last_seen_at,"
                " observation_count) VALUES (?,?,?,?,?,?,NULL,?,?,?,?, 'final', ?,?,1)",
                (event, self.rid, self.relationship["executionGeneration"], NO_DELIVERABLE,
                 REVISION, "relay", PARENT, "turn-verdict-1", "completed",
                 json.dumps({"eventId": event, "relationshipId": self.rid,
                             "executionGeneration":
                                 self.relationship["executionGeneration"],
                             "kind": REVISION, "criteria": []}), now, now),
            )
            self.delivery.enqueue_in(
                db, event, relationship_id=self.rid, kind=REVISION,
                recipient_task_id=CHILD)
        return event

    def test_queuing_a_grant_does_not_answer_a_pending_correction(self):
        revision = self.staged_revision()
        self.promoted()
        with self.store.transaction() as db:
            self.assertIsNone(self.delivery._supersession_reason(db, revision))
        self.assertIsNone(self.store.one(
            "SELECT reason FROM delivery_supersession WHERE event_id = ?", (revision,)))

    def test_the_correction_still_reaches_the_child_after_a_promotion(self):
        revision = self.staged_revision()
        self.promoted()
        record = self.attempt(revision)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertEqual(len(self.adapter.threads[CHILD].turns), 1)


class AnUnaddressableGrantIsRecordedRatherThanLost(MergeTurnWakeTestCase):
    def test_a_turn_naming_no_assignment_still_records_its_grant(self):
        held = self.claim(self.beta, PROJECT_B, "head-b")
        waiter = self.turns.request(
            repository=REPO, base_ref=BASE, project_key=PROJECT_A, holder=self.alpha,
            candidate_head="head-a", ready=True)
        self.turns.release(
            held["turnId"], actor=self.beta.task_id, disposition="returned", reason="done")
        promoted = self.turns.turn(waiter["turnId"])
        self.assertEqual(promoted["state"], "holding")
        self.assertEqual(promoted["grant"]["wake"]["refused"],
                         "the turn names no assignment")
        self.assertEqual(self.wakes(), [])

    def test_an_assignment_addressed_to_another_parent_is_not_borrowed(self):
        # The delivery contract is that an event belongs to one assignment and travels to
        # that assignment's own endpoint. A merge turn whose assignment names somebody else
        # has no address here, and reaching for a different one would be a cross delivery.
        held = self.claim(self.alpha, PROJECT_A, "head-a")
        waiter = self.turns.request(
            repository=REPO, base_ref=BASE, project_key=PROJECT_B, holder=self.beta,
            candidate_head="head-b", ready=True, relationship_id=self.rid)
        self.turns.release(
            held["turnId"], actor=PARENT, disposition="returned", reason="done")
        promoted = self.turns.turn(waiter["turnId"])
        self.assertEqual(promoted["state"], "holding")
        self.assertIn("addressed to parent", promoted["grant"]["wake"]["refused"])
        self.assertEqual(self.wakes(), [])

    def test_a_parent_outside_its_own_assignments_recipients_is_refused_once_here(self):
        # registration only requires the list to be non-empty, and the send-time check is the
        # recipient list. Without this the notice would be queued and refused at every attempt
        # forever instead of once, here, with a reason.
        self.store.db.execute(
            "UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?",
            (json.dumps([CHILD]), self.rid))
        turn = self.promoted()
        self.assertEqual(self.turns.turn(turn)["state"], "holding")
        self.assertIn("is not in the recipients", self.grant_of(turn)["wake"]["refused"])
        self.assertEqual(self.wakes(), [])


class TheCollaboratorIsOptional(MergeTurnWakeTestCase):
    def test_without_a_delivery_service_the_grant_is_exactly_what_it_was(self):
        plain = MergeTurn(self.store, self.clock, self.linkage)
        held = plain.request(
            repository=REPO, base_ref="release", project_key=PROJECT_B, holder=self.beta,
            candidate_head="head-b", ready=True, relationship_id=self.rid)
        waiter = plain.request(
            repository=REPO, base_ref="release", project_key=PROJECT_A, holder=self.alpha,
            candidate_head="head-a", ready=True, relationship_id=self.rid)
        plain.release(
            held["turnId"], actor=self.beta.task_id, disposition="returned", reason="done")
        grant = plain.turn(waiter["turnId"])["grant"]
        self.assertEqual(grant["grantedFrom"], "promotion")
        self.assertNotIn("wake", grant)
        self.assertEqual(self.wakes(), [])
