"""Peer parent links, and what a message can establish about its counterpart.

A peer relation is symmetric and confers nothing. These cases hold it to that: registering it
from either side converges, it survives a handover as one record, and it changes no execution
edge. The counterpart lookup is held to the other half of the contract - it answers with the
real task and host identifiers, it names every problem it can see rather than the first, and a
failure to READ the store is never reported as an absence.
"""

import unittest

from codex_session_relay import linkage
from codex_session_relay.errors import RefusalReason
from codex_session_relay.linkage import link_id

from .support import CHILD, PARENT

from .test_linkage import (
    INITIATIVE,
    OTHER_PARENT,
    OTHER_PROJECT,
    PROJECT,
    SUPERVISOR_TASK,
    LinkageTestCase,
)

THIRD_PROJECT = "PROJ-3"
THIRD_PARENT = "01parent-three"


class PeerLinks(LinkageTestCase):
    def setUp(self):
        super().setUp()
        # One supervisor, two projects, two parents. The shape OPS-7.4 describes.
        self.supervise()
        self.supervise(project=OTHER_PROJECT, parent=self.parent(OTHER_PARENT),
                       kind=linkage.REFERENCE)

    def peer(self):
        return self.linkage.register_peer(
            left_project=PROJECT, left_parent=self.parent(),
            right_project=OTHER_PROJECT, right_parent=self.parent(OTHER_PARENT),
        )

    def execution_edges(self):
        return sorted(
            (row["link_id"], row["lower_key"], row["lower_task_id"])
            for row in self.store.all(
                "SELECT * FROM scope_links WHERE link_kind = 'execution'"
                "  AND status IN ('active','paused')"
            )
        )

    def test_a_peer_link_registered_from_either_side_is_one_record(self):
        first = self.peer()
        mirrored = self.linkage.register_peer(
            left_project=OTHER_PROJECT, left_parent=self.parent(OTHER_PARENT),
            right_project=PROJECT, right_parent=self.parent(),
        )
        self.assertEqual(first["linkId"], mirrored["linkId"])
        self.assertEqual(
            len(self.store.all("SELECT link_id FROM scope_links WHERE link_kind = 'peer'")), 1)

    def test_a_peer_link_survives_a_handover_as_one_record(self):
        before = self.peer()["linkId"]
        self.linkage.handover(
            role=linkage.PARENT, scope_key=OTHER_PROJECT, expect_task_id=OTHER_PARENT,
            endpoint=self.parent(THIRD_PARENT), acknowledged=[],
            evidence="the peer project changed hands", actor="test",
        )
        again = self.linkage.register_peer(
            left_project=PROJECT, left_parent=self.parent(),
            right_project=OTHER_PROJECT, right_parent=self.parent(THIRD_PARENT),
        )
        self.assertEqual(again["linkId"], before)
        self.assertEqual(
            len(self.store.all("SELECT link_id FROM scope_links WHERE link_kind = 'peer'")), 1)

    def test_a_peer_link_adds_no_level_to_the_hierarchy(self):
        """The walk itself, not the rows it reads from.

        Comparing execution rows would pass even if up() and down() traversed peer edges,
        because adding a peer row does not change the execution rows. So this compares the
        answers the walk actually gives.
        """
        rows_before = self.execution_edges()
        down_before = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        up_before = self.linkage.up(task_id=PARENT)
        self.assertTrue(down_before["levels"], "the fixture produced no hierarchy to compare")
        self.peer()
        self.assertEqual(self.execution_edges(), rows_before)
        self.assertEqual(self.linkage.down(linkage.INITIATIVE, INITIATIVE), down_before)
        self.assertEqual(self.linkage.up(task_id=PARENT), up_before)

    def test_two_parents_under_one_supervisor_keep_one_execution_owner_each(self):
        self.peer()
        for project, owner in ((PROJECT, PARENT), (OTHER_PROJECT, OTHER_PARENT)):
            live = self.store.all(
                "SELECT task_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ?"
                "  AND role = ? AND status IN ('active','paused')",
                (linkage.PROJECT, project, linkage.PARENT),
            )
            self.assertEqual([row["task_id"] for row in live], [owner])

    def test_a_project_is_not_its_own_peer(self):
        self.assertRefused(
            RefusalReason.SCOPE_CYCLE, self.linkage.register_peer,
            left_project=PROJECT, left_parent=self.parent(),
            right_project=PROJECT, right_parent=self.parent(),
        )

    def test_a_peer_endpoint_that_is_not_a_project_parent_is_refused(self):
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.linkage.register_peer,
            left_project=PROJECT, left_parent=self.parent(),
            right_project=OTHER_PROJECT, right_parent=self.parent(THIRD_PARENT),
        )

    def test_a_peer_endpoint_naming_an_unregistered_project_is_refused(self):
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.linkage.register_peer,
            left_project=PROJECT, left_parent=self.parent(),
            right_project=THIRD_PROJECT, right_parent=self.parent(THIRD_PARENT),
        )


class WhatAMessageCanEstablish(LinkageTestCase):
    def setUp(self):
        super().setUp()
        self.supervise()
        self.supervise(project=OTHER_PROJECT, parent=self.parent(OTHER_PARENT),
                       kind=linkage.REFERENCE)
        self.peer_link = self.linkage.register_peer(
            left_project=PROJECT, left_parent=self.parent(),
            right_project=OTHER_PROJECT, right_parent=self.parent(OTHER_PARENT),
        )

    def test_a_message_reads_the_current_revision_and_the_real_counterpart(self):
        answer = self.linkage.counterpart(PARENT, OTHER_PARENT)
        self.assertEqual(answer["state"], "linked")
        self.assertEqual(answer["link"]["kind"], linkage.PEER)
        self.assertEqual(answer["link"]["revision"], 1)
        self.assertEqual(answer["counterpart"]["taskId"], OTHER_PARENT)
        self.assertEqual(answer["counterpart"]["hostId"], self.parent(OTHER_PARENT).host_id)
        self.assertEqual(answer["counterpart"]["scopeKey"], OTHER_PROJECT)
        self.assertEqual(answer["findings"], [])

    def test_a_message_quoting_an_older_revision_is_told_so(self):
        self.linkage.handover(
            role=linkage.PARENT, scope_key=OTHER_PROJECT, expect_task_id=OTHER_PARENT,
            endpoint=self.parent(THIRD_PARENT), acknowledged=[],
            evidence="the peer project changed hands", actor="test",
        )
        answer = self.linkage.counterpart(PARENT, THIRD_PARENT, quoted_revision=1)
        self.assertIn("stale_revision", answer["findings"])
        self.assertGreater(answer["link"]["revision"], 1)

    def test_a_message_naming_a_replaced_owner_is_told_who_holds_the_scope_now(self):
        self.linkage.handover(
            role=linkage.PARENT, scope_key=OTHER_PROJECT, expect_task_id=OTHER_PARENT,
            endpoint=self.parent(THIRD_PARENT), acknowledged=[],
            evidence="the peer project changed hands", actor="test",
        )
        answer = self.linkage.counterpart(PARENT, OTHER_PARENT)
        self.assertIn("stale_owner", answer["findings"])
        self.assertEqual(answer["currentOwner"]["taskId"], THIRD_PARENT)

    def test_a_message_naming_another_scope_is_told_so(self):
        answer = self.linkage.counterpart(PARENT, OTHER_PARENT, quoted_scope="PROJ-NOPE")
        self.assertIn("foreign_scope", answer["findings"])

    def test_a_message_from_a_replaced_owner_is_told_so_too(self):
        """Staleness was checked on the recipient only.

        A message FROM a task that no longer owns its scope is exactly as misrouted as one
        addressed to a replaced owner, and reporting only the recipient let an archived sender
        read as a healthy relationship with no findings at all.
        """
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(THIRD_PARENT), acknowledged=[],
            evidence="the sending project changed hands", actor="test",
        )
        answer = self.linkage.counterpart(PARENT, OTHER_PARENT)
        self.assertIn("stale_sender", answer["findings"])

    def test_a_supervisor_addressing_a_child_directly_is_a_wrong_role(self):
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.counterpart(SUPERVISOR_TASK, CHILD)
        self.assertIn("wrong_role", answer["findings"])

    def test_a_supervisor_addressing_its_own_parent_is_not(self):
        answer = self.linkage.counterpart(SUPERVISOR_TASK, PARENT)
        self.assertEqual(answer["state"], "linked")
        self.assertNotIn("wrong_role", answer["findings"])

    def test_a_message_to_an_unregistered_task_is_unlinked_not_unreadable(self):
        answer = self.linkage.counterpart(PARENT, "01nobody-at-all")
        self.assertEqual(answer["state"], "unlinked")
        self.assertIs(answer["readable"], True)
        self.assertIn("unregistered_link", answer["findings"])

    def test_an_unreadable_store_makes_a_lookup_unreadable_not_unlinked(self):
        self.store.db.close()
        answer = self.linkage.counterpart(PARENT, OTHER_PARENT)
        self.assertEqual(answer["state"], "unreadable")
        self.assertIs(answer["readable"], False)
        self.assertEqual(answer["findings"], [],
                         "an unreadable store must not invent findings about what it could "
                         "not read")
        self.assertIsNone(answer["link"])


if __name__ == "__main__":
    unittest.main()
