"""CRW-148: what the level above is told, what it is still owed, and what nobody proved.

Every case runs against a real Store and the real synchronisation outbox. A format fixture
would prove that a message can be shaped; none of these claim a supervisor received anything,
because no row in this store could say so.
"""

import json
import os

from codex_session_relay import cxc, envelope, identity, report, supervision
from codex_session_relay.omitted import SCHEMA as OBSERVATION_SCHEMA
from codex_session_relay.store import Store
from codex_session_relay.sync import (
    CONFIRMED,
    FAILED,
    MAX_ATTEMPTS,
    PENDING,
    SyncOutbox,
    render_block,
)

from .support import CHILD, PARENT, DeliveryTestCase
from .test_report_contract import a_report

DOC = "https://linear.app/example/document/coordination-000000000000"
SUPERVISOR = "01supervisor-task"


class ReportingTestCase(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        self.sync = SyncOutbox(self.store, self.clock)
        self.ack.sync = self.sync

    def reported(self, *, recipients=None, **overrides):
        """A queued event with a work report on it, which is the ordinary completion shape."""
        _relationship, event_id = self.queued_event(recipients=recipients)
        report.record(self.store, self.clock, event_id=event_id, **a_report(**overrides))
        return event_id

    def obligation_for(self, event_id):
        return supervision.from_event(
            self.store, event_id, report.read(self.store, event_id))

    def halted(self, status, reason):
        """A run that stopped rather than delivering, which the contract pairs with this outcome.

        A blocked or needs-human report cannot ride a ready_for_review event: cxc.check_status
        refuses the pair, and rightly, because a report saying nobody can proceed alongside an
        outcome saying the work is reviewable is two claims that contradict each other.
        """
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        payload = self.execution_payload(relationship, "blocked_needs_input")
        self.accept(payload)
        report.record(self.store, self.clock, event_id=payload["eventId"],
                      **a_report(cxc_status=status, cxc_reason=reason, pr_number=None,
                                 pr_url=None, pr_state=None, handoff=None))
        return payload["eventId"]

    def lifecycle(self, task_id, deliverable, reason=None):
        self.store.db.execute(
            "INSERT OR REPLACE INTO recipient_lifecycle (task_id, runtime_status, archived,"
            " goal_status, can_accept_input, deliverable, withhold_reason, detail, observed_at)"
            " VALUES (?,?,?,?,?,?,?,?,?)",
            (task_id, "idle", 0, None, 1, deliverable, reason, "", self.clock.iso()),
        )

    def sync_job(self, event_id):
        row = self.store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
        self.sync.set_target(self._rid, "coordination_document", DOC)
        with self.store.transaction() as db:
            return self.sync.enqueue_in(
                db, relationship_id=self._rid, issue_key="REL-1", subject_kind="verdict",
                summary="the child reported its work is ready", event_id=event_id,
                generation=row["execution_generation"], revision=row["revision_hash"],
                verdict="verified")


class WhatIsNewsForTheLevelAbove(ReportingTestCase):
    def test_a_completion_raises_an_obligation(self):
        one = self.obligation_for(self.reported())
        self.assertEqual(one["kind"], supervision.COMPLETION)
        self.assertEqual(one["basis"]["cxcStatus"], cxc.DONE)

    def test_a_block_and_a_user_decision_are_not_the_same_news(self):
        blocked = self.obligation_for(
            self.halted(cxc.BLOCKED, "the upstream package has not landed"))
        self.assertEqual(blocked["kind"], supervision.BLOCKED)

    def test_a_judgment_only_the_user_can_make_is_a_decision(self):
        one = self.obligation_for(
            self.halted(cxc.NEEDS_HUMAN, "two readings of the criterion are defensible"))
        self.assertEqual(one["kind"], supervision.DECISION)

    def test_an_execution_that_failed_is_the_parents_business(self):
        """How a turn ended is work for the parent, not news for the level above.

        Re-dispatching a failed turn is ordinary. Treating it as a supervisor wake would put a
        turn on the busiest level for something the level below handles without help.
        """
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        payload = self.execution_payload(relationship, "failed")
        self.accept(payload)
        self.assertIsNone(self.obligation_for(payload["eventId"]))

    def test_an_acknowledgement_does_not_add_news_of_its_own(self):
        """An ACK-only round trip must not produce a second obligation.

        The same event is read before and after the parent acknowledges it. One fact, one
        obligation, one id - whatever else happened to the delivery in between.
        """
        event_id = self.reported()
        before = self.obligation_for(event_id)
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(event_id, ack_turn_id=turn.turn_id,
                             ack_proof=identity.ack_proof(event_id, turn.turn_id),
                             accepted=True, adapter=self.adapter)
        after = self.obligation_for(event_id)
        self.assertEqual(before["obligationId"], after["obligationId"])

    def test_the_delivery_seam_answers_no_for_an_ordinary_event(self):
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        payload = self.execution_payload(relationship, "interrupted")
        self.accept(payload)
        decided = self.delivery.supervisor_selection(payload["eventId"])
        self.assertFalse(decided["report"])
        self.assertEqual(decided["reason"], supervision.NOT_NEWS)
        self.assertIsNone(decided["obligationId"])

    def test_a_correction_travelling_down_is_not_news_for_the_level_above(self):
        """A needs-human correction is the parent judging a child, not the user deciding.

        The relay writes the correction event itself on a verdict, and it carries a report with
        a status like any other. Reading that status without asking which way the message was
        going turned an ordinary review round into a decision Jun owed.
        """
        event_id = self.reported(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(event_id, ack_turn_id=turn.turn_id,
                             ack_proof=identity.ack_proof(event_id, turn.turn_id),
                             accepted=True, adapter=self.adapter)
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v1",
            criteria=[{"id": "c1", "verdict": "needs_changes", "note": "the tie rule"}])
        correction = self.store.one(
            "SELECT event_id, producer FROM events WHERE outcome = 'revision_request'")
        self.assertIsNotNone(correction, "the verdict is supposed to open a correction")
        self.assertIsNone(
            supervision.from_event(self.store, correction["event_id"],
                                   report.read(self.store, correction["event_id"])),
            "a message travelling downward raises nothing upward")


class OneFactOneObligation(ReportingTestCase):
    def test_a_second_reading_converges_on_the_same_id(self):
        event_id = self.reported()
        self.assertEqual(self.obligation_for(event_id)["obligationId"],
                         self.obligation_for(event_id)["obligationId"])

    def test_the_same_block_said_twice_is_one_obligation(self):
        """A block re-emitted is not a new block.

        The execution-level event id carries the turn and the attempt, so every re-emission of
        one unresolved block is a different event. Keying the obligation on that made each
        repetition its own wake, which is the noise this issue exists to stop; the issue's
        words are a NEW real block, not the same block explained again.
        """
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        first = self.execution_payload(relationship, "blocked_needs_input", attempt=1)
        self.accept(first)
        report.record(self.store, self.clock, event_id=first["eventId"],
                      **a_report(cxc_status=cxc.BLOCKED, cxc_reason="upstream has not landed",
                                 pr_number=None, pr_url=None, pr_state=None, handoff=None))
        second = self.execution_payload(relationship, "blocked_needs_input", attempt=2)
        self.accept(second)
        report.record(self.store, self.clock, event_id=second["eventId"],
                      **a_report(cxc_status=cxc.BLOCKED, cxc_reason="upstream has not landed",
                                 pr_number=None, pr_url=None, pr_state=None, handoff=None))
        self.assertNotEqual(first["eventId"], second["eventId"], "two events, by construction")
        self.assertEqual(self.obligation_for(first["eventId"])["obligationId"],
                         self.obligation_for(second["eventId"])["obligationId"])

    def test_a_different_cause_in_the_same_generation_is_a_different_block(self):
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        first = self.execution_payload(relationship, "blocked_needs_input", attempt=1)
        self.accept(first)
        report.record(self.store, self.clock, event_id=first["eventId"],
                      **a_report(cxc_status=cxc.BLOCKED, cxc_reason="upstream has not landed",
                                 pr_number=None, pr_url=None, pr_state=None, handoff=None))
        second = self.execution_payload(relationship, "blocked_needs_input", attempt=2)
        self.accept(second)
        report.record(self.store, self.clock, event_id=second["eventId"],
                      **a_report(cxc_status=cxc.BLOCKED,
                                 cxc_reason="the credential this needs was revoked",
                                 pr_number=None, pr_url=None, pr_state=None, handoff=None))
        self.assertNotEqual(self.obligation_for(first["eventId"])["obligationId"],
                            self.obligation_for(second["eventId"])["obligationId"])

    def test_two_blockers_that_share_their_words_are_still_two_blockers(self):
        """The subject hashes prose fields, and prose contains the separator.

        Space-joined, reason "waiting on API" with summary "schema update" hashed the same as
        reason "waiting on" with summary "API schema update", so recording a report for the
        first suppressed the second - a changed blocker read as one already reported.
        """
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        causes = [("waiting on API", "schema update"), ("waiting on", "API schema update")]
        identifiers = []
        for attempt, (reason, summary) in enumerate(causes, start=1):
            payload = self.execution_payload(relationship, "blocked_needs_input",
                                             attempt=attempt)
            self.accept(payload)
            report.record(self.store, self.clock, event_id=payload["eventId"],
                          **a_report(cxc_status=cxc.BLOCKED, cxc_reason=reason,
                                     summary=summary, pr_number=None, pr_url=None,
                                     pr_state=None, handoff=None))
            identifiers.append(self.obligation_for(payload["eventId"])["obligationId"])
        self.assertNotEqual(identifiers[0], identifiers[1])

    def test_the_same_event_survives_a_restart_with_the_same_id_and_still_standing(self):
        """Nothing was remembered between these two readings, which is the whole point."""
        event_id = self.reported()
        first = self.obligation_for(event_id)
        self.store.close()
        self.store = Store(os.path.join(self.tmp, "state", "relay.sqlite3"))
        self.addCleanup(self.store.close)
        second = supervision.from_event(
            self.store, event_id, report.read(self.store, event_id))
        self.assertEqual(first["obligationId"], second["obligationId"])
        self.assertEqual(supervision.select(self.store, second)["standing"],
                         supervision.STANDING)

    def test_a_produced_report_is_recorded_once_and_suppresses_the_next_wake(self):
        event_id = self.reported()
        one = self.obligation_for(event_id)
        self.lifecycle(SUPERVISOR, "yes")
        self.assertTrue(supervision.select(self.store, one, recipient=SUPERVISOR,
                                           now=self.clock.now())["report"])
        first = supervision.record_report(self.store, one, at=self.clock.iso())
        second = supervision.record_report(self.store, one, at=self.clock.iso())
        self.assertTrue(first["recorded"])
        self.assertFalse(second["recorded"], "a second call converges rather than writing again")
        decided = supervision.select(self.store, one, recipient=SUPERVISOR,
                                     now=self.clock.now())
        self.assertFalse(decided["report"])
        self.assertEqual(decided["reason"], supervision.ALREADY_REPORTED)

    def test_a_handover_does_not_change_what_is_owed(self):
        """The obligation is keyed on the relation and the fact, not on who parents it now."""
        event_id = self.reported()
        one = self.obligation_for(event_id)
        self.store.db.execute(
            "UPDATE relationships SET parent_task_id = ? WHERE relationship_id = ?",
            ("01replacement-parent", self._rid))
        self.assertEqual(self.obligation_for(event_id)["obligationId"], one["obligationId"])

    def test_superseding_the_relationship_does_not_retire_what_it_left_owed(self):
        """A real handover registers a successor and supersedes the row it replaces.

        Enumerating only live relationships would drop exactly the obligations a replacement
        owner most needs to see: the ones the outgoing owner never discharged.
        """
        from codex_session_relay.assignment import AssignmentView
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        report.record(self.store, self.clock, event_id=payload["eventId"], **a_report())
        one = self.obligation_for(payload["eventId"])
        self.store.db.execute(
            "UPDATE relationships SET status = 'archived', superseded_by = ?"
            " WHERE relationship_id = ?", ("rel-successor", self._rid))
        answer = supervision.standing_for(self.store, linkage, "CRW")
        self.assertIn(one["obligationId"],
                      [entry["obligationId"] for entry in answer["standing"]])
        self.assertEqual(answer["standing"][0]["relationshipStatus"], "archived")
        del AssignmentView


class WhatDoesNotDischargeIt(ReportingTestCase):
    def test_a_report_nobody_wrote_to_linear_leaves_the_obligation_standing(self):
        event_id = self.reported()
        one = self.obligation_for(event_id)
        supervision.record_report(self.store, one, at=self.clock.iso())
        decided = supervision.select(self.store, one)
        self.assertFalse(decided["report"], "the duplicate is suppressed")
        self.assertEqual(decided["standing"], supervision.STANDING,
                         "and what is owed is still owed")
        self.assertIn("no coordination_document target is configured",
                      decided["dischargeReason"],
                      "with no target there is no record the supervisor reads at all")

    def test_a_failed_linear_write_is_not_a_finished_one(self):
        """SyncOutbox.fail looks terminal and means the opposite of done."""
        event_id = self.reported()
        identifier = self.sync_job(event_id)
        claim = self.sync.claim(identifier, owner="test")
        self.sync.fail(identifier, claim_token=claim["claimToken"], error="linear said no")
        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING)
        self.assertIn(PENDING, decided["dischargeReason"])

    def test_only_a_confirmed_record_discharges_it(self):
        event_id = self.reported()
        identifier = self.sync_job(event_id)
        claim = self.sync.claim(identifier, owner="test")
        row = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,))
        self.sync.complete(identifier, claim_token=claim["claimToken"], target_ref=DOC,
                           readback=render_block(row), external_ref="linear-doc-1")
        self.assertEqual(
            self.store.one("SELECT state FROM sync_outbox WHERE sync_id = ?",
                           (identifier,))["state"], CONFIRMED)
        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.DISCHARGED)
        self.assertEqual(decided["reason"], supervision.ALREADY_RECORDED)

    def test_a_confirmed_progress_note_is_not_the_outcome_being_reported(self):
        """A progress row is a real record about the same event and is different news."""
        event_id = self.reported()
        self.sync.set_target(self._rid, "coordination_document", DOC)
        row = self.store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
        with self.store.transaction() as db:
            identifier = self.sync.enqueue_in(
                db, relationship_id=self._rid, issue_key="REL-1", subject_kind="progress",
                summary="the checks have started", event_id=event_id,
                generation=row["execution_generation"], revision=row["revision_hash"])
        claim = self.sync.claim(identifier, owner="test")
        stored = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,))
        self.sync.complete(identifier, claim_token=claim["claimToken"], target_ref=DOC,
                           readback=render_block(stored), external_ref="linear-doc-1")
        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING)

    def test_a_confirmation_in_a_document_nobody_reads_now_does_not_discharge_it(self):
        event_id = self.reported()
        identifier = self.sync_job(event_id)
        claim = self.sync.claim(identifier, owner="test")
        row = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,))
        self.sync.complete(identifier, claim_token=claim["claimToken"], target_ref=DOC,
                           readback=render_block(row), external_ref="linear-doc-1")
        self.sync.set_target(self._rid, "coordination_document", DOC.replace("000000", "111111"))
        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING)
        self.assertIn("no longer the target", decided["dischargeReason"])


class APausedSupervisorIsNotAFailure(ReportingTestCase):
    def test_an_uncontactable_recipient_keeps_the_obligation_and_is_not_woken(self):
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "no", reason="recipient_paused")
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient=SUPERVISOR)
        self.assertFalse(decided["report"])
        self.assertEqual(decided["reason"], supervision.NO_CONTACT)
        self.assertEqual(decided["standing"], supervision.STANDING)

    def test_an_unobserved_recipient_is_unmeasured_rather_than_reachable(self):
        event_id = self.reported()
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient="01never-observed")
        self.assertIsNone(decided["recipient"]["contactable"])
        self.assertIn("unmeasured", decided["recipient"]["reason"])
        self.assertFalse(decided["report"], "deliverability needs positive evidence")
        self.assertEqual(decided["reason"], supervision.CONTACT_UNMEASURED)

    def test_an_observed_and_reachable_supervisor_is_the_only_way_to_a_wake(self):
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient=SUPERVISOR, now=self.clock.now())
        self.assertTrue(decided["report"])
        self.assertEqual(decided["reason"], supervision.REPORTABLE)

    def test_an_observation_dated_in_the_future_is_not_evidence_about_now(self):
        """A clock that went backwards must not authorize a wake.

        A negative age passed the freshness window by arithmetic, so lifecycle evidence
        nobody currently holds read as current.
        """
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        past = self.clock.now() - supervision.CONTACT_FUTURE_TOLERANCE - 600
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient=SUPERVISOR, now=past)
        self.assertFalse(decided["report"])
        self.assertEqual(decided["reason"], supervision.CONTACT_UNMEASURED)
        self.assertIn("in the future", decided["recipient"]["reason"])

    def test_a_small_disagreement_between_two_clocks_is_tolerated(self):
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient=SUPERVISOR, now=self.clock.now() - 5)
        self.assertTrue(decided["report"])

    def test_asking_without_a_recipient_is_not_the_same_as_finding_none(self):
        """The recipient-less mode is documented, and it used to suppress everything.

        A project enumeration reads without a recipient by design, so answering that as
        unmeasured deliverability reported that nothing in any project was worth telling
        anybody.
        """
        event_id = self.reported()
        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertTrue(decided["report"])
        self.assertEqual(decided["reason"], supervision.NOT_ASKED)
        self.assertIs(decided["recipient"]["asked"], False)
        self.assertIsNone(decided["recipient"]["contactable"],
                          "and it still does not claim anybody is reachable")
        named = supervision.select(self.store, self.obligation_for(event_id),
                                   recipient="01never-observed")
        self.assertFalse(named["report"], "a recipient that WAS named still has to be current")
        self.assertEqual(named["reason"], supervision.CONTACT_UNMEASURED)

    def test_an_observation_nobody_dated_against_a_clock_is_unmeasured(self):
        """A stored yes is a fact about the moment somebody looked."""
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient=SUPERVISOR)
        self.assertFalse(decided["report"])
        self.assertEqual(decided["reason"], supervision.CONTACT_UNMEASURED)
        self.assertIn("no clock was supplied", decided["recipient"]["reason"])

    def test_an_observation_older_than_the_window_does_not_authorize_a_wake(self):
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        self.clock.advance(supervision.CONTACT_FRESH_FOR + 60)
        decided = supervision.select(self.store, self.obligation_for(event_id),
                                     recipient=SUPERVISOR, now=self.clock.now())
        self.assertFalse(decided["report"])
        self.assertEqual(decided["reason"], supervision.CONTACT_UNMEASURED)
        self.assertIn("past the", decided["recipient"]["reason"])

    def test_the_delivery_seam_can_actually_reach_its_reportable_answer(self):
        """Its clock is its own, and without passing it the branch was unreachable.

        Every selection through this method answered contactability unmeasured whatever the
        host had been observed to be, so no caller of the seam could ever be told to report.
        """
        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        decided = self.delivery.supervisor_selection(event_id, recipient=SUPERVISOR)
        self.assertTrue(decided["report"])
        self.assertEqual(decided["reason"], supervision.REPORTABLE)
        self.clock.advance(supervision.CONTACT_FRESH_FOR + 60)
        stale = self.delivery.supervisor_selection(event_id, recipient=SUPERVISOR)
        self.assertFalse(stale["report"], "and it still measures the observation's age")


class TheReportNobodyWrote(ReportingTestCase):
    """The counterexample: a child that simply did not report.

    It leaves no events row, so nothing keyed on events can see it. The reading comes from
    CRW-180's observer, and what is owed because of it is decided here.
    """

    def observation(self, state, **overrides):
        record = {"schema": OBSERVATION_SCHEMA, "reportingState": state,
                  "reason": "terminal_without_report", "relationshipId": "rel-0123456789abcdef",
                  "executionGeneration": 1, "selectors": {"turn": "turn-7"}}
        record.update(overrides)
        return record

    def test_an_admitted_turn_that_ended_without_a_report_still_owes_one(self):
        one = supervision.from_observation(self.observation("unreported"))
        self.assertEqual(one["kind"], supervision.UNREPORTED)
        self.assertEqual(one["subject"], "turn-7")

    def test_the_states_that_said_the_opposite_raise_nothing(self):
        for state in ("reported", "in_progress", "unmanaged"):
            self.assertIsNone(supervision.from_observation(self.observation(state)),
                              f"{state} is not an omission")

    def test_a_reading_that_established_nothing_is_a_gap_and_not_an_obligation(self):
        unmeasured = self.observation("unmeasured", reason="marker_unreadable")
        self.assertIsNone(supervision.from_observation(unmeasured))
        gap = supervision.unmeasured_gap(unmeasured)
        self.assertEqual(gap["gap"], "reporting_unmeasured")
        self.assertEqual(gap["reason"], "marker_unreadable")

    def test_the_project_answer_carries_the_omission_and_the_gap_it_was_given(self):
        """The reading is passed in, because no query over this store could find it.

        A turn that ended without reporting writes no events row. Enumerating events would
        answer that a project owes nothing precisely when a child went quiet, which is the
        counterexample CRW-148 asks to be checked rather than assumed.
        """
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        readings = [self.observation("unreported", relationshipId=self._rid),
                    self.observation("unmeasured", relationshipId=self._rid,
                                     reason="marker_unreadable")]
        answer = supervision.standing_for(self.store, linkage, "CRW", observations=readings)
        self.assertEqual([entry["kind"] for entry in answer["standing"]],
                         [supervision.UNREPORTED])
        self.assertEqual([gap["gap"] for gap in answer["gaps"]], ["reporting_unmeasured"])
        self.assertIn("passed in", answer["limits"])

    def test_one_unusable_reading_does_not_decide_what_the_project_owes(self):
        """A malformed entry used to abort the answer instead of being refused.

        relationshipId as a list is truthy, so it passed every presence check and then reached
        a dict membership test that raises on an unhashable key. One bad file in a caller's
        list therefore took down the whole standing query, and a caller that caught the error
        would have learned nothing about the obligations that ARE standing.
        """
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        report.record(self.store, self.clock, event_id=payload["eventId"], **a_report())
        real = self.obligation_for(payload["eventId"])

        readings = [
            self.observation("unreported", relationshipId=[self._rid]),
            self.observation("unreported", relationshipId=self._rid,
                             selectors={"turn": ["turn-7"]}),
            {"schema": "something-else/1", "relationshipId": self._rid},
            "not an object at all",
        ]
        answer = supervision.standing_for(self.store, linkage, "CRW", observations=readings)
        self.assertEqual([entry["obligationId"] for entry in answer["standing"]],
                         [real["obligationId"]],
                         "the obligations that are standing are still answered")
        self.assertEqual([gap["gap"] for gap in answer["gaps"]],
                         ["reading_unusable"] * 4,
                         "and every reading that could not be placed is named rather than"
                         " dropped, including the one that is not an object: a reading whose"
                         " scope cannot be read is not evidence that it belonged elsewhere")
        self.assertEqual(
            sorted({gap["relationId"] for gap in answer["gaps"]}, key=str),
            sorted({None, self._rid}, key=str),
            "the ones that named a readable scope carry it, and the ones that did not say so")

    def test_an_unusable_reading_is_named_rather_than_raised(self):
        for reading in ([], "text", {"schema": "reporting-observation/1"}, None):
            gap = supervision.unusable_reading(reading)
            self.assertEqual(gap["gap"], "reading_unusable")
            self.assertTrue(gap["reason"])

    def test_a_list_where_a_name_belongs_raises_no_obligation(self):
        self.assertIsNone(supervision.from_observation(
            self.observation("unreported", relationshipId=["rel-1"])))
        self.assertIsNone(supervision.from_observation(
            self.observation("unreported", selectors={"turn": ["turn-7"]})))
        self.assertIsNone(supervision.from_observation(
            self.observation("unreported", selectors={"turn": "   "})))

    def test_a_reading_about_another_project_is_not_this_project_s_business(self):
        """A caller's list must not decide what a project owes."""
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        elsewhere = self.observation("unreported", relationshipId="rel-somewhere-else")
        answer = supervision.standing_for(self.store, linkage, "CRW",
                                          observations=[elsewhere, elsewhere])
        self.assertEqual(answer["standing"], [])
        self.assertEqual(answer["gaps"], [],
                         "not this project's business means nothing about it enters this"
                         " project's answer, gaps included")

    def test_the_same_reading_twice_is_still_one_obligation(self):
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        reading = self.observation("unreported", relationshipId=self._rid)
        answer = supervision.standing_for(self.store, linkage, "CRW",
                                          observations=[reading, reading])
        self.assertEqual(len(answer["standing"]), 1)


ABSENT = object()


class AReadingIsPlacedThenInterpreted(ReportingTestCase):
    """CRW-213: every scope shape crossed with every state the observer can answer.

    The defect this closes is that an ordinary reading - one saying the child DID report - came
    back as a gap claiming it named no usable relationship and turn, when it had named both. Two
    independent audit rounds on the fix each found another case a branch-by-branch reading had
    missed, so the proof is the whole cross-product rather than a list of remembered examples:
    a sixth reportingState has to be added to the parameters here, which is a smaller and more
    visible obligation than keeping prose rows in step with the code.
    """

    NOTHING, OBLIGATION, UNMEASURED, UNUSABLE = "nothing", "obligation", "unmeasured", "unusable"
    SETTLED = ("reported", "in_progress", "unmanaged")
    # A foreign schema and an unknown state are the two ways a record can be outside the
    # vocabulary while still being an object with a relationship in it.
    STATES = SETTLED + ("unmeasured", "unreported", "foreign_schema", "something")

    def project(self):
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        self._relationship = self.register(project_key="CRW")
        self._rid = self._relationship["relationshipId"]
        return linkage, self._rid

    def standing_event(self, _rid):
        """One real event-derived obligation, so both loops can be answered in one call."""
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(self._relationship, [path])
        self.accept(payload)
        report.record(self.store, self.clock, event_id=payload["eventId"], **a_report())
        return self.obligation_for(payload["eventId"])

    def reading(self, state, scope=ABSENT, **overrides):
        """One reading, with the relationship key genuinely ABSENT when that is what is meant.

        The existing fixture always supplies a relationshipId, which is why nothing caught that
        omitted.observe answers unmanaged before it has resolved one.
        """
        record = {"schema": OBSERVATION_SCHEMA, "reportingState": state,
                  "reason": "terminal_without_report", "executionGeneration": 1,
                  "selectors": {"turn": "turn-7"}}
        if state == "foreign_schema":
            record.update(schema="something-else/1", reportingState="reported")
        if scope is not ABSENT:
            record["relationshipId"] = scope
        record.update(overrides)
        return record

    def expected(self, kind, state):
        if kind == "foreign":
            return self.NOTHING
        if kind == "malformed":
            return self.UNUSABLE
        if state in self.SETTLED:
            return self.NOTHING
        if state == "unmeasured":
            return self.UNMEASURED
        if state == "unreported":
            return self.OBLIGATION if kind == "ours" else self.UNUSABLE
        return self.UNUSABLE

    def classify(self, answer, scope):
        kinds = [entry["kind"] for entry in answer["standing"]]
        gaps = [gap["gap"] for gap in answer["gaps"]]
        relation = scope if isinstance(scope, str) else None
        if kinds == [] and gaps == []:
            return self.NOTHING, None
        if kinds == [supervision.UNREPORTED] and gaps == []:
            return self.OBLIGATION, None
        if kinds == [] and gaps == ["reporting_unmeasured"]:
            return self.UNMEASURED, answer["gaps"][0]["relationId"] == relation
        if kinds == [] and gaps == ["reading_unusable"]:
            return self.UNUSABLE, answer["gaps"][0]["relationId"] == relation
        return (kinds, gaps), None

    def test_every_scope_shape_against_every_state_the_observer_answers(self):
        linkage, rid = self.project()
        scopes = [("absent", ABSENT), ("null", None), ("ours", rid),
                  ("foreign", "rel-somewhere-else"),
                  ("malformed", [rid]), ("malformed", "   ")]
        for kind, scope in scopes:
            for state in self.STATES:
                with self.subTest(scope=kind, value=repr(scope), state=state):
                    answer = supervision.standing_for(
                        self.store, linkage, "CRW",
                        observations=[self.reading(state, scope)])
                    outcome, scoped = self.classify(answer, scope)
                    self.assertEqual(outcome, self.expected(kind, state))
                    if scoped is not None:
                        self.assertTrue(scoped, "a gap carries the scope the reading named,"
                                                " or none when it named nothing readable")

    def test_an_explicit_null_relationship_answers_exactly_as_an_absent_one(self):
        """Null and an omitted key assert the same absence, so they cannot diverge."""
        linkage, _rid = self.project()
        for state in self.STATES:
            with self.subTest(state=state):
                absent = supervision.standing_for(
                    self.store, linkage, "CRW", observations=[self.reading(state, ABSENT)])
                null = supervision.standing_for(
                    self.store, linkage, "CRW", observations=[self.reading(state, None)])
                self.assertEqual(absent["gaps"], null["gaps"])
                self.assertEqual([one["kind"] for one in absent["standing"]],
                                 [one["kind"] for one in null["standing"]])

    def test_the_states_this_grid_crosses_are_exactly_the_ones_the_module_accepts(self):
        """The grid's completeness is worth exactly what this assertion is worth.

        Its parameters are written here, so a state quietly added to OBSERVED_SETTLED - which
        would make the module absorb a reading nobody here can interpret, the opposite of what
        this unit is for - would leave every other case green. Compared against the module's
        own tuples rather than trusted alongside them.
        """
        self.assertEqual(set(supervision.OBSERVED_SETTLED), set(self.SETTLED))
        self.assertEqual(
            set(supervision.OBSERVED_STATES),
            set(self.SETTLED) | {supervision.OBSERVED_OMISSION, supervision.OBSERVED_UNMEASURED})
        self.assertEqual(len(supervision.OBSERVED_STATES), 5)
        self.assertEqual((supervision.OBSERVED_OMISSION, supervision.OBSERVED_UNMEASURED),
                         ("unreported", "unmeasured"))

    def test_each_unusable_reason_names_the_thing_that_was_actually_wrong(self):
        """And never names a field the reading supplied perfectly well.

        The reason is the sentence a person reads in the warning, so blaming the relationship
        for a bad turn - or blaming either for a record under another schema - is the same
        defect this unit removed, one layer down.
        """
        _linkage, rid = self.project()
        cases = [
            (self.reading("unreported", rid, selectors={"turn": ["turn-7"]}),
             ("turn",), ("relationship",)),
            (self.reading("unreported", [rid]), ("relationship",), ("turn",)),
            (self.reading("unreported", ABSENT), ("relationship",), ("turn",)),
            (self.reading("foreign_schema", rid), ("something-else/1",),
             ("relationship", "turn")),
            (self.reading("something", rid), ("something",), ("relationship", "turn")),
        ]
        for reading, wanted, unwanted in cases:
            with self.subTest(reading=repr(reading)[:60]):
                reason = supervision.unusable_reading(reading)["reason"]
                for word in wanted:
                    self.assertIn(word, reason)
                for word in unwanted:
                    self.assertNotIn(word, reason, "a field the reading supplied is not named"
                                                   " as the thing that was wrong with it")
        for reading in ("not an object at all", [], None):
            with self.subTest(reading=repr(reading)):
                reason = supervision.unusable_reading(reading)["reason"]
                self.assertIn(type(reading).__name__, reason)
                self.assertNotIn("relationship", reason)
                self.assertNotIn("turn", reason)

    def test_a_reading_that_is_not_an_object_is_named_rather_than_raised(self):
        linkage, _rid = self.project()
        for reading in ("not an object at all", [], None):
            with self.subTest(reading=repr(reading)):
                answer = supervision.standing_for(self.store, linkage, "CRW",
                                                  observations=[reading])
                self.assertEqual([gap["gap"] for gap in answer["gaps"]], ["reading_unusable"])
                self.assertIsNone(answer["gaps"][0]["relationId"])

    def test_one_bad_reading_handed_in_twice_is_one_gap(self):
        linkage, rid = self.project()
        each = [self.reading("unmeasured", rid, reason="marker_unreadable"),
                self.reading("something", rid),
                self.reading("foreign_schema", rid),
                self.reading("unreported", [rid]),
                self.reading("unreported", ABSENT),
                "not an object at all", [], None]
        for reading in each:
            with self.subTest(reading=repr(reading)[:40]):
                answer = supervision.standing_for(self.store, linkage, "CRW",
                                                  observations=[reading, reading])
                self.assertEqual(len(answer["gaps"]), 1)
        two = supervision.standing_for(
            self.store, linkage, "CRW",
            observations=[self.reading("something", rid), self.reading("foreign_schema", rid)])
        self.assertEqual(len(two["gaps"]), 2,
                         "two different bad readings are two things wrong")

    def test_a_malformed_selectors_container_does_not_take_the_answer_down(self):
        """A list where an object belongs used to raise AttributeError out of the whole query.

        The obligation that IS standing has to survive it, which is the whole point of naming a
        bad reading instead of raising on it: a caller that caught the error would have learned
        nothing about what the project actually owes.
        """
        linkage, rid = self.project()
        real = self.standing_event(rid)
        answer = supervision.standing_for(
            self.store, linkage, "CRW",
            observations=[self.reading("unreported", rid, selectors=["turn-7"])])
        self.assertEqual([gap["gap"] for gap in answer["gaps"]], ["reading_unusable"])
        self.assertIn("turn", answer["gaps"][0]["reason"])
        self.assertEqual([entry["obligationId"] for entry in answer["standing"]],
                         [real["obligationId"]])

    def test_the_event_loop_and_the_observation_loop_do_not_erase_each_other(self):
        linkage, rid = self.project()
        real = self.standing_event(rid)
        settled = self.reading("reported", rid)
        answer = supervision.standing_for(
            self.store, linkage, "CRW",
            observations=[settled, settled, self.reading("unmanaged", ABSENT),
                          self.reading("reported", "rel-somewhere-else")])
        self.assertEqual([entry["obligationId"] for entry in answer["standing"]],
                         [real["obligationId"]])
        self.assertEqual(answer["gaps"], [])

    def test_an_omission_the_record_already_carries_is_not_standing(self):
        """The event loop has always filtered on select(); the observation loop did not."""
        linkage, rid = self.project()
        turn = "turn-7"
        self.sync.set_target(rid, "coordination_document", DOC)
        with self.store.transaction() as db:
            sync_id = self.sync.enqueue_in(
                db, relationship_id=rid, issue_key="REL-1", subject_kind="verdict",
                summary="the omission was recorded", event_id=turn, generation=1,
                revision="a" * 40, verdict="verified")
        claim = self.sync.claim(sync_id, owner="test")
        row = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (sync_id,))
        self.sync.complete(sync_id, claim_token=claim["claimToken"], target_ref=DOC,
                           readback=render_block(row), external_ref="linear-doc-2")
        reading = self.reading("unreported", rid)
        decided = supervision.select(self.store, supervision.from_observation(reading))
        self.assertEqual(decided["standing"], supervision.DISCHARGED)
        answer = supervision.standing_for(self.store, linkage, "CRW", observations=[reading])
        self.assertEqual(answer["standing"], [])
        self.assertEqual(answer["gaps"], [],
                         "and it is not quietly re-reported as a reading nobody could read")

    def test_the_observation_schema_is_the_one_the_observer_writes(self):
        from codex_session_relay import omitted

        self.assertEqual(supervision.OBSERVATION_SCHEMA, omitted.SCHEMA)


class AnExplicitRequestIsItsOwnPath(ReportingTestCase):
    def test_a_status_request_is_answered_while_the_automatic_wake_is_suppressed(self):
        from codex_session_relay.assignment import AssignmentView
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        # An issue attaches to a project that already has a parent, which is the ordinary
        # order: the project is owned before its issues are handed out.
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        report.record(self.store, self.clock, event_id=payload["eventId"], **a_report())
        one = self.obligation_for(payload["eventId"])
        supervision.record_report(self.store, one, at=self.clock.iso())
        self.assertFalse(supervision.select(self.store, one)["report"],
                         "the automatic channel is quiet")

        assignments = AssignmentView(self.store, self.registry, self.clock, linkage=linkage)
        answer = supervision.status_answer(self.store, linkage, assignments, "CRW")
        self.assertEqual([entry["obligationId"] for entry in answer["standing"]],
                         [one["obligationId"]],
                         "and the question is still answered with what is owed")
        self.assertIn("explicit status request", answer["answeredBecause"])
        # The project reading is carried through unchanged, and the strongest word it has is
        # complete_candidate. An obligation raised about one issue never becomes a statement
        # about the project.
        self.assertIn(answer["projectState"]["state"],
                      ("unreadable", "unregistered", "ambiguous", "incomplete",
                       "complete_candidate"))
        self.assertNotEqual(answer["projectState"]["state"], "complete")


class TheUpwardEnvelope(ReportingTestCase):
    def test_the_supervisor_direction_claims_no_receipt(self):
        event_id = self.reported()
        one = self.obligation_for(event_id)
        region = supervision.envelope_for(
            self.store, one, sender=PARENT, recipient=SUPERVISOR,
            scope="project CRW, issue REL-1", observed_at=self.clock.iso())
        self.assertEqual(region["kind"], envelope.NOTIFICATION)
        self.assertEqual(region["sender"]["role"], "parent")
        for name in envelope.STAGES:
            self.assertEqual(region["reach"][name]["state"], envelope.IMPOSSIBLE)

    def test_a_decision_envelope_has_to_name_what_is_being_decided(self):
        one = self.obligation_for(self.halted(cxc.NEEDS_HUMAN, "two readings are defensible"))
        with self.assertRaises(envelope.EnvelopeRefused):
            supervision.envelope_for(self.store, one, sender=PARENT, recipient=SUPERVISOR)
        region = supervision.envelope_for(
            self.store, one, sender=PARENT, recipient=SUPERVISOR,
            decision="which of the two readings of the criterion is the agreed one")
        self.assertEqual(region["kind"], envelope.DECISION)
        self.assertEqual(region["answerOwedBy"], envelope.USER)

    def test_the_journal_entry_names_the_message_it_reported(self):
        event_id = self.reported()
        one = self.obligation_for(event_id)
        region = supervision.envelope_for(
            self.store, one, sender=PARENT, recipient=SUPERVISOR,
            observed_at=self.clock.iso())
        supervision.record_report(self.store, one, at=self.clock.iso(),
                                  messageId=region["messageId"])
        recorded = supervision.prior_report(self.store, one["obligationId"])
        self.assertEqual(recorded["detail"]["messageId"], region["messageId"])
        self.assertEqual(json.loads(self.store.one(
            "SELECT detail FROM journal WHERE kind = ? AND subject = ?",
            (supervision.JOURNAL_KIND, one["obligationId"]))["detail"])["kind"],
            supervision.COMPLETION)
class TheProjectAnswerCountsFactsNotEvents(ReportingTestCase):
    def test_one_block_stated_twice_appears_once(self):
        """Two events, one obligation, and the answer used to carry it twice."""
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.models import Endpoint

        linkage = Linkage(self.store, self.clock)
        linkage.bind_scope(role="parent", scope_key="CRW",
                           endpoint=Endpoint(PARENT, "host-a", cwd="/parent",
                                             cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        for attempt in (1, 2):
            payload = self.execution_payload(relationship, "blocked_needs_input",
                                             attempt=attempt)
            self.accept(payload)
            report.record(self.store, self.clock, event_id=payload["eventId"],
                          **a_report(cxc_status=cxc.BLOCKED,
                                     cxc_reason="upstream has not landed", pr_number=None,
                                     pr_url=None, pr_state=None, handoff=None))
        answer = supervision.standing_for(self.store, linkage, "CRW")
        identifiers = [entry["obligationId"] for entry in answer["standing"]]
        self.assertEqual(len(identifiers), len(set(identifiers)))
        self.assertEqual(len(identifiers), 1)


class TheCommandsAParentActuallyRuns(ReportingTestCase):
    """The seam is reachable from the CLI, or the workflow rule is prose nobody can call."""

    class _Services:
        def __init__(self, case, linkage):
            self.store, self.clock, self.delivery = case.store, case.clock, case.delivery
            self.linkage = linkage
            from codex_session_relay.assignment import AssignmentView
            self.assignments = AssignmentView(case.store, case.registry, case.clock,
                                              linkage=linkage)

    def services(self):
        from codex_session_relay.linkage import Linkage

        return self._Services(self, Linkage(self.store, self.clock))

    def test_select_answers_about_one_event_without_writing_anything(self):
        from codex_session_relay import cli
        from argparse import Namespace

        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        before = self.store.one("SELECT COUNT(*) AS n FROM journal")["n"]
        answer = cli.cmd_supervisor_select(
            self.services(), Namespace(event=event_id, recipient=SUPERVISOR))
        self.assertTrue(answer["report"])
        self.assertEqual(self.store.one("SELECT COUNT(*) AS n FROM journal")["n"], before)

    def test_recording_a_report_converges_and_then_suppresses(self):
        from codex_session_relay import cli
        from argparse import Namespace

        event_id = self.reported()
        self.lifecycle(SUPERVISOR, "yes")
        services = self.services()
        first = cli.cmd_supervisor_report_recorded(
            services, Namespace(event=event_id, message="m-1", note=None))
        second = cli.cmd_supervisor_report_recorded(
            services, Namespace(event=event_id, message="m-1", note=None))
        self.assertTrue(first["recorded"])
        self.assertFalse(second["recorded"])
        answer = cli.cmd_supervisor_select(
            services, Namespace(event=event_id, recipient=SUPERVISOR))
        self.assertEqual(answer["reason"], supervision.ALREADY_REPORTED)

    def test_recording_against_an_event_that_owes_nothing_is_refused(self):
        from codex_session_relay import cli
        from argparse import Namespace

        relationship = self.register()
        self._rid = relationship["relationshipId"]
        payload = self.execution_payload(relationship, "failed")
        self.accept(payload)
        with self.assertRaises(cli.SystemExit2):
            cli.cmd_supervisor_report_recorded(
                self.services(), Namespace(event=payload["eventId"], observation=None,
                                           message=None, note=None))

    def test_an_omission_can_be_recorded_by_its_observation(self):
        """The one kind of news nobody sent used to be the one that could not be recorded.

        A turn that ended without reporting has no event, so an event-only surface left that
        obligation unrecordable - and the next reading of the same omission would have
        produced another report, indefinitely.
        """
        import json as json_module
        import os
        from argparse import Namespace
        from codex_session_relay import cli
        from codex_session_relay.models import Endpoint

        services = self.services()
        services.linkage.bind_scope(
            role="parent", scope_key="CRW",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        path = os.path.join(self.tmp, "omission.json")
        with open(path, "w", encoding="utf-8") as handle:
            json_module.dump({"schema": "reporting-observation/1",
                              "reportingState": "unreported",
                              "reason": "terminal_without_report",
                              "relationshipId": self._rid, "executionGeneration": 1,
                              "selectors": {"turn": "turn-7"}}, handle)

        standing = cli.cmd_supervisor_standing(
            services, Namespace(project="CRW", observation=[path]))
        owed = standing["standing"][0]
        self.assertEqual(owed["kind"], supervision.UNREPORTED)

        first = cli.cmd_supervisor_report_recorded(
            services, Namespace(event=None, observation=path, message="m-1", note=None))
        second = cli.cmd_supervisor_report_recorded(
            services, Namespace(event=None, observation=path, message="m-1", note=None))
        self.assertTrue(first["recorded"])
        self.assertFalse(second["recorded"], "the same omission converges on the first report")
        self.assertEqual(first["obligation"]["obligationId"], owed["obligationId"],
                         "and it is the obligation the project answer named")

        again = cli.cmd_supervisor_standing(
            services, Namespace(project="CRW", observation=[path]))
        self.assertEqual(
            again["standing"][0]["decision"]["reason"], supervision.ALREADY_REPORTED,
            "so a later reading of the same omission suppresses another report")

    def test_an_observation_that_owes_nothing_is_refused(self):
        import json as json_module
        import os
        from argparse import Namespace
        from codex_session_relay import cli

        path = os.path.join(self.tmp, "reported.json")
        with open(path, "w", encoding="utf-8") as handle:
            json_module.dump({"schema": "reporting-observation/1",
                              "reportingState": "reported", "relationshipId": "rel-1",
                              "selectors": {"turn": "turn-7"}}, handle)
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_supervisor_report_recorded(
                self.services(), Namespace(event=None, observation=path, message=None,
                                           note=None))
        self.assertIn("state unreported", str(caught.exception))

    def test_standing_reads_a_passed_in_observation_from_disk(self):
        import json as json_module
        import os
        from codex_session_relay import cli
        from argparse import Namespace
        from codex_session_relay.models import Endpoint

        services = self.services()
        services.linkage.bind_scope(
            role="parent", scope_key="CRW",
            endpoint=Endpoint(PARENT, "host-a", cwd="/parent", cxc_session="cxc-parent"))
        relationship = self.register(project_key="CRW")
        self._rid = relationship["relationshipId"]
        path = os.path.join(self.tmp, "observation.json")
        with open(path, "w", encoding="utf-8") as handle:
            json_module.dump({"schema": "reporting-observation/1",
                              "reportingState": "unreported", "reason": "terminal_without_report",
                              "relationshipId": self._rid, "executionGeneration": 1,
                              "selectors": {"turn": "turn-7"}}, handle)
        answer = cli.cmd_supervisor_standing(
            services, Namespace(project="CRW", observation=[path]))
        self.assertEqual([entry["kind"] for entry in answer["standing"]],
                         [supervision.UNREPORTED])
        self.assertIn("explicit status request", answer["answeredBecause"])

    def test_an_observation_file_that_cannot_be_read_says_so(self):
        import os
        from codex_session_relay import cli
        from argparse import Namespace

        broken = os.path.join(self.tmp, "broken.json")
        with open(broken, "wb") as handle:
            handle.write(b"\xff\xfe not utf-8 and not json either")
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_supervisor_standing(
                self.services(), Namespace(project="CRW", observation=[broken]))
        self.assertIn("could not be read", str(caught.exception))

    def test_the_three_commands_are_classified_as_offline(self):
        """doctor reports what an operator can run with no App Server, from these lists.

        All three read and write this store alone. Leaving them out under-reports the surface
        rather than misreporting it, which is why nothing failed until somebody looked.
        """
        from codex_session_relay import cli

        for name in ("supervisor-select", "supervisor-standing", "supervisor-report-recorded"):
            self.assertIn(name, cli.OFFLINE_COMMANDS, name)
            self.assertNotIn(name, cli.HOST_REQUIRED_COMMANDS, name)


class AnAbsenceIsNotAFault(ReportingTestCase):
    """envelope_context turned every exception into unknown context, diagnosis included."""

    def test_a_relationship_this_store_does_not_hold_is_an_absence(self):
        event_id = self.reported()
        row = dict(self.delivery.get(event_id))
        row["relationship_id"] = "rel-0000000000000000"
        self.assertEqual(self.delivery.envelope_context(row), {},
                         "an unregistered relationship is what unknown context is for")

    def test_a_programming_fault_is_not_dressed_as_unknown_context(self):
        event_id = self.reported()
        row = dict(self.delivery.get(event_id))
        del row["kind"]
        with self.assertRaises(KeyError):
            self.delivery.envelope_context(row)

    def test_a_store_that_cannot_answer_is_not_dressed_as_unknown_context(self):
        import sqlite3

        event_id = self.reported()
        row = self.delivery.get(event_id)

        def refuse(sql, params=()):
            raise sqlite3.OperationalError("database is locked")

        original = self.store.one
        self.store.one = refuse
        self.addCleanup(setattr, self.store, "one", original)
        with self.assertRaises(sqlite3.OperationalError):
            self.delivery.envelope_context(row)

    def test_an_ordinary_row_still_reads_its_sender_and_scope(self):
        event_id = self.reported()
        context = self.delivery.envelope_context(self.delivery.get(event_id))
        self.assertEqual(context["senderTaskId"], CHILD)
        self.assertIn("REL-1", context["scope"])


class SeveralRulingsOnOneEvent(ReportingTestCase):
    """CRW-23: identity now separates rulings, so an event can own more than one verdict row.

    Before that, "a confirmed row exists" and "the ruling that stands is recorded" were the same
    sentence. They are not any more, and reading the old one would report the supervisor served
    at exactly the moment the current ruling had not reached it.
    """

    def ruling(self, event_id, *, criteria_digest, ruling=None, target_ref=DOC):
        """One more accepted ruling on the same event, the way a re-review produces one."""
        row = self.store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
        self.sync.set_target(self._rid, "coordination_document", target_ref)
        with self.store.transaction() as db:
            return self.sync.enqueue_in(
                db, relationship_id=self._rid, issue_key="REL-1", subject_kind="verdict",
                summary=f"ruled against {criteria_digest}", event_id=event_id,
                generation=row["execution_generation"], revision=row["revision_hash"],
                verdict="verified", criteria_digest=criteria_digest, ruling=ruling)

    def confirm(self, identifier, *, target_ref=DOC):
        claim = self.sync.claim(identifier, owner="test")
        row = self.store.one("SELECT * FROM sync_outbox WHERE sync_id = ?", (identifier,))
        self.sync.complete(identifier, claim_token=claim["claimToken"], target_ref=target_ref,
                           readback=render_block(row), external_ref="linear-doc-1")

    def test_an_old_confirmed_record_does_not_discharge_the_current_ruling(self):
        event_id = self.reported()
        first = self.ruling(event_id, criteria_digest="digest-a")
        self.confirm(first)
        self.ruling(event_id, criteria_digest="digest-b", ruling=2)

        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING)
        self.assertIn(PENDING, decided["dischargeReason"])

    def test_a_failed_current_ruling_is_not_covered_by_an_older_confirmation(self):
        """failed drops out of automatic selection, so it looks terminal and means the opposite."""
        event_id = self.reported()
        self.confirm(self.ruling(event_id, criteria_digest="digest-a"))
        second = self.ruling(event_id, criteria_digest="digest-b", ruling=2)
        for _ in range(MAX_ATTEMPTS):
            claim = self.sync.claim(second, owner="test")
            self.sync.fail(second, claim_token=claim["claimToken"], error="linear said no")
            self.clock.advance(1000)
        self.assertEqual(
            self.store.one("SELECT state FROM sync_outbox WHERE sync_id = ?",
                           (second,))["state"],
            FAILED,
            "the point of this case is the state that stops being retried",
        )

        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING)
        self.assertIn(FAILED, decided["dischargeReason"])

    def test_the_current_ruling_confirmed_discharges_it(self):
        event_id = self.reported()
        self.confirm(self.ruling(event_id, criteria_digest="digest-a"))
        self.confirm(self.ruling(event_id, criteria_digest="digest-b", ruling=2))

        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.DISCHARGED)
        self.assertEqual(decided["reason"], supervision.ALREADY_RECORDED)

    def test_a_newer_ruling_written_elsewhere_is_not_discharged_by_repointing_back(self):
        """The newest ruling landed in a document this relationship no longer points at."""
        event_id = self.reported()
        self.confirm(self.ruling(event_id, criteria_digest="digest-a"))
        elsewhere = DOC.replace("000000", "111111")
        self.confirm(self.ruling(event_id, criteria_digest="digest-b", ruling=2,
                                 target_ref=elsewhere), target_ref=elsewhere)
        self.sync.set_target(self._rid, "coordination_document", DOC)

        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING)
        self.assertIn("no longer the target", decided["dischargeReason"])

    def test_a_clock_that_moves_backwards_does_not_reorder_the_rulings(self):
        """created_at is a wall clock. Insertion order is what says which ruling is current."""
        event_id = self.reported()
        self.confirm(self.ruling(event_id, criteria_digest="digest-a"))
        second = self.ruling(event_id, criteria_digest="digest-b", ruling=2)
        self.store.db.execute(
            "UPDATE sync_outbox SET created_at = ? WHERE sync_id = ?",
            ("1999-01-01T00:00:00Z", second),
        )

        decided = supervision.select(self.store, self.obligation_for(event_id))
        self.assertEqual(decided["standing"], supervision.STANDING,
                         "the later row is still the later ruling")
        self.assertIn(PENDING, decided["dischargeReason"])
