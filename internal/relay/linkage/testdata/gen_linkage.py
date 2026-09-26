"""Record the Python linkage's caller-visible answers for internal/relay/linkage parity tests.

Run from the repository root with HOME/XDG_*/CODEX_HOME/TMPDIR in a scratch directory:
  uv run --no-sync python internal/relay/linkage/testdata/gen_linkage.py \
      > internal/relay/linkage/testdata/python_linkage.json
Every scenario uses FakeClock(1_700_000_000) and literal roots, so no value depends on the run.
Each scenario is a list of steps: {"ok": <json>} or {"refused": {"reason", "detail"}}. A step
reading a table is {"ok": [row, ...]} with each row as a dict.
"""
import json
import os
import sys
import tempfile

from codex_session_relay import linkage
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RelayError
from codex_session_relay.linkage import Linkage, binding_id, link_id
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry, record_settings
from codex_session_relay.store import Store

sys.path.insert(0, "packages/codex-session-relay")
from tests.support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, task_settings  # noqa: E402

ROOT = "/work"
INITIATIVE = "INIT-1"
PROJECT = "PROJ-1"
OTHER_PROJECT = "PROJ-2"
OTHER_INITIATIVE = "INIT-2"
SUPERVISOR_TASK = "01supervisor-task"
OTHER_SUPERVISOR = "01supervisor-two"
OTHER_PARENT = "01parent-two"
SCENARIOS = {}


def scenario(fn):
    SCENARIOS[fn.__name__] = fn
    return fn


class World:
    def __init__(self):
        directory = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
        self.store = Store(os.path.join(directory, "relay.sqlite3"))
        self.clock = FakeClock()
        self.registry = Registry(self.store, self.clock)
        self.linkage = Linkage(self.store, self.clock)
        self.out = []

    def step(self, call):
        try:
            value = call()
        except RelayError as error:
            self.out.append({"refused": {"reason": error.reason.value if error.reason else None,
                                         "detail": error.detail}})
            return None
        self.out.append({"ok": value})
        return value

    def rows(self, sql, params=()):
        self.out.append({"ok": [dict(row) for row in self.store.all(sql, params)]})

    def supervisor(self, task=SUPERVISOR_TASK):
        return Endpoint(task, HOST, cwd="/supervisor", cxc_session="cxc-supervisor")

    def parent(self, task=PARENT):
        return Endpoint(task, HOST, cwd="/parent", cxc_session="cxc-parent")

    def supervise(self, *, initiative=INITIATIVE, project=PROJECT, supervisor=None, parent=None,
                  kind=linkage.EXECUTION):
        return self.linkage.register_supervision(
            initiative_key=initiative, project_key=project,
            supervisor=supervisor or self.supervisor(), parent=parent or self.parent(),
            link_kind=kind)

    def register(self, **kw):
        """tests.support.RelayTestCase.register: the fixture, with both settings recorded."""
        relationship = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
            child=Endpoint(CHILD, HOST, cwd=ROOT, cxc_session="cxc-child"),
            issue_key=kw.pop("issue_key", ISSUE), artifact_roots=[ROOT],
            allowed_recipients=[PARENT], dispatch_request_id=kw.pop("dispatch_request_id", "dispatch-1"),
            dispatch_turn_id=DISPATCH_TURN, **kw)
        record_settings(self.store, self.clock, PARENT, task_settings("/parent"), source="creation_result")
        record_settings(self.store, self.clock, CHILD, task_settings(ROOT), source="creation_result")
        return relationship["relationshipId"]

    def conflicts(self):
        self.rows("SELECT * FROM linkage_conflicts ORDER BY id")


def scoped(w):
    w.supervise()
    rid = w.register()
    w.linkage.attach_issue(rid, PROJECT)
    return rid


# ------------------------------------------------------------------ LNK-1..LNK-10

@scenario
def lnk1_binding_identity(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.step(lambda: binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, PARENT))


@scenario
def lnk2_replays_converge(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.rows("SELECT binding_id FROM scope_bindings")
    w.step(lambda: w.supervise(project=OTHER_PROJECT, parent=w.parent(OTHER_PARENT)))
    w.step(lambda: w.supervise(project=OTHER_PROJECT, parent=w.parent(OTHER_PARENT)))
    w.rows("SELECT link_id FROM scope_links")


@scenario
def lnk3_link_id_stable_across_handover(w):
    w.step(lambda: link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE, linkage.PROJECT, PROJECT))
    w.step(lambda: w.supervise())
    w.step(lambda: w.linkage.handover(
        role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
        endpoint=w.parent(OTHER_PARENT), acknowledged=[],
        evidence="the outgoing parent handed over its project", actor="test"))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                                          linkage.PROJECT, PROJECT)))


@scenario
def lnk4_one_owner_per_scope(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    for _ in range(3):
        w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                            endpoint=w.parent(OTHER_PARENT)))
    w.conflicts()
    w.rows("SELECT task_id FROM scope_bindings WHERE scope_key = ?", (PROJECT,))


@scenario
def lnk4_archived_is_revalidated(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.store.db.execute("UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", (PROJECT,))
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                        endpoint=w.parent(OTHER_PARENT)))
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))


@scenario
def lnk5_parent_as_supervisor(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.step(lambda: w.linkage.bind_scope(role=linkage.SUPERVISOR, scope_key=INITIATIVE,
                                        endpoint=w.supervisor(PARENT)))


@scenario
def lnk5_child_as_supervisor(w):
    scoped(w)
    w.step(lambda: w.linkage.bind_scope(role=linkage.SUPERVISOR, scope_key=OTHER_INITIATIVE,
                                        endpoint=w.supervisor(CHILD)))


@scenario
def lnk5_parent_as_another_supervisor(w):
    w.supervise()
    w.step(lambda: w.supervise(initiative=OTHER_INITIATIVE, project=OTHER_PROJECT,
                               supervisor=w.supervisor(PARENT), parent=w.parent(OTHER_PARENT)))


@scenario
def lnk5_handover_to_supervisor(w):
    w.supervise()
    w.step(lambda: w.linkage.handover(
        role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
        endpoint=w.supervisor(SUPERVISOR_TASK), acknowledged=[],
        evidence="the supervisor tried to take the project", actor="test"))
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))


@scenario
def lnk5_cross_role_resume(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    w.step(lambda: w.linkage.bind_scope(role=linkage.SUPERVISOR, scope_key="INIT-LATER",
                                        endpoint=Endpoint(CHILD, HOST)))
    w.step(lambda: w.registry.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT],
                                     expect_allowed_recipients=[PARENT], actor="test")
           and None)
    w.conflicts()


@scenario
def lnk5_parent_second_project(w):
    w.supervise()
    w.step(lambda: w.supervise(project=OTHER_PROJECT, parent=w.parent()))
    w.step(lambda: w.linkage.owner(linkage.PROJECT, OTHER_PROJECT))


@scenario
def lnk5_child_second_issue(w):
    w.supervise()
    first = w.register()
    w.linkage.attach_issue(first, PROJECT)
    second = w.registry.register(
        parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
        issue_key="REL-2", artifact_roots=[ROOT], allowed_recipients=[PARENT],
        dispatch_request_id="dispatch-2", dispatch_turn_id="turn-2")["relationshipId"]
    w.step(lambda: w.linkage.attach_issue(second, PROJECT))


@scenario
def lnk5_child_in_another_scope_resume(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    w.step(lambda: w.linkage.bind_scope(role=linkage.CHILD, scope_key="REL-ELSEWHERE",
                                        endpoint=Endpoint(CHILD, HOST)))
    w.step(lambda: w.registry.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT],
                                     expect_allowed_recipients=[PARENT], actor="test")
           and None)
    w.step(lambda: w.registry.get(rid)["status"])
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk6_self_supervision(w):
    w.step(lambda: w.supervise(supervisor=w.supervisor(PARENT), parent=w.parent()))
    w.conflicts()


@scenario
def lnk6_child_supervises_its_project(w):
    scoped(w)
    w.step(lambda: w.supervise(initiative=OTHER_INITIATIVE, supervisor=w.supervisor(CHILD),
                               kind=linkage.REFERENCE))


@scenario
def lnk6_issue_parent_is_its_child(w):
    w.supervise()
    rid = w.registry.register(
        parent=Endpoint("01same-task", HOST), child=Endpoint("01same-task", HOST),
        issue_key="REL-SELF", artifact_roots=[ROOT], allowed_recipients=[PARENT],
        dispatch_request_id="dispatch-self", dispatch_turn_id="turn-self")["relationshipId"]
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))


@scenario
def lnk6_peer_kind(w):
    w.step(lambda: w.supervise(kind=linkage.PEER))


@scenario
def lnk7_foreign_parent(w):
    w.supervise()
    w.supervise(initiative=OTHER_INITIATIVE, project=OTHER_PROJECT,
                supervisor=w.supervisor(OTHER_SUPERVISOR), parent=w.parent(OTHER_PARENT))
    rid = w.register()
    w.step(lambda: w.linkage.attach_issue(rid, OTHER_PROJECT))
    w.conflicts()


@scenario
def lnk7_unregistered_project(w):
    rid = w.register()
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))


@scenario
def lnk7_scoped_elsewhere(w):
    rid = scoped(w)
    w.supervise(initiative=OTHER_INITIATIVE, project=OTHER_PROJECT,
                supervisor=w.supervisor(OTHER_SUPERVISOR), parent=w.parent(OTHER_PARENT))
    w.step(lambda: w.linkage.attach_issue(rid, OTHER_PROJECT))


@scenario
def lnk7_two_projects(w):
    w.supervise()
    w.supervise(initiative="INIT-2", project=OTHER_PROJECT,
                supervisor=w.supervisor(OTHER_SUPERVISOR), parent=w.parent(OTHER_PARENT))
    first = w.register()
    w.linkage.attach_issue(first, PROJECT)
    second = w.registry.register(
        parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint("01child-two", HOST),
        issue_key=ISSUE, artifact_roots=[ROOT], allowed_recipients=[PARENT],
        dispatch_request_id="dispatch-rival", dispatch_turn_id="turn-rival",
        supersedes=first)["relationshipId"]
    w.step(lambda: w.linkage.attach_issue(second, OTHER_PROJECT))
    w.rows("SELECT link_id FROM scope_links WHERE lower_kind = ? AND lower_key = ?"
           "  AND status IN ('active','paused')", (linkage.ISSUE, ISSUE))


@scenario
def lnk7_inactive(w):
    w.supervise()
    for status in ("archived", "cancelled"):
        rid = w.registry.register(
            parent=w.parent(), child=Endpoint("01child-" + status, HOST, cwd=ROOT),
            issue_key="REL-" + status, artifact_roots=[ROOT], allowed_recipients=[PARENT],
            dispatch_request_id="dispatch-" + status, dispatch_turn_id="turn-" + status)["relationshipId"]
        w.registry.set_status(rid, status, actor="test")
        w.step(lambda: w.linkage.attach_issue(rid, PROJECT))


@scenario
def lnk8_attach_writes_and_converges(w):
    w.supervise()
    rid = w.register()
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.rows("SELECT * FROM relationship_scope")
    w.rows("SELECT link_id FROM scope_links WHERE lower_kind = ?", (linkage.ISSUE,))


@scenario
def lnk8_completes_partial(w):
    w.supervise()
    rid = w.register()
    w.store.db.execute("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"
                       " VALUES (?,?,?)", (rid, PROJECT, w.clock.iso()))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT,
                                          linkage.ISSUE, ISSUE)))


@scenario
def lnk8_paused_stays_paused(w):
    w.supervise()
    rid = w.register()
    w.registry.set_status(rid, "paused", actor="test")
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk9_second_initiative_references(w):
    w.supervise()
    w.step(lambda: w.supervise(initiative=OTHER_INITIATIVE, supervisor=w.supervisor(OTHER_SUPERVISOR),
                               kind=linkage.REFERENCE))
    w.rows("SELECT task_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ?"
           "  AND role = ? AND status IN ('active','paused')", (linkage.PROJECT, PROJECT, linkage.PARENT))


@scenario
def lnk9_second_execution_edge(w):
    w.supervise()
    w.step(lambda: w.supervise(initiative=OTHER_INITIATIVE, supervisor=w.supervisor(OTHER_SUPERVISOR)))
    w.rows("SELECT link_id FROM scope_links WHERE lower_key = ? AND link_kind = 'execution'"
           "  AND status IN ('active','paused')", (PROJECT,))


@scenario
def lnk9_reference_naming_another_parent(w):
    w.supervise()
    w.step(lambda: w.supervise(initiative=OTHER_INITIATIVE, supervisor=w.supervisor(OTHER_SUPERVISOR),
                               parent=w.parent(OTHER_PARENT), kind=linkage.REFERENCE))
    w.conflicts()


@scenario
def lnk9_supervisor_cannot_reference(w):
    w.supervise()
    w.step(lambda: w.supervise(kind=linkage.REFERENCE))
    w.rows("SELECT link_id FROM scope_links WHERE lower_key = ? AND status IN ('active','paused')", (PROJECT,))
    w.step(lambda: w.supervise(initiative=OTHER_INITIATIVE, supervisor=w.supervisor(OTHER_SUPERVISOR),
                               kind=linkage.REFERENCE))


@scenario
def lnk9_reference_not_first(w):
    w.step(lambda: w.supervise(initiative="INIT-3", project="PROJ-3",
                               supervisor=w.supervisor("01supervisor-three"),
                               parent=w.parent("01parent-three"), kind=linkage.REFERENCE))
    w.step(lambda: w.linkage.owner(linkage.PROJECT, "PROJ-3"))


def directives_world(w):
    execution = w.supervise()
    reference = w.supervise(initiative=OTHER_INITIATIVE, supervisor=w.supervisor(OTHER_SUPERVISOR),
                            kind=linkage.REFERENCE)
    return execution["linkId"], reference["linkId"]


def record(w, *, origin, task, link, digest, kind=linkage.PROJECT, key=PROJECT):
    return w.step(lambda: w.linkage.record_directive(
        scope_kind=kind, scope_key=key, from_task_id=task, from_scope_key=origin,
        link_id_value=link, digest=digest))


@scenario
def lnk10_directive_authority(w):
    execution, reference = directives_world(w)
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-one")
    record(w, origin=OTHER_INITIATIVE, task=OTHER_SUPERVISOR, link=reference, digest="d-two")
    record(w, origin=INITIATIVE, task=OTHER_SUPERVISOR, link=execution, digest="d-three")
    w.supervise(initiative="INIT-3", project="PROJ-3", supervisor=w.supervisor("01supervisor-three"),
                parent=w.parent("01parent-three"))
    other = link_id(linkage.EXECUTION, linkage.INITIATIVE, "INIT-3", linkage.PROJECT, "PROJ-3")
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=other, digest="d-four")
    record(w, origin=INITIATIVE, task="01nobody", link=execution, digest="d-one")
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-kind",
           kind=linkage.ISSUE, key=PROJECT)
    w.conflicts()


@scenario
def lnk10_upper_endpoint(w):
    w.supervise()
    w.supervise(initiative="INIT-2", project=OTHER_PROJECT, supervisor=w.supervisor(OTHER_SUPERVISOR),
                parent=w.parent(OTHER_PARENT))
    other = link_id(linkage.EXECUTION, linkage.INITIATIVE, "INIT-2", linkage.PROJECT, OTHER_PROJECT)
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=other, digest="d-two")


# ------------------------------------------------------------------ LNK-11..LNK-20

def handover(w, *, expect=PARENT, endpoint=None, acknowledged=(), evidence="taking over",
             role=linkage.PARENT, key=PROJECT):
    return w.step(lambda: w.linkage.handover(
        role=role, scope_key=key, expect_task_id=expect, endpoint=endpoint or w.parent(OTHER_PARENT),
        acknowledged=list(acknowledged), evidence=evidence, actor="test"))


def reg(w, *, parent=None, child=None, issue_key=ISSUE, recipients=None, dispatch, **kw):
    return w.step(lambda: w.registry.register(
        parent=parent or Endpoint(PARENT, HOST, cwd="/parent"), child=child or Endpoint("01child-two", HOST),
        issue_key=issue_key, artifact_roots=[ROOT], allowed_recipients=recipients or [PARENT],
        dispatch_request_id=dispatch, dispatch_turn_id=dispatch + "-turn", **kw)["relationshipId"])


def resume(w, rid, generation=1):
    w.step(lambda: w.registry.resume(rid, expect_generation=generation, expect_artifact_roots=[ROOT],
                                     expect_allowed_recipients=[PARENT], actor="test") and None)


def status_of(w, rid):
    w.step(lambda: w.registry.get(rid)["status"])


@scenario
def lnk11_contested_directives(w):
    execution = w.supervise()["linkId"]
    first = record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-one")
    second = record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-two")
    w.step(lambda: w.linkage.contested_directives(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.settle_directive(first["directiveId"], "chosen", decided_by="alice"))
    w.step(lambda: w.linkage.settle_directive(second["directiveId"], "superseded", decided_by="alice",
                                              reason="the initiative deferred"))
    w.step(lambda: w.linkage.contested_directives(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.settle_directive(first["directiveId"], "superseded", decided_by="bob"))
    w.step(lambda: w.linkage.settle_directive(first["directiveId"], "chosen", decided_by="a replaying caller"))
    w.step(lambda: w.linkage.directives(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.conflicts(linkage.PROJECT, PROJECT))


@scenario
def lnk11_same_instruction_after_handover(w):
    execution = w.supervise()["linkId"]
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-same")
    handover(w, role=linkage.SUPERVISOR, key=INITIATIVE, expect=SUPERVISOR_TASK,
             endpoint=w.supervisor(OTHER_SUPERVISOR), evidence="a new supervisor")
    record(w, origin=INITIATIVE, task=OTHER_SUPERVISOR, link=execution, digest="d-same")


@scenario
def lnk12_handover_unconfirmed(w):
    rid = scoped(w)
    handover(w)
    handover(w, expect="01somebody-else")
    handover(w, evidence="   ")
    handover(w, endpoint=w.parent(), acknowledged=[rid], evidence="handing over to myself")
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.conflicts(linkage.PROJECT, PROJECT))
    for endpoint in (Endpoint("", HOST), Endpoint(OTHER_PARENT, "")):
        handover(w, endpoint=endpoint, evidence="a blank replacement")
    handover(w, key="PROJ-NOBODY", endpoint=w.parent(), evidence="handing over a scope that does not exist")


@scenario
def lnk12_second_stale_handover(w):
    w.supervise()
    handover(w, evidence="the first handover")
    handover(w, endpoint=w.parent("01parent-three"), evidence="a stale second handover")
    w.rows("SELECT task_id FROM scope_bindings WHERE scope_key = ? AND role = ?"
           "  AND status IN ('active','paused')", (PROJECT, linkage.PARENT))


@scenario
def lnk12_self_handover_settled(w):
    w.supervise()
    handover(w, endpoint=w.parent(), evidence="handing over to myself")
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.conflicts(linkage.PROJECT, PROJECT))


@scenario
def lnk13_would_strand(w):
    rid = scoped(w)
    w.step(lambda: w.linkage.outstanding(PROJECT))
    w.step(lambda: w.linkage.outstanding(PROJECT, task_id="01nobody"))
    w.step(lambda: w.linkage.attached(PROJECT))
    handover(w, acknowledged=[rid], evidence="the outgoing parent listed its unfinished issues")
    w.store.db.execute("UPDATE relationships SET status = 'active' WHERE relationship_id = ?", (rid,))
    handover(w, acknowledged=w.linkage.outstanding(PROJECT), evidence="a settled-looking project")
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))


@scenario
def lnk13_third_parent(w):
    rid = scoped(w)
    reg(w, parent=Endpoint("01parent-three", HOST), recipients=["01parent-three"],
        dispatch="dispatch-third", supersedes=rid)
    w.step(lambda: w.linkage.attached(PROJECT, PARENT))
    handover(w, acknowledged=w.linkage.outstanding(PROJECT), evidence="work parked on a third parent")


@scenario
def lnk14_settled_handover(w):
    w.supervise()
    handover(w, evidence="the project has no unfinished issues left")
    w.step(lambda: w.linkage.binding(binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, PARENT)))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE,
                                          linkage.PROJECT, PROJECT)))
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))


@scenario
def lnk14_escape_route(w):
    rid = scoped(w)
    handover(w, acknowledged=[rid], evidence="before the work moved")
    reg(w, parent=Endpoint(OTHER_PARENT, HOST, cwd="/parent"), recipients=[OTHER_PARENT],
        dispatch="dispatch-moved", supersedes=rid)
    w.step(lambda: w.linkage.attached(PROJECT, PARENT))
    w.step(lambda: w.linkage.outstanding(PROJECT))
    handover(w, acknowledged=w.linkage.outstanding(PROJECT), evidence="the work moved first")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk14_child_not_handed_over(w):
    scoped(w)
    handover(w, role=linkage.CHILD, key=ISSUE, expect=CHILD, endpoint=Endpoint("01child-two", HOST),
             evidence="trying to move a child sideways")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk15_replay_other_host(w):
    w.supervise()
    w.step(lambda: w.linkage.register_supervision(
        initiative_key=INITIATIVE, project_key=PROJECT, supervisor=w.supervisor(),
        parent=Endpoint(PARENT, "some-other-host", cwd="/parent")))
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.conflicts(linkage.PROJECT, PROJECT))


@scenario
def lnk15_take_back_from_new_host(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                        endpoint=Endpoint(PARENT, HOST, cwd="/first", cxc_session="cxc-first")))
    handover(w, evidence="handed away")
    handover(w, expect=OTHER_PARENT, endpoint=Endpoint(PARENT, "host-two", cwd="/new-host", cxc_session="cxc-second"),
             evidence="taken back, from a different machine")
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT,
                                        endpoint=Endpoint(PARENT, "host-three")))


@scenario
def lnk15_restore_keeps_status(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST),
                                        status="archived"))
    w.step(lambda: w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST),
                                        status="paused"))


@scenario
def lnk16_blank_endpoints(w):
    for supervisor, parent in ((w.supervisor(""), w.parent()), (Endpoint(SUPERVISOR_TASK, ""), w.parent()),
                               (w.supervisor(), w.parent("")), (w.supervisor(), Endpoint(PARENT, ""))):
        w.step(lambda: w.supervise(supervisor=supervisor, parent=parent))
    w.rows("SELECT 1 FROM scope_bindings")
    w.supervise()
    w.supervise(project=OTHER_PROJECT, parent=w.parent(OTHER_PARENT))
    w.step(lambda: w.linkage.register_peer(left_project=PROJECT, left_parent=Endpoint("", HOST),
                                           right_project=OTHER_PROJECT, right_parent=w.parent(OTHER_PARENT)))


@scenario
def lnk16_blank_child_host(w):
    w.supervise()
    rid = w.register()
    w.store.db.execute("UPDATE relationships SET child_host_id = '' WHERE relationship_id = ?", (rid,))
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk17_pause_and_cancel(w):
    rid = scoped(w)
    w.registry.set_status(rid, "paused", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.down(linkage.PROJECT, PROJECT))
    w.registry.set_status(rid, "cancelled", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk17_archived_then_resume(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.registry.set_status(rid, "paused", actor="test") and None)
    status_of(w, rid)
    resume(w, rid, generation=7)
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    status_of(w, rid)
    resume(w, rid)
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.attachment(rid))


@scenario
def lnk18_reassigned_issue(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint("01child-two", HOST))
    resume(w, rid)
    w.rows("SELECT task_id FROM scope_bindings WHERE scope_key = ? AND role = ?"
           "  AND status IN ('active','paused')", (ISSUE, linkage.CHILD))
    status_of(w, rid)
    w.step(lambda: w.linkage.conflicts(linkage.ISSUE, ISSUE))


@scenario
def lnk18_project_changed_hands(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    handover(w, evidence="the archived assignment left nothing behind")
    resume(w, rid)
    status_of(w, rid)
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.conflicts(linkage.PROJECT, PROJECT))


@scenario
def lnk18_project_without_parent(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    w.store.db.execute("UPDATE scope_bindings SET status = 'archived'  WHERE scope_kind = ? AND scope_key = ? AND role = ?",
                       (linkage.PROJECT, PROJECT, linkage.PARENT))
    resume(w, rid)
    status_of(w, rid)


@scenario
def lnk18_reclaimed_elsewhere(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, "host-two"))
    resume(w, rid)
    status_of(w, rid)
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.conflicts(linkage.ISSUE, ISSUE))


@scenario
def lnk18_refused_resume_contest(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint("01child-two", HOST))
    resume(w, rid)
    w.step(lambda: w.linkage.conflicts(linkage.ISSUE, ISSUE))
    status_of(w, rid)


@scenario
def lnk19_other_issue_successor(w):
    rid = scoped(w)
    reg(w, child=Endpoint("01child-far", HOST), issue_key="REL-FAR", dispatch="dispatch-far", supersedes=rid)
    status_of(w, rid)
    w.step(lambda: w.linkage.attachment(rid))


@scenario
def lnk19_borrowed_predecessor(w):
    rid = scoped(w)
    reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint("01child-far", HOST), issue_key="REL-FAR",
        recipients=[OTHER_PARENT], dispatch="dispatch-far", supersedes=rid, project_key=PROJECT)
    status_of(w, rid)


@scenario
def lnk19_dead_predecessor(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint("01child-two", HOST, cwd=ROOT),
        recipients=[OTHER_PARENT], dispatch="dispatch-dead", supersedes=rid, project_key=PROJECT)
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))


@scenario
def lnk19_project_history_only(w):
    w.supervise()
    w.supervise(project=OTHER_PROJECT, parent=w.parent(OTHER_PARENT))
    first = w.register()
    w.linkage.attach_issue(first, PROJECT)
    w.registry.set_status(first, "cancelled", actor="test")
    unrelated = reg(w, parent=Endpoint(OTHER_PARENT, HOST), child=Endpoint("01child-far", HOST),
                    recipients=[OTHER_PARENT], dispatch="dispatch-unrelated")
    w.step(lambda: w.linkage.attach_issue(unrelated, PROJECT))


@scenario
def lnk19_same_child_second_parent(w):
    rid = scoped(w)
    reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint(CHILD, HOST, cwd=ROOT), recipients=[OTHER_PARENT],
        dispatch="dispatch-second-parent")
    w.rows("SELECT relationship_id FROM relationships WHERE issue_key = ?"
           "  AND status IN ('active','paused') AND superseded_by IS NULL", (ISSUE,))


@scenario
def lnk19_reclaimed_issue_successor(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST))
    reg(w, parent=w.parent(), child=Endpoint("01child-two", HOST, cwd=ROOT), dispatch="dispatch-successor",
        supersedes=rid, project_key=PROJECT)
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk20_replacement_inherits(w):
    rid = scoped(w)
    new = reg(w, dispatch="dispatch-successor", supersedes=rid)
    w.step(lambda: w.linkage.attachment(new))
    w.registry.set_status(rid, "cancelled", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT, linkage.ISSUE, ISSUE)))


@scenario
def lnk20_same_child_again(w):
    w.supervise()
    first = w.register()
    w.linkage.attach_issue(first, PROJECT)
    w.registry.set_status(first, "archived", actor="test")
    handover(w, evidence="the archived assignment left nothing behind")
    again = reg(w, parent=Endpoint(OTHER_PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
                recipients=[OTHER_PARENT], dispatch="dispatch-again", project_key=PROJECT)
    w.registry.set_status(first, "cancelled", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.attachment(again))


@scenario
def lnk20_direct_rebind_survives(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    w.step(lambda: w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST)))
    w.registry.set_status(rid, "cancelled", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lnk20_supersede_dead_leaves_reclaim(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    w.linkage.bind_scope(role=linkage.CHILD, scope_key=ISSUE, endpoint=Endpoint(CHILD, HOST))
    w.registry.supersede(rid, new_relationship_id="rel-elsewhere")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


# ------------------------------------------------------------------ LNK-21..LNK-29

def handed_to(w, incoming, *, supersedes, dispatch):
    moved = reg(w, parent=w.parent(incoming), child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch=dispatch,
                supersedes=supersedes, project_key=PROJECT)
    holder = w.linkage.owner(linkage.PROJECT, PROJECT)["taskId"]
    handover(w, expect=holder, endpoint=w.parent(incoming), acknowledged=w.linkage.outstanding(PROJECT),
             evidence="the assignment moved first")
    return moved


def full(w, rid):
    w.step(lambda: w.registry.get(rid))


@scenario
def lnk21_handback(w):
    original = scoped(w)
    away = handed_to(w, OTHER_PARENT, supersedes=original, dispatch="dispatch-away")
    back = handed_to(w, PARENT, supersedes=away, dispatch="dispatch-back")
    full(w, back)
    w.step(lambda: w.linkage.owner(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.attachment(original))
    full(w, away)
    w.rows("SELECT relationship_id, supersedes, superseded_by FROM relationships ORDER BY relationship_id")


@scenario
def lnk21_handback_new_host(w):
    original = scoped(w)
    away = handed_to(w, OTHER_PARENT, supersedes=original, dispatch="dispatch-away")
    moved = reg(w, parent=Endpoint(PARENT, "host-two", cwd="/moved", cxc_session="cxc-moved"),
                child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch="dispatch-moved", supersedes=away,
                project_key=PROJECT)
    full(w, moved)


@scenario
def lnk21_restores_retained_project(w):
    original = scoped(w)
    w.registry.set_status(original, "cancelled", actor="test")
    interim = reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint("01child-two", HOST, cwd=ROOT),
                  recipients=[OTHER_PARENT], dispatch="dispatch-interim")
    w.step(lambda: w.linkage.attachment(interim))
    back = reg(w, parent=w.parent(), child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch="dispatch-back",
               supersedes=interim)
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.attachment(original))
    w.step(lambda: w.linkage.up(issue_key=ISSUE))


@scenario
def lnk21_refusals(w):
    original = scoped(w)
    w.registry.set_status(original, "archived", actor="test")
    reg(w, parent=w.parent(), child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch="dispatch-silent")
    status_of(w, original)


@scenario
def lnk21_predecessor_released(w):
    original = scoped(w)
    away = handed_to(w, OTHER_PARENT, supersedes=original, dispatch="dispatch-away")
    w.registry.set_status(away, "cancelled", actor="test")
    reg(w, parent=w.parent(), child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch="dispatch-late",
        supersedes=away, project_key=PROJECT)
    status_of(w, original)


@scenario
def lnk21_live_replay_other_host(w):
    w.supervise()
    w.register()
    reg(w, parent=Endpoint(PARENT, "host-two", cwd="/parent"), child=Endpoint(CHILD, HOST, cwd=ROOT),
        dispatch="dispatch-elsewhere")


@scenario
def lnk21_earlier_dispatch(w):
    original = scoped(w)
    away = reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint(CHILD, HOST, cwd=ROOT),
               dispatch="dispatch-away", supersedes=original, project_key=PROJECT)
    w.step(lambda: w.registry.register(
        parent=w.parent(), child=Endpoint(CHILD, HOST, cwd=ROOT), issue_key=ISSUE, artifact_roots=[ROOT],
        allowed_recipients=[PARENT], dispatch_request_id="dispatch-1", dispatch_turn_id="turn-replayed",
        supersedes=away, project_key=PROJECT)["relationshipId"])
    status_of(w, original)


@scenario
def lnk21_refused_tenure_contest(w):
    original = scoped(w)
    away = reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint("01child-two", HOST, cwd=ROOT),
               dispatch="dispatch-away", supersedes=original, project_key=PROJECT)
    w.linkage.bind_scope(role=linkage.CHILD, scope_key="REL-ELSEWHERE", endpoint=Endpoint(CHILD, HOST))
    reg(w, parent=w.parent(), child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch="dispatch-back",
        supersedes=away, project_key=PROJECT)
    w.step(lambda: w.linkage.conflicts(linkage.ISSUE, ISSUE))
    full(w, away)
    full(w, original)


def contest_the_project(w):
    w.store.db.execute("DROP INDEX scope_bindings_one_live_owner")
    w.store.db.execute(
        "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
        " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
        " superseded_by, handover_note, created_at, updated_at)"
        " VALUES (?,?,?,?,?,?,NULL,NULL,'active',2,NULL,NULL,NULL,?,?)",
        (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT),
         linkage.PARENT, linkage.PROJECT, PROJECT, OTHER_PARENT, HOST,
         "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))


@scenario
def lnk22_contested_writers(w):
    w.supervise()
    rid = w.register()
    w.supervise(project=OTHER_PROJECT, parent=w.parent("01parent-three"))
    contest_the_project(w)
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.attachment(rid))
    w.step(lambda: w.linkage.register_peer(left_project=PROJECT, left_parent=w.parent(),
                                           right_project=OTHER_PROJECT, right_parent=w.parent("01parent-three")))
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent()))
    w.step(lambda: w.linkage.conflicts(linkage.PROJECT, PROJECT))


@scenario
def lnk23_walk_states(w):
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, "INIT-NEVER-SEEN"))
    w.supervise()
    w.store.db.execute(
        "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"
        " host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"
        " handover_note, created_at, updated_at)"
        " VALUES ('bnd-staged-second','parent','project',?,?,?,NULL,NULL,'active',1,"
        "         NULL,NULL,NULL,?,?)", (OTHER_PROJECT, PARENT, HOST, w.clock.iso(), w.clock.iso()))
    w.step(lambda: w.linkage.up(task_id=PARENT))
    w.step(lambda: w.linkage.up(task_id=PARENT, scope_key=OTHER_PROJECT))
    rid = w.register()
    w.linkage.attach_issue(rid, PROJECT)
    w.step(lambda: w.linkage.up(task_id=PARENT, issue_key=ISSUE))
    w.step(lambda: w.linkage.up(relationship_id=rid))
    w.store.db.close()
    for answer in (lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE), lambda: w.linkage.up(task_id=PARENT),
                   lambda: w.linkage.counterpart(PARENT, CHILD)):
        w.step(answer)


@scenario
def lnk24_instruction_conflict(w):
    execution = w.supervise()["linkId"]
    w.supervise(initiative="INIT-2", supervisor=w.supervisor(OTHER_SUPERVISOR), kind=linkage.REFERENCE)
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-one")
    record(w, origin=INITIATIVE, task=SUPERVISOR_TASK, link=execution, digest="d-two")
    w.step(lambda: w.linkage.down(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.up(task_id=PARENT))


@scenario
def lnk24_cycle(w):
    w.supervise()
    w.store.db.execute(
        "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
        " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
        " superseded_by, created_at, updated_at)"
        " VALUES ('lnk-forced-cycle','execution','project',?,?,'initiative',?,?,"
        "         'active',1,NULL,?,?)", (PROJECT, PARENT, INITIATIVE, SUPERVISOR_TASK, w.clock.iso(), w.clock.iso()))
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))


@scenario
def lnk24_two_parents_of_issue(w):
    w.supervise()
    w.supervise(project=OTHER_PROJECT, parent=w.parent(OTHER_PARENT))
    for project in (PROJECT, OTHER_PROJECT):
        lid = link_id(linkage.EXECUTION, linkage.PROJECT, project, linkage.ISSUE, ISSUE)
        w.store.db.execute(
            "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
            " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
            " superseded_by, created_at, updated_at)"
            " VALUES (?,'execution','project',?,?,'issue',?,?,'active',1,NULL,?,?)",
            (lid, project, PARENT, ISSUE, CHILD, "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))


def stray_link(w, kind, initiative, *, revision, upper_task):
    lid = link_id(kind, linkage.INITIATIVE, initiative, linkage.PROJECT, PROJECT)
    w.store.db.execute(
        "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"
        " upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"
        " superseded_by, created_at, updated_at)"
        " VALUES (?,?,?,?,?,?,?,?,'active',?,NULL,?,?)",
        (lid, kind, linkage.INITIATIVE, initiative, upper_task, linkage.PROJECT,
         PROJECT, PARENT, revision, "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))
    return lid


@scenario
def lnk24_two_supervisions(w):
    w.supervise()
    stray_link(w, linkage.EXECUTION, OTHER_INITIATIVE, revision=2, upper_task=OTHER_SUPERVISOR)
    w.step(lambda: w.linkage.up(task_id=PARENT))


def duplicate_owners(w):
    w.store.db.execute("DROP INDEX scope_bindings_one_live_owner")
    for task, revision in (("01owner-one", 1), ("01owner-two", 2)):
        w.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
            " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
            " superseded_by, handover_note, created_at, updated_at)"
            " VALUES (?,?,?,?,?,?,NULL,NULL,'active',?,NULL,NULL,NULL,?,?)",
            (binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, task),
             linkage.PARENT, linkage.PROJECT, PROJECT, task, HOST, revision,
             "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z"))


@scenario
def lnk24_two_live_owners(w):
    duplicate_owners(w)
    w.step(lambda: w.linkage.down(linkage.PROJECT, PROJECT))
    w.step(lambda: w.linkage.up(task_id="01owner-two"))


@scenario
def lnk24_handover_staging(w):
    rid = scoped(w)
    reg(w, parent=w.parent(OTHER_PARENT), child=Endpoint("01child-two", HOST, cwd=ROOT),
        recipients=[OTHER_PARENT], dispatch="dispatch-moved", supersedes=rid, project_key=PROJECT)
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))
    w.step(lambda: w.linkage.up(issue_key=ISSUE))


@scenario
def lnk25_counterpart(w):
    scoped(w)
    w.step(lambda: w.linkage.counterpart(PARENT, CHILD))
    w.step(lambda: w.linkage.counterpart(PARENT, CHILD, quoted_revision=99))
    w.step(lambda: w.linkage.counterpart(PARENT, CHILD, from_scope="PROJ-NOT-MINE"))
    w.step(lambda: w.linkage.counterpart(PARENT, CHILD, quoted_scope="ISS-NOT-HELD"))
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, PARENT))


@scenario
def lnk25_key_reused_at_another_level(w):
    w.linkage.bind_scope(role=linkage.SUPERVISOR, scope_key="SHARED-KEY", endpoint=w.supervisor())
    w.linkage.bind_scope(role=linkage.CHILD, scope_key="SHARED-KEY", endpoint=Endpoint("01child-far", HOST))
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, "01child-far"))


@scenario
def lnk25_two_edges(w):
    w.supervise()
    stray_link(w, linkage.REFERENCE, INITIATIVE, revision=1, upper_task=SUPERVISOR_TASK)
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, PARENT))


@scenario
def lnk25_two_historical_scopes(w):
    w.supervise()
    handover(w, evidence="the first project moved on")
    w.supervise(project=OTHER_PROJECT)
    handover(w, key=OTHER_PROJECT, endpoint=w.parent("01parent-three"), evidence="and so did the second")
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, PARENT))
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, PARENT, quoted_scope=OTHER_PROJECT))


@scenario
def lnk26_guard_index(w):
    w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent())
    import sqlite3
    try:
        w.store.db.execute(
            "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"
            " task_id, host_id, cwd, cxc_session, status, revision, supersedes,"
            " superseded_by, handover_note, created_at, updated_at)"
            " VALUES ('bnd-forced','parent','project',?,?,?,NULL,NULL,'active',1,NULL,"
            "         NULL,NULL,?,?)", (PROJECT, OTHER_PARENT, HOST, w.clock.iso(), w.clock.iso()))
        w.step(lambda: "inserted")
    except sqlite3.IntegrityError as fault:
        w.step(lambda: type(fault).__name__ + ": " + str(fault))


@scenario
def lnk26_unenforced(w):
    import argparse
    from codex_session_relay import cli
    parsed = cli.build_parser().parse_args(["linkage-down", "--scope-kind", "project", "--scope", PROJECT])
    w.step(lambda: cli.cmd_linkage_down(argparse.Namespace(linkage=w.linkage, store=w.store), parsed))
    duplicate_owners(w)
    reopened = Store(w.store.path)
    w.step(lambda: reopened.unenforced_indexes)
    w.step(lambda: Linkage(reopened, w.clock).up(task_id="01owner-one"))
    w.step(lambda: cli.cmd_linkage_down(argparse.Namespace(linkage=Linkage(reopened, w.clock), store=reopened), parsed))
    reopened.close()


@scenario
def lnk27_ids(w):
    w.step(lambda: binding_id(linkage.PARENT, linkage.PROJECT, PROJECT, PARENT))
    w.step(lambda: link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE, linkage.PROJECT, PROJECT))
    w.step(lambda: link_id(linkage.PEER, linkage.PROJECT, OTHER_PROJECT, linkage.PROJECT, PROJECT))
    w.step(lambda: linkage.directive_id(linkage.PROJECT, PROJECT, INITIATIVE, "d-one", 1))


@scenario
def lnk29_contested_assignment_view(w):
    from codex_session_relay.assignment import AssignmentView
    scoped(w)
    contest_the_project(w)
    answer = AssignmentView(w.store, w.registry, w.clock).for_issue(ISSUE)
    w.step(lambda: {k: answer[k] for k in ("projectKey", "projectParentTaskId", "scopeState",
                                           "projectParentCandidates", "parentOwnsProject")})


@scenario
def lnk_literal_reasons(w):
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent(), status="bogus"))
    w.step(lambda: w.linkage.settle_directive("dir-x", "bogus", decided_by="a"))
    w.step(lambda: w.linkage.settle_directive("dir-x", "chosen", decided_by="a"))
    w.step(lambda: w.linkage.bind_scope(role="bogus", scope_key=PROJECT, endpoint=w.parent()))
    w.step(lambda: w.linkage.bind_scope(role=linkage.PARENT, scope_key="A|B", endpoint=w.parent()))
    w.step(lambda: w.linkage.handover(role=linkage.PARENT, scope_key=PROJECT, expect_task_id=PARENT,
                                      endpoint=w.parent(OTHER_PARENT), acknowledged=[], evidence="", actor="t"))
    w.step(lambda: w.linkage.register_peer(left_project=PROJECT, left_parent=w.parent(), right_project=PROJECT,
                                           right_parent=w.parent(OTHER_PARENT)))
    w.step(lambda: w.linkage.attach_issue("rel-missing", PROJECT))


@scenario
def lnk19_racer(w):
    from unittest import mock
    w.supervise()
    original = type(w.registry)._register_in_transaction

    def racing(registry, rid, parent, child, issue_key, *rest, **options):
        w.linkage.bind_scope(role=linkage.CHILD, scope_key=issue_key, endpoint=Endpoint("01child-racer", HOST))
        return original(registry, rid, parent, child, issue_key, *rest, **options)

    with mock.patch.object(type(w.registry), "_register_in_transaction", racing):
        reg(w, parent=w.parent(), child=Endpoint(CHILD, HOST, cwd=ROOT), dispatch="dispatch-raced", project_key=PROJECT)
    w.step(lambda: w.linkage.conflicts(linkage.ISSUE, ISSUE))
    w.rows("SELECT relationship_id FROM relationships WHERE issue_key = ?", (ISSUE,))


def main():
    only = set(sys.argv[1:])
    out = {}
    for name, fn in SCENARIOS.items():
        if only and name not in only:
            continue
        world = World()
        try:
            fn(world)
        finally:
            world.store.close()
        out[name] = json.loads(json.dumps(world.out))
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
