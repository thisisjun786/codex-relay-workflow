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
import shlex
import types

from codex_session_relay import NO_DELIVERABLE
from codex_session_relay import cli, report, rolepolicy
from codex_session_relay.delivery import MERGE_TURN_GRANT, REVISION
from codex_session_relay.errors import CoordinationError, RefusalReason
from codex_session_relay.identity import merge_turn_grant_event_id
from codex_session_relay.linkage import Linkage, PARENT as PARENT_ROLE, PROJECT
from codex_session_relay.mergeturn import (
    MERGE_TURN_ABSENT, MERGE_TURN_CLOSED, MERGE_TURN_GRANT_ANSWERED,
    MERGE_TURN_GRANT_UNREADABLE, MERGE_TURN_REGRANTED, MergeTurn,
)
from codex_session_relay.models import Endpoint
from codex_session_relay.transport import DISPATCHED, HELD_UNCERTAIN, SUPERSEDED

from .support import CHILD, HOST, PARENT, DeliveryTestCase
from .test_report_contract import a_handoff, a_report

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

    def attach(self, project_key):
        """Record which project this assignment belongs to.

        Staged directly rather than driven through linkage.attach_in, because what is under
        test is the wake READING the attachment, not the route that writes it.
        """
        self.store.db.execute(
            "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"
            " VALUES (?,?,?) ON CONFLICT(relationship_id)"
            " DO UPDATE SET project_key = excluded.project_key",
            (self.rid, project_key, self.clock.iso()))

    def test_an_assignment_attached_to_another_project_does_not_carry_this_turns_wake(self):
        # A parent can own more than one project, so its assignment under one of them passes
        # every authorization check for a turn under another: active, addressed to it, and
        # authorizing it as a recipient. It is still the wrong ledger. _relationship_refusal
        # already refuses to MERGE such a turn as FOREIGN_SCOPE, so a wake pushed through it
        # would file this target's notice in another project's stream and invite the
        # recipient to do something begin_merge then refuses.
        self.attach("PRJ-C")
        turn = self.promoted()
        self.assertEqual(self.turns.turn(turn)["state"], "holding")
        self.assertIn("attached to project", self.grant_of(turn)["wake"]["refused"])
        self.assertEqual(self.wakes(), [])

    def test_an_assignment_attached_to_this_turns_own_project_still_carries_it(self):
        self.attach(PROJECT_A)
        turn = self.promoted()
        self.assertEqual(self.grant_of(turn)["wake"]["eventId"], self.wakes()[0]["event_id"])


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

# ----------------------------------------------------------- PR132 post-merge review


class DistinctGrantsAreDistinctNotices(MergeTurnWakeTestCase):
    """Two grants are two notices, even on one assignment (PR132-RB3).

    test_the_event_id_is_derived_from_the_grant_and_not_from_the_clock reads ONE grant twice,
    and an identity keyed on the relationship alone passes it just as well. What the grant in
    the identity buys is that a parent handed the same target a second time, through the same
    assignment, is woken a second time rather than folded into a notice it already has.
    """

    def granted_twice(self):
        """A promotion, a return, then a second promotion to the same parent and assignment."""
        first_turn = self.promoted()
        first = self.grant_of(first_turn)
        self.answer_grant(first_turn, PARENT)
        self.turns.release(
            first_turn, actor=PARENT, disposition="returned", reason="handing it back")
        held = self.claim(self.beta, PROJECT_B, "head-b2")
        waiter = self.claim(self.alpha, PROJECT_A, "head-a2")
        self.assertEqual(waiter["state"], "waiting")
        self.turns.release(
            held["turnId"], actor=self.beta.task_id, disposition="returned", reason="done")
        return first, self.grant_of(waiter["turnId"])

    def test_two_grants_on_one_assignment_queue_two_notices(self):
        first, second = self.granted_twice()
        self.assertNotEqual(first["grantId"], second["grantId"])
        self.assertNotEqual(first["wake"]["eventId"], second["wake"]["eventId"])
        queued = {row["event_id"]: json.loads(row["receipt"])["grantId"] for row in self.wakes()}
        self.assertEqual(queued, {first["wake"]["eventId"]: first["grantId"],
                                  second["wake"]["eventId"]: second["grantId"]})

    def test_the_second_grant_reaches_the_parent_as_a_turn_of_its_own(self):
        _first, second = self.granted_twice()
        record = self.attempt(second["wake"]["eventId"])
        self.assertEqual(record["deliveryState"], DISPATCHED)
        _turn_id, text = self.adapter.threads[PARENT].items[-1]
        self.assertIn(second["grantId"], text)

    def test_one_grant_derives_one_event_and_another_grant_another(self):
        one = merge_turn_grant_event_id(self.rid, "mtg-one")
        self.assertEqual(one, merge_turn_grant_event_id(self.rid, "mtg-one"))
        self.assertNotEqual(one, merge_turn_grant_event_id(self.rid, "mtg-two"))

    def test_the_same_grant_queued_again_converges_on_its_one_notice(self):
        turn = self.promoted()
        grant = self.grant_of(turn)
        (row,) = self.wakes()
        self.clock.advance(3600)
        with self.store.transaction() as db:
            channel = self.delivery.grant_channel_in(
                db, relationship_id=self.rid, recipient_task_id=PARENT,
                grant=grant["grantId"], project_key=PROJECT_A)
            self.delivery.queue_grant_in(
                db, event_id=channel["eventId"], relationship_id=self.rid,
                recipient_task_id=PARENT, receipt=row["receipt"], grant=grant["grantId"],
                at=self.clock.iso())
        self.assertEqual(channel["eventId"], row["event_id"])
        self.assertEqual([one["event_id"] for one in self.wakes()], [row["event_id"]])


class AGrantThatNoLongerAppliesOwesNothing(MergeTurnWakeTestCase):
    """Every status field for a grant answers from the grant's own turn (PR132-RB2).

    A grant is answered on its merge turn, never through an acks row, so both the phase and
    the reported state have to be read from there. Reading only the acknowledged case left a
    grant that was replaced, returned or lost reporting an acknowledgement nobody can give.
    """

    def status(self, event):
        (item,) = [one for one in self.delivery.snapshot()["deliveries"]
                   if one["eventId"] == event]
        return item["phase"], item["reported"]

    def delivered(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.assertEqual(self.attempt(event)["deliveryState"], DISPATCHED)
        return turn, event

    def test_a_delivered_current_grant_waits_for_its_acknowledgement(self):
        _turn, event = self.delivered()
        self.assertEqual(self.status(event), ("awaiting_grant_acknowledgement",
                                              "dispatched_awaiting_grant_acknowledgement"))

    def test_a_regranted_notice_reports_what_replaced_it(self):
        turn, event = self.delivered()
        self.turns.declare_ready(turn, actor=PARENT, ready=True, candidate_head="head-a2")
        expected = "superseded:" + MERGE_TURN_REGRANTED
        self.assertEqual(self.status(event), (expected, expected))

    def test_a_turn_returned_unanswered_closes_its_notice(self):
        turn, event = self.delivered()
        self.turns.release(turn, actor=PARENT, disposition="returned", reason="cannot land it")
        expected = "superseded:" + MERGE_TURN_CLOSED
        self.assertEqual(self.status(event), (expected, expected))

    def test_a_grant_answered_and_landed_stays_acknowledged(self):
        # The ordinary end of a grant. Its turn closes because it was used, and reporting the
        # notice as overtaken by that closure would describe the success path as a loss.
        turn, event = self.delivered()
        self.answer_grant(turn, PARENT)
        self.turns.begin_merge(
            turn, actor=PARENT, head_sha="head-a", base_sha="base-0",
            checks=run_checks("head-a"), review=dict(GREEN), required=["dev-gate"])
        landed = self.turns.land(
            turn, actor=PARENT, landed_sha="merge-1", observed_base_sha="base-1",
            evidence="the merge commit is on the base")
        self.assertEqual(landed["released"]["state"], "landed")
        self.assertEqual(self.status(event), ("grant_acknowledged", "grant_acknowledged"))

    def test_a_queued_notice_regranted_before_any_send_is_not_awaiting_one(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.turns.declare_ready(turn, actor=PARENT, ready=True, candidate_head="head-a2")
        expected = "superseded:" + MERGE_TURN_REGRANTED
        self.assertEqual(self.status(event), (expected, expected))

    def test_an_unsettled_send_regranted_meanwhile_is_not_awaiting_evidence(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        # Staged rather than driven through a transport that goes silent: what is under test
        # is the report for a send whose outcome is unknown, not the route that leaves one.
        self.store.db.execute(
            "UPDATE deliveries SET state = ? WHERE event_id = ?", (HELD_UNCERTAIN, event))
        self.turns.declare_ready(turn, actor=PARENT, ready=True, candidate_head="head-a2")
        expected = "superseded:" + MERGE_TURN_REGRANTED
        self.assertEqual(self.status(event), (expected, expected))

    def test_a_notice_whose_turn_is_gone_says_so(self):
        turn, event = self.delivered()
        # A store that lost the turn row: damaged, not a state this package writes.
        self.store.db.execute("DELETE FROM merge_turns WHERE turn_id = ?", (turn,))
        expected = "superseded:" + MERGE_TURN_ABSENT
        self.assertEqual(self.status(event), (expected, expected))

    def test_a_notice_nobody_can_read_says_so(self):
        _turn, event = self.delivered()
        # Hand-edited, the only way this package's own receipt stops reading as a grant.
        self.store.db.execute(
            "UPDATE events SET receipt = ? WHERE event_id = ?", ("not a grant", event))
        expected = "superseded:" + MERGE_TURN_GRANT_UNREADABLE
        self.assertEqual(self.status(event), (expected, expected))

    def test_a_notice_answered_before_it_was_sent_is_acknowledged(self):
        # A promoted parent that comes back reads its own claims and can answer the grant
        # before the daemon has sent anything. Nothing is owed after that, send included.
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.answer_grant(turn, PARENT)
        self.assertEqual(self.status(event), ("grant_acknowledged", "grant_acknowledged"))

    def test_an_unsettled_send_answered_meanwhile_is_acknowledged(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.store.db.execute(
            "UPDATE deliveries SET state = ? WHERE event_id = ?", (HELD_UNCERTAIN, event))
        self.answer_grant(turn, PARENT)
        self.assertEqual(self.status(event), ("grant_acknowledged", "grant_acknowledged"))

    def test_an_answered_notice_the_send_path_suppressed_stays_acknowledged(self):
        # The usual route to a suppressed grant: the parent answered first, and the next
        # attempt declines to send. The row keeps its raw state and hold for reconciliation;
        # what an operator reads is still that the grant was acknowledged.
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.answer_grant(turn, PARENT)
        self.assertEqual(self.attempt(event)["sendAttempted"], "no")
        self.assertEqual(self.delivery_row(event)["state"], SUPERSEDED)
        self.assertEqual(self.status(event), ("grant_acknowledged", "grant_acknowledged"))

    def test_a_regranted_notice_the_send_path_suppressed_keeps_its_reason(self):
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.turns.declare_ready(turn, actor=PARENT, ready=True, candidate_head="head-a2")
        self.assertEqual(self.attempt(event)["sendAttempted"], "no")
        self.assertEqual(self.delivery_row(event)["state"], SUPERSEDED)
        expected = "superseded:" + MERGE_TURN_REGRANTED
        self.assertEqual(self.status(event), (expected, expected))


class TheNoticeDeclaresTheRequiredChecks(MergeTurnWakeTestCase):
    """A parent that follows the notice to the letter cannot merge past a red gate (PR132-RB1).

    merge-turn-check stores whatever --required its caller declares, and a command without
    one declares that nothing is required - so a failed dev-gate beside a green optional
    check passed. The notice now proposes the names the candidate's own merge-evidence
    reading recorded, and says plainly when there is no such reading.
    """

    def candidate(self, *, head="head-a", required=("dev-gate",), event=None,
                  submission_no=1, name="candidate", **fields):
        """The child's completion report for the turn's own assignment, as merge-evidence read it.

        Built on this fixture's registered assignment rather than through ready_event, which
        registers another one: the notice reads the reading recorded for the turn it grants.
        """
        if event is None:
            payload = self.ready_payload(self.relationship, [self.artifact(name + ".txt", name)])
            self.accept(payload)
            event = payload["eventId"]
        stated = {"repository": REPO, "base_ref": BASE, "head_sha": head}
        if required is not None:
            names = list(required)
            green = [{"runId": "run-" + str(n), "name": one, "headSha": head,
                      "conclusion": "success", "attempt": 1}
                     for n, one in enumerate(names or ["dev-gate"])]
            stated["handoff"] = a_handoff(head, requiredDeclared=names, checks=green)
        stated.update(fields)
        stored = report.record(self.store, self.clock, event_id=event,
                               submission_no=submission_no, **a_report(**stated))
        self.assertEqual(stored["submissionNo"], submission_no)
        return event

    def notice(self):
        """The bytes the promoted parent actually received on its own thread."""
        turn = self.promoted()
        event = self.wakes()[0]["event_id"]
        self.assertEqual(self.attempt(event)["deliveryState"], DISPATCHED)
        _turn_id, text = self.adapter.threads[PARENT].items[-1]
        return turn, event, text

    @staticmethod
    def line(text, verb):
        (found,) = [one.strip() for one in text.splitlines()
                    if one.strip().startswith(verb + " ")]
        return found

    def parsed(self, text, verb, *fills):
        """One command from the notice, its placeholders filled, parsed by the real CLI."""
        line = self.line(text, verb)
        for placeholder, value in fills:
            self.assertIn(placeholder, line)
            line = line.replace(placeholder, shlex.quote(value), 1)
        try:
            return cli.build_parser().parse_args(shlex.split(line))
        except SystemExit as refused:
            # argparse exits rather than raising. A command the notice hands over that the CLI
            # will not even parse is the defect under test, so it is reported as one.
            self.fail(f"the CLI refused the notice's command (exit {refused.code}): {line}")

    def invoke(self, args):
        return args.handler(types.SimpleNamespace(merge_turn=self.turns), args)

    def check_command(self, text, dev_gate):
        checks = [{"runId": "run-dev", "name": "dev-gate", "headSha": "head-a",
                   "conclusion": dev_gate, "attempt": 1},
                  {"runId": "run-lint", "name": "optional-lint", "headSha": "head-a",
                   "conclusion": "success", "attempt": 1}]
        return self.parsed(
            text, "merge-turn-check", ("<your task id>", PARENT), ("<head>", "head-a"),
            ("<base>", "base-0"), ("<json>", json.dumps(checks)), ("<json>", json.dumps(GREEN)))

    def assertNotRecorded(self, text, reason):
        self.assertIn("requiredDeclared: not recorded (", text)
        self.assertIn(reason, text)
        self.assertIn(" --required=<", self.line(text, "merge-turn-check"))
        self.assertIn("merge-evidence --repository " + REPO, text)

    # ------------------------------------------------------- a recorded reading

    def test_a_verbatim_parent_cannot_merge_past_a_failed_required_check(self):
        self.candidate()
        turn, _event, text = self.notice()
        self.assertIn('requiredDeclared: ["dev-gate"]', text)
        self.invoke(self.parsed(text, "merge-turn-acknowledge", ("<your task id>", PARENT),
                                ("<what you read>", "read the notice and the candidate")))
        red = self.check_command(text, "failure")
        self.assertEqual(red.required, ["dev-gate"])
        with self.assertRaises(CoordinationError) as caught:
            self.invoke(red)
        self.assertEqual(caught.exception.reason, RefusalReason.MERGE_CURRENCY_STALE)
        self.assertIn("dev-gate", caught.exception.detail)
        self.assertEqual(self.turns.turn(turn)["state"], "holding")
        # The same command with the gate green reaches merging, so the refusal above was the
        # gate's and not something else about the candidate.
        answer = self.invoke(self.check_command(text, "success"))
        self.assertEqual(answer["state"], "merging")
        self.assertEqual(answer["requiredDeclared"], ["dev-gate"])

    def test_the_preview_proposes_the_same_names(self):
        self.candidate()
        self.promoted()
        preview = self.delivery.preview_message(self.wakes()[0]["event_id"])
        self.assertTrue(self.line(preview, "merge-turn-check").endswith(" --required=dev-gate"))

    def test_names_that_need_quoting_survive_the_round_trip(self):
        # A space, a quote and a leading dash are all legal in a check name.
        names = ["--check", "build linux", "dev-gate", "lint'; echo x"]
        self.candidate(required=names)
        _turn, _event, text = self.notice()
        self.assertEqual(self.check_command(text, "success").required, sorted(names))
        self.assertIn("requiredDeclared: " + json.dumps(sorted(names)), text)

    def test_the_current_submission_decides_not_an_earlier_one(self):
        event = self.candidate(head="head-old", required=("old-gate",))
        self.candidate(head="head-a", required=("dev-gate",), event=event, submission_no=2)
        _turn, _event, text = self.notice()
        self.assertEqual(self.check_command(text, "success").required, ["dev-gate"])
        self.assertNotIn("old-gate", text)

    def test_a_reading_that_found_nothing_required_says_so(self):
        self.candidate(required=())
        _turn, _event, text = self.notice()
        self.assertIn("requiredDeclared: none", text)
        self.assertIsNone(self.check_command(text, "success").required)

    # ------------------------------------------------------------- no reading

    def test_no_report_leaves_the_required_set_for_the_parent_to_read(self):
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "no work report")

    def test_a_report_without_a_handoff_is_not_a_reading(self):
        self.candidate(required=None, pr_number=None, pr_url=None, pr_state=None, handoff=None)
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "no merge-readiness handoff")

    def test_a_reading_about_another_head_is_not_this_candidates(self):
        self.candidate(head="head-old")
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "head-old")

    def test_a_reading_about_another_repository_is_not_this_targets(self):
        self.candidate(repository="owner/other")
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "another repository")

    def test_a_reading_about_another_base_is_not_this_targets(self):
        self.candidate(base_ref="main")
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "another base")

    def test_a_reading_that_names_no_base_is_not_this_targets(self):
        # Required checks belong to a branch. An empty set read against no named branch is not
        # a statement that this target requires nothing.
        self.candidate(base_ref=None, required=())
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "records no base")

    def test_a_current_report_without_a_reading_beside_one_with_is_not_a_reading(self):
        self.candidate(required=(), name="one")
        self.candidate(required=None, pr_number=None, pr_url=None, pr_state=None, handoff=None,
                       name="two")
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "no merge-readiness handoff")

    def test_two_current_readings_that_disagree_propose_neither(self):
        self.candidate(required=("dev-gate",), name="one")
        self.candidate(required=("other-gate",), name="two")
        _turn, _event, text = self.notice()
        self.assertNotRecorded(text, "disagree")
