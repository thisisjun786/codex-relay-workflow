"""CRW-163: can a coordinator LIST the children that stopped, and was anyone actually told.

Every case here is written from the coordinator's side. The question is never whether a field
exists, but whether somebody holding only this answer would reach a true conclusion: that a blocked
child is blocked, that an unreadable store is not an empty one, and that nothing here claims a
recipient saw anything the store cannot show it saw.

Most cases are contract scenarios under contract/fixtures/cli-shape/test_dispositions__*.json,
driven through the real relay CLI against a real store; the tests below that call run_fixture
are their thin runners. The rest stay Python for the reasons contract/notes/test_dispositions.md
records.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from codex_session_relay import dispositions, report
from codex_session_relay.delivery import EXECUTION_ONLY_OUTCOMES
from codex_session_relay.receipts import OUTCOMES
from codex_session_relay.store import resolve_state_dir

from .support import DISPATCH_TURN, DeliveryTestCase
from .test_cli import REPO

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT))
from contract.runner import FIXTURES, run_scenario

PROJECT = "PROJ-CRW-163"


def run_fixture(name):
    with tempfile.TemporaryDirectory(prefix="relay-dispositions-") as raw:
        run_scenario(FIXTURES / "cli-shape" / f"test_dispositions__{name}.json", Path(raw))


class TheTurnDisposition(unittest.TestCase):
    """Which disposition is current, and what happens when the store holds two answers."""

    def test_a_child_that_reported_nothing_is_not_a_child_that_is_fine(self):
        run_fixture("test_a_child_that_reported_nothing_is_not_a_child_that_is_fine")

    def test_a_single_blocked_event_is_the_disposition(self):
        run_fixture("test_a_single_blocked_event_is_the_disposition")

    def test_a_reviewable_event_is_counted_and_never_becomes_the_disposition(self):
        run_fixture("test_a_reviewable_event_is_counted_and_never_becomes_the_disposition")

    def test_several_reviewable_events_are_all_listed_and_none_is_called_the_head(self):
        run_fixture("test_several_reviewable_events_are_all_listed_and_none_is_called_the_head")

    def test_two_events_with_the_same_outcome_anchor_to_the_newest_and_keep_both(self):
        run_fixture("test_two_events_with_the_same_outcome_anchor_to_the_newest_and_keep_both")

    def test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer(self):
        for label in ("equal", "newer"):
            with self.subTest(label):
                run_fixture("test_disagreeing_outcomes_are_a_contest_even_when_one_is_newer__"
                            + label)

    def test_a_suppressed_claim_is_listed_and_never_chosen(self):
        run_fixture("test_a_suppressed_claim_is_listed_and_never_chosen")

    def test_a_staged_claim_is_listed_and_never_chosen(self):
        run_fixture("test_a_staged_claim_is_listed_and_never_chosen")

    def test_a_directly_final_event_with_no_finalized_at_still_orders(self):
        run_fixture("test_a_directly_final_event_with_no_finalized_at_still_orders")

    def test_earlier_generations_are_counted_and_not_listed(self):
        run_fixture("test_earlier_generations_are_counted_and_not_listed")


class TheWorkReport(unittest.TestCase):
    """BLOCKED, UNSAFE and NEEDS_HUMAN collapse onto one outcome; only the report separates them."""

    def test_a_recorded_report_carries_the_status_that_separates_them(self):
        run_fixture("test_a_recorded_report_carries_the_status_that_separates_them")

    def test_no_report_says_the_separation_is_unavailable_rather_than_guessing(self):
        run_fixture("test_no_report_says_the_separation_is_unavailable_rather_than_guessing")


class TheDeliveryAxis(unittest.TestCase):
    """Whether a send was measured at all, which is the question absence used to swallow."""

    def test_no_delivery_and_no_intent_is_unmeasured_not_undelivered(self):
        run_fixture("test_no_delivery_and_no_intent_is_unmeasured_not_undelivered")

    def test_an_intent_without_a_delivery_is_a_refusal_that_may_not_last(self):
        run_fixture("test_an_intent_without_a_delivery_is_a_refusal_that_may_not_last")

    def test_a_queued_delivery_is_not_sent_and_carries_the_stores_own_word(self):
        run_fixture("test_a_queued_delivery_is_not_sent_and_carries_the_stores_own_word")

    def test_a_dispatched_delivery_says_dispatched_and_nothing_more(self):
        run_fixture("test_a_dispatched_delivery_says_dispatched_and_nothing_more")

    def test_an_inbox_only_delivery_is_stored_not_woken(self):
        run_fixture("test_an_inbox_only_delivery_is_stored_not_woken")

    def test_an_uncertain_send_is_not_evidence_of_non_delivery(self):
        for state in ("sending", "held_uncertain"):
            with self.subTest(state):
                run_fixture("test_an_uncertain_send_is_not_evidence_of_non_delivery__" + state)

    def test_a_supersession_note_outranks_the_state_but_never_erases_it(self):
        run_fixture("test_a_supersession_note_outranks_the_state_but_never_erases_it")

    def test_a_state_this_reader_has_no_word_for_is_not_folded_into_one(self):
        run_fixture("test_a_state_this_reader_has_no_word_for_is_not_folded_into_one")

    def test_a_staged_event_is_never_reported_as_delivered(self):
        run_fixture("test_a_staged_event_is_never_reported_as_delivered")

    def test_a_suppressed_event_is_called_suppressed_rather_than_staged(self):
        run_fixture("test_a_suppressed_event_is_called_suppressed_rather_than_staged")

    def test_an_acknowledgement_with_no_delivery_row_is_a_disagreement_not_unmeasured(self):
        run_fixture(
            "test_an_acknowledgement_with_no_delivery_row_is_a_disagreement_not_unmeasured")


class TheReceivingAxis(unittest.TestCase):
    """A dispatch proves a send was accepted, never that the recipient observed anything."""

    def test_a_dispatched_send_still_leaves_the_receiving_side_unmeasured(self):
        run_fixture("test_a_dispatched_send_still_leaves_the_receiving_side_unmeasured")

    def test_a_host_read_acknowledgement_is_an_observation(self):
        run_fixture("test_a_host_read_acknowledgement_is_an_observation")

    def test_an_unverified_acknowledgement_is_a_claim_rather_than_an_observation(self):
        run_fixture("test_an_unverified_acknowledgement_is_a_claim_rather_than_an_observation")

    def test_evidence_with_no_acknowledgement_beside_it_has_no_supportable_answer(self):
        run_fixture("test_evidence_with_no_acknowledgement_beside_it_has_no_supportable_answer")

    def test_the_acknowledgement_block_keeps_every_field_the_assignment_view_reports(self):
        run_fixture(
            "test_the_acknowledgement_block_keeps_every_field_the_assignment_view_reports")

    def test_no_acknowledgement_row_is_unrecorded_rather_than_unverified(self):
        run_fixture("test_no_acknowledgement_row_is_unrecorded_rather_than_unverified")


class TheVocabulary(unittest.TestCase):
    """The words come from the store. A word that drifts from it fails here rather than in the field."""

    def test_the_execution_only_outcomes_are_still_the_stores_own_tuple(self):
        run_fixture("test_the_execution_only_outcomes_are_still_the_stores_own_tuple")
        self.assertEqual(dispositions.EXECUTION_ONLY, EXECUTION_ONLY_OUTCOMES)

    def test_the_statement_selects_exactly_the_stores_four_outcomes(self):
        run_fixture("test_the_statement_selects_exactly_the_stores_four_outcomes")
        for outcome in OUTCOMES:
            self.assertIn("'" + outcome + "'", dispositions._SQL,
                          "an outcome the store records is missing from the read")

    def test_every_delivery_state_the_transport_defines_has_a_word(self):
        """Enumerated from transport itself, so a state added later fails this test.

        The four subtracted names are receipt statuses rather than delivery states: they describe
        what a transport receipt said, not what a delivery row holds.
        """
        run_fixture("test_every_delivery_state_the_transport_defines_has_a_word")
        from codex_session_relay import transport

        receipt_statuses = {"ACCEPTED", "FAILED", "OUTCOME_UNKNOWN", "UNFINISHED"}
        states = {
            value for name, value in vars(transport).items()
            if name.isupper() and isinstance(value, str) and name not in receipt_statuses
        }
        self.assertEqual(states, set(dispositions.OBSERVATION_BY_STATE),
                         "a delivery state with no word would land in the wrong one")

    def test_every_word_the_map_produces_has_a_detail_entry(self):
        run_fixture("test_every_word_the_map_produces_has_a_detail_entry")
        for word in set(dispositions.OBSERVATION_BY_STATE.values()):
            self.assertIn(word, dispositions.OBSERVATION_DETAIL)


class TheCounts(unittest.TestCase):
    def test_the_counts_cannot_disagree_with_the_list_they_summarise(self):
        run_fixture("test_the_counts_cannot_disagree_with_the_list_they_summarise")


class TheReadItself(unittest.TestCase):
    """An empty list and an unreadable store are the two answers this contract refuses to merge."""

    def setUp(self):
        import shutil

        self.tmp = tempfile.mkdtemp(prefix="relay-dispositions-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_a_directory_with_no_database_is_unreadable_rather_than_empty(self):
        run_fixture("test_a_directory_with_no_database_is_unreadable_rather_than_empty")

    def test_a_file_that_is_not_a_relay_database_is_unreadable_and_stays_untouched(self):
        run_fixture("test_a_file_that_is_not_a_relay_database_is_unreadable_and_stays_untouched")

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

    def blocked(self, relationship, *, status=None, attempt=1, turn_id=DISPATCH_TURN):
        # The turn has to be this generation's anchor: a receipt on any other turn needs an
        # explicit continuation admission, which is a different test's subject.
        turn = self.assigned_turn("completed", turn=turn_id)
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
        run_fixture("test_a_blocked_child_is_enumerable_by_project")

    def test_the_three_statuses_that_share_one_outcome_stay_separable(self):
        run_fixture("test_the_three_statuses_that_share_one_outcome_stay_separable")

    def test_a_child_with_no_work_report_says_the_separation_is_unavailable(self):
        run_fixture("test_a_child_with_no_work_report_says_the_separation_is_unavailable")

    def test_a_queued_then_dispatched_delivery_is_measured_at_each_step(self):
        run_fixture("test_a_queued_then_dispatched_delivery_is_measured_at_each_step")

    def test_a_staged_claim_is_recorded_progress_and_never_delivery(self):
        run_fixture("test_a_staged_claim_is_recorded_progress_and_never_delivery")

    def test_the_project_selector_hides_what_is_not_live_and_the_relationship_selector_does_not(self):
        run_fixture("test_the_project_selector_hides_what_is_not_live_and_the_relationship"
                    "_selector_does_not")

    def test_an_unscoped_assignment_is_absent_by_project_and_present_by_relationship(self):
        run_fixture("test_an_unscoped_assignment_is_absent_by_project_and_present_by_relationship")

    def test_an_event_in_an_earlier_generation_is_counted_not_listed(self):
        run_fixture("test_an_event_in_an_earlier_generation_is_counted_not_listed")


class TheCommand(DeliveryTestCase):
    """The exit code is the part a polling coordinator reads first."""

    def cli(self, *args, state=None, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli",
             "--state", state or os.path.join(self.tmp, "state"), *args],
            capture_output=True, text=True, env=environment, timeout=60, check=False,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def test_the_command_reads_a_blocked_child_through_a_subprocess(self):
        run_fixture("test_the_command_reads_a_blocked_child_through_a_subprocess")

    def test_an_unreadable_store_refuses_rather_than_reporting_nobody(self):
        """doctor keeps exit 0 for an unreadable database; this command must not.

        A coordinator asking which children are blocked and checking only the exit code would read
        an unreadable store as "nobody is blocked", which is the merge this whole contract refuses.
        """
        run_fixture("test_an_unreadable_store_refuses_rather_than_reporting_nobody")

    def test_the_command_turns_no_unrelated_file_into_a_relay_database(self):
        run_fixture("test_the_command_turns_no_unrelated_file_into_a_relay_database")

    def test_the_two_selectors_are_mutually_exclusive_and_one_is_required(self):
        run_fixture("test_the_two_selectors_are_mutually_exclusive_and_one_is_required")

    def test_the_command_is_listed_as_offline_because_it_opens_no_adapter(self):
        run_fixture("test_the_command_is_listed_as_offline_because_it_opens_no_adapter")

class TheCorrectionBlock(unittest.TestCase):
    """CRW-222: the correction a verdict queued is relay-produced, so it is not in events.

    In CRW-5 c6 this reader showed a generation-2 child with no events and every count 0 while
    its correction sat withheld as lifecycle_unknown. The block says whether the correction went
    out and, when it did not, why.
    """

    def test_a_child_without_a_correction_has_none_and_counts_nothing(self):
        run_fixture("test_a_child_without_a_correction_has_none_and_counts_nothing")

    def test_a_correction_withheld_by_the_lifecycle_is_named(self):
        run_fixture("test_a_correction_withheld_by_the_lifecycle_is_named")

    def test_a_withheld_correction_without_a_lifecycle_record_is_still_counted(self):
        run_fixture("test_a_withheld_correction_without_a_lifecycle_record_is_still_counted")

    def test_a_busy_deferral_is_not_sent_but_not_withheld(self):
        run_fixture("test_a_busy_deferral_is_not_sent_but_not_withheld")

    def test_a_paused_assignment_names_the_relationship(self):
        run_fixture("test_a_paused_assignment_names_the_relationship")

    def test_a_superseded_assignment_is_not_called_merely_inactive(self):
        run_fixture("test_a_superseded_assignment_is_not_called_merely_inactive")

    def test_a_held_correction_names_its_hold(self):
        run_fixture("test_a_held_correction_names_its_hold")

    def test_a_dispatched_correction_has_no_reason(self):
        run_fixture("test_a_dispatched_correction_has_no_reason")

    def test_a_state_this_reader_has_no_word_for_is_said_so(self):
        run_fixture("test_a_state_this_reader_has_no_word_for_is_said_so")

    def test_a_correction_the_child_already_answered_is_superseded_and_not_counted(self):
        run_fixture(
            "test_a_correction_the_child_already_answered_is_superseded_and_not_counted")


class TheCorrectionAgainstARealStore(DeliveryTestCase):
    """The c6 shape against what the relay writes: an archived child's correction, withheld."""

    def test_a_withheld_correction_is_listed_with_its_reason(self):
        run_fixture("test_a_withheld_correction_is_listed_with_its_reason")


if __name__ == "__main__":
    unittest.main()
