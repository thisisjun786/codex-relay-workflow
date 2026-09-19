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
        # A second initiative may only REFERENCE a project that already has an execution
        # supervisor, and a reference carries no authority to instruct. Both halves are
        # asserted below rather than assumed.
        self.reference = self.supervise(
            initiative=OTHER_INITIATIVE, supervisor=self.supervisor(OTHER_SUPERVISOR),
            kind=linkage.REFERENCE,
        )

    def record(self, *, origin, task, link, digest):
        return self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=task,
            from_scope_key=origin, link_id_value=link, digest=digest,
        )

    def test_only_the_execution_supervisor_may_instruct(self):
        """A secondary initiative references a project's outcome instead of issuing it work,
        so a directive arriving through a reference edge carries no authority."""
        first = self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                            link=self.execution["linkId"], digest="d-one")
        self.assertEqual(first["linkKind"], linkage.EXECUTION)
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, self.record,
            origin=OTHER_INITIATIVE, task=OTHER_SUPERVISOR,
            link=self.reference["linkId"], digest="d-two")

    def test_a_reference_cannot_be_the_first_link_to_a_project(self):
        """Allowed to go first it would BIND its nominated task as the project's parent, which
        is choosing the execution parent by referencing rather than by supervising."""
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, self.supervise,
            initiative="INIT-3", project="PROJ-3",
            supervisor=self.supervisor("01supervisor-three"),
            parent=self.parent("01parent-three"), kind=linkage.REFERENCE)
        self.assertIsNone(self.linkage.owner(linkage.PROJECT, "PROJ-3"))

    def test_two_instructions_for_one_project_are_both_kept_and_reported(self):
        # Two successive instructions from the one supervisor that may issue them. Requiring
        # two ORIGINS meant the ordinary conflict was never reported, now that only the
        # execution supervisor can instruct at all.
        self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                    link=self.execution["linkId"], digest="d-one")
        self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                    link=self.execution["linkId"], digest="d-two")
        contested = self.linkage.contested_directives(linkage.PROJECT, PROJECT)
        self.assertEqual(len(contested), 2)
        self.assertEqual({d["digest"] for d in contested}, {"d-one", "d-two"})

    def test_a_settled_instruction_leaves_the_loser_readable(self):
        first = self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                            link=self.execution["linkId"], digest="d-one")
        second = self.record(origin=INITIATIVE, task=SUPERVISOR_TASK,
                             link=self.execution["linkId"], digest="d-two")
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

    def test_a_handover_is_refused_while_the_scope_still_has_work_it_cannot_move(self):
        """Refuse, and say what it could not move.

        An assignment's identity is sha256(parentTaskId|childTaskId|issueKey) and its queued
        deliveries name the parent's thread, so a handover cannot carry the endpoint across.
        Letting it proceed produced a replacement parent that could not receive the work it had
        just accepted, which three reviewers reported from three directions.
        """
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        refusal = self.assertRefused(
            RefusalReason.HANDOVER_WOULD_STRAND,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT), acknowledged=[rid],
            evidence="the outgoing parent listed its unfinished issues", actor="test")
        self.assertIn(rid, refusal.detail)
        self.assertIn("supersedes", refusal.detail)
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)

    def test_a_handover_of_a_settled_scope_succeeds(self):
        self.supervise()
        replacement = self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="the project has no unfinished issues left", actor="test",
        )
        self.assertEqual(replacement["taskId"], OTHER_PARENT)
        self.assertEqual(replacement["revision"], 2)
        self.assertEqual(replacement["handoverNote"],
                         "the project has no unfinished issues left")
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
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-two")
        self.assertEqual(reference["kind"], linkage.REFERENCE)
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

    def test_a_parent_cannot_hold_a_second_project(self):
        """The contract rule, which is what dissolves the disambiguation problem.

        Each level is one task bound to one Linear level by stable id, so several ready
        projects mean several parents rather than one parent holding several projects. An
        earlier draft of this suite assumed the opposite and built a lookup that had to choose
        between a task's scopes; the coordinator settled it against that reading, so the second
        binding is refused and the lookup never faces the choice.
        """
        self.supervise()
        self.assertRefused(
            RefusalReason.ROLE_ALREADY_BOUND, self.supervise,
            project=OTHER_PROJECT, parent=self.parent())
        self.assertIsNone(self.linkage.owner(linkage.PROJECT, OTHER_PROJECT))

    def test_a_child_cannot_hold_a_second_issue(self):
        """The same rule one level down: a ready batch is several children, not one child on
        several issues. Attaching the second issue to the same child is refused."""
        self.supervise()
        first = self.register()
        self.linkage.attach_issue(first["relationshipId"], PROJECT)
        self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
            issue_key="REL-2", artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-2", dispatch_turn_id="turn-2")
        second = self.store.one(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?", ("REL-2",))
        self.assertRefused(
            RefusalReason.ROLE_ALREADY_BOUND,
            self.linkage.attach_issue, second["relationship_id"], PROJECT)

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
        self.supervise(initiative="INIT-2", project=OTHER_PROJECT,
                       supervisor=self.supervisor(OTHER_SUPERVISOR),
                       parent=self.parent(OTHER_PARENT))
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
        # Refused for belonging to another parent's project, which is the rule that fires
        # first now that one parent cannot hold both. The invariant it protects is the same.
        self.assertIn(OTHER_PROJECT, refusal.detail)
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

    def test_outstanding_follows_the_project_rather_than_one_parent(self):
        """It filtered by the parent task, so work under a previous parent became invisible.

        Now that a handover is refused outright while the scope has unfinished work, this is
        what makes that refusal reachable at all: the set is the PROJECT's, whoever parents
        each row.
        """
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.assertEqual(self.linkage.outstanding(PROJECT), [rid])
        self.assertEqual(self.linkage.outstanding(PROJECT, task_id="01nobody"), [])

    def test_a_settled_project_changes_hands_cleanly(self):
        """The whole point of refusing a stranding handover: once the work is gone, the scope
        moves and nothing is left answering to the parent that stepped down."""
        self.supervise()
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="a settled project", actor="test")
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], OTHER_PARENT)
        edge = self.linkage.link(link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                                         linkage.PROJECT, PROJECT))
        self.assertEqual(edge["lower"]["taskId"], OTHER_PARENT)


class TheObservationsFromTheSameRound(LinkageTestCase):
    def test_a_sender_scope_the_sender_does_not_own_is_reported(self):
        """An unmatched from_scope fell back to the sender's other scopes, so a message could
        be answered linked with no findings about a scope it never named."""
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.counterpart(PARENT, CHILD, from_scope="PROJ-NOT-MINE")
        self.assertIn("foreign_sender_scope", answer["findings"])

    def stage_a_second_scope(self):
        """Write a state the write paths now refuse, to exercise the reader's defence.

        One task holding two live scopes of one role is not reachable through bind_scope any
        more. It is staged directly here because the reader still has to answer safely if a
        store somehow contains it - an older writer, a hand edit, a future bug - and the rule
        is to report the ambiguity rather than pick a row.
        """
        self.supervise()
        self.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
            " host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"
            " handover_note, created_at, updated_at)"
            " VALUES ('bnd-staged-second','parent','project',?,?,?,NULL,NULL,'active',1,"
            "         NULL,NULL,NULL,?,?)",
            (OTHER_PROJECT, PARENT, HOST, self.clock.iso(), self.clock.iso()))

    def test_an_upward_walk_from_an_ambiguous_store_reports_it_rather_than_picking(self):
        self.stage_a_second_scope()
        answer = self.linkage.up(task_id=PARENT)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertEqual(answer["levels"], [])
        self.assertEqual(
            sorted(answer["contention"][0]["candidates"]), sorted([PROJECT, OTHER_PROJECT]))
        named = self.linkage.up(task_id=PARENT, scope_key=OTHER_PROJECT)
        self.assertEqual(named["state"], "resolved")
        self.assertEqual(named["levels"][0]["scopeKey"], OTHER_PROJECT)

    def test_an_explicit_selector_is_not_overridden_by_the_ambiguity(self):
        """A caller that also named an issue or a relationship has already said which
        hierarchy it means, so answering with a question would ignore the selector it gave."""
        self.stage_a_second_scope()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.up(task_id=PARENT, issue_key=ISSUE)
        self.assertEqual(answer["state"], "resolved")
        self.assertEqual(answer["levels"][0]["scopeKey"], ISSUE)
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

class TheThirdRoundFoundTheseToo(LinkageTestCase):
    def test_an_archived_binding_is_revalidated_rather_than_restored(self):
        """bind_scope returned a matching archived binding as success without asking who holds
        the scope now, and binding_plan reactivated one without the role check."""
        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        self.store.db.execute(
            "UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", (PROJECT,))
        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent(OTHER_PARENT))
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER,
            self.linkage.bind_scope, role=linkage.PARENT, scope_key=PROJECT,
            endpoint=self.parent())
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], OTHER_PARENT)

    def test_two_identical_concurrent_binds_converge(self):
        """The lookup happened before BEGIN IMMEDIATE, so both could miss the row and the
        loser collided on the primary key with a database error instead of converging."""
        gate = threading.Barrier(2)
        errors, wins = [], []

        def claim():
            store = Store(self.store.path)
            try:
                worker = Linkage(store, FakeClock())
                gate.wait(timeout=10)
                wins.append(worker.bind_scope(
                    role=linkage.PARENT, scope_key=PROJECT,
                    endpoint=Endpoint(PARENT, HOST, cwd="/parent"))["bindingId"])
            except Exception as problem:
                errors.append(problem)
            finally:
                store.close()

        threads = [threading.Thread(target=claim) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=20)
        self.assertEqual(errors, [], "an identical replay raised instead of converging")
        self.assertEqual(len(set(wins)), 1)
        self.assertEqual(
            len(self.store.all("SELECT 1 FROM scope_bindings WHERE scope_key = ?", (PROJECT,))),
            1)

    def test_the_database_holds_one_live_owner_per_scope(self):
        """The invariant was enforced only by the code that writes it. An index reaches an
        existing store on reopen, which a CHECK constraint never would."""
        import sqlite3

        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        with self.assertRaises(sqlite3.IntegrityError):
            self.store.db.execute(
                "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
                " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
                " superseded_by, handover_note, created_at, updated_at)"
                " VALUES ('bnd-forced','parent','project',?,?,?,NULL,NULL,'active',1,NULL,"
                "         NULL,NULL,?,?)",
                (PROJECT, OTHER_PARENT, HOST, self.clock.iso(), self.clock.iso()))

    def test_a_relay_owned_identifier_keeps_more_than_the_frozen_one(self):
        """64 bits made a collision merge two scopes silently. These ids are not frozen."""
        bid = binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, PARENT)
        self.assertEqual(len(bid), len("bnd-") + 32)
        lid = link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                      linkage.PROJECT, PROJECT)
        self.assertEqual(len(lid), len("lnk-") + 32)

class TheFourthRoundFoundTheseToo(LinkageTestCase):
    def scoped(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def test_a_superseded_relationship_does_not_archive_its_successors_lower_level(self):
        """Archiving is how supersession records itself, so the old row keeps receiving status
        writes. Acting on them archived the binding and edge the successor had taken over."""
        original = self.scoped()
        self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"),
            child=Endpoint("01child-two", HOST), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-successor", dispatch_turn_id="turn-successor",
            supersedes=original)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], "01child-two")
        # Another status write on the superseded row must not reach the successor's level.
        self.registry.set_status(original, "cancelled", actor="test")
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], "01child-two")
        edge = self.linkage.link(
            link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT, linkage.ISSUE, ISSUE))
        self.assertEqual(edge["status"], "active")

    def test_settled_work_still_blocks_a_handover_because_it_can_reopen(self):
        """outstanding excludes a merged assignment, but merged is not gone: its next
        generation opens under the parent named on its own row."""
        rid = self.scoped()
        self.assertEqual(self.linkage.attached(PROJECT), [rid])
        refusal = self.assertRefused(
            RefusalReason.HANDOVER_WOULD_STRAND,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT),
            acknowledged=self.linkage.outstanding(PROJECT),
            evidence="the project still holds assignments", actor="test")
        self.assertIn("reopens", refusal.detail)

    def test_a_successor_for_another_issue_does_not_inherit_the_project(self):
        """supersedes naming a relationship for a DIFFERENT issue pulled its project across."""
        rid = self.scoped()
        other = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint("01child-far", HOST),
            issue_key="REL-FAR", artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-far", dispatch_turn_id="turn-far",
            supersedes=rid)
        self.assertIsNone(self.linkage.attachment(other["relationshipId"]))

    def test_a_downward_walk_survives_a_cyclic_store(self):
        """_reaches was bounded and _descend was not, so a corrupt or hand-edited edge set
        would have recursed until the interpreter stopped it."""
        self.supervise()
        self.store.db.execute(
            "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
            " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
            " superseded_by, created_at, updated_at)"
            " VALUES ('lnk-forced-cycle','execution','project',?,?,'initiative',?,?,"
            "         'active',1,NULL,?,?)",
            (PROJECT, PARENT, INITIATIVE, SUPERVISOR_TASK, self.clock.iso(),
             self.clock.iso()))
        answer = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        self.assertEqual(answer["state"], "resolved")
        self.assertIn("scope_cycle",
                      [row.get("contention") for row in answer["contention"]])

class TheFifthRoundFoundTheseToo(LinkageTestCase):
    def scoped(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def test_a_child_that_took_another_scope_cannot_be_restored_into_this_one(self):
        """The unique index is scoped by issue and resume only checks the same issue, so the
        direct reactivation could put one task live in two scopes at once."""
        rid = self.scoped()
        self.registry.set_status(rid, "cancelled", actor="test")
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key="REL-ELSEWHERE", endpoint=Endpoint(CHILD, HOST))
        self.assertRefused(
            RefusalReason.ROLE_ALREADY_BOUND, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertEqual(self.registry.get(rid)["status"], "cancelled")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))

    def test_a_handover_to_the_current_owner_is_refused(self):
        """The binding id derives from the task, so this superseded a binding with itself:
        one row pointing at its own id, archived and live at once."""
        self.supervise()
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(), acknowledged=[],
            evidence="handing over to myself", actor="test")
        binding = self.linkage.owner(linkage.PROJECT, PROJECT)
        self.assertEqual(binding["revision"], 1)
        self.assertIsNone(binding["supersededBy"])

    def test_a_handover_to_a_blank_endpoint_is_refused(self):
        self.supervise()
        for endpoint in (Endpoint("", HOST), Endpoint(OTHER_PARENT, "")):
            self.assertRefused(
                RefusalReason.UNREGISTERED_SCOPE,
                self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
                expect_task_id=PARENT, endpoint=endpoint, acknowledged=[],
                evidence="a blank replacement", actor="test")
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)

    def test_a_foreign_quoted_scope_is_not_answered_with_another_link(self):
        """Keeping the endpoint's other bindings let a message that named a scope its
        recipient does not hold come back linked, against a scope nobody mentioned, with the
        finding sitting beside a state that contradicted it."""
        rid = self.scoped()
        self.assertIsNotNone(rid)
        answer = self.linkage.counterpart(PARENT, CHILD, quoted_scope="ISS-NOT-HELD")
        self.assertIn("foreign_scope", answer["findings"])
        self.assertEqual(answer["state"], "unlinked")
        self.assertIsNone(answer["link"])

    def test_a_merged_assignment_still_blocks_a_handover(self):
        """Reported again after the attached() fix, so it is pinned directly: merged is an
        ACTIVE relationship and open_generation permits a correction on it."""
        rid = self.scoped()
        self.store.db.execute(
            "UPDATE relationships SET status = 'active' WHERE relationship_id = ?", (rid,))
        self.assertIn(rid, self.linkage.attached(PROJECT))
        self.assertRefused(
            RefusalReason.HANDOVER_WOULD_STRAND,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT),
            acknowledged=self.linkage.outstanding(PROJECT),
            evidence="a settled-looking project", actor="test")

class TheSixthRoundFoundTheseToo(LinkageTestCase):
    def test_a_supervision_with_a_blank_endpoint_is_refused(self):
        """register_supervision planned bindings straight from its endpoints, so an explicit
        empty task or host reached binding_plan without passing _exact."""
        for supervisor, parent in ((self.supervisor(""), self.parent()),
                                   (Endpoint(SUPERVISOR_TASK, ""), self.parent()),
                                   (self.supervisor(), self.parent("")),
                                   (self.supervisor(), Endpoint(PARENT, ""))):
            self.assertRefused(
                RefusalReason.UNREGISTERED_SCOPE, self.supervise,
                supervisor=supervisor, parent=parent)
        self.assertEqual(len(self.store.all("SELECT 1 FROM scope_bindings")), 0)

    def test_a_peer_link_with_a_blank_endpoint_is_refused(self):
        self.supervise()
        self.supervise(project=OTHER_PROJECT, parent=self.parent(OTHER_PARENT))
        self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE, self.linkage.register_peer,
            left_project=PROJECT, left_parent=Endpoint("", HOST),
            right_project=OTHER_PROJECT, right_parent=self.parent(OTHER_PARENT))

    def test_an_unregistered_scope_is_told_so_even_when_the_tasks_match(self):
        """The same-owner refusal ran before the scope was read, so a scope nobody owns got a
        refusal about equal task ids and no conflict record."""
        refusal = self.assertRefused(
            RefusalReason.UNREGISTERED_SCOPE,
            self.linkage.handover, role=linkage.PARENT, scope_key="PROJ-NOBODY",
            expect_task_id=PARENT, endpoint=self.parent(), acknowledged=[],
            evidence="handing over a scope that does not exist", actor="test")
        self.assertIn("no live owner", refusal.detail)

    def test_a_same_owner_handover_is_recorded_as_a_contest_like_any_other(self):
        self.supervise()
        self.assertRefused(
            RefusalReason.HANDOVER_UNCONFIRMED,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(), acknowledged=[],
            evidence="handing over to myself", actor="test")
        self.assertTrue(self.linkage.conflicts(linkage.PROJECT, PROJECT),
                        "the refusal left no record of the contest")

    def test_a_cross_role_resume_says_role_mismatch_rather_than_already_bound(self):
        """Different-role conflicts have their own contract reason; reusing the same-role one
        told the caller the wrong thing about what went wrong."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "cancelled", actor="test")
        self.linkage.bind_scope(
            role=linkage.SUPERVISOR, scope_key="INIT-LATER", endpoint=Endpoint(CHILD, HOST))
        self.assertRefused(
            RefusalReason.SCOPE_ROLE_MISMATCH, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")

class TheSeventhRoundFoundTheseToo(LinkageTestCase):
    def test_the_escape_route_a_stranding_refusal_prescribes_is_reachable(self):
        """The refusal told a caller to move each assignment with supersedes, and then
        attach_refusal rejected the successor for naming a parent that was not yet the
        project's - so the only prescribed way out of the refusal was itself refused.
        """
        self.supervise()
        original = self.register()
        rid = original["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.assertRefused(
            RefusalReason.HANDOVER_WOULD_STRAND,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT), acknowledged=[rid],
            evidence="before the work moved", actor="test")

        # Move the assignment to the incoming parent, which is what the refusal asked for.
        self.registry.register(
            parent=Endpoint(OTHER_PARENT, HOST, cwd="/parent"),
            child=Endpoint("01child-two", HOST), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[OTHER_PARENT],
            dispatch_request_id="dispatch-moved", dispatch_turn_id="turn-moved",
            supersedes=rid)
        self.assertEqual(self.linkage.attached(PROJECT, PARENT), [])

        # And now the scope follows it. The acknowledgement is still the PROJECT's unfinished
        # work, whoever parents it: the incoming owner confirms what it is taking on, and the
        # stranding check is the separate question of what still names the outgoing one.
        replacement = self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT),
            acknowledged=self.linkage.outstanding(PROJECT),
            evidence="the work moved first", actor="test")
        self.assertEqual(replacement["taskId"], OTHER_PARENT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], "01child-two")

    def test_an_identical_instruction_after_a_handover_is_its_own_record(self):
        """A handover advances the link revision. Without it in the identity, a replacement
        supervisor re-issuing the same instruction derived its predecessor's id and silently
        replayed that record."""
        execution = self.supervise()
        first = self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-same")
        self.linkage.handover(
            role=linkage.SUPERVISOR, scope_key=INITIATIVE, expect_task_id=SUPERVISOR_TASK,
            endpoint=self.supervisor(OTHER_SUPERVISOR), acknowledged=[],
            evidence="a new supervisor", actor="test")
        second = self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=OTHER_SUPERVISOR,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-same")
        self.assertNotEqual(first["directiveId"], second["directiveId"])
        self.assertEqual(second["fromTaskId"], OTHER_SUPERVISOR)

    def test_a_sender_scope_the_sender_does_not_own_is_not_answered_with_another_link(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        answer = self.linkage.counterpart(PARENT, CHILD, from_scope="PROJ-NOT-MINE")
        self.assertIn("foreign_sender_scope", answer["findings"])
        self.assertEqual(answer["state"], "unlinked")
        self.assertIsNone(answer["link"])

class TheEighthRoundFoundTheseToo(LinkageTestCase):
    def test_project_history_alone_does_not_let_a_foreign_parent_attach(self):
        """The relaxation for a moving assignment was too wide.

        Accepting any assignment whose issue merely HAS project history let an unrelated
        registration under a foreign parent repoint the issue edge away from the project's
        live owner, which is the guard's whole job. It is relaxed only for a genuine
        successor: one that supersedes a relationship itself scoped to this project.
        """
        self.supervise()
        self.supervise(project=OTHER_PROJECT, parent=self.parent(OTHER_PARENT))
        first = self.register()
        self.linkage.attach_issue(first["relationshipId"], PROJECT)
        self.registry.set_status(first["relationshipId"], "cancelled", actor="test")
        unrelated = self.registry.register(
            parent=Endpoint(OTHER_PARENT, HOST), child=Endpoint("01child-far", HOST),
            issue_key=ISSUE, artifact_roots=[self.root],
            allowed_recipients=[OTHER_PARENT], dispatch_request_id="dispatch-unrelated",
            dispatch_turn_id="turn-unrelated")
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE,
            self.linkage.attach_issue, unrelated["relationshipId"], PROJECT)

    def test_an_assignment_parked_on_a_third_parent_still_blocks_a_handover(self):
        """attached() asked only about the OUTGOING owner, so work moved to somebody who is
        not the incoming owner slipped through and the project owner and its issue edges ended
        up naming different tasks."""
        self.supervise()
        original = self.register()
        rid = original["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.register(
            parent=Endpoint("01parent-three", HOST), child=Endpoint("01child-two", HOST),
            issue_key=ISSUE, artifact_roots=[self.root],
            allowed_recipients=["01parent-three"], dispatch_request_id="dispatch-third",
            dispatch_turn_id="turn-third", supersedes=rid)
        self.assertEqual(self.linkage.attached(PROJECT, PARENT), [])
        self.assertRefused(
            RefusalReason.HANDOVER_WOULD_STRAND,
            self.linkage.handover, role=linkage.PARENT, scope_key=PROJECT,
            expect_task_id=PARENT, endpoint=self.parent(OTHER_PARENT),
            acknowledged=self.linkage.outstanding(PROJECT),
            evidence="work parked on a third parent", actor="test")

    def test_the_same_child_registered_again_does_not_let_its_predecessor_archive_it(self):
        """The guard compared TASKS, which is enough while a replacement uses a different
        child and wrong the moment the same one is registered again for the same issue: the
        ids matched, the archived predecessor looked like the owner, and its status writes
        reached the successor's binding and edge."""
        self.supervise()
        first = self.register()
        self.linkage.attach_issue(first["relationshipId"], PROJECT)
        self.registry.set_status(first["relationshipId"], "archived", actor="test")
        # The project changes hands, then the SAME child is assigned the same issue again
        # under the new parent. That is a different relationship with the same child, which is
        # the shape a task-keyed guard cannot tell from its own predecessor.
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="the archived assignment left nothing behind", actor="test")
        again = self.registry.register(
            parent=Endpoint(OTHER_PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[OTHER_PARENT],
            dispatch_request_id="dispatch-again", dispatch_turn_id="turn-again",
            project_key=PROJECT)
        self.assertNotEqual(again["relationshipId"], first["relationshipId"])
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        # A late status write on the predecessor must not reach the successor's level.
        self.registry.set_status(first["relationshipId"], "cancelled", actor="test")
        self.assertIsNotNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.assertEqual(
            self.linkage.attachment(again["relationshipId"])["projectKey"], PROJECT)


class TheNinthRoundFoundTheseToo(LinkageTestCase):
    def scoped(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def test_the_same_child_under_a_second_parent_is_still_a_duplicate_assignment(self):
        """The rival check excluded rows sharing this child.

        That was meant to let a caller restate its own assignment, but an identical
        restatement derives the SAME relationship id and returns long before the check, so
        the only thing the exclusion actually admitted was one child registered for one issue
        under TWO parents: two live assignments, both parents authorized to deliver, and a
        project whose owner matches neither.
        """
        original = self.scoped()
        self.assertRefused(
            RefusalReason.DUPLICATE_ASSIGNMENT, self.registry.register,
            parent=self.parent(OTHER_PARENT), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root],
            allowed_recipients=[OTHER_PARENT], dispatch_request_id="dispatch-second-parent",
            dispatch_turn_id="turn-second-parent")
        live = self.store.all(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?"
            "  AND status IN ('active','paused') AND superseded_by IS NULL", (ISSUE,))
        self.assertEqual([row["relationship_id"] for row in live], [original])

    def test_resuming_after_the_project_changed_hands_is_refused(self):
        """Archiving releases the issue and leaves nothing live for attached() to see, so the
        project hands on with no work to strand and no refusal. Coming back afterwards
        restored an edge under a parent the project no longer has, and the relationship and
        the delivery authorization derived from it named the old parent while linkage named
        the new one - the split routing a handover exists to prevent."""
        rid = self.scoped()
        self.registry.set_status(rid, "archived", actor="test")
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="the archived assignment left nothing behind", actor="test")
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertEqual(self.registry.get(rid)["status"], "archived",
                         "the refused resume moved the relationship anyway")
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], OTHER_PARENT)
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))

    def test_a_project_left_without_a_parent_does_not_get_its_issue_edge_back(self):
        """The same restoration under a project nobody owns at all. attach_in refuses a fresh
        attachment to a parentless project, and reactivation is the other way in."""
        rid = self.scoped()
        self.registry.set_status(rid, "archived", actor="test")
        with self.store.transaction() as db:
            db.execute(
                "UPDATE scope_bindings SET status = 'archived'"
                "  WHERE scope_kind = ? AND scope_key = ? AND role = ?",
                (linkage.PROJECT, PROJECT, linkage.PARENT))
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertEqual(self.registry.get(rid)["status"], "archived")

if __name__ == "__main__":
    unittest.main()
