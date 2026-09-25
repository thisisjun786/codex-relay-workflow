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
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
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
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        self.regions.restate_revision(
            repository=REPO, from_revision="rev-2", to_revision="rev-3", actor=self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertFalse(self.regions.current_revision(REPO, "rev-2"))
        self.assertTrue(self.regions.current_revision(REPO, "rev-3"))

    def test_one_revision_has_one_successor(self):
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        replay = self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        self.assertEqual(replay["toRevision"], "rev-2")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision=REV, to_revision="rev-9",
                actor=self.beta.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)

    def test_the_owner_carries_an_agreement_onto_the_current_revision(self):
        record = self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
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


class AuthorityOverAgreementsAndTheWorkTheyImply(EditRegionTestCase):
    def agreed(self, path="src/a.py"):
        record = self.propose(path)
        self.regions.settle(
            record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        return record

    def test_a_refused_proposal_leaves_no_region_behind(self):
        """An orphan region rejected the corrected proposal it existed to describe."""
        with self.assertRaises(CoordinationError):
            self.propose(
                "scripts/components.json", region_class="generated", task=self.zeta.task_id,
                regenerate_from="derive")
        rows = self.store.all("SELECT * FROM edit_regions", ())
        self.assertEqual(rows, [])
        corrected = self.propose("scripts/components.json")
        self.assertEqual(corrected["state"], "proposed")

    def test_a_separator_cannot_enter_a_region_path(self):
        with self.assertRaises(CoordinationError) as caught:
            self.propose("src/a|b.py")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_TOO_BROAD)

    def test_a_parent_records_its_acceptance_under_its_own_project(self):
        record = self.propose("src/a.py")
        item = self.regions.followup(
            record["agreementId"], trigger_text="t", acceptance_text="a",
            recorded_by=self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.regions.accept_followup(
                item["followupId"], actor=self.beta.task_id, assignee_project="PRJ-A")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_failed_reaffirmation_leaves_the_predecessor_live(self):
        """Retiring first traded two live agreements for none."""
        record = self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2",
            actor=self.alpha.task_id)
        self.propose("src/a.py", revision="rev-2", right="PRJ-Z", link=self.other)
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)
        self.assertEqual(
            self.regions.agreement(record["agreementId"])["state"], "reopened",
            "the predecessor survives a refused carry-forward")

    def test_a_proposer_pre_accepts_its_own_side_whichever_argument_it_used(self):
        """Reversed arguments made a proposer pre-accept the PEER's side.

        It could then accept the remaining one itself and hold both, which is an agreement
        with one participant.
        """
        reversed_order = self.regions.propose(
            repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
            left_project="PRJ-B", right_project="PRJ-A", peer_link_id=self.pair,
            proposer_task_id=self.alpha.task_id, constraint_text="keep the signature")
        # PRJ-A sorts low, and alpha owns it, so alpha's acceptance is the left one.
        self.assertIsNotNone(reversed_order["leftAcceptedAt"])
        self.assertIsNone(reversed_order["rightAcceptedAt"])
        # Accepting again only restates its own side, so one parent cannot complete both.
        self.regions.settle(
            reversed_order["agreementId"], actor=self.alpha.task_id, disposition="accepted")
        still = self.regions.agreement(reversed_order["agreementId"])
        self.assertEqual(still["state"], "proposed")
        self.assertIsNone(still["rightAcceptedAt"])
        self.regions.settle(
            reversed_order["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertEqual(
            self.regions.agreement(reversed_order["agreementId"])["state"], "agreed")

    def test_losing_ownership_mid_proposal_refuses_rather_than_taking_the_peers_side(self):
        """The last fix's own race.

        With owned resolved to None the side fell through to the high project, so a proposer
        that had just stopped owning the low one pre-accepted the PEER's side - the very bug
        that line was added to prevent, one step later.
        """
        original = self.regions._owned_side
        calls = {"n": 0}

        def vanishing(low, high, actor):
            calls["n"] += 1
            return original(low, high, actor) if calls["n"] == 1 else None

        self.regions._owned_side = vanishing
        try:
            with self.assertRaises(CoordinationError) as caught:
                self.propose("src/a.py")
        finally:
            self.regions._owned_side = original
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(self.store.all("SELECT * FROM edit_agreements", ()), [])


    def test_a_closed_agreement_is_proposed_again_rather_than_carried_forward(self):
        record = self.propose("src/a.py")
        self.regions.settle(
            record["agreementId"], actor=self.alpha.task_id, disposition="withdrawn")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision=REV)
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_NOT_OPEN)

    def test_a_blocked_reaffirmation_keeps_the_contest_it_recorded(self):
        """Raising inside the transaction rolled back the row recording the contest."""
        record = self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2",
            actor=self.alpha.task_id)
        self.propose("src/a.py", revision="rev-2", right="PRJ-Z", link=self.other)
        with self.assertRaises(CoordinationError):
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        contests = self.regions.show(repository=REPO)["conflicts"]
        self.assertTrue(
            [c for c in contests if c["reason"] == "region_overlap"],
            "the contest survives the refusal that recorded it")


    def test_a_stranger_cannot_propose_an_agreement_between_two_other_projects(self):
        """A proposal pre-accepts its own side, so a forged one blocks an overlapping region."""
        with self.assertRaises(CoordinationError) as caught:
            self.propose("src/a.py", task=self.zeta.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_key_belongs_to_a_symbol_or_data_region_and_to_nothing_else(self):
        with self.assertRaises(CoordinationError) as caught:
            self.propose("src/a.py", kind="file", key="parse")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_TOO_BROAD)
        with self.assertRaises(CoordinationError) as caught:
            self.propose("src/a.py", kind="symbol")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_TOO_BROAD)

    def test_a_place_is_classified_once(self):
        self.propose(
            "scripts/components.json", region_class="generated", regenerate_from="derive")
        with self.assertRaises(CoordinationError) as caught:
            self.propose("scripts/components.json", right="PRJ-Z", link=self.other)
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)

    def test_a_stranger_cannot_append_work_to_an_agreement(self):
        record = self.propose("src/a.py")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.followup(
                record["agreementId"], trigger_text="t", acceptance_text="a",
                recorded_by=self.zeta.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_terminal_follow_up_does_not_change_its_disposition(self):
        record = self.propose("src/a.py")
        item = self.regions.followup(
            record["agreementId"], trigger_text="t", acceptance_text="a",
            recorded_by=self.alpha.task_id)
        self.regions.settle_followup(
            item["followupId"], actor=self.alpha.task_id, disposition="dropped")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle_followup(
                item["followupId"], actor=self.alpha.task_id, disposition="done")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_NOT_OPEN)

    def test_a_stranger_cannot_restate_a_repositorys_revision(self):
        """Restating reopens every agreement on that revision, so it is not anybody's call."""
        self.agreed("src/a.py")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision=REV, to_revision="rev-2",
                actor=self.zeta.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_revision_cycle_is_refused_so_some_tree_stays_current(self):
        """A to B then B to A left BOTH marked, so nothing was current at all."""
        self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2",
            actor=self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision="rev-2", to_revision=REV,
                actor=self.alpha.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertTrue(self.regions.current_revision(REPO, "rev-2"))

    def test_reaffirming_onto_a_revision_the_chain_never_reached_is_refused(self):
        record = self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2",
            actor=self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision="rev-typo")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)

    def test_reaffirming_leaves_exactly_one_live_agreement(self):
        record = self.agreed("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2",
            actor=self.alpha.task_id)
        successor = self.regions.reaffirm(
            record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        live = self.store.all(
            "SELECT agreement_id FROM edit_agreements"
            "  WHERE state IN ('proposed','agreed','reopened') AND superseded_by IS NULL", ())
        self.assertEqual([row["agreement_id"] for row in live], [successor["agreementId"]])

    def test_a_caller_cannot_accept_a_follow_up_for_another_task(self):
        record = self.propose("src/a.py")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.followup(
                record["agreementId"], trigger_text="t", acceptance_text="a",
                recorded_by=self.alpha.task_id, assignee_task_id=self.beta.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_peer_cannot_report_another_assignees_work_done(self):
        record = self.propose("src/a.py")
        item = self.regions.followup(
            record["agreementId"], trigger_text="t", acceptance_text="a",
            recorded_by=self.alpha.task_id)
        self.regions.accept_followup(
            item["followupId"], actor=self.beta.task_id, assignee_project="PRJ-B")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle_followup(
                item["followupId"], actor=self.alpha.task_id, disposition="done")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_closed_follow_up_cannot_be_reopened_by_accepting_it(self):
        record = self.propose("src/a.py")
        item = self.regions.followup(
            record["agreementId"], trigger_text="t", acceptance_text="a",
            recorded_by=self.alpha.task_id)
        self.regions.settle_followup(
            item["followupId"], actor=self.alpha.task_id, disposition="dropped")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.accept_followup(
                item["followupId"], actor=self.beta.task_id, assignee_project="PRJ-B")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_NOT_OPEN)


class AReaffirmationCarriesWhatWasAgreed(EditRegionTestCase):
    """CRW-237. Carrying an agreement onto a moved base must not rewrite it.

    CRW-124 G3 found the answering side's reaffirmation making itself the proposer, dropping both
    sides' conditions and clearing the other side's acceptance without a word, while the constraint
    kept line numbers of a tree the successor no longer stood on.
    """

    BETA_CONDITION = "beta publishes parse() and restates this before renaming it"
    CONSTRAINT = "keep parse() at src/a.py lines 12-13 at rev-1"

    def proposed_by_beta(self, condition=BETA_CONDITION):
        return self.regions.propose(
            repository=REPO, base_revision=REV, path="src/a.py", region_kind="file",
            left_project="PRJ-B", right_project="PRJ-A", peer_link_id=self.pair,
            proposer_task_id=self.beta.task_id, constraint_text=self.CONSTRAINT,
            condition=condition, issue_key="CRW-1", next_owner=self.alpha.task_id)

    def moved(self, *revisions):
        previous = REV
        for revision in revisions:
            self.regions.restate_revision(
                repository=REPO, from_revision=previous, to_revision=revision,
                actor=self.beta.task_id)
            previous = revision

    def carries(self):
        return self.store.all("SELECT * FROM edit_reaffirmations", ())

    def racing(self, concurrent):
        """Land ``concurrent`` after reaffirm's validation and before its write.

        reaffirm validates in one transaction and writes in propose()'s, so wrapping propose puts
        the concurrent write exactly in the gap a second parent could reach.
        """
        original = self.regions.propose

        def interleaved(**arguments):
            self.regions.propose = original
            concurrent()
            return original(**arguments)

        self.regions.propose = interleaved
        self.addCleanup(vars(self.regions).pop, "propose", None)

    def test_the_answering_side_reaffirming_keeps_proposer_conditions_and_constraint(self):
        """The G3 order: propose, two recorded moves, a refused late acceptance, reaffirm, accept."""
        original = self.proposed_by_beta()
        self.moved("rev-2", "rev-3")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                original["agreementId"], actor=self.alpha.task_id, disposition="accepted")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-3")
        self.assertEqual(successor["proposerTaskId"], self.beta.task_id)
        self.assertEqual(successor["rightCondition"], self.BETA_CONDITION)
        self.assertIsNone(successor["leftCondition"])
        self.assertEqual(successor["constraintText"], self.CONSTRAINT)
        self.assertEqual(
            successor["statedOn"],
            {"constraint": REV, "leftCondition": None, "rightCondition": REV})
        self.assertEqual(
            successor["textFromEarlierRevision"], ["constraint", "rightCondition"])
        self.assertIsNone(successor["legacyCarry"])
        self.assertIsNotNone(successor["leftAcceptedAt"], "reaffirming accepts the carrier's side")
        self.assertIsNone(successor["rightAcceptedAt"])
        carried = successor["reaffirmation"]
        self.assertEqual(
            (carried["predecessor"], carried["actor"], carried["fromRevision"],
             carried["toRevision"]),
            (original["agreementId"], self.alpha.task_id, REV, "rev-3"))
        self.assertEqual(carried["awaitingAcceptance"], {
            "project": "PRJ-B", "task": self.beta.task_id,
            "reason": "acceptance_on_prior_revision",
            "priorAcceptedAt": original["rightAcceptedAt"], "priorRevision": REV,
            "command": "region-settle --agreement " + successor["agreementId"]
                       + " --actor " + self.beta.task_id + " --disposition accepted",
            "precondition": None,
        })
        self.assertEqual(successor["nextOwner"], self.beta.task_id)
        self.assertEqual(
            self.regions.agreement(original["agreementId"])["supersededBy"],
            successor["agreementId"])
        agreed = self.regions.settle(
            successor["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertEqual(agreed["state"], "agreed")
        self.assertEqual(agreed["rightCondition"], self.BETA_CONDITION)
        self.assertIsNone(agreed["reaffirmation"]["awaitingAcceptance"])

    def test_the_proposing_side_reaffirming_is_the_control(self):
        original = self.proposed_by_beta()
        self.moved("rev-2")
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.beta.task_id, base_revision="rev-2")
        self.assertEqual(successor["proposerTaskId"], self.beta.task_id)
        self.assertEqual(successor["rightCondition"], self.BETA_CONDITION)
        self.assertIsNotNone(successor["rightAcceptedAt"])
        self.assertIsNone(successor["leftAcceptedAt"])
        awaiting = successor["reaffirmation"]["awaitingAcceptance"]
        self.assertEqual(
            (awaiting["project"], awaiting["task"], awaiting["reason"],
             awaiting["priorAcceptedAt"]),
            ("PRJ-A", self.alpha.task_id, "not_yet_accepted", None))
        self.assertEqual(successor["nextOwner"], self.alpha.task_id)

    def test_an_agreed_agreement_carried_forward_asks_the_other_side_again(self):
        original = self.proposed_by_beta()
        self.regions.settle(
            original["agreementId"], actor=self.alpha.task_id, disposition="accepted")
        self.moved("rev-2")
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        awaiting = successor["reaffirmation"]["awaitingAcceptance"]
        self.assertEqual(
            (awaiting["task"], awaiting["reason"]),
            (self.beta.task_id, "acceptance_on_prior_revision"))
        self.assertEqual(successor["state"], "proposed")

    def test_the_reaffirming_side_restates_only_its_own_condition(self):
        original = self.proposed_by_beta()
        self.moved("rev-2")
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2",
            condition="alpha's condition, lines 14-15 at rev-2")
        self.assertEqual(successor["leftCondition"], "alpha's condition, lines 14-15 at rev-2")
        self.assertEqual(successor["rightCondition"], self.BETA_CONDITION)
        self.assertEqual(
            successor["statedOn"],
            {"constraint": REV, "leftCondition": "rev-2", "rightCondition": REV})
        self.assertEqual(
            successor["textFromEarlierRevision"], ["constraint", "rightCondition"])

    def test_both_conditions_survive_a_second_carry(self):
        original = self.proposed_by_beta()
        self.moved("rev-2")
        first = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2",
            condition="alpha's condition at rev-2")
        self.regions.restate_revision(
            repository=REPO, from_revision="rev-2", to_revision="rev-3",
            actor=self.alpha.task_id)
        second = self.regions.reaffirm(
            first["agreementId"], actor=self.beta.task_id, base_revision="rev-3")
        self.assertEqual(
            (second["leftCondition"], second["rightCondition"]),
            ("alpha's condition at rev-2", self.BETA_CONDITION))
        self.assertEqual(
            second["statedOn"],
            {"constraint": REV, "leftCondition": "rev-2", "rightCondition": REV})
        self.assertEqual(
            second["textFromEarlierRevision"],
            ["constraint", "leftCondition", "rightCondition"])
        self.assertEqual(second["proposerTaskId"], self.beta.task_id)

    def test_a_decline_after_a_carry_states_its_condition_on_the_current_revision(self):
        original = self.proposed_by_beta()
        self.moved("rev-2")
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        declined = self.regions.settle(
            successor["agreementId"], actor=self.beta.task_id, disposition="declined",
            condition="only if parse keeps forwarding", reason="the tree moved")
        self.assertEqual(declined["rightCondition"], "only if parse keeps forwarding")
        self.assertEqual(declined["statedOn"]["rightCondition"], "rev-2")

    def test_a_conditionless_decline_after_a_carry_states_no_revision(self):
        original = self.proposed_by_beta()
        self.moved("rev-2")
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        declined = self.regions.settle(
            successor["agreementId"], actor=self.beta.task_id, disposition="declined",
            reason="changed our mind")
        self.assertIsNone(declined["rightCondition"])
        self.assertIsNone(declined["statedOn"]["rightCondition"])

    def test_reaffirming_onto_the_revision_it_already_stands_on_is_refused(self):
        original = self.proposed_by_beta()
        self.regions.settle(
            original["agreementId"], actor=self.alpha.task_id, disposition="accepted")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                original["agreementId"], actor=self.alpha.task_id, base_revision=REV)
        self.assertEqual(caught.exception.reason, RefusalReason.LINK_NOT_ACTIVE)
        still = self.regions.agreement(original["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("agreed", None))
        self.assertIsNotNone(still["rightAcceptedAt"], "nobody's acceptance was cleared")

    def test_a_carry_onto_a_place_the_pair_already_holds_is_refused_before_retiring(self):
        original = self.proposed_by_beta()
        self.moved("rev-2")
        standing = self.propose("src/a.py", revision="rev-2")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)
        self.assertIn(standing["agreementId"], caught.exception.detail)
        still = self.regions.agreement(original["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("reopened", None))
        self.assertEqual(self.carries(), [])

    def test_a_move_recorded_while_carrying_refuses_and_keeps_the_predecessor(self):
        record = self.proposed_by_beta()
        self.moved("rev-2")
        self.racing(lambda: self.regions.restate_revision(
            repository=REPO, from_revision="rev-2", to_revision="rev-3",
            actor=self.beta.task_id))
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertIn("rev-3", caught.exception.detail)
        still = self.regions.agreement(record["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("reopened", None))
        self.assertEqual(self.carries(), [])
        self.assertEqual(
            self.store.all(
                "SELECT * FROM edit_agreements WHERE base_revision = 'rev-2'", ()), [])
        contests = self.regions.show(repository=REPO)["conflicts"]
        self.assertEqual(
            [(c["incumbent"], c["challenger"]) for c in contests
             if c["reason"] == "agreement_revision_stale"],
            [("rev-3", "rev-2")], "the refusal was recorded, not only raised")

    def test_a_same_pair_proposal_arriving_while_carrying_refuses_and_keeps_the_predecessor(self):
        record = self.proposed_by_beta()
        self.moved("rev-2")
        standing = {}
        self.racing(lambda: standing.update(self.propose("src/a.py", revision="rev-2")))
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)
        still = self.regions.agreement(record["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("reopened", None))
        self.assertEqual(self.carries(), [])
        self.assertEqual(
            self.regions.agreement(standing["agreementId"])["reaffirmation"], None,
            "nothing was carried onto the agreement that arrived")
        contests = self.regions.show(repository=REPO)["conflicts"]
        self.assertIn(
            standing["agreementId"],
            [c["incumbent"] for c in contests if c["reason"] == "region_overlap"])

    def test_a_carry_whose_destination_is_not_the_predecessors_place_is_refused(self):
        """A carry retires its predecessor, so where it lands is checked against that row."""
        record = self.proposed_by_beta()
        self.moved("rev-2")
        for path, right, link in (("src/b.py", "PRJ-B", self.pair),
                                  ("src/a.py", "PRJ-Z", self.other)):
            with self.assertRaises(CoordinationError) as caught:
                self.regions.propose(
                    repository=REPO, base_revision="rev-2", path=path, region_kind="file",
                    left_project="PRJ-A", right_project=right, peer_link_id=link,
                    proposer_task_id=self.alpha.task_id, constraint_text="elsewhere",
                    supersedes=record["agreementId"],
                    carry={"predecessor": record["agreementId"], "restated": None})
            self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_SCOPE, path)
        still = self.regions.agreement(record["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("reopened", None))
        self.assertEqual(self.carries(), [])
        self.assertEqual(
            self.store.all(
                "SELECT * FROM edit_agreements WHERE base_revision = 'rev-2'", ()), [])

    def test_a_handover_while_carrying_is_refused_recorded_and_keeps_the_predecessor(self):
        """Third review: the carrier lost its project between validation and write, and the
        refusal was raised before the write transaction, so nothing recorded it."""
        record = self.proposed_by_beta()
        self.moved("rev-2")
        self.racing(lambda: self.linkage.handover(
            role=PARENT, scope_key="PRJ-A", expect_task_id=self.alpha.task_id,
            endpoint=Endpoint("task-alpha-next", "host-a"), acknowledged=[],
            evidence="project A changed hands during the carry", actor="test"))
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                record["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        still = self.regions.agreement(record["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("reopened", None))
        self.assertEqual(self.carries(), [])
        contests = self.regions.show(repository=REPO)["conflicts"]
        self.assertEqual(
            [c["challenger"] for c in contests if c["reason"] == "scope_role_mismatch"],
            [self.alpha.task_id], "the refusal was recorded, not only raised")
        self.assertEqual(
            self.store.all("SELECT * FROM edit_regions WHERE base_revision = 'rev-2'", ()), [],
            "fourth review: the refusal came after the region insert and left an orphan region")

    def test_an_acceptance_carrying_a_condition_is_refused_rather_than_dropped(self):
        original = self.proposed_by_beta()
        with self.assertRaises(CoordinationError) as caught:
            self.regions.settle(
                original["agreementId"], actor=self.alpha.task_id, disposition="accepted",
                condition="alpha's terms")
        self.assertEqual(caught.exception.reason, RefusalReason.LINK_NOT_ACTIVE)
        self.assertIsNone(self.regions.agreement(original["agreementId"])["leftAcceptedAt"])

    def test_a_successor_carried_before_carries_were_recorded_says_where_its_text_was_written(self):
        """Fourth review: a successor the earlier reaffirm left in a store has no carry row.

        Its constraint was copied verbatim from the agreement it supersedes, so reading it as
        written on its own revision hid exactly the stale line numbers criterion 2 is about.
        """
        original = self.proposed_by_beta()
        self.moved("rev-2")
        # What that reaffirm wrote: an ordinary proposal on the new revision naming what it
        # supersedes and copying the constraint, with nothing recorded about the carry.
        legacy = self.regions.propose(
            repository=REPO, base_revision="rev-2", path="src/a.py", region_kind="file",
            left_project="PRJ-A", right_project="PRJ-B", peer_link_id=self.pair,
            proposer_task_id=self.alpha.task_id, constraint_text=self.CONSTRAINT,
            issue_key="CRW-1", supersedes=original["agreementId"])
        self.assertEqual(legacy["statedOn"]["constraint"], REV)
        self.assertEqual(legacy["textFromEarlierRevision"], ["constraint"])
        # Fifth review: that carry also made its caller the proposer and dropped both
        # conditions; the agreement it came from still holds them, and a read says so.
        self.assertEqual(legacy["legacyCarry"], {
            "origin": original["agreementId"], "proposerTaskId": self.beta.task_id,
            "leftCondition": None, "rightCondition": self.BETA_CONDITION})
        shown = [r for r in self.regions.show(repository=REPO)["exclusive"]
                 if r["agreementId"] == legacy["agreementId"]]
        self.assertEqual(shown[0]["statedOn"]["constraint"], REV)
        self.assertEqual(shown[0]["legacyCarry"], legacy["legacyCarry"])
        self.regions.restate_revision(
            repository=REPO, from_revision="rev-2", to_revision="rev-3",
            actor=self.beta.task_id)
        carried = self.regions.reaffirm(
            legacy["agreementId"], actor=self.alpha.task_id, base_revision="rev-3")
        self.assertEqual(carried["statedOn"]["constraint"], REV)
        # ... and carrying it again brings the original terms back rather than the lost ones.
        self.assertEqual(
            (carried["proposerTaskId"], carried["rightCondition"],
             carried["statedOn"]["rightCondition"], carried["legacyCarry"]),
            (self.beta.task_id, self.BETA_CONDITION, REV, None))
        self.assertEqual(
            carried["reaffirmation"]["awaitingAcceptance"]["task"], self.beta.task_id)
        self.assertEqual(carried["nextOwner"], self.beta.task_id)

    def test_the_next_owner_follows_a_handover_while_the_carry_waits(self):
        """Review of the first head: nextOwner kept naming a parent that had handed over."""
        original = self.proposed_by_beta()
        self.moved("rev-2")
        successor = self.regions.reaffirm(
            original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.linkage.handover(
            role=PARENT, scope_key="PRJ-B", expect_task_id=self.beta.task_id,
            endpoint=Endpoint("task-beta-next", "host-b"), acknowledged=[],
            evidence="project B changed hands after the carry", actor="test")
        waiting = self.regions.agreement(successor["agreementId"])
        awaiting = waiting["reaffirmation"]["awaitingAcceptance"]
        self.assertEqual(
            (waiting["nextOwner"], awaiting["task"]), ("task-beta-next", "task-beta-next"))
        self.assertIn("--actor task-beta-next", awaiting["command"])
        agreed = self.regions.settle(
            successor["agreementId"], actor="task-beta-next", disposition="accepted")
        self.assertEqual(agreed["state"], "agreed")

    def test_a_classification_clash_on_the_new_revision_is_refused_and_recorded(self):
        """Review of the first head: this refusal was raised inside its transaction, unrecorded."""
        original = self.proposed_by_beta()
        self.moved("rev-2")
        self.propose("src/a.py", revision="rev-2", right="PRJ-Z", link=self.other,
                     region_class="generated", regenerate_from="derive")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.reaffirm(
                original["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        self.assertEqual(caught.exception.reason, RefusalReason.REGION_OVERLAP)
        still = self.regions.agreement(original["agreementId"])
        self.assertEqual((still["state"], still["supersededBy"]), ("reopened", None))
        self.assertEqual(self.carries(), [])
        contests = self.regions.show(repository=REPO)["conflicts"]
        self.assertEqual(
            [c["challenger"] for c in contests if c["reason"] == "region_overlap"], ["source"])


class ABaseMoveChainsFromTheLastRecordedRevision(EditRegionTestCase):
    """CRW-237. One revision has one successor, so only the first move starts at the proposal."""

    def test_consecutive_moves_chain_from_the_end_of_the_recorded_chain(self):
        record = self.propose("src/a.py")
        first = self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        self.assertEqual(
            (first["alreadyRecorded"], first["currentRevision"]), (False, "rev-2"))
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision=REV, to_revision="rev-3",
                actor=self.alpha.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertIn("--from-revision rev-2 --to-revision rev-3", caught.exception.detail)
        chained = self.regions.restate_revision(
            repository=REPO, from_revision="rev-2", to_revision="rev-3",
            actor=self.alpha.task_id)
        self.assertEqual(
            (chained["alreadyRecorded"], chained["currentRevision"]), (False, "rev-3"))
        again = self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-3", actor=self.beta.task_id)
        self.assertEqual(
            (again["alreadyRecorded"], again["currentRevision"]), (True, "rev-3"))
        self.assertEqual(
            len(self.store.all("SELECT * FROM edit_revision_marks", ())), 2,
            "a move the chain already holds writes no mark")
        shown = self.regions.show(repository=REPO)["exclusive"]
        self.assertEqual([r["currentRevision"] for r in shown], ["rev-3"])
        with self.assertRaises(CoordinationError) as stale:
            self.regions.settle(
                record["agreementId"], actor=self.beta.task_id, disposition="accepted")
        self.assertIn("--revision rev-3", stale.exception.detail)
        with self.assertRaises(CoordinationError) as short:
            self.regions.reaffirm(
                record["agreementId"], actor=self.beta.task_id, base_revision="rev-2")
        self.assertEqual(short.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        successor = self.regions.reaffirm(
            record["agreementId"], actor=self.beta.task_id, base_revision="rev-3")
        self.assertEqual(
            (successor["baseRevision"], successor["currentRevision"]), ("rev-3", "rev-3"))

    def test_a_move_from_a_revision_nothing_stands_on_is_refused(self):
        """Review of the first head: after A->B, C->D was recorded and never reached A."""
        self.propose("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision="rev-9", to_revision="rev-10",
                actor=self.alpha.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertIn("'rev-2'", caught.exception.detail)
        self.assertEqual(len(self.store.all("SELECT * FROM edit_revision_marks", ())), 1)
        contests = self.regions.show(repository=REPO)["conflicts"]
        self.assertIn(
            ("rev-2", "rev-9"), [(c["incumbent"], c["challenger"]) for c in contests])

    def test_an_agreement_on_another_revision_starts_its_own_chain(self):
        self.propose("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        self.propose("src/b.py", revision="other-1")
        moved = self.regions.restate_revision(
            repository=REPO, from_revision="other-1", to_revision="other-2",
            actor=self.alpha.task_id)
        self.assertEqual(
            (moved["alreadyRecorded"], moved["currentRevision"]), (False, "other-2"))

    def test_a_closed_agreement_does_not_make_its_revision_a_starting_point(self):
        """Second review: a withdrawn agreement's revision let the wrong move start anyway.

        The live agreement's chain stayed where it was, so its late acceptance went through on a
        tree that had moved.
        """
        closed = self.propose("src/old.py", revision="closed-1")
        self.regions.settle(
            closed["agreementId"], actor=self.alpha.task_id, disposition="withdrawn")
        live = self.propose("src/a.py")
        self.regions.restate_revision(
            repository=REPO, from_revision=REV, to_revision="rev-2", actor=self.alpha.task_id)
        successor = self.regions.reaffirm(
            live["agreementId"], actor=self.alpha.task_id, base_revision="rev-2")
        with self.assertRaises(CoordinationError) as caught:
            self.regions.restate_revision(
                repository=REPO, from_revision="closed-1", to_revision="rev-3",
                actor=self.alpha.task_id)
        self.assertEqual(caught.exception.reason, RefusalReason.AGREEMENT_REVISION_STALE)
        self.assertIn("'rev-2'", caught.exception.detail)
        self.assertEqual(len(self.store.all("SELECT * FROM edit_revision_marks", ())), 1)
        self.assertEqual(
            self.regions.agreement(successor["agreementId"])["currentRevision"], "rev-2")


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
