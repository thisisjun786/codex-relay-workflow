"""Compatibility with the two-level store, and recovery when a transition does not finish.

Two things are proven here that a green suite does not otherwise say. An existing store that
predates these tables keeps everything it had, including work that was in flight when the
schema changed - so the tables are read back row by row rather than merely counted. And a
refusal that arrives after an earlier write in the same transaction leaves nothing behind,
which is measured by driving a real refusal rather than by injecting a fault: no fault hook is
armed anywhere in this module.
"""

import threading
import unittest

from codex_session_relay import linkage
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RefusalReason
from codex_session_relay.linkage import Linkage, binding_id, link_id
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

from .support import CHILD, HOST, ISSUE, PARENT

from .test_linkage import (
    INITIATIVE,
    OTHER_PARENT,
    OTHER_PROJECT,
    PROJECT,
    LinkageTestCase,
)

NEW_TABLES = ("scope_bindings", "scope_links", "relationship_scope", "scope_directives",
              "linkage_conflicts")


class AnExistingStoreKeepsWhatItHad(LinkageTestCase):
    """The upgrade path a deployed store actually takes: drop the new tables, reopen, compare.

    Reopening runs the DDL again, which is exactly what happens to a database that predates
    this work, so this exercises the real transition rather than a simulation of it.
    """

    def snapshot(self, table, columns):
        return [tuple(row[column] for column in columns)
                for row in self.store.all("SELECT * FROM " + table + " ORDER BY rowid")]

    def reopen_without_the_new_tables(self):
        for table in NEW_TABLES:
            self.store.db.execute("DROP TABLE IF EXISTS " + table)
        self.store.close()
        self.store = Store(self.store.path)
        self.linkage = Linkage(self.store, self.clock)
        from codex_session_relay.registry import Registry

        self.registry = Registry(self.store, self.clock)

    def test_existing_two_level_records_survive_the_new_schema(self):
        relationship = self.register()
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        before = {
            "relationships": self.snapshot(
                "relationships", ("relationship_id", "issue_key", "status",
                                  "parent_task_id", "child_task_id", "execution_generation")),
            "generations": self.snapshot(
                "generations", ("relationship_id", "execution_generation",
                                "dispatch_request_id", "anchor_state", "dispatch_turn_id")),
            "events": self.snapshot(
                "events", ("event_id", "relationship_id", "revision_hash", "outcome",
                           "stage")),
        }
        self.reopen_without_the_new_tables()
        for table, rows in before.items():
            self.assertTrue(rows, table + " had nothing to preserve")
        self.assertEqual(
            self.snapshot("relationships", ("relationship_id", "issue_key", "status",
                                            "parent_task_id", "child_task_id",
                                            "execution_generation")),
            before["relationships"])
        self.assertEqual(
            self.snapshot("generations", ("relationship_id", "execution_generation",
                                          "dispatch_request_id", "anchor_state",
                                          "dispatch_turn_id")),
            before["generations"])
        self.assertEqual(
            self.snapshot("events", ("event_id", "relationship_id", "revision_hash",
                                     "outcome", "stage")),
            before["events"])

    def test_an_in_flight_delivery_and_attempt_survive_the_new_schema(self):
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.fakehost import FakeHostAdapter

        relationship = self.register()
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        delivery.enqueue(payload["eventId"])
        # An attempt has to EXIST before it can be said to survive. Enqueuing alone leaves the
        # attempts table empty, so this case asserted nothing about attempts until the delivery
        # was actually attempted against a host.
        adapter = FakeHostAdapter(self.clock)
        adapter.add_thread(PARENT)
        adapter.add_thread(CHILD)
        delivery.attempt(payload["eventId"], adapter)
        before = self.snapshot(
            "deliveries", ("event_id", "relationship_id", "kind", "recipient_task_id",
                           "state", "attempt_count", "hold_reason"))
        attempts_before = self.snapshot(
            "attempts", ("request_id", "event_id", "attempt_no", "kind", "internal_state",
                         "state", "sealed"))
        self.assertTrue(attempts_before, "the fixture produced no attempt to preserve")
        self.reopen_without_the_new_tables()
        self.assertEqual(
            self.snapshot("deliveries", ("event_id", "relationship_id", "kind",
                                         "recipient_task_id", "state", "attempt_count",
                                         "hold_reason")),
            before,
            "an in-flight delivery changed across the schema upgrade")
        self.assertEqual(
            self.snapshot("attempts", ("request_id", "event_id", "attempt_no", "kind",
                                       "internal_state", "state", "sealed")),
            attempts_before,
            "an unfinished attempt changed across the schema upgrade")

    def test_an_assignment_registered_before_scoping_can_be_attached_afterwards(self):
        relationship = self.register()
        self.reopen_without_the_new_tables()
        self.supervise()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)


class RegisteringWithAProject(LinkageTestCase):
    def test_registering_with_a_project_writes_the_whole_lower_level(self):
        self.supervise()
        relationship = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
            issue_key="REL-NEW", artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-new", dispatch_turn_id="turn-new",
            project_key=PROJECT,
        )
        self.assertEqual(
            self.store.one("SELECT project_key FROM relationship_scope"
                           "  WHERE relationship_id = ?",
                           (relationship["relationshipId"],))["project_key"], PROJECT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, "REL-NEW")["taskId"], CHILD)
        self.assertIsNotNone(self.linkage.link(
            link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT, linkage.ISSUE, "REL-NEW")))

    def test_attaching_through_register_never_reinserts_the_relationship(self):
        self.supervise()
        relationship = self.register()
        again = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
            child=Endpoint(CHILD, HOST, cwd=self.root, cxc_session="cxc-child"),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-1", dispatch_turn_id="turn-dispatch-1",
            project_key=PROJECT,
        )
        self.assertEqual(again["relationshipId"], relationship["relationshipId"])
        self.assertEqual(again["executionGeneration"], 1)
        self.assertEqual(len(again["generations"]), 1)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)

    def test_reregistering_an_unscoped_relationship_with_a_project_records_it(self):
        self.supervise()
        relationship = self.register()
        self.assertIsNone(self.linkage.attachment(relationship["relationshipId"]))
        self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
            child=Endpoint(CHILD, HOST, cwd=self.root, cxc_session="cxc-child"),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-1", dispatch_turn_id="turn-dispatch-1",
            project_key=PROJECT,
        )
        record = self.linkage.attachment(relationship["relationshipId"])
        self.assertEqual(record["projectKey"], PROJECT)

    def test_reregistering_a_relationship_under_another_project_is_refused(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.registry.register,
            parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
            child=Endpoint(CHILD, HOST, cwd=self.root, cxc_session="cxc-child"),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-1", dispatch_turn_id="turn-dispatch-1",
            project_key=OTHER_PROJECT,
        )


class Lifecycle(LinkageTestCase):
    def scoped(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def issue_binding(self):
        return self.linkage.binding(
            binding_id(linkage.CHILD, linkage.ISSUE, ISSUE, CHILD))

    def issue_edge(self):
        return self.linkage.link(
            link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT, linkage.ISSUE, ISSUE))

    def test_an_archived_assignment_releases_its_issue_scope(self):
        rid = self.scoped()
        self.registry.set_status(rid, "archived", actor="test")
        self.assertEqual(self.issue_binding()["status"], "archived")
        self.assertEqual(self.issue_edge()["status"], "archived")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))

    def test_resuming_an_assignment_reactivates_its_lower_level(self):
        rid = self.scoped()
        self.registry.set_status(rid, "cancelled", actor="test")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.registry.resume(
            rid, expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        self.assertEqual(self.issue_edge()["status"], "active")

    def test_lifecycle_propagation_is_a_no_op_for_an_unscoped_assignment(self):
        relationship = self.register()
        self.registry.set_status(relationship["relationshipId"], "archived", actor="test")
        self.assertEqual(len(self.store.all("SELECT 1 FROM scope_bindings")), 0)
        self.assertEqual(len(self.store.all("SELECT 1 FROM scope_links")), 0)

    def test_a_replacement_child_gets_the_repointed_lower_edge(self):
        original = self.scoped()
        replacement = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("01child-two", HOST), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-replacement", dispatch_turn_id="turn-replacement",
            supersedes=original, project_key=PROJECT,
        )
        self.assertEqual(self.registry.get(original)["status"], "archived")
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], "01child-two")
        edge = self.issue_edge()
        self.assertEqual(edge["status"], "active")
        self.assertEqual(edge["lower"]["taskId"], "01child-two")
        self.assertGreater(edge["revision"], 1)
        self.assertEqual(
            self.linkage.attachment(replacement["relationshipId"])["projectKey"], PROJECT)


class NothingPartialSurvivesARefusal(LinkageTestCase):
    def test_a_refused_supervision_leaves_neither_binding_behind(self):
        """The defect this module found by measurement rather than by review.

        register_supervision binds the supervisor, then the parent, then the edge. With the
        decision and the write folded together, a refusal on the SECOND binding committed the
        FIRST one alongside the conflict row: the supervisor ended up owning an initiative that
        no accepted operation ever created. Splitting binding_plan from apply_binding_plan is
        what makes the refusal leave nothing, and this is the case that pins it.
        """
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key="ISS-OTHER", endpoint=self.parent(OTHER_PARENT))
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.supervise,
            parent=self.parent(OTHER_PARENT),
        )
        self.assertIsNone(self.linkage.owner(linkage.INITIATIVE, INITIATIVE))
        self.assertEqual(
            [row["scope_key"] for row in self.store.all(
                "SELECT scope_key FROM scope_bindings ORDER BY scope_key")],
            ["ISS-OTHER"],
        )
        self.assertEqual(len(self.store.all("SELECT 1 FROM scope_links")), 0)
        self.assertEqual(len(self.conflicts()), 1,
                         "the contest was lost along with the refusal")

    def test_a_half_written_transition_is_reported_rather_than_guessed(self):
        self.supervise()
        relationship = self.register()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"
                " VALUES (?,?,?)",
                (relationship["relationshipId"], PROJECT, self.clock.iso()),
            )
        answer = self.linkage.up(relationship_id=relationship["relationshipId"])
        self.assertEqual(answer["state"], "resolved")
        self.assertIn("issue_without_child", [gap["gap"] for gap in answer["gaps"]])

    def test_a_failure_partway_through_the_transition_leaves_nothing_behind(self):
        """A write failure AFTER the first table is written, at a named point.

        attach_in writes three tables in the caller's transaction: the child binding, the
        scope row, and the project to issue edge. This lets it complete all three and then
        fails the transaction, which is the shape a crash between two of those writes has. No
        fault hook is armed - the exception is raised by this test inside the transaction, and
        Store.transaction rolls back on any exception including this one.
        """
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]

        class TransitionInterrupted(Exception):
            pass

        with self.assertRaises(TransitionInterrupted):
            with self.store.transaction() as db:
                row = db.execute(
                    "SELECT * FROM relationships WHERE relationship_id = ?", (rid,)
                ).fetchone()
                self.assertIsNone(self.linkage.attach_in(db, row, PROJECT))
                # Everything the transition writes is now in this transaction and none of it
                # is committed.
                self.assertIsNotNone(db.execute(
                    "SELECT 1 FROM relationship_scope WHERE relationship_id = ?", (rid,)
                ).fetchone())
                raise TransitionInterrupted("the writer stopped partway through")

        self.assertEqual(
            len(self.store.all("SELECT 1 FROM relationship_scope WHERE relationship_id = ?",
                               (rid,))), 0)
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.assertIsNone(self.linkage.link(
            link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT, linkage.ISSUE, ISSUE)))
        # And the transition is still available afterwards rather than half-applied.
        self.linkage.attach_issue(rid, PROJECT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)


class ConcurrentAttachment(LinkageTestCase):
    def test_concurrent_attachment_of_one_issue_settles_as_one_project(self):
        self.supervise()
        self.supervise(initiative="INIT-2", project=OTHER_PROJECT,
                       supervisor=self.supervisor("01supervisor-two"),
                       parent=self.parent(OTHER_PARENT))
        rid = self.register()["relationshipId"]
        gate = threading.Barrier(2)
        errors, wins = [], []

        def attach(project):
            store = Store(self.store.path)
            try:
                worker = Linkage(store, FakeClock())
                gate.wait(timeout=10)
                worker.attach_issue(rid, project)
                wins.append(project)
            except Exception as problem:
                errors.append(problem)
            finally:
                store.close()

        threads = [threading.Thread(target=attach, args=(project,))
                   for project in (PROJECT, OTHER_PROJECT)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=20)

        rows = self.store.all("SELECT project_key FROM relationship_scope"
                              "  WHERE relationship_id = ?", (rid,))
        self.assertEqual(len(rows), 1, "one issue ended up scoped to more than one project")
        self.assertEqual(len(wins), 1, "both attachments reported success")
        self.assertEqual(len(errors), 1, "the losing attachment was not refused")


if __name__ == "__main__":
    unittest.main()
