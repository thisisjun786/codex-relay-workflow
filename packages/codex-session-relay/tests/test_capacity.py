"""Counting what this store can count, and refusing to infer what it cannot.

The distinction every case here turns on: how many execution subjects a parent is holding a
slot for is a number this store takes for itself, and how many file descriptors or dollars a
host is using is not. A task count is not evidence about either, however many tasks there are,
so a declared ceiling on such a dimension answers unmeasured until something measures it.

The slot cases are the leak cases. Completion, failure, a resume and a duplicated notification
all arrive as a release of the same subject, so the second one must not be a second release,
and a reservation replayed after a crash must not be a second reservation.
"""

import threading
import unittest

from codex_session_relay.capacity import Capacity
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import CoordinationError, RefusalReason
from codex_session_relay.linkage import Linkage, PARENT
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

from .support import RelayTestCase

PROJECT_A = "PRJ-A"
PROJECT_B = "PRJ-B"
ASSIGNMENT = "assignment"


class CapacityTestCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.linkage = Linkage(self.store, self.clock)
        self.capacity = Capacity(self.store, self.clock, self.linkage)
        self.alpha = Endpoint("task-alpha", "host-a", cwd="/alpha")
        self.beta = Endpoint("task-beta", "host-b", cwd="/beta")
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_A, endpoint=self.alpha)
        self.linkage.bind_scope(role=PARENT, scope_key=PROJECT_B, endpoint=self.beta)
        # A ceiling is somebody's statement, so the declarer has to own the scope it speaks
        # for. The store scope has no owner of its own, so a registered supervisor sets it.
        self.supervisor = Endpoint("task-supervisor", "host-s", cwd="/sup")
        for project, parent in ((PROJECT_A, self.alpha), (PROJECT_B, self.beta)):
            self.linkage.register_supervision(
                initiative_key="INIT-1", project_key=project,
                supervisor=self.supervisor, parent=parent)

    def take(self, subject, *, endpoint=None, project=PROJECT_A):
        endpoint = endpoint or self.alpha
        return self.capacity.reserve(
            subject_kind=ASSIGNMENT, subject_key=subject, parent_task_id=endpoint.task_id,
            project_key=project, reserved_by=endpoint.task_id)

    def give_back(self, subject, reason="completed", *, endpoint=None):
        endpoint = endpoint or self.alpha
        return self.capacity.release(
            subject_kind=ASSIGNMENT, subject_key=subject, released_by=endpoint.task_id,
            reason=reason)

    def ceiling(self, dimension, amount, *, scope=PROJECT_A, kind="project", unit="runs",
                enforce=True):
        owner = self.supervisor.task_id if kind != "project" else self.alpha.task_id
        return self.capacity.declare_limit(
            scope_kind=kind, scope_key=scope, dimension=dimension, unit=unit,
            ceiling=amount, declared_by=owner, source="operator", enforce=enforce)

    def dimensions(self, *, scope=PROJECT_A, kind="project"):
        return {entry["dimension"]: entry
                for entry in self.capacity.headroom(kind, scope)["dimensions"]}


class ASlotNeitherLeaksNorReturnsTwice(CapacityTestCase):
    def test_a_replayed_reservation_counts_once(self):
        self.take("REL-1")
        again = self.take("REL-1")
        self.assertTrue(again["alreadyHeld"])
        self.assertEqual(self.capacity.report()["total"], 1)

    def test_a_replayed_release_releases_once(self):
        self.take("REL-1")
        self.give_back("REL-1")
        again = self.give_back("REL-1")
        self.assertTrue(again["alreadyReleased"])
        self.assertEqual(self.capacity.report()["total"], 0)

    def test_completion_failure_and_a_resume_notification_converge_on_one_release(self):
        self.take("REL-1")
        for _ in range(3):
            self.give_back("REL-1", "completed")
        rows = self.store.all(
            "SELECT * FROM execution_slots WHERE subject_key = ? AND state = 'released'",
            ("REL-1",))
        self.assertEqual(len(rows), 1)

    def test_a_release_restating_a_different_reason_is_refused_and_retained(self):
        self.take("REL-1")
        self.give_back("REL-1", "completed")
        with self.assertRaises(CoordinationError) as caught:
            self.give_back("REL-1", "failed")
        self.assertEqual(caught.exception.reason, RefusalReason.DISPOSITION_CONFLICT)
        self.assertEqual(
            self.capacity.slot(ASSIGNMENT, "REL-1")["releaseReason"], "completed")

    def test_releasing_a_subject_that_never_reserved_is_refused_by_name(self):
        with self.assertRaises(CoordinationError) as caught:
            self.give_back("REL-NEVER")
        self.assertEqual(caught.exception.reason, RefusalReason.SLOT_UNKNOWN)

    def test_a_resume_opens_a_second_tenure_and_keeps_the_first(self):
        self.take("REL-1")
        self.give_back("REL-1")
        self.take("REL-1")
        rows = self.store.all(
            "SELECT tenure, state FROM execution_slots WHERE subject_key = ? ORDER BY tenure",
            ("REL-1",))
        self.assertEqual(
            [(row["tenure"], row["state"]) for row in rows], [(1, "released"), (2, "held")])
        self.assertEqual(self.capacity.report()["total"], 1)

    def test_a_second_project_cannot_inherit_another_projects_slot_as_a_replay(self):
        """alreadyHeld handed another project's slot back as if it were theirs.

        Neither the second caller's ownership nor its ceiling was ever consulted, because the
        replay branch answers before either is read.
        """
        self.take("REL-1")
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-1", endpoint=self.beta, project=PROJECT_B)
        self.assertEqual(caught.exception.reason, RefusalReason.DISPOSITION_CONFLICT)

    def test_a_foreign_caller_cannot_release_another_parents_slot(self):
        self.take("REL-1")
        with self.assertRaises(CoordinationError) as caught:
            self.give_back("REL-1", endpoint=self.beta)
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)
        self.assertEqual(self.capacity.report()["total"], 1)

    def test_the_supervisor_may_release_a_slot_its_parent_left_held(self):
        self.take("REL-1")
        released = self.capacity.release(
            subject_kind=ASSIGNMENT, subject_key="REL-1",
            released_by=self.supervisor.task_id, reason="parent stopped answering")
        self.assertEqual(released["state"], "released")

    def test_a_task_that_does_not_own_the_project_cannot_reserve_for_it(self):
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-1", endpoint=self.beta, project=PROJECT_A)
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_project_with_no_parent_has_nobody_to_reserve_for_it(self):
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-1", project="PRJ-NONE")
        self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_SCOPE)


class CountsAndDeclaredCeilingsAreDifferentKindsOfFact(CapacityTestCase):
    def test_a_per_project_ceiling_and_the_store_total_are_counted_separately(self):
        self.ceiling("runs", 1)
        self.ceiling("runs", 3, scope="store", kind="store")
        self.take("REL-1")
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-2")
        self.assertEqual(caught.exception.reason, RefusalReason.CAPACITY_EXHAUSTED)
        self.take("REL-3", endpoint=self.beta, project=PROJECT_B)
        self.assertEqual(self.capacity.report()["total"], 2)
        self.assertEqual(
            self.capacity.report()["perParent"],
            {self.alpha.task_id: 1, self.beta.task_id: 1})

    def test_a_run_count_never_becomes_evidence_about_a_file_descriptor_bound(self):
        # Declared without enforcement, so slots can be held while the dimension stays
        # unmeasured. An enforced one would refuse the reservation first and the comparison
        # this case is about would never be reached.
        self.ceiling("file_descriptors", 100, unit="fds", enforce=False)
        empty = self.dimensions()["file_descriptors"]
        self.take("REL-1")
        self.take("REL-2")
        self.take("REL-3")
        loaded = self.dimensions()["file_descriptors"]
        self.assertEqual(empty["used"], None)
        self.assertEqual(empty["proof"], "unmeasured")
        self.assertEqual(loaded, empty, "three held slots say nothing about a descriptor bound")

    def test_an_enforced_bound_nobody_measured_refuses_rather_than_reporting_room(self):
        self.ceiling("model_cost", 50, unit="usd")
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-1")
        self.assertEqual(caught.exception.reason, RefusalReason.CAPACITY_UNMEASURED)
        self.assertIn("not a measurement", caught.exception.detail)

    def test_only_an_observation_gives_a_resource_dimension_a_value(self):
        self.ceiling("file_descriptors", 100, unit="fds")
        self.capacity.observe(
            scope_kind="project", scope_key=PROJECT_A, dimension="file_descriptors",
            observed=99, observed_by=self.alpha.task_id, method="counted /proc/<pid>/fd")
        entry = self.dimensions()["file_descriptors"]
        self.assertEqual((entry["used"], entry["proof"], entry["state"]),
                         (99.0, "observed", "within"))
        self.capacity.observe(
            scope_kind="project", scope_key=PROJECT_A, dimension="file_descriptors",
            observed=120, observed_by=self.alpha.task_id, method="counted /proc/<pid>/fd")
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-1")
        self.assertEqual(caught.exception.reason, RefusalReason.CAPACITY_EXHAUSTED)

    def test_a_run_dimension_says_how_its_number_was_reached(self):
        self.ceiling("runs", 5)
        self.take("REL-1")
        entry = self.dimensions()["runs"]
        self.assertEqual((entry["used"], entry["proof"]), (1, "derived_from_slots"))
        self.assertIsNone(entry["note"])

    def test_an_unenforced_ceiling_is_reported_and_never_refuses(self):
        self.ceiling("model_cost", 1, unit="usd", enforce=False)
        self.take("REL-1")
        self.assertFalse(self.dimensions()["model_cost"]["enforce"])

    def test_lowering_a_ceiling_below_current_use_revokes_nothing_and_says_so(self):
        self.ceiling("runs", 5)
        self.take("REL-1")
        self.take("REL-2")
        self.take("REL-3")
        lowered = self.ceiling("runs", 1)
        self.assertEqual(lowered["overBy"], 2)
        self.assertEqual(lowered["state"], "over_ceiling")
        self.assertEqual(lowered["revision"], 2)
        self.assertEqual(self.capacity.report()["total"], 3)
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-4")
        self.assertEqual(caught.exception.reason, RefusalReason.CAPACITY_EXHAUSTED)

    def test_no_answer_changes_because_time_passed(self):
        self.ceiling("runs", 5)
        self.ceiling("file_descriptors", 100, unit="fds", enforce=False)
        self.take("REL-1")
        before = (self.capacity.report(), self.capacity.headroom("project", PROJECT_A))
        self.clock.advance(1_000_000)
        self.assertEqual(
            (self.capacity.report(), self.capacity.headroom("project", PROJECT_A)), before)


class StatingABoundIsNotAnybodysCall(CapacityTestCase):
    """A caller who could raise a ceiling or publish a usage figure could admit execution the
    owner had bounded, so the declarer has to own the scope it speaks for."""

    def test_a_stranger_cannot_state_a_project_ceiling(self):
        with self.assertRaises(CoordinationError) as caught:
            self.capacity.declare_limit(
                scope_kind="project", scope_key=PROJECT_A, dimension="runs", unit="runs",
                ceiling=99, declared_by="task-stranger", source="operator")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_stranger_cannot_publish_a_usage_figure(self):
        with self.assertRaises(CoordinationError) as caught:
            self.capacity.observe(
                scope_kind="project", scope_key=PROJECT_A, dimension="file_descriptors",
                observed=1, observed_by="task-stranger", method="claimed")
        self.assertEqual(caught.exception.reason, RefusalReason.SCOPE_ROLE_MISMATCH)

    def test_a_store_ceiling_under_any_other_key_is_refused_rather_than_ignored(self):
        """The shape that succeeded and was then never consulted.

        Enforcement reads (store, store). A declaration under 'global' was stored, reported
        back as accepted, and never applied to a single reservation.
        """
        with self.assertRaises(CoordinationError) as caught:
            self.capacity.declare_limit(
                scope_kind="store", scope_key="global", dimension="runs", unit="runs",
                ceiling=1, declared_by=self.supervisor.task_id, source="operator")
        self.assertEqual(caught.exception.reason, RefusalReason.LINK_NOT_ACTIVE)

    def test_the_canonical_store_ceiling_does_apply(self):
        self.ceiling("runs", 1, scope="store", kind="store")
        self.take("REL-1")
        with self.assertRaises(CoordinationError) as caught:
            self.take("REL-2", endpoint=self.beta, project=PROJECT_B)
        self.assertEqual(caught.exception.reason, RefusalReason.CAPACITY_EXHAUSTED)


class TwoParentsRacingOneCeiling(CapacityTestCase):
    """Different subjects, so the race is the ceiling rather than the replay branch.

    Racing one subject only exercises the already-held path, which answers before any ceiling
    is consulted. The assertions hold under every interleaving including both serial ones.
    """

    def reserve_in_thread(self, subject, endpoint, project, results, errors, barrier):
        def run():
            store = Store(self.store.path)
            try:
                barrier.wait(timeout=20)
                clock = FakeClock()
                capacity = Capacity(store, clock, Linkage(store, clock))
                results[subject] = capacity.reserve(
                    subject_kind=ASSIGNMENT, subject_key=subject,
                    parent_task_id=endpoint.task_id, project_key=project,
                    reserved_by=endpoint.task_id)
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors[subject] = error
            finally:
                store.close()

        return threading.Thread(target=run)

    def test_a_ceiling_of_one_admits_exactly_one_of_two_racing_subjects(self):
        self.capacity.declare_limit(
            scope_kind="store", scope_key="store", dimension="runs", unit="runs",
            ceiling=1, declared_by=self.supervisor.task_id, source="operator")
        results, errors = {}, {}
        barrier = threading.Barrier(2)
        threads = [
            self.reserve_in_thread("REL-1", self.alpha, PROJECT_A, results, errors, barrier),
            self.reserve_in_thread("REL-2", self.beta, PROJECT_B, results, errors, barrier),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        for thread in threads:
            self.assertFalse(thread.is_alive(), "a thread never finished")
        self.assertEqual(len(results), 1, f"exactly one should be admitted: {errors}")
        self.assertEqual(len(errors), 1)
        self.assertEqual(
            list(errors.values())[0].reason, RefusalReason.CAPACITY_EXHAUSTED)
        self.assertEqual(self.capacity.report()["total"], 1)


if __name__ == "__main__":
    unittest.main()
