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

    def duplicated_store(self):
        """A store holding two live owners of one project, the way an older writer could.

        The index that forbids it is dropped first, which is the state a store that could not
        install it arrives in. This is what an unenforced index proves can exist, and
        therefore what these readers have to be able to describe.
        """
        import os

        path = os.path.join(self.tmp, "duplicated.sqlite")
        legacy = Store(path)
        legacy.db.execute("DROP INDEX scope_bindings_one_live_owner")
        with legacy.transaction() as db:
            for task, revision in (("01owner-one", 1), ("01owner-two", 2)):
                db.execute(
                    "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
                    " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
                    " superseded_by, handover_note, created_at, updated_at)"
                    " VALUES (?,?,?,?,?,?,NULL,NULL,'active',?,NULL,NULL,NULL,?,?)",
                    (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, task),
                     linkage.PARENT, linkage.PROJECT, PROJECT, task, HOST, revision,
                     "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
        return Linkage(Store(path), self.clock)


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
        """Inactive means DEAD. This first used a paused assignment, which was wrong on the
        module's own terms: paused is live ownership everywhere else here, and refusing it
        made migrating an existing paused assignment into the hierarchy impossible without
        first resuming it to active - a status change nobody asked for, to perform a read-only
        migration. Archived and cancelled are what this rule is about."""
        self.supervise()
        for status in ("archived", "cancelled"):
            relationship = self.registry.register(
                parent=self.parent(), child=Endpoint("01child-" + status, HOST, cwd=self.root),
                issue_key="REL-" + status, artifact_roots=[self.root],
                allowed_recipients=[PARENT], dispatch_request_id="dispatch-" + status,
                dispatch_turn_id="turn-" + status)
            self.registry.set_status(relationship["relationshipId"], status, actor="test")
            self.assertRefused(
                RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                self.linkage.attach_issue, relationship["relationshipId"], PROJECT,
            )

    def test_a_paused_assignment_can_still_be_migrated_into_the_hierarchy(self):
        """And it arrives paused. Attaching it as active would have made this the one place
        that disagreed about what paused means, and left a paused child holding a live
        binding - the split state the lifecycle path exists to prevent."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.registry.set_status(rid, "paused", actor="test")
        self.linkage.attach_issue(rid, PROJECT)
        owner = self.linkage.owner(linkage.ISSUE, ISSUE)
        self.assertEqual(owner["taskId"], CHILD)
        self.assertEqual(owner["status"], "paused")
        self.assertEqual(self.linkage.attachment(rid)["link"]["status"], "paused")

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
        """supersedes naming a relationship for a DIFFERENT issue pulled its project across.

        Refusing the registration outright replaces the weaker guarantee this test first
        pinned - that the successor simply did not inherit the project. Letting it register
        still archived the predecessor on the way past, so an unrelated live assignment was
        discarded and a project handover could afterwards report nothing outstanding over work
        nobody had moved. A successor replaces the assignment for its own issue.
        """
        rid = self.scoped()
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.registry.register,
            parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint("01child-far", HOST),
            issue_key="REL-FAR", artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-far", dispatch_turn_id="turn-far",
            supersedes=rid)
        # The predecessor it tried to borrow is untouched, which is the part that mattered.
        self.assertEqual(self.registry.get(rid)["status"], "active")
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        self.assertEqual(self.linkage.attachment(rid)["projectKey"], PROJECT)

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


class TheTenthRoundFoundTheseToo(LinkageTestCase):
    def test_the_supervising_initiative_cannot_also_reference_its_own_project(self):
        """A reference and an execution edge for ONE scope pair are both live at once.

        The unique index is per kind, so it permits that, and the reference check only asked
        whether a reference named the execution parent - never whether it came from the
        initiative that already supervises. Two live edges then answer the same message
        differently about the one thing a reference exists to say: that it carries no
        directive authority.
        """
        execution = self.supervise()
        self.assertRefused(
            RefusalReason.LINK_CONFLICT, self.supervise, kind=linkage.REFERENCE)
        live = self.store.all(
            "SELECT link_id FROM scope_links WHERE lower_key = ?"
            "  AND status IN ('active','paused')", (PROJECT,))
        self.assertEqual([row["link_id"] for row in live], [execution["linkId"]])
        # A DIFFERENT initiative may still reference it. That is the whole point of the kind.
        self.supervise(initiative=OTHER_INITIATIVE, supervisor=self.supervisor(
            OTHER_SUPERVISOR), kind=linkage.REFERENCE)

    def test_two_live_edges_for_one_pair_are_reported_rather_than_chosen(self):
        """Registration refuses to create this now, so the state is written directly - a store
        that predates the refusal can hold it. The reader must not pick: taking the highest
        revision answered about whichever edge sorted first, and equal revisions made that
        arbitrary. Reporting it as unlinked would be worse still, denying a link that exists.
        """
        self.supervise()
        stray = link_id(linkage.REFERENCE, linkage.INITIATIVE, INITIATIVE,
                        linkage.PROJECT, PROJECT)
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
                " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
                " superseded_by, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,'active',1,NULL,?,?)",
                (stray, linkage.REFERENCE, linkage.INITIATIVE, INITIATIVE, SUPERVISOR_TASK,
                 linkage.PROJECT, PROJECT, PARENT, "2026-09-19T00:00:00Z",
                 "2026-09-19T00:00:00Z"))
        answer = self.linkage.counterpart(SUPERVISOR_TASK, PARENT)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertIs(answer["readable"], True)
        self.assertIsNone(answer["link"])
        self.assertIn("link_contention", answer["findings"])
        self.assertEqual(len(answer["candidates"]), 2)
        self.assertIn(stray, [c["linkId"] for c in answer["candidates"]])
        self.assertEqual({c["scopeKey"] for c in answer["candidates"]}, {PROJECT})

    def test_a_settled_directive_is_not_re_decided_by_the_next_caller(self):
        """settle_directive overwrote the disposition and the decider, so a second decision
        took the record from the first and left nothing saying the two disagreed - the
        opposite of retaining a contested instruction."""
        execution = self.supervise()
        directive = self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-one")
        self.linkage.settle_directive(
            directive["directiveId"], "chosen", decided_by="the parent that ruled")
        self.assertRefused(
            RefusalReason.LINK_CONFLICT, self.linkage.settle_directive,
            directive["directiveId"], "superseded", decided_by="somebody later")
        settled = [d for d in self.linkage.directives(linkage.PROJECT, PROJECT)
                   if d["directiveId"] == directive["directiveId"]][0]
        self.assertEqual(settled["disposition"], "chosen")
        self.assertEqual(settled["decidedBy"], "the parent that ruled")
        # Restating the same decision converges and keeps the ORIGINAL decider.
        again = self.linkage.settle_directive(
            directive["directiveId"], "chosen", decided_by="a replaying caller")
        self.assertEqual(again["decidedBy"], "the parent that ruled")

    def test_a_directly_rebound_child_scope_survives_a_late_relationship_write(self):
        """The guard read "no live assignment anywhere" as "the scope is still mine".

        Archiving an assignment frees its issue scope, and bind_scope can then claim it
        directly - with the same child, which derives the SAME binding id. A direct claim
        writes no relationship row, so _owns_its_issue found nothing live and answered that
        the archived relationship still owned the scope; repeating the status write on that
        dead row then archived the new standalone claim and owner(issue) went back to None.

        A relationship releases its issue scope once, when it stops being live.
        """
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "archived", actor="test")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST))
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        self.registry.set_status(rid, "cancelled", actor="test")
        held = self.linkage.owner(linkage.ISSUE, ISSUE)
        self.assertIsNotNone(
            held, "a late write from an already-released relationship took the new claim")
        self.assertEqual(held["taskId"], CHILD)


THIRD_PARENT = "01parent-three"


class TheEleventhRoundFoundTheseToo(LinkageTestCase):
    def stray_link(self, kind, initiative, *, revision, upper_task):
        """Write an edge the API now refuses to create, the way an older writer could have."""
        lid = link_id(kind, linkage.INITIATIVE, initiative, linkage.PROJECT, PROJECT)
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
                " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
                " superseded_by, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,'active',?,NULL,?,?)",
                (lid, kind, linkage.INITIATIVE, initiative, upper_task, linkage.PROJECT,
                 PROJECT, PARENT, revision, "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
        return lid

    def test_two_live_supervisions_of_one_project_are_reported_not_walked(self):
        """up() took the highest revision and called the result resolved, so a caller got one
        arbitrary hierarchy and no sign that another initiative claimed the same project. The
        stray edge here carries the HIGHER revision, so the old code would have returned it."""
        self.supervise()
        stray = self.stray_link(linkage.EXECUTION, OTHER_INITIATIVE,
                                revision=2, upper_task=OTHER_SUPERVISOR)
        answer = self.linkage.up(task_id=PARENT)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertIs(answer["readable"], True)
        competing = [c for c in answer["contention"]
                     if c["contention"] == "competing_parents"]
        self.assertEqual(len(competing), 1)
        self.assertIn(stray, competing[0]["candidates"])
        self.assertEqual(len(competing[0]["candidates"]), 2)

    def test_a_task_with_two_historical_scopes_is_not_answered_about_one_of_them(self):
        """counterpart stopped at the first candidate pair that had any link. Bindings outlive
        a handover on purpose, so a task accumulates archived scopes, and two historical pairs
        can both be joined by a live edge - whereupon the one that sorted first was answered
        about as though the other did not exist."""
        self.supervise()
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="the first project moved on", actor="test")
        self.supervise(project=OTHER_PROJECT)
        self.linkage.handover(
            role=linkage.PARENT, scope_key=OTHER_PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(THIRD_PARENT), acknowledged=[],
            evidence="and so did the second", actor="test")
        answer = self.linkage.counterpart(SUPERVISOR_TASK, PARENT)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertIsNone(answer["link"])
        self.assertIn("link_contention", answer["findings"])
        self.assertEqual({c["scopeKey"] for c in answer["candidates"]},
                         {PROJECT, OTHER_PROJECT})
        # Naming the scope the message is about is how a caller resolves it, which is what
        # OPS-7.4 says a message carries in the first place.
        narrowed = self.linkage.counterpart(
            SUPERVISOR_TASK, PARENT, quoted_scope=OTHER_PROJECT)
        self.assertEqual(narrowed["state"], "linked")
        self.assertEqual(narrowed["counterpart"]["scopeKey"], OTHER_PROJECT)

    def test_a_refused_settlement_is_retained_as_a_contest(self):
        """The refusal raised from inside the transaction, so the rollback took the evidence
        with it: the disagreement this module exists to preserve was the one thing the refusal
        destroyed."""
        execution = self.supervise()
        directive = self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest="d-one")
        self.linkage.settle_directive(
            directive["directiveId"], "chosen", decided_by="alice")
        self.assertRefused(
            RefusalReason.LINK_CONFLICT, self.linkage.settle_directive,
            directive["directiveId"], "superseded", decided_by="bob")
        contests = [c for c in self.linkage.conflicts(linkage.PROJECT, PROJECT)
                    if c["reason"] == RefusalReason.LINK_CONFLICT]
        self.assertEqual(len(contests), 1)
        self.assertEqual(contests[0]["incumbent"], "alice")
        self.assertEqual(contests[0]["challenger"], "bob")
        # And the decision itself still stands.
        settled = [d for d in self.linkage.directives(linkage.PROJECT, PROJECT)
                   if d["directiveId"] == directive["directiveId"]][0]
        self.assertEqual(settled["disposition"], "chosen")
        self.assertEqual(settled["decidedBy"], "alice")

    def test_an_owner_that_gets_a_scope_back_reports_where_it_is_running_now(self):
        """Reactivation moved status and nothing else, so a returning owner kept reporting the
        cwd and CXC session of its previous tenure - metadata that had not been true since the
        handover that took the scope away."""
        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT,
            endpoint=Endpoint(PARENT, HOST, cwd="/first", cxc_session="cxc-first"))
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="handed away", actor="test")
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=OTHER_PARENT,
            endpoint=Endpoint(PARENT, HOST, cwd="/second", cxc_session="cxc-second"),
            acknowledged=[], evidence="and taken back, from a new checkout", actor="test")
        owner = self.linkage.owner(linkage.PROJECT, PROJECT)
        self.assertEqual(owner["taskId"], PARENT)
        self.assertEqual(owner["cwd"], "/second")
        self.assertEqual(owner["_bindings"]["cxcSession"], "cxc-second")

    def test_a_reclaimed_issue_refuses_a_successor_rather_than_breaking_the_index(self):
        """supersedes excused the predecessor's child from the rival check even after that
        predecessor had died and the same task had reclaimed the issue directly. The successor
        was then inserted beside a live owner and met the one-live-owner index - a database
        error where the caller was owed a refusal it could act on."""
        self.supervise()
        first = self.register()
        rid = first["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "cancelled", actor="test")
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST))
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.registry.register,
            parent=self.parent(), child=Endpoint("01child-two", HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-successor", dispatch_turn_id="turn-successor",
            supersedes=rid, project_key=PROJECT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)

    def test_superseding_a_dead_relationship_leaves_a_reclaimed_issue_alone(self):
        """The decision supersede takes - whether this relationship is still releasing its
        scope - is now read under the same lock as the write. This pins the decision itself;
        the interleaving that made reading it early wrong is a window, not a state a test can
        reach without an injection point this suite does not have."""
        self.supervise()
        first = self.register()
        rid = first["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "cancelled", actor="test")
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST))
        self.registry.supersede(rid, new_relationship_id="rel-elsewhere")
        held = self.linkage.owner(linkage.ISSUE, ISSUE)
        self.assertIsNotNone(held, "supersession took a claim it had already released")
        self.assertEqual(held["taskId"], CHILD)

    def test_a_handover_driven_from_the_command_line_keeps_the_cxc_session(self):
        """The CLI built the replacement Endpoint without a session, so every handover made
        through it recorded none. That became destructive rather than merely incomplete once
        reactivation started writing the endpoint: a returning owner's stored session was
        overwritten with null by the very call meant to bring it up to date.
        """
        import argparse

        from codex_session_relay import cli

        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT,
            endpoint=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-first"))
        parsed = cli.build_parser().parse_args([
            "linkage-handover", "--role", "parent", "--scope", PROJECT,
            "--expect-task", PARENT, "--task", OTHER_PARENT, "--host", HOST,
            "--cwd", "/replacement", "--cxc-session", "cxc-replacement",
            "--evidence", "the project changed hands", "--actor", "test",
        ])
        cli.cmd_linkage_handover(argparse.Namespace(linkage=self.linkage), parsed)
        owner = self.linkage.owner(linkage.PROJECT, PROJECT)
        self.assertEqual(owner["taskId"], OTHER_PARENT)
        self.assertEqual(owner["cwd"], "/replacement")
        self.assertEqual(owner["_bindings"]["cxcSession"], "cxc-replacement")


class TheTwelfthRoundFoundTheseToo(LinkageTestCase):
    def test_a_former_owner_can_take_a_scope_back_from_a_new_host(self):
        """The host check ran ahead of the branch that exists to record a new host, so a
        cross-host handback was refused on its way to the update that would have made it
        true. Two hosts claiming a LIVE binding is still the contradiction it always was;
        an archived one is a claim being revalidated from scratch."""
        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT,
            endpoint=Endpoint(PARENT, HOST, cwd="/old-host"))
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
            endpoint=self.parent(OTHER_PARENT), acknowledged=[],
            evidence="handed away", actor="test")
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=OTHER_PARENT,
            endpoint=Endpoint(PARENT, "host-two", cwd="/new-host"),
            acknowledged=[], evidence="taken back, from a different machine", actor="test")
        owner = self.linkage.owner(linkage.PROJECT, PROJECT)
        self.assertEqual(owner["taskId"], PARENT)
        self.assertEqual(owner["hostId"], "host-two")
        self.assertEqual(owner["cwd"], "/new-host")
        # And the live case is unchanged: the binding is held on host-two right now, so a
        # third host asserting the same claim is still a conflict.
        self.assertRefused(
            RefusalReason.LINK_CONFLICT, self.linkage.bind_scope,
            role=linkage.PARENT, scope_key=PROJECT,
            endpoint=Endpoint(PARENT, "host-three"))

    def test_restoring_a_claim_keeps_the_status_the_caller_asked_for(self):
        """The insert branch has always honoured the status argument and the reactivate branch
        hard-coded active, so one call disagreed with itself about what the caller asked for
        and an equivalent paused claim came back live."""
        first = self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST),
            status="archived")
        self.assertEqual(first["status"], "archived")
        again = self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST),
            status="paused")
        self.assertEqual(again["bindingId"], first["bindingId"])
        self.assertEqual(again["status"], "paused")

    def test_the_attachment_guard_refuses_a_borrowed_predecessor_on_its_own(self):
        """The lower-level guard relaxed for any predecessor scoped to this project, without
        asking whether it was assigned to THIS issue, so a relationship for another issue
        borrowed one to get past the foreign-parent refusal. It runs before the registration
        check that now also refuses a cross-issue supersedes, so it has to hold by itself.
        """
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE, self.registry.register,
            parent=self.parent(OTHER_PARENT), child=Endpoint("01child-far", HOST),
            issue_key="REL-FAR", artifact_roots=[self.root],
            allowed_recipients=[OTHER_PARENT], dispatch_request_id="dispatch-far",
            dispatch_turn_id="turn-far", supersedes=rid, project_key=PROJECT)
        self.assertEqual(self.registry.get(rid)["status"], "active")


class TheThirteenthRoundFoundTheseToo(LinkageTestCase):
    def scoped(self):
        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        return relationship["relationshipId"]

    def test_an_archived_assignment_cannot_be_paused_back_to_life(self):
        """paused is live on both sides of this boundary, and set_status is deactivation only.

        The first fix made the lifecycle path restore the lower level for any live target,
        which removed the split state - a live relationship whose binding stayed archived -
        and opened a second one: a dead assignment regained its issue without restating the
        generation or the scope resume() exists to make a caller restate. A status flip does
        not become a reactivation by choosing a different live word. It is refused, and
        NEITHER side moves.
        """
        rid = self.scoped()
        self.registry.set_status(rid, "archived", actor="test")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE, self.registry.set_status,
            rid, "paused", actor="test")
        self.assertEqual(self.registry.get(rid)["status"], "archived")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))

    def test_resume_is_the_way_back_and_it_restores_the_issue(self):
        """The validated route does what the unvalidated one was doing: it restates the
        generation and the scope, and the binding and the edge come back with it."""
        rid = self.scoped()
        self.registry.set_status(rid, "archived", actor="test")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.registry.resume(
            rid, expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        owner = self.linkage.owner(linkage.ISSUE, ISSUE)
        self.assertIsNotNone(owner, "the restored assignment was left without its issue")
        self.assertEqual(owner["taskId"], CHILD)
        self.assertEqual(
            self.linkage.attachment(rid)["link"]["status"], "active")

    def test_a_resume_that_misstates_its_scope_restores_nothing(self):
        """Which is the point of routing it here: the restatement has to be right, and a
        refused resume leaves the lower level exactly as archived as it found it."""
        rid = self.scoped()
        self.registry.set_status(rid, "archived", actor="test")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_NOT_ACTIVE, self.registry.resume,
            rid, expect_generation=7, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))
        self.assertEqual(self.registry.get(rid)["status"], "archived")

    def test_two_parents_of_one_issue_are_competing_not_a_cycle(self):
        """One global visited set could not tell a back edge from a second parent, so a scope
        reached from two projects was reported as a corrupt cycle - and the walk still called
        itself resolved, which let a consumer read straight past it."""
        self.supervise()
        self.supervise(project=OTHER_PROJECT, parent=self.parent(OTHER_PARENT))
        edges = []
        with self.store.transaction() as db:
            for project in (PROJECT, OTHER_PROJECT):
                lid = link_id(linkage.EXECUTION, linkage.PROJECT, project,
                              linkage.ISSUE, ISSUE)
                edges.append(lid)
                db.execute(
                    "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
                    " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
                    " superseded_by, created_at, updated_at)"
                    " VALUES (?,'execution','project',?,?,'issue',?,?,'active',1,NULL,?,?)",
                    (lid, project, PARENT, ISSUE, CHILD, "2026-09-19T00:00:00Z",
                     "2026-09-19T00:00:00Z"))
        answer = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        self.assertEqual(answer["state"], "ambiguous")
        reported = [row.get("contention") for row in answer["contention"]]
        self.assertIn("competing_parents", reported)
        self.assertNotIn("scope_cycle", reported)
        competing = [row for row in answer["contention"]
                     if row.get("contention") == "competing_parents"][0]
        self.assertEqual(sorted(competing["candidates"]), sorted(edges))

    def test_a_dead_predecessor_does_not_relax_the_foreign_parent_check(self):
        """A relationship_scope row outlives the assignment that wrote it, so a cancelled
        predecessor's history was enough to get a successor past the foreign-parent guard and
        repoint the project's issue edge to a parent the project does not have."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "cancelled", actor="test")
        self.assertRefused(
            RefusalReason.FOREIGN_SCOPE, self.registry.register,
            parent=self.parent(OTHER_PARENT), child=Endpoint("01child-two", HOST,
                                                             cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[OTHER_PARENT],
            dispatch_request_id="dispatch-dead", dispatch_turn_id="turn-dead",
            supersedes=rid, project_key=PROJECT)
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)

    def test_a_contest_that_only_appears_after_the_pre_check_is_still_recorded(self):
        """The window between the pre-check transaction and the insert.

        A refusal decided there rolls the whole registration back and would take its conflict
        row with it, so it is re-recorded afterwards in its own transaction. That refusal rode
        on Registry instance state, which two registrations sharing one Registry could read
        from each other; it travels on the error now, which is the only thing belonging to one
        call. The racer is injected exactly in the window rather than raced for, so the
        interleaving is decided rather than hoped for and nothing here spends real time.
        """
        from unittest import mock

        self.supervise()
        original = type(self.registry)._register_in_transaction

        def racing(registry, rid, parent, child, issue_key, *rest, **options):
            self.linkage.bind_scope(
                role=linkage.CHILD, scope_key=issue_key,
                endpoint=Endpoint("01child-racer", HOST))
            return original(registry, rid, parent, child, issue_key, *rest, **options)

        with mock.patch.object(
                type(self.registry), "_register_in_transaction", racing):
            self.assertRefused(
                RefusalReason.DUPLICATE_SCOPE_OWNER, self.registry.register,
                parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
                issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
                dispatch_request_id="dispatch-raced", dispatch_turn_id="turn-raced",
                project_key=PROJECT)
        self.assertFalse(hasattr(self.registry, "_raced"))
        self.assertTrue(
            self.linkage.conflicts(linkage.ISSUE, ISSUE),
            "the contest decided after the pre-check was lost with the rollback")
        # The registration itself rolled back whole, which is what makes the re-record
        # necessary in the first place.
        self.assertEqual(
            self.store.all("SELECT relationship_id FROM relationships WHERE issue_key = ?",
                           (ISSUE,)), [])

    def test_a_store_that_already_breaks_a_guard_index_still_opens(self):
        """The partial unique indexes were created by the schema script, so a store already
        holding duplicate live rows failed to OPEN - and the ambiguity-aware readers written
        for exactly that store could never run. An operator got an IntegrityError where an
        answer was owed."""
        import os

        path = os.path.join(self.tmp, "legacy-store.sqlite")
        legacy = Store(path)
        self.assertEqual(legacy.unenforced_indexes, [])
        legacy.db.execute("DROP INDEX scope_bindings_one_live_owner")
        with legacy.transaction() as db:
            for task in ("01owner-one", "01owner-two"):
                db.execute(
                    "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
                    " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
                    " superseded_by, handover_note, created_at, updated_at)"
                    " VALUES (?,?,?,?,?,?,NULL,NULL,'active',1,NULL,NULL,NULL,?,?)",
                    (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, task),
                     linkage.PARENT, linkage.PROJECT, PROJECT, task, HOST,
                     "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
        reopened = Store(path)
        self.assertEqual([entry["index"] for entry in reopened.unenforced_indexes],
                         ["scope_bindings_one_live_owner"])
        # And the reader answers about it rather than the store refusing to exist.
        answer = Linkage(reopened, self.clock).up(task_id="01owner-one")
        self.assertIs(answer["readable"], True)

    def test_two_live_owners_are_reported_rather_than_resolved(self):
        """owner() answers with a row, so it ordered the duplicates and returned one - and the
        walk called that resolved. A deterministic pick is still a guess, and one that looks
        settled is worse than an error, because nothing downstream can tell. The higher
        revision is written second here, so the old code would have named 01owner-two.
        """
        legacy = self.duplicated_store()
        answer = legacy.down(linkage.PROJECT, PROJECT)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertIs(answer["readable"], True)
        competing = [row for row in answer["contention"]
                     if row.get("contention") == "competing_owners"]
        self.assertEqual(len(competing), 1)
        self.assertEqual(competing[0]["candidates"], ["01owner-one", "01owner-two"])
        self.assertIsNone(answer["levels"][0]["owner"])
        # Two owners is not nobody. The gap would have said the project has no parent.
        self.assertEqual(answer["gaps"], [])

    def test_the_upward_walk_reports_the_same_contest(self):
        legacy = self.duplicated_store()
        answer = legacy.up(task_id="01owner-two")
        self.assertEqual(answer["state"], "ambiguous")
        self.assertIsNone(answer["levels"][0]["owner"])
        self.assertIn("competing_owners",
                      [row.get("contention") for row in answer["contention"]])
        # no_supervisor is true and unrelated: this bare store has no initiative edge. What
        # must NOT appear is a gap saying the project has no parent, when it has two.
        self.assertNotIn("project_without_parent",
                         [gap["gap"] for gap in answer["gaps"]])


class TheFourteenthRoundFoundTheseToo(LinkageTestCase):
    def test_the_handover_staging_state_is_not_reported_as_consistent(self):
        """Both walks checked only an edge's LOWER endpoint.

        The sequence a stranding refusal prescribes moves each assignment to the incoming
        parent BEFORE the scope moves, so the project-to-issue edge names the new parent while
        the project binding still names the old one. Checking one end reported that
        transitional tree as internally consistent, in both directions at once.
        """
        self.supervise()
        first = self.register()
        rid = first["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.register(
            parent=self.parent(OTHER_PARENT),
            child=Endpoint("01child-two", HOST, cwd=self.root), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[OTHER_PARENT],
            dispatch_request_id="dispatch-moved", dispatch_turn_id="turn-moved",
            supersedes=rid, project_key=PROJECT)
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)
        downward = self.linkage.down(linkage.INITIATIVE, INITIATIVE)
        self.assertIn("owner_drift",
                      [row.get("contention") for row in downward["contention"]])
        upward = self.linkage.up(issue_key=ISSUE)
        self.assertIn("owner_drift",
                      [row.get("contention") for row in upward["contention"]])

    def test_the_upward_walk_needs_exactly_one_starting_point(self):
        """Independently optional selectors accepted none, which answers nothing, and several
        at once, which _starting_scope resolved by a precedence the caller never saw: naming a
        relationship AND an issue returned the relationship's hierarchy with no sign the issue
        was ignored."""
        from codex_session_relay import cli

        parser = cli.build_parser()
        with self.assertRaises(SystemExit):
            parser.parse_args(["linkage-up"])
        with self.assertRaises(SystemExit):
            parser.parse_args(["linkage-up", "--relationship", "rel-a", "--issue", ISSUE])

    def test_a_scope_without_a_task_is_refused_rather_than_ignored(self):
        """--scope narrows only the task path, so beside --issue it read as a second filter
        that was never applied."""
        import argparse

        from codex_session_relay import cli

        parsed = cli.build_parser().parse_args(
            ["linkage-up", "--issue", ISSUE, "--scope", PROJECT])
        with self.assertRaises(cli.PayloadExit) as caught:
            cli.cmd_linkage_up(argparse.Namespace(linkage=self.linkage), parsed)
        self.assertEqual(caught.exception.payload["reason"], "bad_invocation")

    def test_a_reader_is_told_the_database_is_not_enforcing_ownership(self):
        """The unenforced index was recorded on the Store and shown nowhere, so the promise to
        name it was unkept. An ambiguous answer and a missing index are the same fact from two
        sides, and a caller deciding whether to act on a contested owner needs both."""
        import argparse

        from codex_session_relay import cli

        legacy = self.duplicated_store()
        services = argparse.Namespace(linkage=legacy, store=legacy.store)
        parsed = cli.build_parser().parse_args(
            ["linkage-down", "--scope-kind", "project", "--scope", PROJECT])
        answer = cli.cmd_linkage_down(services, parsed)
        self.assertEqual(answer["state"], "ambiguous")
        self.assertEqual([entry["index"] for entry in answer["unenforcedIndexes"]],
                         ["scope_bindings_one_live_owner"])
        # A healthy store says nothing, rather than carrying an empty key everywhere.
        healthy = argparse.Namespace(linkage=self.linkage, store=self.store)
        self.assertNotIn("unenforcedIndexes", cli.cmd_linkage_down(healthy, parsed))

    def handed_to(self, incoming, *, supersedes, dispatch):
        """Move the assignment to the incoming parent, then move the scope after it.

        The sequence a stranding refusal prescribes, run for real rather than described.
        """
        moved = self.registry.register(
            parent=self.parent(incoming), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id=dispatch, dispatch_turn_id=dispatch + "-turn",
            supersedes=supersedes, project_key=PROJECT)
        holder = self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"]
        self.linkage.handover(
            role=linkage.PARENT, scope_key=PROJECT, expect_task_id=holder,
            endpoint=self.parent(incoming),
            acknowledged=self.linkage.outstanding(PROJECT),
            evidence="the assignment moved first", actor="test")
        return moved

    def test_a_project_can_be_handed_back_to_the_parent_it_came_from(self):
        """A -> B -> A was impossible.

        relationship_id is sha256(parentTaskId|childTaskId|issueKey), so handing back to the
        earlier parent with the same child derives the ORIGINAL id. That row is archived, so
        the existing-row path handed it to attach_in and got relationship_not_active, the live
        assignment under B went on blocking the handover, and the only escape was to invent a
        child or parent task that does not exist. The same triple is the same relationship by
        the contract's own identity rule, so the return is a second TENURE of it: a new
        generation, with the lineage recorded on both rows.
        """
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)

        away = self.handed_to(OTHER_PARENT, supersedes=original, dispatch="dispatch-away")
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], OTHER_PARENT)

        back = self.handed_to(PARENT, supersedes=away["relationshipId"],
                              dispatch="dispatch-back")
        self.assertEqual(back["relationshipId"], original,
                         "the returning tenure is the same relationship, by identity")
        self.assertEqual(back["status"], "active")
        self.assertEqual(back["executionGeneration"], 2)
        self.assertEqual(len(back["generations"]), 2)
        self.assertEqual(self.linkage.owner(linkage.PROJECT, PROJECT)["taskId"], PARENT)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        self.assertEqual(self.linkage.attachment(original)["projectKey"], PROJECT)
        # Both directions of the lineage, so neither tenure pretends the other did not happen.
        middle = self.registry.get(away["relationshipId"])
        self.assertEqual(middle["status"], "archived")
        self.assertEqual(
            self.store.one("SELECT superseded_by, supersedes FROM relationships"
                           "  WHERE relationship_id = ?",
                           (away["relationshipId"],))["superseded_by"], original)
        self.assertEqual(
            self.store.one("SELECT supersedes FROM relationships WHERE relationship_id = ?",
                           (original,))["supersedes"], away["relationshipId"])

    def test_a_retired_identity_is_not_returned_as_a_registration(self):
        """The silent half. Unscoped, the archived row came straight back as though a
        registration had happened, over an assignment that owned nothing. It now says which
        route applies instead of letting the matching id speak for itself."""
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        self.registry.set_status(original, "archived", actor="test")
        refusal = self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.registry.register,
            parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-silent", dispatch_turn_id="turn-silent")
        self.assertIn("relationship-resume", refusal.detail)
        self.assertEqual(self.registry.get(original)["status"], "archived")

    def test_a_returning_tenure_needs_a_predecessor_that_still_holds_the_issue(self):
        """Naming a predecessor that has already let the issue go is not a handback, and the
        returning row must not come back on the strength of a retired one."""
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        away = self.handed_to(OTHER_PARENT, supersedes=original, dispatch="dispatch-away")
        self.registry.set_status(away["relationshipId"], "cancelled", actor="test")
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.registry.register,
            parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-late", dispatch_turn_id="turn-late",
            supersedes=away["relationshipId"], project_key=PROJECT)
        self.assertEqual(self.registry.get(original)["status"], "archived")

    def test_a_handback_still_works_after_the_returning_parent_moved_host(self):
        """The host comparison belonged to a LIVE replay only.

        Requiring the old host on a dead row made the handback either unreachable or a lie:
        passing the original host to get past the check would have stored routing metadata
        that had not been true since the move.
        """
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        away = self.handed_to(OTHER_PARENT, supersedes=original, dispatch="dispatch-away")
        moved = self.registry.register(
            parent=Endpoint(PARENT, "host-two", cwd="/moved", cxc_session="cxc-moved"),
            child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-moved", dispatch_turn_id="turn-moved",
            supersedes=away["relationshipId"], project_key=PROJECT)
        self.assertEqual(moved["relationshipId"], original)
        self.assertEqual(moved["executionGeneration"], 2)
        self.assertEqual(moved["parent"]["hostId"], "host-two")
        self.assertEqual(moved["parent"]["cwd"], "/moved")

    def test_a_live_replay_from_another_host_is_still_a_conflict(self):
        """The half that stays: one relationship cannot be running in two places at once."""
        self.supervise()
        self.register()
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.registry.register,
            parent=Endpoint(PARENT, "host-two", cwd="/parent"),
            child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-elsewhere", dispatch_turn_id="turn-elsewhere")

    def test_a_refused_returning_tenure_still_leaves_its_contest(self):
        """Its transaction rolls back and takes the conflict row with it, and the call sits
        outside the handler that re-records one. A refused handback lost its evidence, which
        is the one thing the write protocol says a refusal must not do."""
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        # The intermediate tenure uses a DIFFERENT child, which frees the original one.
        away = self.registry.register(
            parent=self.parent(OTHER_PARENT),
            child=Endpoint("01child-two", HOST, cwd=self.root), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-away", dispatch_turn_id="turn-away",
            supersedes=original, project_key=PROJECT)
        # While the project was elsewhere, the original child took another issue.
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key="REL-ELSEWHERE", endpoint=Endpoint(CHILD, HOST))
        self.assertRefused(
            RefusalReason.ROLE_ALREADY_BOUND, self.registry.register,
            parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-back", dispatch_turn_id="turn-back",
            supersedes=away["relationshipId"], project_key=PROJECT)
        self.assertTrue(
            self.linkage.conflicts(linkage.ISSUE, ISSUE),
            "the refused handback left no record that it was contested")
        # And it rolled back whole: the tenure it tried to take over is untouched.
        self.assertEqual(
            self.registry.get(away["relationshipId"])["status"], "active")

    def test_an_assignment_read_reports_a_contested_project_rather_than_one_owner(self):
        """for_issue is what a coordinator reads before acting on an issue, so naming
        whichever duplicate sorted first would hand it a guessed parent wearing the same shape
        as a known one."""
        from codex_session_relay.assignment import AssignmentView

        self.supervise()
        relationship = self.register()
        self.linkage.attach_issue(relationship["relationshipId"], PROJECT)
        self.store.db.execute("DROP INDEX scope_bindings_one_live_owner")
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
                " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
                " superseded_by, handover_note, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,NULL,NULL,'active',2,NULL,NULL,NULL,?,?)",
                (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT),
                 linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT, HOST,
                 "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
        answer = AssignmentView(self.store, self.registry, self.clock).for_issue(ISSUE)
        self.assertEqual(answer["scopeState"], "ambiguous")
        self.assertIsNone(answer["projectParentTaskId"])
        self.assertIsNone(answer["parentOwnsProject"])
        self.assertEqual(answer["projectParentCandidates"], sorted([PARENT, OTHER_PARENT]))


class TheFifteenthRoundFoundTheseToo(LinkageTestCase):
    def contest_the_project(self):
        """Give PROJECT a second live parent, the way a store with no guard index can."""
        self.store.db.execute("DROP INDEX scope_bindings_one_live_owner")
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
                " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
                " superseded_by, handover_note, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,NULL,NULL,'active',2,NULL,NULL,NULL,?,?)",
                (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT),
                 linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT, HOST,
                 "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))

    def test_a_contested_project_refuses_an_attachment_rather_than_picking_an_owner(self):
        """The readers report two live owners and the writers were still taking whichever row
        came back first. That guess is worse on the write side: an attachment turns it into a
        durable parent-to-issue edge for a project that has no sole parent."""
        self.supervise()
        relationship = self.register()
        self.contest_the_project()
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER,
            self.linkage.attach_issue, relationship["relationshipId"], PROJECT)
        self.assertIsNone(self.linkage.attachment(relationship["relationshipId"]))
        # And it is retained, like every other linkage refusal.
        self.assertTrue(self.linkage.conflicts(linkage.PROJECT, PROJECT))

    def test_a_contested_project_refuses_a_peer_link_too(self):
        """Same single-row assumption, same answer: a peer link joins two project PARENTS, and
        a project with two of them has none to join."""
        self.supervise()
        self.supervise(project=OTHER_PROJECT, parent=self.parent(OTHER_PARENT))
        self.contest_the_project()
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.linkage.register_peer,
            left_project=PROJECT, left_parent=self.parent(),
            right_project=OTHER_PROJECT, right_parent=self.parent(OTHER_PARENT))

    def test_a_refused_resume_still_leaves_its_contest(self):
        """The refusal is decided inside resume's write transaction, so the rollback that
        makes a refused resume leave both relationships untouched was also taking the conflict
        row with it. Every other linkage write path keeps the contest it lost."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "archived", actor="test")
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint("01child-two", HOST))
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertTrue(
            self.linkage.conflicts(linkage.ISSUE, ISSUE),
            "the refused resume left no record that it was contested")
        # And it still rolled back whole.
        self.assertEqual(self.registry.get(rid)["status"], "archived")


class TheSixteenthRoundFoundTheseToo(LinkageTestCase):
    def test_a_returning_tenure_restores_the_project_it_retained(self):
        """The predecessor can be UNSCOPED - every relationship from before the three-level
        linkage is - while the row coming back still holds its own relationship_scope record.
        Inheriting None there left the tenure active with its binding and edge archived:
        attachment() reported the retained project while the upward walk reported
        issue_without_child, which is one store answering two ways."""
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        self.registry.set_status(original, "cancelled", actor="test")
        interim = self.registry.register(
            parent=self.parent(OTHER_PARENT),
            child=Endpoint("01child-two", HOST, cwd=self.root), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[OTHER_PARENT],
            dispatch_request_id="dispatch-interim", dispatch_turn_id="turn-interim")
        self.assertIsNone(self.linkage.attachment(interim["relationshipId"]))
        back = self.registry.register(
            parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-back", dispatch_turn_id="turn-back",
            supersedes=interim["relationshipId"])
        self.assertEqual(back["relationshipId"], original)
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["taskId"], CHILD)
        self.assertEqual(self.linkage.attachment(original)["projectKey"], PROJECT)
        upward = self.linkage.up(issue_key=ISSUE)
        self.assertNotIn("issue_without_child", [gap["gap"] for gap in upward["gaps"]])

    def test_a_refused_returning_tenure_commits_its_contest_and_applies_nothing(self):
        """Recording after the rollback leaves a window where a crash loses both the change
        and the evidence. The attachment is decided before any mutation, so the transaction
        that refuses commits the contest itself."""
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        away = self.registry.register(
            parent=self.parent(OTHER_PARENT),
            child=Endpoint("01child-two", HOST, cwd=self.root), issue_key=ISSUE,
            artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-away", dispatch_turn_id="turn-away",
            supersedes=original, project_key=PROJECT)
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key="REL-ELSEWHERE", endpoint=Endpoint(CHILD, HOST))
        self.assertRefused(
            RefusalReason.ROLE_ALREADY_BOUND, self.registry.register,
            parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-back", dispatch_turn_id="turn-back",
            supersedes=away["relationshipId"], project_key=PROJECT)
        self.assertTrue(self.linkage.conflicts(linkage.ISSUE, ISSUE))
        # Nothing was half-applied: the tenure it tried to take over is untouched and the
        # returning identity is still dead.
        self.assertEqual(self.registry.get(away["relationshipId"])["status"], "active")
        self.assertEqual(self.registry.get(original)["status"], "archived")
        self.assertEqual(self.registry.get(original)["executionGeneration"], 1)

    def test_a_returning_tenure_refuses_a_dispatch_id_from_an_earlier_tenure(self):
        """The unique index would have refused it as a raw database error out of a public
        registration call, and generations could no longer say which dispatch opened which
        tenure."""
        self.supervise()
        first = self.register()
        original = first["relationshipId"]
        self.linkage.attach_issue(original, PROJECT)
        away = self.registry.register(
            parent=self.parent(OTHER_PARENT), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-away", dispatch_turn_id="turn-away",
            supersedes=original, project_key=PROJECT)
        self.assertRefused(
            RefusalReason.RELATIONSHIP_CONFLICT, self.registry.register,
            parent=self.parent(), child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-1", dispatch_turn_id="turn-replayed",
            supersedes=away["relationshipId"], project_key=PROJECT)
        self.assertEqual(self.registry.get(original)["status"], "archived")

    def test_an_attachment_refuses_a_blank_child_endpoint(self):
        """Only this path built an Endpoint out of stored columns instead of caller arguments,
        so it skipped the check every other entry point makes - and a blank host produced a
        live child binding with no routable endpoint at all."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        with self.store.transaction() as db:
            db.execute(
                "UPDATE relationships SET child_host_id = '' WHERE relationship_id = ?",
                (rid,))
        with self.assertRaises(Exception) as caught:
            self.linkage.attach_issue(rid, PROJECT)
        self.assertIn("host id", str(caught.exception))
        self.assertIsNone(self.linkage.owner(linkage.ISSUE, ISSUE))

    def test_a_contested_scope_refuses_a_rebinding_of_its_own_live_owner(self):
        """binding_plan read its own row as live and answered present, which said nothing
        about whether anybody ELSE was. Rebinding and handover then reported a settled owner
        for a scope that has two."""
        self.linkage.bind_scope(
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())
        self.store.db.execute("DROP INDEX scope_bindings_one_live_owner")
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
                " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
                " superseded_by, handover_note, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,NULL,NULL,'active',2,NULL,NULL,NULL,?,?)",
                (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT),
                 linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT, HOST,
                 "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
        self.assertRefused(
            RefusalReason.DUPLICATE_SCOPE_OWNER, self.linkage.bind_scope,
            role=linkage.PARENT, scope_key=PROJECT, endpoint=self.parent())

    def test_a_child_that_reclaimed_its_issue_elsewhere_blocks_the_resume(self):
        """The rival check excludes the same child, which is right - it is not its own rival -
        but excluding it also skipped asking WHERE it is. A child that took this issue back on
        another host leaves the relationship's recorded endpoint pointing somewhere it no
        longer runs, and resuming would restore an assignment nothing can route to."""
        self.supervise()
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.linkage.attach_issue(rid, PROJECT)
        self.registry.set_status(rid, "archived", actor="test")
        self.linkage.bind_scope(
            role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, "host-two"))
        self.assertRefused(
            RefusalReason.LINK_CONFLICT, self.registry.resume, rid,
            expect_generation=1, expect_artifact_roots=[self.root],
            expect_allowed_recipients=[PARENT], actor="test")
        self.assertEqual(self.registry.get(rid)["status"], "archived")
        self.assertEqual(self.linkage.owner(linkage.ISSUE, ISSUE)["hostId"], "host-two")
        # Retained like every other linkage refusal.
        self.assertTrue(self.linkage.conflicts(linkage.ISSUE, ISSUE))

if __name__ == "__main__":
    unittest.main()
