"""Bidirectional queries: the hierarchy, what is missing from it, and what is contended.

The criterion these cases exist for is the last one: a query failure is never converted into
"unregistered" or "complete". Every read here has three distinguishable answers, and the
unreadable one is produced by closing the store's connection so a real sqlite3 error travels the
real path. No fault hook is armed.
"""

import unittest

from codex_session_relay import linkage
from codex_session_relay.assignment import AssignmentView

from .support import CHILD, ISSUE, PARENT

from .test_linkage import (
    INITIATIVE,
    OTHER_PARENT,
    OTHER_PROJECT,
    PROJECT,
    SUPERVISOR_TASK,
    LinkageTestCase,
)


class ThreeLevelsBothWays(LinkageTestCase):
    def setUp(self):
        super().setUp()
        self.supervise()
        self.relationship = self.register()
        self.linkage.attach_issue(self.relationship["relationshipId"], PROJECT)

    def test_a_full_three_level_tree_reads_downward(self):
        answer = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        self.assertEqual(answer["state"], "resolved")
        self.assertEqual(
            [(level["scopeKind"], level["scopeKey"]) for level in answer["levels"]],
            [(linkage.INITIATIVE, INITIATIVE), (linkage.PROJECT, PROJECT),
             (linkage.ISSUE, ISSUE)],
        )
        self.assertEqual(answer["gaps"], [])

    def test_the_same_tree_reads_upward_from_a_child(self):
        answer = self.linkage.up(task_id=CHILD)
        self.assertEqual(answer["state"], "resolved")
        self.assertEqual(
            [level["scopeKey"] for level in answer["levels"]],
            [ISSUE, PROJECT, INITIATIVE],
        )

    def test_every_level_reports_its_real_task_and_host(self):
        answer = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        owners = [(level["owner"]["taskId"], level["owner"]["hostId"])
                  for level in answer["levels"]]
        self.assertEqual([task for task, _host in owners],
                         [SUPERVISOR_TASK, PARENT, CHILD])
        for _task, host in owners:
            self.assertTrue(host, "a level reported no host identifier")

    def test_upward_from_a_relationship_is_the_same_chain(self):
        answer = self.linkage.up(relationship_id=self.relationship["relationshipId"])
        self.assertEqual([level["scopeKey"] for level in answer["levels"]],
                         [ISSUE, PROJECT, INITIATIVE])

    def test_an_unreadable_store_is_unreadable_and_not_unregistered(self):
        self.store.db.close()
        for answer in (self.linkage.down(linkage.INITIATIVE, INITIATIVE),
                       self.linkage.up(task_id=CHILD)):
            self.assertEqual(answer["state"], "unreadable")
            self.assertIs(answer["readable"], False)
            self.assertEqual(answer["levels"], [])
            self.assertEqual(answer["gaps"], [],
                             "an unreadable store must not report gaps it could not read")


class WhatIsMissingIsNamed(LinkageTestCase):
    def test_upward_from_an_unscoped_assignment_reports_the_gap(self):
        self.register()
        answer = self.linkage.up(issue_key=ISSUE)
        self.assertEqual(answer["state"], "unregistered")
        self.assertIs(answer["readable"], True)
        self.assertEqual([gap["gap"] for gap in answer["gaps"]], ["unscoped_assignment"])

    def test_a_project_without_a_parent_is_a_gap_not_an_omission(self):
        self.supervise()
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="handing over", actor="test",
        )
        self.store.db.execute(
            "UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", (PROJECT,))
        answer = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        self.assertEqual(answer["state"], "resolved")
        self.assertIn("project_without_parent", [gap["gap"] for gap in answer["gaps"]])

    def test_a_project_with_no_initiative_above_it_says_so(self):
        self.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                endpoint=self.parent())
        answer = self.linkage.up(task_id=PARENT)
        self.assertIn("no_supervisor", [gap["gap"] for gap in answer["gaps"]])


class ContentionIsVisible(LinkageTestCase):
    def test_a_recorded_conflict_appears_as_contention(self):
        self.supervise()
        with self.assertRaises(Exception):
            self.supervise(initiative="INIT-9",
                           supervisor=self.supervisor("01supervisor-nine"))
        answer = self.linkage.down(linkage.PROJECT, PROJECT)
        self.assertTrue(
            [row for row in answer["contention"]
             if row.get("reason") == "duplicate_scope_owner"],
            "the contest that was refused is not visible in the query",
        )

    def test_an_undisposed_instruction_pair_appears_as_contention(self):
        execution = self.supervise()
        reference = self.supervise(
            initiative="INIT-2", supervisor=self.supervisor("01supervisor-two"),
            kind=linkage.REFERENCE)
        self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-one")
        # Only the execution supervisor may instruct, so a conflict is its successive
        # instructions rather than two origins. The reference is registered anyway, to show it
        # is present and still carries no authority.
        self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-two")
        self.assertEqual(reference["kind"], linkage.REFERENCE)
        answer = self.linkage.down(linkage.PROJECT, PROJECT)
        kinds = [row.get("contention") for row in answer["contention"]]
        self.assertIn("instruction_conflict", kinds)

    def test_owner_drift_is_reported(self):
        self.supervise()
        # A binding moved without the handover that keeps the edge in step.
        self.store.db.execute(
            "UPDATE scope_bindings SET task_id = ? WHERE scope_key = ? AND role = ?",
            (OTHER_PARENT, PROJECT, linkage.PARENT))
        answer = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        drift = [row for row in answer["contention"]
                 if row.get("contention") == "owner_drift"]
        self.assertEqual(len(drift), 1)
        self.assertEqual(drift[0]["recorded"], PARENT)
        self.assertEqual(drift[0]["live"], OTHER_PARENT)


class TheAssignmentViewKnowsItsProject(LinkageTestCase):
    def view(self):
        from codex_session_relay.registry import Registry

        return AssignmentView(self.store, Registry(self.store, self.clock), self.clock)

    def test_for_issue_reports_the_project(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        record = self.view().for_issue(ISSUE)
        self.assertEqual(record["projectKey"], PROJECT)
        self.assertEqual(record["projectParentTaskId"], PARENT)
        self.assertEqual(record["scopeState"], "scoped")

    def test_for_issue_reports_the_unscoped_case_as_a_normal_answer(self):
        self.register()
        record = self.view().for_issue(ISSUE)
        self.assertIsNone(record["projectKey"])
        self.assertEqual(record["scopeState"], "unscoped")
        # Every pre-existing key keeps its meaning.
        self.assertEqual(record["responsibleChild"], CHILD)

    def test_the_project_context_reports_unreadable_rather_than_unscoped(self):
        """Named for the unit it exercises, which is the project lookup and not for_issue.

        for_issue on a closed store raises from its own state() calls long before it reaches
        this, and that is pre-existing behaviour this work did not change and does not claim
        to. What IS claimed is narrower and is what this checks: the project lookup added here
        tells an unreadable store apart from an unscoped assignment instead of reporting the
        first as the second.
        """
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        view = self.view()
        record = view.for_issue(ISSUE)
        self.assertEqual(record["scopeState"], "scoped")
        owning = [{"relationshipId": relationship["relationshipId"]}]
        self.store.db.close()
        self.assertEqual(view._project_context(owning)["scopeState"], "unreadable")


if __name__ == "__main__":
    unittest.main()
