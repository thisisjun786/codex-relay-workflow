"""Agreements about places in a tree, and what an agreement deliberately is not.

The cases divide three ways. Granularity: one shared file must not block every parent with
business elsewhere in it, and a claim on the repository root must be refused outright.
Currency: an agreement is about a place in a specific tree, so restating the revision reopens
it and a settlement on a superseded revision is refused. And absence: a peer agreement confers
nothing, which is checked by showing the merge answer is the same with and without one rather
than by reading a sentence in a record.
"""

import threading
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.editregion import EditRegions
from codex_session_relay.errors import CoordinationError, RefusalReason
from codex_session_relay.linkage import Linkage, PARENT
from codex_session_relay.mergeturn import MergeTurn
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

from .support import RelayTestCase

REPO = "owner/repo"
REV = "rev-1"


class EditRegionTestCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.linkage = Linkage(self.store, self.clock)
        self.regions = EditRegions(self.store, self.clock, self.linkage)
        self.alpha = Endpoint("task-alpha", "host-a", cwd="/alpha")
        self.beta = Endpoint("task-beta", "host-b", cwd="/beta")
        self.zeta = Endpoint("task-zeta", "host-z", cwd="/zeta")
        for key, endpoint in (("PRJ-A", self.alpha), ("PRJ-B", self.beta),
                              ("PRJ-Z", self.zeta)):
            self.linkage.bind_scope(role=PARENT, scope_key=key, endpoint=endpoint)
        self.pair = self.linkage.register_peer(
            left_project="PRJ-A", left_parent=self.alpha,
            right_project="PRJ-B", right_parent=self.beta)["linkId"]
        self.other = self.linkage.register_peer(
            left_project="PRJ-A", left_parent=self.alpha,
            right_project="PRJ-Z", right_parent=self.zeta)["linkId"]

    def propose(self, path, *, kind="file", key="", region_class="source",
                regenerate_from=None, link=None, right="PRJ-B", revision=REV,
                task=None, next_owner="task-beta"):
        return self.regions.propose(
            repository=REPO, base_revision=revision, path=path, region_kind=kind,
            region_key=key, region_class=region_class, regenerate_from=regenerate_from,
            left_project="PRJ-A", right_project=right, peer_link_id=link or self.pair,
            proposer_task_id=task or self.alpha.task_id,
            constraint_text="keep the public signature", issue_key="CRW-1",
            next_owner=next_owner)


class ARegionIsAPlaceNotAFileName(EditRegionTestCase):
    def test_two_symbols_in_one_file_are_independent_regions(self):
        first = self.propose("src/a.py", kind="symbol", key="parse")
        second = self.propose("src/a.py", kind="symbol", key="render")
        elsewhere = self.propose("src/b.py")
        self.assertNotEqual(first["regionId"], second["regionId"])
        for record in (first, second, elsewhere):
            self.assertEqual(record["state"], "proposed")

    def test_a_whole_file_claim_overlaps_a_symbol_inside_it(self):
        self.propose("src/a.py", kind="symbol", key="parse")
        with self.assertRaises(CoordinationError) as caught:
            self.propose("src/a.py", right="PRJ-Z", link=self.other)
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)

    def test_two_symbols_of_one_name_in_different_files_do_not_collide(self):
        self.propose("src/a.py", kind="symbol", key="parse")
        other = self.propose(
            "src/b.py", kind="symbol", key="parse", right="PRJ-Z", link=self.other)
        self.assertEqual(other["state"], "proposed")

    def test_a_tree_contains_its_files_by_component(self):
        self.propose("src/a", kind="tree")
        cousin = self.propose("src/ab/x.py", right="PRJ-Z", link=self.other)
        self.assertEqual(cousin["state"], "proposed")
        with self.assertRaises(CoordinationError) as caught:
            self.propose("src/a/x.py", right="PRJ-Z", link=self.other)
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)

    def test_a_claim_on_the_repository_root_is_refused(self):
        for root in (".", ""):
            with self.assertRaises(CoordinationError) as caught:
                self.propose(root, kind="tree")
            self.assertEqual(caught.exception.reason, RefusalReason.REGION_TOO_BROAD)

    def test_a_non_canonical_path_is_refused_before_it_becomes_an_identity(self):
        for spelling in ("src//a.py", "/src/a.py", "src/../a.py", "src/a.py/", "./a.py"):
            with self.assertRaises(CoordinationError) as caught:
                self.propose(spelling)
            self.assertEqual(
                caught.exception.reason, RefusalReason.REGION_TOO_BROAD, spelling)

    def test_either_side_proposing_converges_on_one_agreement(self):
        first = self.propose("src/a.py")
        again = self.regions.propose(
            repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
            left_project="PRJ-B", right_project="PRJ-A", peer_link_id=self.pair,
            proposer_task_id=self.beta.task_id, constraint_text="keep the signature")
        self.assertEqual(again["agreementId"], first["agreementId"])
        self.assertTrue(again["alreadyProposed"])

    def test_an_agreement_records_the_revision_its_region_was_persisted_with(self):
        record = self.propose("src/a.py")
        region = self.store.one(
            "SELECT * FROM edit_regions WHERE region_id = ?", (record["regionId"],))
        self.assertEqual(record["baseRevision"], region["base_revision"])
        self.assertEqual(record["repository"], region["repository"])
        self.assertLess(record["leftProject"], record["rightProject"])

    def test_projects_that_never_registered_as_peers_cannot_agree(self):
        with self.assertRaises(CoordinationError) as caught:
            self.regions.propose(
                repository=REPO, base_revision=REV, path="src/c.py", region_kind="file",
                left_project="PRJ-B", right_project="PRJ-Z", peer_link_id=self.pair,
                proposer_task_id=self.beta.task_id, constraint_text="x")
        self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_SCOPE)


class GeneratedMetadataIsRederivedNotContested(EditRegionTestCase):
    def test_two_claims_on_one_generated_artefact_do_not_conflict(self):
        first = self.propose(
            "scripts/components.json", region_class="generated",
            regenerate_from="runtime_install.py verify-definition")
        second = self.propose(
            "scripts/components.json", region_class="generated",
            regenerate_from="runtime_install.py verify-definition",
            right="PRJ-Z", link=self.other)
        self.assertEqual(first["state"], "proposed")
        self.assertEqual(second["state"], "proposed")

    def test_a_generated_region_is_reported_as_one_to_regenerate(self):
        self.propose(
            "scripts/components.json", region_class="generated", regenerate_from="derive")
        self.propose("src/a.py")
        answer = self.regions.show(repository=REPO)
        self.assertEqual(len(answer["regenerate"]), 1)
        self.assertEqual(len(answer["exclusive"]), 1)
        self.assertEqual(answer["regenerate"][0]["resolution"], "rederive")

    def test_a_generated_region_has_to_say_what_it_is_derived_from(self):
        with self.assertRaises(CoordinationError) as caught:
            self.propose("scripts/components.json", region_class="generated")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_TOO_BROAD)


class BothSidesAcceptOrTheConditionIsKept(EditRegionTestCase):
    def test_an_agreement_needs_both_sides(self):
        record = self.propose("src/a.py")
        self.assertEqual(record["state"], "proposed")
        self.regions.settle(
            record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertEqual(self.regions.agreement(record["agreementId"])["state"], "agreed")

    def test_a_conditional_decline_keeps_the_condition_it_would_accept_under(self):
        record = self.propose("src/a.py")
        self.regions.settle(
            record["agreementId"], actor=self.beta.task_id, disposition="declined",
            condition="only if the old name keeps forwarding", reason="too broad")
        closed = self.regions.agreement(record["agreementId"])
        self.assertEqual(closed["state"], "declined")
        self.assertEqual(
            closed["rightCondition"], "only if the old name keeps forwarding")

    def test_a_withdrawal_frees_the_region_for_another_pair(self):
        record = self.propose("src/a.py")
        self.regions.settle(
            record["agreementId"], actor=self.alpha.task_id, disposition="withdrawn")
        self.assertEqual(
            self.propose("src/a.py", right="PRJ-Z", link=self.other)["state"], "proposed")

    def test_only_the_proposer_withdraws_its_own_proposal(self):
        record = self.propose("src/a.py")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                record["agreementId"], actor=self.beta.task_id, disposition="withdrawn")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

class AnAgreementIsAboutAPlaceInASpecificTree(EditRegionTestCase):
    def agreed(self, path="src/a.py"):
        record = self.propose(path)
        self.regions.settle(
            record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        return record

    def test_restating_the_revision_reopens_proposed_and_agreed_rows_alike(self):
        settled = self.agreed("src/a.py")
        open_one = self.propose("src/b.py")
        answer = self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor="supervisor-1")
        self.assertEqual(
            sorted(answer["reopened"]),
            sorted([settled["agreementId"], open_one["agreementId"]]))
        for identifier in answer["reopened"]:
            record = self.regions.agreement(identifier)
            self.assertEqual(record["state"], "reopened")
            self.assertEqual(record["baseRevision"], REV, "the old revision is not rewritten")

    def test_a_settlement_on_a_superseded_revision_is_refused_anywhere_in_a_chain(self):
        record = self.propose("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor="supervisor-1")
        self.regions.restate_revision(
            repository=REPO, from_revision="rev-2", to_revision="rev-3", actor="supervisor-1")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertFalse(self.regions.current_revision(REPO, "rev-2"))
        self.assertTrue(self.regions.current_revision(REPO, "rev-3"))

    def test_one_revision_has_one_successor(self):
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor="supervisor-1")
        replay = self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor="supervisor-1")
        self.assertEqual(replay["toRevision"], "rev-2")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision=REV, to_revision="rev-9",
                actor="supervisor-2")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)

    def test_the_owner_carries_an_agreement_onto_the_current_revision(self):
        record = self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor="supervisor-1")
        successor = self.regions.reaffirm(
            record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(successor["baseRevision"], "rev-2")
        self.assertEqual(successor["supersedes"], record["agreementId"])
        self.assertEqual(successor["state"], "proposed", "the counterpart accepts again")
        self.assertEqual(
            self.regions.agreement(record["agreementId"])["supersededBy"],
            successor["agreementId"])

    def test_a_stranger_cannot_reaffirm_somebody_else_s_agreement(self):
        record = self.agreed("src/a.py")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.zeta.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)


class WorkNobodyTookIsNobodysWork(EditRegionTestCase):
    def with_followup(self, **kwargs):
        record = self.propose("src/a.py")
        return record, self.regions.followup(
            record["agreementId"], trigger_text="a temporary duplicate implementation",
            acceptance_text="the duplicate is gone and one caller remains",
            recorded_by=self.alpha.task_id, issue_ref="CRW-200", **kwargs)

    def test_an_unassigned_follow_up_is_counted_for_neither_parent(self):
        self.with_followup()
        answer = self.regions.show(repository=REPO)
        self.assertEqual(len(answer["followups"]["unassigned"]), 1)
        self.assertEqual(answer["followups"]["accepted"], [])
        for project in ("PRJ-A", "PRJ-B"):
            scoped = self.regions.show(repository=REPO, project_key=project)
            self.assertEqual(scoped["followups"]["accepted"], [])

    def test_nobody_can_report_done_what_nobody_took(self):
        _record, item = self.with_followup()
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle_followup(
                item["followupId"], actor=self.alpha.task_id, disposition="done")
        self.assertEqual(caught.exception.reason, RefusalReason.FOLLOWUP_UNASSIGNED)

    def test_an_unassigned_follow_up_can_still_be_dropped(self):
        _record, item = self.with_followup()
        dropped = self.regions.settle_followup(
            item["followupId"], actor=self.alpha.task_id, disposition="dropped")
        self.assertEqual(dropped["state"], "dropped")

    def test_accepting_records_who_took_it_and_keeps_everything_else(self):
        _record, item = self.with_followup()
        taken = self.regions.accept_followup(
            item["followupId"], actor=self.beta.task_id, assignee_project="PRJ-B")
        self.assertEqual(taken["assigneeTaskId"], self.beta.task_id)
        self.assertIsNotNone(taken["acceptedAt"])
        self.assertEqual(taken["trigger"], "a temporary duplicate implementation")
        self.assertEqual(
            taken["acceptance"], "the duplicate is gone and one caller remains")
        self.assertEqual(taken["issueRef"], "CRW-200")
        done = self.regions.settle_followup(
            item["followupId"], actor=self.beta.task_id, disposition="done")
        self.assertEqual(done["state"], "done")

    def test_a_follow_up_re_proposed_converges_on_one_record(self):
        record, item = self.with_followup()
        again = self.regions.followup(
            record["agreementId"], trigger_text="a temporary duplicate implementation",
            acceptance_text="restated differently", recorded_by=self.alpha.task_id)
        self.assertEqual(again["followupId"], item["followupId"])
        self.assertEqual(
            again["acceptance"], "the duplicate is gone and one caller remains")

    def test_the_next_owner_survives_the_follow_up_being_accepted(self):
        record, item = self.with_followup()
        self.regions.accept_followup(
            item["followupId"], actor=self.beta.task_id, assignee_project="PRJ-B")
        self.assertEqual(
            self.regions.agreement(record["agreementId"])["nextOwner"], "task-beta")


class AnAgreementIsNotPermission(EditRegionTestCase):
    def merge_answer(self, with_agreement):
        """The same claim sequence on two fresh stores, one of which has an agreement."""
        store = Store(str(self.store.path) + ("-with" if with_agreement else "-without"))
        self.addCleanup(store.close)
        clock = FakeClock()
        linkage = Linkage(store, clock)
        linkage.bind_scope(role=PARENT, scope_key="PRJ-A", endpoint=self.alpha)
        linkage.bind_scope(role=PARENT, scope_key="PRJ-B", endpoint=self.beta)
        if with_agreement:
            link = linkage.register_peer(
                left_project="PRJ-A", left_parent=self.alpha,
                right_project="PRJ-B", right_parent=self.beta)["linkId"]
            regions = EditRegions(store, clock, linkage)
            agreement = regions.propose(
                repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
                left_project="PRJ-A", right_project="PRJ-B", peer_link_id=link,
                proposer_task_id=self.alpha.task_id, constraint_text="keep the signature")
            regions.settle(
                agreement["agreementId"], actor=self.beta.task_id, disposition="accepted")
        turns = MergeTurn(store, clock, linkage)
        record = turns.request(
            repository=REPO, base_ref="dev", project_key="PRJ-A", holder=self.alpha,
            candidate_head="head-a", ready=True)
        return {key: record[key] for key in
                ("state", "holderTaskId", "declaredReady", "projectKey", "candidateHead")}

    def test_the_merge_answer_is_the_same_with_and_without_an_agreed_region(self):
        self.assertEqual(self.merge_answer(True), self.merge_answer(False))

    def test_an_agreement_record_says_it_authorizes_nothing(self):
        record = self.propose("src/a.py")
        self.assertEqual(record["authorizes"], [])
        self.assertFalse(record["grantsMergePermission"])


class TwoPairsProposingOverlappingRegionsAtOnce(EditRegionTestCase):
    """One independent Store per thread, a barrier, bounded joins, errors collected.

    The barrier aligns the starts and the scheduler may still run either to completion first,
    so the assertion holds under every interleaving: one live agreement and one refusal,
    whichever arrives first.
    """

    def propose_in_thread(self, right, link, task, results, errors, barrier):
        def run():
            store = Store(self.store.path)
            try:
                barrier.wait(timeout=20)
                clock = FakeClock()
                regions = EditRegions(store, clock, Linkage(store, clock))
                results[right] = regions.propose(
                    repository=REPO, base_revision=REV, path="src/shared.py",
                    region_kind="file", left_project="PRJ-A", right_project=right,
                    peer_link_id=link, proposer_task_id=task,
                    constraint_text="keep the signature")
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors[right] = error
            finally:
                store.close()

        return threading.Thread(target=run)

    def test_one_proposal_survives_and_the_contest_is_retained(self):
        results, errors = {}, {}
        barrier = threading.Barrier(2)
        threads = [
            self.propose_in_thread(
                "PRJ-B", self.pair, self.alpha.task_id, results, errors, barrier),
            self.propose_in_thread(
                "PRJ-Z", self.other, self.alpha.task_id, results, errors, barrier),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        for thread in threads:
            self.assertFalse(thread.is_alive(), "a thread never finished")
        self.assertEqual(len(results), 1, f"exactly one should survive: {errors}")
        self.assertEqual(len(errors), 1)
        self.assertEqual(list(errors.values())[0].reason, RefusalReason.REGION_OVERLAP)
        live = self.store.all(
            "SELECT * FROM edit_agreements"
            "  WHERE state IN ('proposed','agreed','reopened') AND superseded_by IS NULL", ())
        self.assertEqual(len(live), 1)


if __name__ == "__main__":
    unittest.main()


    def test_a_closed_agreement_admits_no_further_settlement(self):
        record = self.propose("src/a.py")
        self.regions.settle(
            record["agreementId"], actor=self.alpha.task_id, disposition="withdrawn")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_NOT_OPEN)

    def test_a_task_owning_neither_project_cannot_settle(self):
        record = self.propose("src/a.py")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                record["agreementId"], actor=self.zeta.task_id, disposition="accepted")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
