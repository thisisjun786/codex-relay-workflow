"""Three-level linkage: identity, the role contract, every refusal, and handover.

Contention is built the way test_registry.py established it - one independent Store per thread,
a barrier, joins with a timeout, errors collected rather than raised in a thread nobody is
watching. A barrier synchronises without measuring a duration, so nothing here spends real time.

Nothing here arms the store's fault hook. Atomicity is asserted by driving a real refusal and
then reading the store, which is what the contract actually promises: a refused operation leaves
no state, and the contest it lost is retained.
"""

import threading
import unittest

from codex_session_relay import linkage
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RefusalReason
from codex_session_relay.linkage import Linkage, binding_id, link_id
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store

from .support import CHILD, HOST, ISSUE, PARENT, RelayTestCase

INITIATIVE = "INIT-1"
PROJECT = "PROJ-1"
OTHER_PROJECT = "PROJ-2"
OTHER_INITIATIVE = "INIT-2"
SUPERVISOR_TASK = "01supervisor-task"
OTHER_SUPERVISOR = "01supervisor-two"
OTHER_PARENT = "01parent-two"


class LinkageTestCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.linkage = Linkage(self.store, self.clock)

    def supervisor(self, task=SUPERVISOR_TASK):
        return Endpoint(task, HOST, cwd="/supervisor", cxc_session="cxc-supervisor")

    def parent(self, task=PARENT):
        return Endpoint(task, HOST, cwd="/parent", cxc_session="cxc-parent")

    def supervise(self, *, initiative=INITIATIVE, project=PROJECT, supervisor=None,
                  parent=None, kind=linkage.EXECUTION):
        return self.linkage.register_supervision(
            initiative_key=initiative, project_key=project,
            supervisor=supervisor or self.supervisor(), parent=parent or self.parent(),
            link_kind=kind,
        )

    def conflicts(self):
        return self.store.all("SELECT * FROM linkage_conflicts ORDER BY id")


class Identity(LinkageTestCase):
    def test_a_binding_is_the_role_scope_and_task_and_nothing_else(self):
        record = self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        self.assertEqual(
            record["bindingId"], binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, PARENT))
        flat = str({k: v for k, v in record.items() if k != "_bindings"}).lower()
        for forbidden in ("title", "latest", "name"):
            self.assertNotIn(forbidden, flat)

    def test_a_binding_carries_the_real_task_and_host_identifiers(self):
        record = self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        self.assertEqual(record["taskId"], PARENT)
        self.assertEqual(record["hostId"], HOST)
        self.assertEqual(record["cwd"], "/parent")
        self.assertEqual(record["_bindings"]["cxcSession"], "cxc-parent")

    def test_rebinding_the_same_scope_and_task_converges(self):
        first = self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        again = self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        self.assertEqual(first["bindingId"], again["bindingId"])
        self.assertEqual(again["revision"], 1)
        rows = self.store.all("SELECT binding_id FROM scope_bindings")
        self.assertEqual(len(rows), 1)

    def test_a_link_id_does_not_change_when_its_owner_does(self):
        before = link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                         linkage.PROJECT, PROJECT)
        self.supervise()
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="the outgoing parent handed over its project", actor="test",
        )
        after = link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                        linkage.PROJECT, PROJECT)
        self.assertEqual(before, after)
        self.assertEqual(self.linkage.link(after)["lower"]["taskId"], OTHER_PARENT)

    def test_a_replayed_supervision_returns_the_same_link(self):
        first = self.supervise()
        again = self.supervise()
        self.assertEqual(first["linkId"], again["linkId"])
        self.assertEqual(again["revision"], 1)
        self.assertEqual(len(self.store.all("SELECT link_id FROM scope_links")), 1)


class OneOwnerPerScope(LinkageTestCase):
    def test_a_second_task_for_one_scope_is_refused_and_the_contest_is_retained(self):
        self.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                endpoint=self.parent())
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER,
            self.linkage.bind_scope, role=linkage.PARENT, scope_key=PROJECT,
            endpoint=self.parent(OTHER_PARENT),
        )
        rows = self.conflicts()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["reason"], "duplicate_scope_owner")
        self.assertEqual(rows[0]["incumbent"], PARENT)
        self.assertEqual(rows[0]["challenger"], OTHER_PARENT)

    def test_a_refused_binding_leaves_no_binding_behind(self):
        self.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                endpoint=self.parent())
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER,
            self.linkage.bind_scope, role=linkage.PARENT, scope_key=PROJECT,
            endpoint=self.parent(OTHER_PARENT),
        )
        owners = self.store.all("SELECT task_id FROM scope_bindings WHERE scope_key = ?",
                                (PROJECT,))
        self.assertEqual([row["task_id"] for row in owners], [PARENT])

    def test_a_replayed_refusal_records_one_conflict_not_two(self):
        self.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                endpoint=self.parent())
        for _ in range(3):
            self.assertRefused(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                self.linkage.bind_scope, role=linkage.PARENT, scope_key=PROJECT,
                endpoint=self.parent(OTHER_PARENT),
            )
        self.assertEqual(len(self.conflicts()), 1)

    def test_one_task_cannot_hold_two_roles(self):
        self.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                endpoint=self.parent())
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            self.linkage.bind_scope, role=linkage.SUPERVISOR, scope_key=INITIATIVE,
            endpoint=self.supervisor(PARENT),
        )

    def test_a_registered_child_cannot_become_a_supervisor(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            self.linkage.bind_scope, role=linkage.SUPERVISOR, scope_key=OTHER_INITIATIVE,
            endpoint=self.supervisor(CHILD),
        )


class TheRoleContract(LinkageTestCase):
    def test_a_supervisor_supervising_itself_is_refused(self):
        self.assertRefused(
            RefusalReason.SCOPE_CYCLE, self.supervise,
            supervisor=self.supervisor(PARENT), parent=self.parent(),
        )

    def test_a_parent_cannot_become_somebody_elses_supervisor(self):
        """Where mutual supervision actually dies.

        Role uniqueness reaches this before the reachability walk does, and it is the more
        specific answer: the attempt is refused because one task cannot hold two roles, not
        because these two particular scopes form a loop. Asserting the cycle reason here would
        have been asserting a rule that never ran.
        """
        self.supervise()
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.supervise,
            initiative=OTHER_INITIATIVE, project=OTHER_PROJECT,
            supervisor=self.supervisor(PARENT), parent=self.parent(OTHER_PARENT),
        )

    def test_a_child_cannot_supervise_the_project_it_works_under(self):
        """The reachability walk, on a chain that genuinely closes.

        CHILD owns an issue that hangs off PROJ-1, so supervising PROJ-1 from CHILD would make
        the hierarchy reach itself. _reaches finds it by walking live execution edges downward
        from the project, and it runs before the role check, so this is the cycle refusal
        rather than the role one.
        """
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        self.assertRefused(
            RefusalReason.SCOPE_CYCLE, self.supervise,
            initiative=OTHER_INITIATIVE, supervisor=self.supervisor(CHILD),
            kind=linkage.REFERENCE,
        )
    def test_a_peer_kind_is_not_a_supervision(self):
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.supervise, kind=linkage.PEER)

    def test_an_issue_whose_parent_is_its_own_child_is_refused(self):
        self.supervise()
        self.registry.register(
            parent=Endpoint("01same-task", HOST), child=Endpoint("01same-task", HOST),
            issue_key="REL-SELF", artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-self", dispatch_turn_id="turn-self",
        )
        rid = self.store.one(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?", ("REL-SELF",)
        )["relationship_id"]
        self.assertRefused(
            RefusalReason.SCOPE_CYCLE, self.linkage.attach_issue, rid, PROJECT)

    def test_an_issue_under_another_projects_parent_is_refused(self):
        self.supervise()
        self.supervise(initiative=OTHER_INITIATIVE, project=OTHER_PROJECT,
                       supervisor=self.supervisor(OTHER_SUPERVISOR),
                       parent=self.parent(OTHER_PARENT))
        relationship = self.register()
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE,
            self.linkage.attach_issue, relationship["relationshipId"], OTHER_PROJECT,
        )

    def test_an_issue_under_an_unregistered_project_is_refused(self):
        relationship = self.register()
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE,
            self.linkage.attach_issue, relationship["relationshipId"], PROJECT,
        )


class ASharedProject(LinkageTestCase):
    def test_a_second_initiative_references_the_project_without_a_second_parent(self):
        self.supervise()
        reference = self.supervise(
            initiative=OTHER_INITIATIVE, supervisor=self.supervisor(OTHER_SUPERVISOR),
            kind=linkage.REFERENCE,
        )
        self.assertEqual(reference["kind"], linkage.REFERENCE)
        parents = self.store.all(
            "SELECT task_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ?"
            "  AND role = ? AND status IN ('active','paused')",
            (linkage.PROJECT, PROJECT, linkage.PARENT),
        )
        self.assertEqual([row["task_id"] for row in parents], [PARENT])

    def test_a_second_initiative_cannot_open_a_second_execution_edge(self):
        self.supervise()
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.supervise,
            initiative=OTHER_INITIATIVE, supervisor=self.supervisor(OTHER_SUPERVISOR),
        )
        edges = self.store.all(
            "SELECT link_id FROM scope_links WHERE lower_key = ? AND link_kind = 'execution'"
            "  AND status IN ('active','paused')",
            (PROJECT,),
        )
        self.assertEqual(len(edges), 1)

    def test_a_reference_naming_another_parent_is_refused_and_recorded(self):
        self.supervise()
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.supervise,
            initiative=OTHER_INITIATIVE, supervisor=self.supervisor(OTHER_SUPERVISOR),
            parent=self.parent(OTHER_PARENT), kind=linkage.REFERENCE,
        )
        rows = self.conflicts()
        self.assertEqual(rows[-1]["incumbent"], PARENT)
        self.assertEqual(rows[-1]["challenger"], OTHER_PARENT)


class Attachment(LinkageTestCase):
    def attached(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def test_attaching_writes_the_scope_row_the_child_binding_and_the_edge(self):
        rid = self.attached()
        record = self.linkage.attachment(rid)
        self.assertEqual(record["projectKey"], PROJECT)
        self.assertEqual(record["child"]["taskId"], CHILD)
        self.assertEqual(record["parent"]["taskId"], PARENT)
        self.assertEqual(record["link"]["lower"]["taskId"], CHILD)

    def test_attaching_twice_converges(self):
        rid = self.attached()
        self.linkage.attach_issue(rid, PROJECT)
        self.assertEqual(
            len(self.store.all("SELECT relationship_id FROM relationship_scope")), 1)
        self.assertEqual(
            len(self.store.all("SELECT link_id FROM scope_links WHERE lower_kind = ?",
                               (linkage.ISSUE,))), 1)

    def test_attaching_completes_a_partially_attached_relationship(self):
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        # A run that stopped after the scope row, or an older writer that only knew about it.
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"
                " VALUES (?,?,?)",
                (rid, PROJECT, self.clock.iso()),
            )
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.linkage.attach_issue(rid, PROJECT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        self.assertIsNotNone(
            self.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT,
                                      linkage.ISSUE, ISSUE)))

    def test_attaching_an_inactive_relationship_is_refused(self):
        self.supervise()
        relationship = self.register()
        self.registry.set_status(relationship["relationshipId"], "paused", actor="test")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE,
            self.linkage.attach_issue, relationship["relationshipId"], PROJECT,
        )

    def test_an_issue_already_scoped_elsewhere_is_refused(self):
        rid = self.attached()
        self.supervise(initiative=OTHER_INITIATIVE, project=OTHER_PROJECT,
                       supervisor=self.supervisor(OTHER_SUPERVISOR),
                       parent=self.parent(OTHER_PARENT))
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE, self.linkage.attach_issue, rid, OTHER_PROJECT)


class Directives(LinkageTestCase):
    def setUp(self):
        super().setUp()
        self.execution = self.supervise()
        self.reference = self.supervise(
            initiative=OTHER_INITIATIVE, supervisor=self.supervisor(OTHER_SUPERVISOR),
            kind=linkage.REFERENCE,
        )

    def record(self, *, origin, task, link, digest):
        return self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=task,
            from_scope_key=origin, link_id_value=link, digest=digest,
        )

    def test_a_directive_records_whether_it_came_by_execution_or_reference(self):
        first = self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                            link=self.execution["linkId"], digest="d-one")
        second = self.record(origin=OTHER_INITIATIVE, task=OTHER_SUPERVISOR,
                             link=self.reference["linkId"], digest="d-two")
        self.assertEqual(first["linkKind"], linkage.EXECUTION)
        self.assertEqual(second["linkKind"], linkage.REFERENCE)

    def test_two_instructions_for_one_project_are_both_kept_and_reported(self):
        self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                    link=self.execution["linkId"], digest="d-one")
        self.record(origin=OTHER_INITIATIVE, task=OTHER_SUPERVISOR,
                    link=self.reference["linkId"], digest="d-two")
        contested = self.linkage.contested_directives(linkage.PROJECT, PROJECT)
        self.assertEqual(len(contested), 2)
        self.assertEqual({d["digest"] for d in contested}, {"d-one", "d-two"})

    def test_a_settled_instruction_leaves_the_loser_readable(self):
        first = self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                            link=self.execution["linkId"], digest="d-one")
        second = self.record(origin=OTHER_INITIATIVE, task=OTHER_SUPERVISOR,
                             link=self.reference["linkId"], digest="d-two")
        self.linkage.settle_directive(first["directiveId"], "chosen", decided_by="parent")
        self.linkage.settle_directive(second["directiveId"], "superseded",
                                      decided_by="parent", reason="the initiative deferred")
        self.assertEqual(self.linkage.contested_directives(linkage.PROJECT, PROJECT), [])
        loser = [d for d in self.linkage.directives(linkage.PROJECT, PROJECT)
                 if d["directiveId"] == second["directiveId"]][0]
        self.assertEqual(loser["digest"], "d-two")
        self.assertEqual(loser["disposition"], "superseded")

    def test_a_directive_from_a_task_that_does_not_own_its_scope_is_refused(self):
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.record,
            origin=INITIATIVE, task=OTHER_SUPERVISOR, link=self.execution["linkId"],
            digest="d-three",
        )

    def test_a_directive_naming_a_link_that_does_not_join_the_scopes_is_refused(self):
        self.supervise(initiative="INIT-3", project="PROJ-3",
                       supervisor=self.supervisor("01supervisor-three"),
                       parent=self.parent("01parent-three"))
        other = link_id(linkage.EXECUTION, linkage.INITIATIVE, "INIT-3",
                        linkage.PROJECT, "PROJ-3")
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, self.record,
            origin=INITIATIVE, task=SUPERVISOR_TASK, link=other, digest="d-four",
        )


class Handover(LinkageTestCase):
    def test_a_handover_without_the_outstanding_work_is_refused(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="taking over", actor="test",
        )
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)

    def test_a_handover_naming_the_wrong_outgoing_owner_is_refused(self):
        self.supervise()
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id="01somebody-else", endpoint=self.parent(OTHER_PARENT),
            acknowledged=[], evidence="taking over", actor="test",
        )

    def test_a_handover_without_evidence_is_refused(self):
        self.supervise()
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="   ", actor="test",
        )

    def test_a_handover_that_restates_the_outstanding_work_succeeds(self):
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        replacement = self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[rid],
            evidence="the outgoing parent listed its unfinished issues", actor="test",
        )
        self.assertEqual(replacement["taskId"], OTHER_PARENT)
        self.assertEqual(replacement["revision"], 2)
        self.assertEqual(replacement["handoverNote"],
                         "the outgoing parent listed its unfinished issues")
        old = self.linkage.binding(
            binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, PARENT))
        self.assertEqual(old["status"], "archived")
        self.assertEqual(old["supersededBy"], replacement["bindingId"])
        edge = self.linkage.link(link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                                         linkage.PROJECT, PROJECT))
        self.assertEqual(edge["lower"]["taskId"], OTHER_PARENT)


class Contention(LinkageTestCase):
    """Two coordinators registering at once, each on its own Store."""

    def test_two_concurrent_registrations_settle_as_one_owner(self):
        gate = threading.Barrier(2)
        errors = []
        outcomes = []

        def claim(task):
            store = Store(self.store.path)
            try:
                registry = Linkage(store, FakeClock())
                gate.wait(timeout=10)
                outcomes.append(registry.bind_scope(
                    role=linkage.PARENT, scope_key=PROJECT,
                    endpoint=Endpoint(task, HOST, cwd="/parent"),
                )["taskId"])
            except Exception as problem:            # collected, never raised in a thread
                errors.append(problem)
            finally:
                store.close()

        threads = [threading.Thread(target=claim, args=(task,))
                   for task in (PARENT, OTHER_PARENT)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=20)

        owners = self.store.all(
            "SELECT task_id FROM scope_bindings WHERE scope_key = ?"
            "  AND status IN ('active','paused')",
            (PROJECT,),
        )
        self.assertEqual(len(owners), 1, "one scope ended with more than one live owner")
        self.assertEqual(len(outcomes), 1, "both registrations reported success")
        self.assertEqual(len(errors), 1, "the losing registration was not refused")
        self.assertEqual(errors[0].reason, RefusalReason.DUPLICATE_SCOPE_OWNER)


if __name__ == "__main__":
    unittest.main()
