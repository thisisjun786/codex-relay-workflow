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


class ReviewFoundTheseByReproducingThem(LinkageTestCase):
    """Each case pins a defect an independent review reproduced in the delivered code.

    They are grouped rather than scattered so the next reader can see what was actually wrong
    and what now stops it, instead of inferring it from a diff.
    """

    def test_a_handover_decides_inside_the_transaction_it_writes_in(self):
        """The owner and the outstanding work were read before BEGIN IMMEDIATE.

        Two callers could each see the same outgoing owner, each find the outstanding set
        unchanged, and both write - leaving one scope with two live owners. Reading inside the
        transaction is what serialises them, and the second caller now sees the first one's
        write and is refused for naming an owner that no longer holds the scope.
        """
        self.supervise()
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="the first handover", actor="test")
        # A second caller still holding the pre-handover reading.
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent("01parent-three"), acknowledged=[],
            evidence="a stale second handover", actor="test")
        live = self.store.all(
            "SELECT task_id FROM scope_bindings WHERE scope_key = ? AND role = ?"
            "  AND status IN ('active','paused')",
            (PROJECT, linkage.PARENT))
        self.assertEqual([row["task_id"] for row in live], [OTHER_PARENT])

    def test_a_handover_is_not_a_way_to_hold_two_roles(self):
        """It bypassed binding_plan, so the incoming owner skipped the role rule."""
        self.supervise()
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.supervisor(SUPERVISOR_TASK),
            acknowledged=[], evidence="the supervisor tried to take the project",
            actor="test")
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)

    def test_a_replayed_supervision_must_agree_about_hosts(self):
        """The replay compared task ids only, so a different host returned success."""
        self.supervise()
        self.assertRefused(
            RefusalReason.LINK_CONFLICT, self.linkage.register_supervision,
            initiative_key=INITIATIVE, project_key=PROJECT,
            supervisor=self.supervisor(),
            parent=Endpoint(PARENT, "some-other-host", cwd="/parent"),
        )
        self.assertEqual(
            self.linkage.owner(linkage.PROJECT, PROJECT)["hostId"], HOST)

    def test_a_replayed_directive_is_not_a_way_past_validation(self):
        """The derived id returned an existing row BEFORE anything was checked.

        Replaying the same digest with a task that owns nothing and a link that joins nothing
        was accepted, because only the id had to match.
        """
        execution = self.supervise()
        self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-one")
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.linkage.record_directive,
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id="01nobody",
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-one")

    def test_a_directive_must_come_from_the_links_own_upper_endpoint(self):
        self.supervise()
        self.supervise(initiative="INIT-2", project=OTHER_PROJECT,
                       supervisor=self.supervisor(OTHER_SUPERVISOR),
                       parent=self.parent(OTHER_PARENT))
        other = link_id(linkage.EXECUTION, linkage.INITIATIVE, "INIT-2",
                        linkage.PROJECT, OTHER_PROJECT)
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, self.linkage.record_directive,
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=other, digest="d-two")

    def test_a_scope_nobody_registered_reads_as_unregistered(self):
        """down() always appended a level, so its unregistered branch was unreachable."""
        answer = self.linkage.down(linkage.INITIATIVE, "INIT-NEVER-SEEN")
        self.assertEqual(answer["state"], "unregistered")
        self.assertIs(answer["readable"], True)
        self.assertEqual(answer["levels"], [])
        self.assertEqual([gap["gap"] for gap in answer["gaps"]],
                         ["initiative_without_supervisor"])

    def test_an_upward_walk_reports_the_same_contention_a_downward_one_does(self):
        """Upward omitted instruction conflicts and drift, so the same store answered
        differently depending on which way it was read."""
        execution = self.supervise()
        reference = self.supervise(
            initiative="INIT-2", supervisor=self.supervisor(OTHER_SUPERVISOR),
            kind=linkage.REFERENCE)
        self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-one")
        self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=OTHER_SUPERVISOR,
            from_scope_key="INIT-2", link_id_value=reference["linkId"], digest="d-two")
        downward = self.linkage.down(linkage.PROJECT, PROJECT)
        upward = self.linkage.up(task_id=PARENT)
        self.assertEqual(
            len([row for row in downward["contention"]
                 if row.get("contention") == "instruction_conflict"]),
            len([row for row in upward["contention"]
                 if row.get("contention") == "instruction_conflict"]),
        )
        self.assertTrue([row for row in upward["contention"]
                         if row.get("contention") == "instruction_conflict"])

class TheHostedReviewFoundTheseOnTheOpenPullRequest(LinkageTestCase):
    """Seven findings from two hosted reviewers on PR 61, each reproduced before it was fixed.

    Their root is one assumption that was wrong twice over: that a task owns exactly one scope,
    and that a relationship status is either active or gone. Neither holds - the role rule only
    forbids two different ROLES, and a paused assignment still owns its issue.
    """

    def scoped(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def test_pausing_an_assignment_keeps_its_issue(self):
        """paused was collapsed into archived, so one store answered two ways.

        AssignmentView still reported the child as responsible while the issue scope reported
        no owner at all and linkage-down reported issue_without_child.
        """
        rid = self.scoped()
        self.registry.set_status(rid, "paused", actor="test")
        owner = self.linkage.owner(linkage.ISSUE, ISSUE)
        self.assertIsNotNone(owner, "pausing released the issue instead of holding it")
        self.assertEqual(owner["taskId"], CHILD)
        self.assertEqual(owner["status"], "paused")
        answer = self.linkage.down(linkage.PROJECT, PROJECT)
        self.assertNotIn("issue_without_child", [gap["gap"] for gap in answer["gaps"]])

    def test_cancelling_still_releases_the_issue(self):
        rid = self.scoped()
        self.registry.set_status(rid, "cancelled", actor="test")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))

    def test_resuming_over_a_reassigned_issue_is_refused(self):
        """Cancelling RELEASES an issue, so another child can take the scope meanwhile.

        resume checked relationships and never looked at who holds the scope now, so it
        reactivated the old binding and left the issue with two live owners.
        """
        rid = self.scoped()
        self.registry.set_status(rid, "cancelled", actor="test")
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint("01child-two", HOST))
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        live = self.store.all(
            "SELECT task_id FROM scope_bindings WHERE scope_key = ? AND role = ?"
            "  AND status IN ('active','paused')",
            (ISSUE, linkage.CHILD))
        self.assertEqual([row["task_id"] for row in live], ["01child-two"])
        self.assertEqual(self.registry.get(rid)["status"], "cancelled",
                         "the refused resume moved the relationship anyway")

    def test_a_child_is_not_handed_over_through_linkage(self):
        """It moved the binding and the edge while relationships kept naming the old child."""
        self.scoped()
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH,
            self.linkage.handover, role=linkage.CHILD, scope_key=ISSUE,
            expect_task_id=CHILD, endpoint=Endpoint("01child-two", HOST),
            acknowledged=[], evidence="trying to move a child sideways", actor="test")
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)

    def test_a_parent_owning_two_projects_is_answered_about_the_right_one(self):
        """_any_binding picked one scope per task by revision alone.

        With parent P owning projects A and B and the child under B, the lookup could answer
        about A and report unregistered_link despite a live B to issue edge.
        """
        self.supervise()
        self.supervise(initiative="INIT-2", project=OTHER_PROJECT, parent=self.parent(),
                       supervisor=self.supervisor(OTHER_SUPERVISOR), kind=linkage.REFERENCE)
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.counterpart(PARENT, CHILD)
        self.assertEqual(answer["state"], "linked")
        self.assertEqual(answer["counterpart"]["scopeKey"], ISSUE)
        self.assertNotIn("unregistered_link", answer["findings"])

    def test_a_quoted_scope_selects_the_binding_instead_of_being_checked_against_one(self):
        """A child holding two issues was answered about whichever binding sorted first, so a
        correctly routed message was told foreign_scope about a scope nobody named."""
        self.supervise()
        first = self.register()
        self.linkage.attach_issue(first["relationshipId"], PROJECT)
        second = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
            issue_key="REL-2", artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-2", dispatch_turn_id="turn-2",
            project_key=PROJECT)
        self.assertEqual(second["issueKey"], "REL-2")
        for issue in (ISSUE, "REL-2"):
            answer = self.linkage.counterpart(PARENT, CHILD, quoted_scope=issue)
            self.assertEqual(answer["counterpart"]["scopeKey"], issue)
            self.assertNotIn("foreign_scope", answer["findings"], issue)

    def test_a_revision_that_does_not_exist_yet_is_not_current_either(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.counterpart(PARENT, CHILD, quoted_revision=99)
        self.assertIn("stale_revision", answer["findings"])

class TheSecondReviewRoundFoundTheseToo(LinkageTestCase):
    """Five more, from the same two reviewers, on the head that fixed the first seven."""

    def test_a_scope_key_reused_at_another_level_cannot_forge_a_link(self):
        """_joining_link compared keys without their kinds.

        A scope identity is its kind AND its key, so an identifier that appears at two levels
        let a reversed edge match and two unlinked tasks be reported as linked.
        """
        shared = "SHARED-KEY"
        self.linkage.bind_scope(
            role=linkage.SUPERVISOR, scope_key=shared, endpoint=self.supervisor())
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=shared, endpoint=Endpoint("01child-far", HOST))
        answer = self.linkage.counterpart(SUPERVISOR_TASK, "01child-far")
        self.assertEqual(answer["state"], "unlinked")
        self.assertIn("unregistered_link", answer["findings"])

    def test_a_directive_naming_the_right_key_at_the_wrong_level_is_refused(self):
        execution = self.supervise()
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, self.linkage.record_directive,
            scope_kind=linkage.ISSUE, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-kind")

    def test_one_issue_cannot_belong_to_two_projects(self):
        """An issue belongs to one project whichever assignment asks.

        Two rules now hold this, and which one fires depends on the route. A replacement
        inherits the project of what it supersedes, so it carries its own scope row and the
        per-relationship rule answers first - that is what happens here. The
        cross-relationship rule added alongside it is the backstop for a sibling that is
        scoped while this one is not, which the per-relationship rule cannot see.
        """
        self.supervise()
        # One parent owning both projects, which the role contract allows. Without it the
        # attempt is refused a step earlier, for belonging to another parent's project, and
        # this case would pass without ever reaching the rule it is named for.
        self.supervise(initiative="INIT-2", project=OTHER_PROJECT,
                       supervisor=self.supervisor(OTHER_SUPERVISOR),
                       parent=self.parent(), kind=linkage.REFERENCE)
        first = self.register()
        self.linkage.attach_issue(first["relationshipId"], PROJECT)
        second = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("01child-two", HOST),
            issue_key=ISSUE, artifact_roots=[self.root],
            allowed_recipients=[PARENT], dispatch_request_id="dispatch-rival",
            dispatch_turn_id="turn-rival", supersedes=first["relationshipId"])
        refusal = self.assertRefused(
            RefusalReason.FOREIGN_SCOPE,
            self.linkage.attach_issue, second["relationshipId"], OTHER_PROJECT)
        self.assertIn(PROJECT, refusal.detail)
        edges = self.store.all(
            "SELECT link_id FROM scope_links WHERE lower_kind = ? AND lower_key = ?"
            "  AND status IN ('active','paused')",
            (linkage.ISSUE, ISSUE))
        self.assertEqual(len(edges), 1, "the issue acquired an edge from a second project")

    def test_a_replacement_inherits_the_project_of_what_it_supersedes(self):
        """Superseding a scoped assignment without restating the project archived the outgoing
        binding and edge and attached no successor, so the issue lost its level silently."""
        self.supervise()
        original = self.register()
        self.linkage.attach_issue(original["relationshipId"], PROJECT)
        replacement = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("01child-two", HOST), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-successor", dispatch_turn_id="turn-successor",
            supersedes=original["relationshipId"],
        )
        self.assertEqual(
            self.linkage.attachment(replacement["relationshipId"])["projectKey"], PROJECT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], "01child-two")

    def test_a_second_handover_still_sees_the_projects_unfinished_work(self):
        """outstanding filtered by the parent task, so after one handover the replacement
        appeared to owe nothing and a second replacement could take the project free."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[rid],
            evidence="the first handover", actor="test")
        self.assertEqual(self.linkage.outstanding(PROJECT), [rid])
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=OTHER_PARENT, endpoint=self.parent("01parent-three"),
            acknowledged=[], evidence="a second handover owing nothing", actor="test")

    def test_a_parent_handover_moves_the_assignments_it_acknowledged(self):
        """A parent handover does NOT reparent its assignments, and that is deliberate.

        An earlier attempt here rewrote relationships.parent_task_id in place, and review
        caught what that costs: relationship_id is sha256(parent|child|issue), so the row's
        stored identity would no longer derive from its own columns, and queued deliveries
        still name the old parent's thread. It was reverted rather than patched, because
        moving an assignment to a new parent is supersession - a new relationship with a new
        identity - and that is a larger change than this issue owns.

        What holds today: the scope moves, the assignments do not, and the mismatch is
        visible rather than silent. counterpart reports owner_drift, so a message about one of
        these assignments is told the recorded owner and the live one disagree.
        """
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[rid],
            evidence="taking on the unfinished issue", actor="test")
        kept = self.registry.get(rid)
        self.assertEqual(kept["parent"]["taskId"], PARENT,
                         "the assignment's identity columns were rewritten under it")
        self.assertEqual(kept["relationshipId"], rid)
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], OTHER_PARENT)

class TheObservationsFromTheSameRound(LinkageTestCase):
    def test_a_sender_scope_the_sender_does_not_own_is_reported(self):
        """An unmatched from_scope fell back to the sender's other scopes, so a message could
        be answered linked with no findings about a scope it never named."""
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.counterpart(PARENT, CHILD, from_scope="PROJ-NOT-MINE")
        self.assertIn("foreign_sender_scope", answer["findings"])

    def test_an_upward_walk_from_a_task_owning_several_scopes_says_so(self):
        """owner_of_task chose one with LIMIT 1, dropping the other hierarchies silently."""
        self.supervise()
        self.supervise(initiative="INIT-2", project=OTHER_PROJECT, parent=self.parent(),
                       supervisor=self.supervisor(OTHER_SUPERVISOR), kind=linkage.REFERENCE)
        answer = self.linkage.up(task_id=PARENT)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertEqual(answer["levels"], [])
        self.assertEqual(
            sorted(answer["contention"][0]["candidates"]), sorted([PROJECT, OTHER_PROJECT]))
        # Naming the scope answers it.
        named = self.linkage.up(task_id=PARENT, scope_key=OTHER_PROJECT)
        self.assertEqual(named["state"], "resolved")
        self.assertEqual(named["levels"][0]["scopeKey"], OTHER_PROJECT)

    def test_an_unreadable_answer_keeps_the_fault_that_caused_it(self):
        """Every sqlite3.Error collapsed into one word, so corruption, schema drift and a
        query fault were indistinguishable to whoever had to act on them."""
        self.supervise()
        self.store.db.close()
        for answer in (self.linkage.down(linkage.INITIATIVE, INITIATIVE),
                       self.linkage.up(task_id=PARENT),
                       self.linkage.counterpart(PARENT, CHILD)):
            self.assertEqual(answer["state"], "unreadable")
            self.assertEqual(answer["findings"] if "findings" in answer else [], [])
            self.assertTrue(answer["detail"], "the fault was discarded with the answer")

if __name__ == "__main__":
    unittest.main()
