"""A project-level reading of what its children report, and what it refuses to call finished.

The canonical criterion is that a parent's completion is computed from its children's real
criteria, verification and integration, and that partial Done, a shared project's duplicate
rows, a child's own goal being complete, or an empty first page of review must not close it.

What is asserted here is the composition and its refusals, so the linkage reader is a stub
returning the documented shapes and `state()` is overridden to a fixed answer. Both are covered
by their own suites; this file is about not turning any of their answers into completion.
"""

import unittest

from codex_session_relay import linkage as linkage_module
from codex_session_relay.assignment import PROJECT_SCOPE_KIND, AssignmentView

PROJECT = "PRJ-1"


class _Reader:
    def __init__(self, attached, outstanding, owners=(), raises=None):
        self._attached, self._outstanding = attached, outstanding
        self._owners, self._raises = owners, raises

    def attached(self, project_key, task_id=None, *, other_than=None):
        if self._raises is not None:
            raise self._raises
        return list(self._attached)

    def outstanding(self, project_key, task_id=None):
        if self._raises is not None:
            raise self._raises
        return list(self._outstanding)

    def owners(self, scope_kind, scope_key):
        return list(self._owners)


class _View(AssignmentView):
    """state() is the per-assignment derivation; its own suite owns it."""

    def __init__(self, reader, fixed="merged"):
        self.linkage = reader
        self.fixed = fixed

    def state(self, relationship_id):
        return {"state": self.fixed}


def _owner(task_id):
    return {"taskId": task_id}


class TheVocabularyIsShared(unittest.TestCase):
    def test_the_local_project_constant_equals_the_linkage_one(self):
        self.assertEqual(PROJECT_SCOPE_KIND, linkage_module.PROJECT)


class NoAnswerIsNeverCompletion(unittest.TestCase):
    def test_without_a_reader_it_says_it_cannot_answer(self):
        view = _View(None)
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "unreadable")
        self.assertFalse(reading["readable"])
        self.assertEqual(reading["outstanding"], [])
        self.assertIn("could not be enumerated", reading["basis"])

    def test_a_store_that_raises_is_unreadable_and_names_the_fault(self):
        view = _View(_Reader([], [], raises=RuntimeError("disk gone")))
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "unreadable")
        self.assertFalse(reading["readable"])
        self.assertIn("disk gone", reading["basis"])

    def test_no_attached_assignment_is_unregistered_not_finished(self):
        view = _View(_Reader([], [], owners=[_owner("parent-a")]))
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "unregistered")
        self.assertTrue(reading["readable"])
        self.assertIn("not the", reading["basis"])

    def test_two_live_owners_are_ambiguous_and_not_resolved(self):
        view = _View(_Reader(["r1"], [], owners=[_owner("parent-a"), _owner("parent-b")]))
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "ambiguous")
        self.assertEqual(reading["competingOwners"], ["parent-a", "parent-b"])


class CompletionNeedsEveryChild(unittest.TestCase):
    def test_one_unfinished_assignment_keeps_the_project_incomplete(self):
        view = _View(_Reader(["r1", "r2", "r3"], ["r3"], owners=[_owner("parent-a")]),
                     fixed="verifying")
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "incomplete")
        self.assertEqual([u["relationshipId"] for u in reading["unfinished"]], ["r3"])
        self.assertEqual(reading["unfinished"][0]["state"], "verifying")

    def test_partial_done_cannot_close_the_parent(self):
        """Most finished is still not finished: the outstanding set must be EMPTY, not small."""
        view = _View(_Reader(["r%d" % n for n in range(10)], ["r9"], owners=[_owner("p")]))
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "incomplete")
        self.assertEqual(len(reading["attached"]), 10)

    def test_work_parked_on_another_parent_still_counts(self):
        """attached() is the PROJECT's set, so a shared project cannot be closed by one parent."""
        view = _View(_Reader(["mine", "someone-elses"], ["someone-elses"],
                             owners=[_owner("p")]))
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "incomplete")
        self.assertIn("someone-elses", [u["relationshipId"] for u in reading["unfinished"]])

    def test_everything_finished_is_a_candidate_and_never_complete(self):
        view = _View(_Reader(["r1", "r2"], [], owners=[_owner("parent-a")]))
        reading = view.project_state(PROJECT)
        self.assertEqual(reading["state"], "complete_candidate")
        self.assertEqual(reading["outstanding"], [])
        self.assertNotEqual(reading["state"], "complete")
        self.assertIn("not a completion verdict", reading["limits"])
        self.assertIn("goal status is never consulted", reading["limits"])


if __name__ == "__main__":
    unittest.main()
