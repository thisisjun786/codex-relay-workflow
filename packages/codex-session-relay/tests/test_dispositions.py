"""CRW-163: can a coordinator LIST the children that stopped, and was anyone actually told.

Every case here is written from the coordinator's side. The question is never whether a field
exists, but whether somebody holding only this answer would reach a true conclusion: that a blocked
child is blocked, that an unreadable store is not an empty one, and that nothing here claims a
recipient saw anything the store cannot show it saw.
"""

import json
import os
import subprocess
import sys
import unittest

from codex_session_relay import cxc, dispositions, report
from codex_session_relay.delivery import EXECUTION_ONLY_OUTCOMES
from codex_session_relay.models import TurnRef
from codex_session_relay.receipts import OUTCOMES
from codex_session_relay.store import resolve_state_dir

from .support import CHILD, DISPATCH_TURN, PARENT, DeliveryTestCase
from .test_cli import REPO, CliBase

PROJECT = "PROJ-CRW-163"


def a_row(**overrides) -> dict:
    """One flat row in the shape the single statement returns, defaulting to an empty child."""
    row = {
        "kind": "child", "store_id": None,
        "relationship_id": "rel-1", "issue_key": "CRW-1", "relationship_status": "active",
        "parent_task_id": PARENT, "child_task_id": CHILD, "execution_generation": 1,
        "project_key": PROJECT, "reviewable_count": 0, "earlier_events": 0,
        "event_id": None, "outcome": None, "producer": None, "stage": None,
        "suppressed_reason": None, "turn_id": None, "attempt": None,
        "became_final_at": None, "ordering_at": None,
        "report_submission": None, "cxc_status": None, "cxc_reason": None, "next_action": None,
        "delivery_event": None, "delivery_state": None, "delivery_kind": None,
        "recipient_task_id": None, "attempt_count": None, "hold_reason": None,
        "dispatch_turn_id": None, "dispatch_evidence": None,
        "intent_event": None, "intent_attempts": None, "intent_error": None,
        "intent_next_retry_at": None,
        "supersession_reason": None, "supersession_applied": None,
        "request_id": None, "attempt_no": None, "attempt_internal_state": None,
        "attempt_state": None, "attempt_sent_at": None,
        "ack_event": None, "ack_accepted": None, "ack_rejection": None, "ack_verified": None,
        "ack_evidence_event": None, "ack_tier": None,
    }
    row.update(overrides)
    return row


def an_event(event_id, outcome, **overrides) -> dict:
    base = {
        "event_id": event_id, "outcome": outcome, "producer": "child", "stage": "final",
        "turn_id": DISPATCH_TURN, "attempt": 1, "became_final_at": "2026-09-21T00:00:00Z",
        "ordering_at": "2026-09-21T00:00:00Z",
    }
    base.update(overrides)
    return a_row(**base)


def derived(rows):
    return dispositions.derive(rows, selector={"projectKey": PROJECT})


def only_child(rows):
    answer = derived(rows)
    return answer["children"][0]


def only_event(rows):
    return only_child(rows)["events"][0]


class TheTurnDisposition(unittest.TestCase):
    """Which disposition is current, and what happens when the store holds two answers."""

    def test_a_child_that_reported_nothing_is_not_a_child_that_is_fine(self):
        child = only_child([a_row()])
        self.assertIsNone(child["turnDisposition"]["outcome"])
        self.assertEqual(child["turnDisposition"]["basis"], "none")
        self.assertIn("not", child["turnDisposition"]["detail"],
                      "the absence has to say what it is not")
        self.assertEqual(child["events"], [])

    def test_a_single_blocked_event_is_the_disposition(self):
        child = only_child([an_event("e1", "blocked_needs_input")])
        self.assertEqual(child["turnDisposition"]["outcome"], "blocked_needs_input")
        self.assertEqual(child["turnDisposition"]["eventId"], "e1")
        self.assertEqual(child["turnDisposition"]["basis"], "sole")

    def test_a_reviewable_event_is_counted_and_never_becomes_the_disposition(self):
        """The store treats an execution-only event as a different kind of fact, not a rival.

        A reviewable receipt beside a blocked one is not a contest, so the reviewable axis is
        listed separately and this reader never names a head for it.
        """
        child = only_child([an_event("r1", "ready_for_review", reviewable_count=1)])
        self.assertEqual(child["turnDisposition"]["basis"], "none")
        self.assertEqual(child["reviewable"]["eventIds"], ["r1"])
        self.assertEqual(child["reviewable"]["eventsInGeneration"], 1)
        self.assertEqual(child["reviewable"]["head"], "not_derived_here")
        self.assertIn("assignment-show", child["reviewable"]["readWith"])
        self.assertEqual([e["eventId"] for e in child["events"]], ["r1"],
                         "the event itself still travels, so lastEvent can be built from it")

    def test_several_reviewable_events_are_all_listed_and_none_is_called_the_head(self):
        child = only_child([
            an_event("r1", "ready_for_review", reviewable_count=2),
            an_event("r2", "ready_for_review", reviewable_count=2),
        ])
        self.assertEqual(sorted(child["reviewable"]["eventIds"]), ["r1", "r2"])
        self.assertEqual(child["reviewable"]["head"], "not_derived_here")

    def test_two_events_with_the_same_outcome_anchor_to_the_newest_and_keep_both(self):
        child = only_child([
            an_event("e1", "blocked_needs_input", ordering_at="2026-09-21T00:00:00Z"),
            an_event("e2", "blocked_needs_input", ordering_at="2026-09-21T01:00:00Z"),
        ])
        self.assertEqual(child["turnDisposition"]["eventId"], "e2")
        self.assertEqual(child["turnDisposition"]["basis"], "latest_of_same_outcome")
        self.assertEqual(child["turnDisposition"]["candidates"], ["e1", "e2"])

    def test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer(self):
        """Arrival order decides nothing here, so a later timestamp does not win the question."""
        for label, later in (("equal", "2026-09-21T00:00:00Z"), ("newer", "2026-09-21T02:00:00Z")):
            with self.subTest(label):
                child = only_child([
                    an_event("e1", "blocked_needs_input", ordering_at="2026-09-21T00:00:00Z"),
                    an_event("e2", "failed", ordering_at=later),
                ])
                self.assertIsNone(child["turnDisposition"]["outcome"])
                self.assertEqual(child["turnDisposition"]["basis"], "contested")
                self.assertEqual(child["turnDisposition"]["candidates"], ["e1", "e2"])

    def test_a_suppressed_claim_is_listed_and_never_chosen(self):
        child = only_child([
            an_event("e1", "blocked_needs_input", stage="suppressed",
                     suppressed_reason="the turn ended failed, so the staged claim is not promoted"),
        ])
        self.assertEqual(child["turnDisposition"]["basis"], "none")
        self.assertEqual(child["events"][0]["suppressedReason"][:3], "the")

    def test_a_staged_claim_is_listed_and_never_chosen(self):
        child = only_child([an_event("e1", "blocked_needs_input", stage="staged")])
        self.assertEqual(child["turnDisposition"]["basis"], "none")
        self.assertEqual(child["events"][0]["stage"], "staged")

    def test_a_directly_final_event_with_no_finalized_at_still_orders(self):
        child = only_child([
            an_event("e1", "failed", became_final_at=None, ordering_at="2026-09-21T00:00:00Z"),
            an_event("e2", "failed", became_final_at=None, ordering_at=None),
        ])
        self.assertEqual(child["turnDisposition"]["eventId"], "e1")
        self.assertEqual(child["turnDisposition"]["candidates"], ["e1", "e2"])

    def test_earlier_generations_are_counted_and_not_listed(self):
        child = only_child([an_event("e1", "blocked_needs_input", earlier_events=2)])
        self.assertEqual(child["earlierGenerationEvents"], 2)
        self.assertEqual(len(child["events"]), 1)


class TheWorkReport(unittest.TestCase):
    """BLOCKED, UNSAFE and NEEDS_HUMAN collapse onto one outcome; only the report separates them."""

    def test_a_recorded_report_carries_the_status_that_separates_them(self):
        event = only_event([an_event(
            "e1", "blocked_needs_input", report_submission=2, cxc_status=cxc.NEEDS_HUMAN,
            cxc_reason="an approval nobody here can grant", next_action="ask the operator",
        )])
        self.assertTrue(event["workReport"]["recorded"])
        self.assertEqual(event["workReport"]["cxcStatus"], cxc.NEEDS_HUMAN)
        self.assertEqual(event["workReport"]["submissionNo"], 2)

    def test_no_report_says_the_separation_is_unavailable_rather_than_guessing(self):
        event = only_event([an_event("e1", "blocked_needs_input")])
        self.assertFalse(event["workReport"]["recorded"])
        self.assertIsNone(event["workReport"]["cxcStatus"])


class TheDeliveryAxis(unittest.TestCase):
    """Whether a send was measured at all, which is the question absence used to swallow."""

    def observation(self, **overrides):
        return only_event([an_event("e1", "blocked_needs_input", **overrides)])["delivery"]

    def test_no_delivery_and_no_intent_is_unmeasured_not_undelivered(self):
        delivery = self.observation()
        self.assertEqual(delivery["observation"], "unmeasured")
        self.assertIn("--no-enqueue", delivery["detail"],
                      "the detail has to name what absence cannot separate")
        self.assertIsNone(delivery["state"])

    def test_an_intent_without_a_delivery_is_a_refusal_that_may_not_last(self):
        delivery = self.observation(intent_event="e1", intent_attempts=3,
                                    intent_error="parent is paused")
        self.assertEqual(delivery["observation"], "refused_pre_queue")
        self.assertTrue(delivery["intent"]["recorded"])
        self.assertEqual(delivery["intent"]["attempts"], 3)
        self.assertEqual(delivery["intent"]["lastError"], "parent is paused")

    def test_a_queued_delivery_is_not_sent_and_carries_the_stores_own_word(self):
        delivery = self.observation(delivery_event="e1", delivery_state="queued",
                                    delivery_kind="completion_event", attempt_count=0)
        self.assertEqual(delivery["observation"], "not_sent")
        self.assertEqual(delivery["state"], "queued")
        self.assertIn("status", delivery["readWith"])

    def test_a_dispatched_delivery_says_dispatched_and_nothing_more(self):
        delivery = self.observation(delivery_event="e1", delivery_state="dispatched",
                                    dispatch_turn_id="turn-parent-9",
                                    dispatch_evidence="transport_accepted")
        self.assertEqual(delivery["observation"], "dispatched")
        self.assertTrue(delivery["dispatchEvidence"])
        self.assertEqual(delivery["dispatchTurnId"], "turn-parent-9")

    def test_an_inbox_only_delivery_is_stored_not_woken(self):
        delivery = self.observation(delivery_event="e1", delivery_state="inbox_only")
        self.assertEqual(delivery["observation"], "stored_not_woken")
        self.assertIn("not a successful wake", delivery["detail"])

    def test_an_uncertain_send_is_not_evidence_of_non_delivery(self):
        for state in ("sending", "held_uncertain"):
            with self.subTest(state):
                delivery = self.observation(delivery_event="e1", delivery_state=state)
                self.assertEqual(delivery["observation"], "send_uncertain")

    def test_a_supersession_note_outranks_the_state_but_never_erases_it(self):
        delivery = self.observation(delivery_event="e1", delivery_state="dispatched",
                                    supersession_reason="a newer generation replaced it",
                                    supersession_applied=1)
        self.assertEqual(delivery["observation"], "superseded")
        self.assertEqual(delivery["state"], "dispatched", "the store word survives the derivation")
        self.assertEqual(delivery["supersession"]["reason"], "a newer generation replaced it")

    def test_a_state_this_reader_has_no_word_for_is_not_folded_into_one(self):
        delivery = self.observation(delivery_event="e1", delivery_state="teleported")
        self.assertEqual(delivery["observation"], "state_unrecognised")
        self.assertIn("teleported", delivery["detail"])

    def test_a_staged_event_is_never_reported_as_delivered(self):
        delivery = only_event([an_event("e1", "blocked_needs_input", stage="staged")])["delivery"]
        self.assertEqual(delivery["observation"], "not_deliverable:staged")
        self.assertIn("only a final event", delivery["detail"])

    def test_a_suppressed_event_is_called_suppressed_rather_than_staged(self):
        """resolve_staged_in writes stage suppressed WITH the reason, so order matters here."""
        delivery = only_event([an_event(
            "e1", "blocked_needs_input", stage="suppressed",
            suppressed_reason="the turn ended failed, so the staged claim is not promoted",
        )])["delivery"]
        self.assertEqual(delivery["observation"], "suppressed")
        self.assertIn("suppressed", delivery["detail"])

    def test_an_acknowledgement_with_no_delivery_row_is_a_disagreement_not_unmeasured(self):
        delivery = self.observation(ack_event="e1", ack_accepted=1, ack_verified="verified")
        self.assertEqual(delivery["observation"], "records_disagree")
        self.assertIn("an acknowledgement", delivery["detail"])


class TheReceivingAxis(unittest.TestCase):
    """A dispatch proves a send was accepted, never that the recipient observed anything."""

    def receiving(self, **overrides):
        return only_event([an_event("e1", "blocked_needs_input", **overrides)])["delivery"]

    def test_a_dispatched_send_still_leaves_the_receiving_side_unmeasured(self):
        delivery = self.receiving(delivery_event="e1", delivery_state="dispatched",
                                  dispatch_turn_id="turn-parent-9")
        self.assertEqual(delivery["observation"], "dispatched")
        self.assertEqual(delivery["recipientObservation"], "unmeasured")
        self.assertIn("not an acknowledgement", delivery["recipientDetail"])

    def test_a_host_read_acknowledgement_is_an_observation(self):
        delivery = self.receiving(delivery_event="e1", delivery_state="acknowledged",
                                  ack_event="e1", ack_accepted=1, ack_verified="verified",
                                  ack_evidence_event="e1", ack_tier="host_read")
        self.assertEqual(delivery["recipientObservation"], "observed_host_read")

    def test_an_unverified_acknowledgement_is_a_claim_rather_than_an_observation(self):
        delivery = self.receiving(delivery_event="e1", delivery_state="dispatched",
                                  ack_event="e1", ack_accepted=1, ack_verified="unverified_turn",
                                  ack_evidence_event="e1", ack_tier="unverified")
        self.assertEqual(delivery["recipientObservation"], "observed_claimed")
        self.assertIn("claim", delivery["recipientDetail"])

    def test_evidence_with_no_acknowledgement_beside_it_has_no_supportable_answer(self):
        delivery = self.receiving(delivery_event="e1", delivery_state="dispatched",
                                  ack_evidence_event="e1", ack_tier="unverified")
        self.assertEqual(delivery["recipientObservation"], "records_disagree")

    def test_the_acknowledgement_block_keeps_every_field_the_assignment_view_reports(self):
        event = only_event([an_event(
            "e1", "blocked_needs_input", delivery_event="e1", delivery_state="dispatched",
            ack_event="e1", ack_accepted=0, ack_rejection="the criteria set moved",
            ack_verified="verified", ack_evidence_event="e1", ack_tier="host_read",
        )])
        acknowledgement = event["acknowledgement"]
        self.assertTrue(acknowledgement["recorded"])
        self.assertFalse(acknowledgement["accepted"])
        self.assertEqual(acknowledgement["rejectionReason"], "the criteria set moved")
        self.assertEqual(acknowledgement["settlement"], "verified")
        self.assertEqual(acknowledgement["evidenceTier"], "host_read")

    def test_no_acknowledgement_row_is_unrecorded_rather_than_unverified(self):
        event = only_event([an_event("e1", "blocked_needs_input")])
        self.assertFalse(event["acknowledgement"]["recorded"])
        self.assertEqual(event["acknowledgement"]["evidenceTier"], "unrecorded")
        self.assertIsNone(event["acknowledgement"]["settlement"])


class TheVocabulary(unittest.TestCase):
    """The words come from the store. A word that drifts from it fails here rather than in the field."""

    def test_the_execution_only_outcomes_are_still_the_stores_own_tuple(self):
        self.assertEqual(dispositions.EXECUTION_ONLY, EXECUTION_ONLY_OUTCOMES)

    def test_the_statement_selects_exactly_the_stores_four_outcomes(self):
        for outcome in OUTCOMES:
            self.assertIn("'" + outcome + "'", dispositions._SQL,
                          "an outcome the store records is missing from the read")

    def test_every_delivery_state_the_transport_defines_has_a_word(self):
        """Enumerated from transport itself, so a state added later fails this test.

        The four subtracted names are receipt statuses rather than delivery states: they describe
        what a transport receipt said, not what a delivery row holds.
        """
        from codex_session_relay import transport

        receipt_statuses = {"ACCEPTED", "FAILED", "OUTCOME_UNKNOWN", "UNFINISHED"}
        states = {
            value for name, value in vars(transport).items()
            if name.isupper() and isinstance(value, str) and name not in receipt_statuses
        }
        self.assertEqual(states, set(dispositions.OBSERVATION_BY_STATE),
                         "a delivery state with no word would land in the wrong one")

    def test_every_word_the_map_produces_has_a_detail_entry(self):
        for word in set(dispositions.OBSERVATION_BY_STATE.values()):
            self.assertIn(word, dispositions.OBSERVATION_DETAIL)


class TheCounts(unittest.TestCase):
    def test_the_counts_cannot_disagree_with_the_list_they_summarise(self):
        answer = derived([
            an_event("e1", "blocked_needs_input"),
            a_row(relationship_id="rel-2", issue_key="CRW-2"),
            an_event("e3", "failed", relationship_id="rel-3", issue_key="CRW-3",
                     delivery_event="e3", delivery_state="dispatched",
                     report_submission=1, cxc_status=cxc.BLOCKED, cxc_reason="waiting on a review"),
        ])
        counts = answer["counts"]
        self.assertEqual(counts["children"], 3)
        self.assertEqual(counts["blocked"], 1)
        self.assertEqual(counts["withExecutionOnlyDisposition"], 2)
        self.assertEqual(counts["deliveryUnmeasured"], 1, "only the one with no delivery record")
        self.assertEqual(counts["recipientUnmeasured"], 2)
        self.assertEqual(counts["workReportMissing"], 1)


class TheReadItself(unittest.TestCase):
    """An empty list and an unreadable store are the two answers this contract refuses to merge."""

    def setUp(self):
        import shutil
        import tempfile

        self.tmp = tempfile.mkdtemp(prefix="relay-dispositions-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_a_directory_with_no_database_is_unreadable_rather_than_empty(self):
        absent = os.path.join(self.tmp, "absent")
        answer = dispositions.read(resolve_state_dir(absent), project_key=PROJECT)
        self.assertFalse(answer["readable"])
        self.assertEqual(answer["children"], [])
        self.assertIsNotNone(answer["detail"])
        self.assertFalse(os.path.exists(absent), "the read created the directory it was asked about")

    def test_a_file_that_is_not_a_relay_database_is_unreadable_and_stays_untouched(self):
        """A legacy or unrelated file answers with the sqlite message, never with no children."""
        state = os.path.join(self.tmp, "borrowed")
        os.makedirs(state)
        target = os.path.join(state, "relay.sqlite3")
        open(target, "w").close()

        answer = dispositions.read(resolve_state_dir(state), project_key=PROJECT)

        self.assertFalse(answer["readable"])
        self.assertEqual(answer["children"], [])
        self.assertIsNotNone(answer["detail"])
        self.assertEqual(os.path.getsize(target), 0, "the read wrote a schema into it")
        self.assertEqual(sorted(os.listdir(state)), ["relay.sqlite3"],
                         "no WAL or shm sidecar either")

    def test_a_replaced_file_is_unreadable_rather_than_attributed_to_the_wrong_store(self):
        """The caller half of the rename case.

        read_only_rows answers a replacement with readable True, no rows and a detail, because the
        device and inode it measured before the read no longer match. Reproducing the race itself
        would need a scheduler inside that function, so this asserts the handling: the reader must
        not read "no rows" as "no children".
        """
        from codex_session_relay import store as store_module

        replaced = {"device": None, "inode": None, "links": None, "readable": True, "rows": [],
                    "detail": "the database was replaced while it was being read"}
        original = store_module.read_only_rows
        store_module.read_only_rows = lambda *a, **k: replaced
        self.addCleanup(setattr, store_module, "read_only_rows", original)

        answer = dispositions.read(resolve_state_dir(self.tmp), project_key=PROJECT)

        self.assertFalse(answer["readable"])
        self.assertEqual(answer["children"], [])
        self.assertEqual(answer["detail"], replaced["detail"])


class AgainstARealStore(DeliveryTestCase):
    """The same questions, against what the relay actually writes."""

    def setUp(self):
        super().setUp()
        self.selection = resolve_state_dir(os.path.join(self.tmp, "state"))

    def scope(self, relationship_id, project_key=PROJECT):
        """The project edge, written raw.

        A scope binding is normally established through the linkage surface with a supervisor and a
        parent tenure. This test needs the edge and not that ceremony, and support.py already writes
        records raw where a stricter path would demand more than the case under test.
        """
        self.store.db.execute(
            "INSERT OR REPLACE INTO relationship_scope (relationship_id, project_key, recorded_at)"
            " VALUES (?,?,?)",
            (relationship_id, project_key, self.clock.iso()),
        )
        self.store.db.commit()

    def blocked(self, relationship, *, status=None, staged=False, attempt=1,
                turn_id=DISPATCH_TURN):
        # The turn has to be this generation's anchor: a receipt on any other turn needs an
        # explicit continuation admission, which is a different test's subject.
        turn = self.assigned_turn("inProgress" if staged else "completed", turn=turn_id)
        payload = self.execution_payload(
            relationship, "blocked_needs_input", attempt=attempt, turn=turn)
        self.accept(payload)
        event_id = payload["eventId"]
        if status is not None:
            report.record(
                self.store, self.clock, event_id=event_id,
                repository="thisisjun786/codex-relay-workflow",
                cxc_status=status, cxc_reason="recorded by the child",
                summary="the child stopped and said why",
                next_action="read the reason and decide",
            )
        return event_id

    def read(self, **kwargs):
        return dispositions.read(self.selection, **kwargs)

    def child_for(self, answer, relationship_id):
        for child in answer["children"]:
            if child["relationshipId"] == relationship_id:
                return child
        raise AssertionError(f"{relationship_id} is not in the answer")

    def test_a_blocked_child_is_enumerable_by_project(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.scope(rid)
        event_id = self.blocked(relationship, status=cxc.BLOCKED)

        answer = self.read(project_key=PROJECT)

        self.assertTrue(answer["readable"])
        child = self.child_for(answer, rid)
        self.assertEqual(child["turnDisposition"]["outcome"], "blocked_needs_input")
        self.assertEqual(child["turnDisposition"]["eventId"], event_id)
        self.assertEqual(child["issueKey"], relationship["issueKey"])
        self.assertEqual(child["childTaskId"], CHILD)
        self.assertEqual(child["events"][0]["workReport"]["cxcStatus"], cxc.BLOCKED)
        self.assertEqual(child["events"][0]["delivery"]["observation"], "unmeasured",
                         "nothing enqueued it, and that is not the same as not delivered")
        self.assertIsNotNone(answer["store"]["storeId"])

    def test_the_three_statuses_that_share_one_outcome_stay_separable(self):
        seen = {}
        for index, status in enumerate((cxc.BLOCKED, cxc.UNSAFE, cxc.NEEDS_HUMAN)):
            relationship = self.register(
                issue_key=f"CRW-16300{index}", dispatch_request_id=f"dispatch-{index}",
                dispatch_turn_id=f"turn-dispatch-{index}",
            )
            rid = relationship["relationshipId"]
            self.scope(rid)
            self.blocked(relationship, status=status, attempt=index + 1,
                         turn_id=f"turn-dispatch-{index}")
            seen[rid] = status

        answer = self.read(project_key=PROJECT)

        for rid, status in seen.items():
            child = self.child_for(answer, rid)
            self.assertEqual(child["turnDisposition"]["outcome"], "blocked_needs_input")
            self.assertEqual(child["events"][0]["workReport"]["cxcStatus"], status)
        self.assertEqual(answer["counts"]["blocked"], 3)

    def test_a_child_with_no_work_report_says_the_separation_is_unavailable(self):
        relationship = self.register()
        self.scope(relationship["relationshipId"])
        self.blocked(relationship)

        child = self.child_for(self.read(project_key=PROJECT), relationship["relationshipId"])

        self.assertFalse(child["events"][0]["workReport"]["recorded"])
        self.assertEqual(self.read(project_key=PROJECT)["counts"]["workReportMissing"], 1)

    def test_a_queued_then_dispatched_delivery_is_measured_at_each_step(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.scope(rid)
        event_id = self.blocked(relationship, status=cxc.BLOCKED)

        self.delivery.enqueue(event_id)
        queued = self.child_for(self.read(relationship_id=rid), rid)["events"][0]["delivery"]
        self.assertEqual(queued["observation"], "not_sent")
        self.assertEqual(queued["state"], "queued")

        self.attempt(event_id)
        sent = self.child_for(self.read(relationship_id=rid), rid)["events"][0]["delivery"]
        self.assertEqual(sent["observation"], "dispatched")
        self.assertEqual(sent["recipientObservation"], "unmeasured",
                         "a dispatch is not evidence that the parent observed anything")
        self.assertIsNotNone(sent["currentAttempt"], "the current attempt travels with it")

    def test_a_staged_claim_is_recorded_progress_and_never_delivery(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.scope(rid)
        self.blocked(relationship, staged=True)

        event = self.child_for(self.read(relationship_id=rid), rid)["events"][0]

        self.assertEqual(event["stage"], "staged")
        self.assertEqual(event["delivery"]["observation"], "not_deliverable:staged")

    def test_the_project_selector_hides_what_is_not_live_and_the_relationship_selector_does_not(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.scope(rid)
        self.blocked(relationship, status=cxc.BLOCKED)
        self.store.db.execute(
            "UPDATE relationships SET status = ? WHERE relationship_id = ?", ("archived", rid))
        self.store.db.commit()

        by_project = self.read(project_key=PROJECT)
        by_relationship = self.read(relationship_id=rid)

        self.assertEqual(by_project["children"], [], "an archived assignment is not live work")
        self.assertTrue(by_project["readable"], "and that is still a readable answer")
        child = self.child_for(by_relationship, rid)
        self.assertEqual(child["relationshipStatus"], "archived",
                         "the selector that named it answers about it, and says what it is")

    def test_an_unscoped_assignment_is_absent_by_project_and_present_by_relationship(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.blocked(relationship, status=cxc.BLOCKED)

        self.assertEqual(self.read(project_key=PROJECT)["children"], [])
        self.assertEqual(
            self.child_for(self.read(relationship_id=rid), rid)["projectKey"], None)

    def test_an_event_in_an_earlier_generation_is_counted_not_listed(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.scope(rid)
        self.blocked(relationship, status=cxc.BLOCKED)
        self.store.db.execute(
            "UPDATE relationships SET execution_generation = 2 WHERE relationship_id = ?", (rid,))
        self.store.db.commit()

        child = self.child_for(self.read(relationship_id=rid), rid)

        self.assertEqual(child["events"], [], "the answer is about the current generation")
        self.assertEqual(child["earlierGenerationEvents"], 1, "and never drops the older one")


class TheCommand(DeliveryTestCase):
    """The exit code is the part a polling coordinator reads first."""

    def cli(self, *args, state=None, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli",
             "--state", state or os.path.join(self.tmp, "state"), *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def test_the_command_reads_a_blocked_child_through_a_subprocess(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        payload = self.execution_payload(relationship, "blocked_needs_input")
        self.accept(payload)

        answer = self.cli("dispositions-show", "--relationship", rid)

        self.assertTrue(answer["readable"])
        self.assertEqual(answer["children"][0]["turnDisposition"]["outcome"],
                         "blocked_needs_input")
        self.assertIn("unreadable", answer["limits"])

    def test_an_unreadable_store_refuses_rather_than_reporting_nobody(self):
        """doctor keeps exit 0 for an unreadable database; this command must not.

        A coordinator asking which children are blocked and checking only the exit code would read
        an unreadable store as "nobody is blocked", which is the merge this whole contract refuses.
        """
        absent = os.path.join(self.tmp, "absent")

        answer = self.cli("dispositions-show", "--project", PROJECT, state=absent, expect=2)

        self.assertFalse(answer["readable"])
        self.assertIsNotNone(answer["detail"])
        self.assertEqual(answer["children"], [])
        self.assertFalse(os.path.exists(absent), "the command created the directory")

    def test_the_command_turns_no_unrelated_file_into_a_relay_database(self):
        state = os.path.join(self.tmp, "borrowed")
        os.makedirs(state)
        target = os.path.join(state, "relay.sqlite3")
        open(target, "w").close()

        self.cli("dispositions-show", "--project", PROJECT, state=state, expect=2)

        self.assertEqual(os.path.getsize(target), 0, "the command wrote a schema into it")
        self.assertEqual(sorted(os.listdir(state)), ["relay.sqlite3"])

    def test_the_two_selectors_are_mutually_exclusive_and_one_is_required(self):
        for args in ((), ("--project", PROJECT, "--relationship", "rel-1")):
            with self.subTest(args=args):
                environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
                completed = subprocess.run(
                    [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp,
                     "dispositions-show", *args],
                    capture_output=True, text=True, env=environment, timeout=60,
                )
                self.assertEqual(completed.returncode, 2, completed.stderr)

    def test_the_command_is_listed_as_offline_because_it_opens_no_adapter(self):
        from codex_session_relay.cli import OFFLINE_COMMANDS

        self.assertIn("dispositions-show", OFFLINE_COMMANDS)


if __name__ == "__main__":
    unittest.main()

