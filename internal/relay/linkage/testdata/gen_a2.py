"""Record the Python answers for todo 26 part A2 (test_linkage_peer/queries/recovery.py).

Run from the repository root with HOME/XDG_*/CODEX_HOME/TMPDIR in a scratch directory:
  uv run --no-sync python internal/relay/linkage/testdata/gen_a2.py \
      > internal/relay/linkage/testdata/python_a2.json
Same step format and World as gen_linkage.py (FakeClock, literal roots).
"""
import json
import os
import sys
import threading

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gen_linkage as g  # noqa: E402
from gen_linkage import (  # noqa: E402
    CHILD, HOST, INITIATIVE, ISSUE, OTHER_PARENT, OTHER_PROJECT, PARENT, PROJECT, ROOT,
    SUPERVISOR_TASK, World, scoped,
)
from codex_session_relay import linkage  # noqa: E402
from codex_session_relay.assignment import AssignmentView  # noqa: E402
from codex_session_relay.clock import FakeClock  # noqa: E402
from codex_session_relay.errors import RelayError  # noqa: E402
from codex_session_relay.linkage import Linkage, binding_id, link_id  # noqa: E402
from codex_session_relay.models import Endpoint  # noqa: E402
from codex_session_relay.registry import Registry  # noqa: E402
from codex_session_relay.store import Store  # noqa: E402

THIRD_PROJECT = "PROJ-3"
THIRD_PARENT = "01parent-three"
SCENARIOS = {}
NEW_TABLES = ("scope_bindings", "scope_links", "relationship_scope", "scope_directives",
              "linkage_conflicts")


def scenario(fn):
    SCENARIOS[fn.__name__] = fn
    return fn


def two_projects(w):
    w.supervise()
    w.supervise(project=OTHER_PROJECT, parent=w.parent(OTHER_PARENT))


def peer(w, left=PROJECT, left_task=PARENT, right=OTHER_PROJECT, right_task=OTHER_PARENT):
    return w.step(lambda: w.linkage.register_peer(
        left_project=left, left_parent=w.parent(left_task),
        right_project=right, right_parent=w.parent(right_task)))


def hand(w, key, expect, to, evidence):
    w.linkage.handover(role=linkage.PARENT, scope_key=key, expect_task_id=expect,
                       endpoint=w.parent(to), acknowledged=[], evidence=evidence, actor="test")


def execution_edges(w):
    w.rows("SELECT link_id, lower_key, lower_task_id FROM scope_links"
           " WHERE link_kind = 'execution' AND status IN ('active','paused') ORDER BY link_id")


def pick(record, keys):
    return {k: record[k] for k in keys}


# ------------------------------------------------------------------ test_linkage_peer.py

@scenario
def lpr1_one_record(w):
    two_projects(w)
    peer(w)
    peer(w, OTHER_PROJECT, OTHER_PARENT, PROJECT, PARENT)
    w.rows("SELECT link_id FROM scope_links WHERE link_kind = 'peer'")
    hand(w, OTHER_PROJECT, OTHER_PARENT, THIRD_PARENT, "the peer project changed hands")
    peer(w, right_task=THIRD_PARENT)
    w.rows("SELECT link_id FROM scope_links WHERE link_kind = 'peer'")


@scenario
def lpr2_no_level(w):
    two_projects(w)
    execution_edges(w)
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))
    w.step(lambda: w.linkage.up(task_id=PARENT))
    peer(w)
    execution_edges(w)
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))
    w.step(lambda: w.linkage.up(task_id=PARENT))
    for project in (PROJECT, OTHER_PROJECT):
        w.rows("SELECT task_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ?"
               "  AND role = ? AND status IN ('active','paused')",
               (linkage.PROJECT, project, linkage.PARENT))


@scenario
def lpr3_refusals(w):
    two_projects(w)
    peer(w, PROJECT, PARENT, PROJECT, PARENT)
    peer(w, PROJECT, PARENT, OTHER_PROJECT, THIRD_PARENT)
    peer(w, PROJECT, PARENT, THIRD_PROJECT, THIRD_PARENT)


def peered(w):
    two_projects(w)
    w.linkage.register_peer(left_project=PROJECT, left_parent=w.parent(),
                            right_project=OTHER_PROJECT, right_parent=w.parent(OTHER_PARENT))


@scenario
def lpr4_linked(w):
    peered(w)
    w.step(lambda: w.linkage.counterpart(PARENT, OTHER_PARENT))


@scenario
def lpr5_recipient_replaced(w):
    peered(w)
    hand(w, OTHER_PROJECT, OTHER_PARENT, THIRD_PARENT, "the peer project changed hands")
    w.step(lambda: w.linkage.counterpart(PARENT, THIRD_PARENT, quoted_revision=1))
    w.step(lambda: w.linkage.counterpart(PARENT, OTHER_PARENT))


@scenario
def lpr5_other_scope_and_roles(w):
    peered(w)
    w.step(lambda: w.linkage.counterpart(PARENT, OTHER_PARENT, quoted_scope="PROJ-NOPE"))
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, PARENT))
    rid = w.register()
    w.linkage.attach_issue(rid, PROJECT)
    w.step(lambda: w.linkage.counterpart(SUPERVISOR_TASK, CHILD))


@scenario
def lpr5_sender_replaced(w):
    peered(w)
    hand(w, PROJECT, PARENT, THIRD_PARENT, "the sending project changed hands")
    w.step(lambda: w.linkage.counterpart(PARENT, OTHER_PARENT))


@scenario
def lpr6_unregistered_and_unreadable(w):
    peered(w)
    w.step(lambda: w.linkage.counterpart(PARENT, "01nobody-at-all"))
    w.store.db.close()
    w.step(lambda: w.linkage.counterpart(PARENT, OTHER_PARENT))


# ------------------------------------------------------------------ test_linkage_queries.py

@scenario
def lqy1_three_levels(w):
    rid = scoped(w)
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))
    w.step(lambda: w.linkage.up(task_id=CHILD))
    w.step(lambda: w.linkage.up(relationship_id=rid))


@scenario
def lqy2_unreadable(w):
    scoped(w)
    w.store.db.close()
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))
    w.step(lambda: w.linkage.up(task_id=CHILD))


@scenario
def lqy3_unscoped_assignment(w):
    w.register()
    w.step(lambda: w.linkage.up(issue_key=ISSUE))


@scenario
def lqy3_project_without_parent(w):
    w.supervise()
    hand(w, PROJECT, PARENT, OTHER_PARENT, "handing over")
    w.store.db.execute("UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", (PROJECT,))
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))


@scenario
def lqy3_no_supervisor(w):
    w.linkage.bind_scope(role=linkage.PARENT, scope_key=PROJECT, endpoint=w.parent())
    w.step(lambda: w.linkage.up(task_id=PARENT))


@scenario
def lqy4_recorded_conflict(w):
    w.supervise()
    w.step(lambda: w.supervise(initiative="INIT-9", supervisor=w.supervisor("01supervisor-nine")))
    w.step(lambda: w.linkage.down(linkage.PROJECT, PROJECT))


@scenario
def lqy4_instruction_pair(w):
    execution = w.supervise()
    w.step(lambda: w.supervise(initiative="INIT-2", supervisor=w.supervisor("01supervisor-two"),
                               kind=linkage.REFERENCE))
    for digest in ("d-one", "d-two"):
        w.step(lambda: w.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR_TASK,
            from_scope_key=INITIATIVE, link_id_value=execution["linkId"], digest=digest))
    w.step(lambda: w.linkage.down(linkage.PROJECT, PROJECT))


@scenario
def lqy4_owner_drift(w):
    w.supervise()
    w.store.db.execute("UPDATE scope_bindings SET task_id = ? WHERE scope_key = ? AND role = ?",
                       (OTHER_PARENT, PROJECT, linkage.PARENT))
    w.step(lambda: w.linkage.down(linkage.INITIATIVE, INITIATIVE))


PROJECT_KEYS = ("projectKey", "projectParentTaskId", "scopeState", "parentOwnsProject",
                "responsibleChild", "responsibleRelationship")


def view(w):
    return AssignmentView(w.store, Registry(w.store, w.clock), w.clock)


@scenario
def lqy5_project_context(w):
    scoped(w)
    w.step(lambda: pick(view(w).for_issue(ISSUE), PROJECT_KEYS))
    w.store.db.execute("DROP TABLE relationship_scope")
    record = view(w).for_issue(ISSUE)
    w.step(lambda: {k: record.get(k, "<absent>") for k in PROJECT_KEYS})


@scenario
def lqy5_unscoped(w):
    w.register()
    record = view(w).for_issue(ISSUE)
    w.step(lambda: {k: record.get(k, "<absent>") for k in PROJECT_KEYS})


# ------------------------------------------------------------------ test_linkage_recovery.py

LEGACY_ROWS = [
    "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome,"
    " producer, turn_thread_id, turn_id, turn_status, receipt, path_binding_mode, stage,"
    " first_seen_at, last_seen_at) VALUES ('evt-legacy', '{rid}', 1, 'sha256:abc', 'ready_for_review',"
    " 'child', '01child-task', 'turn-dispatch-1', 'completed', '{{}}', NULL, 'accepted', 't0', 't0')",
    "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id,"
    " state, attempt_count, hold_reason, created_at, updated_at) VALUES ('evt-legacy', '{rid}',"
    " 'completion', '01parent-task', '01parent-task', 'in_flight', 1, NULL, 't0', 't0')",
    "INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, record,"
    " sealed, observed_at) VALUES ('req-legacy', 'evt-legacy', 1, 'completion', 'transmitting',"
    " NULL, NULL, 0, 't0')",
]
SNAPSHOTS = [
    "SELECT relationship_id, issue_key, status, parent_task_id, child_task_id, execution_generation"
    " FROM relationships ORDER BY rowid",
    "SELECT relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id"
    " FROM generations ORDER BY rowid",
    "SELECT event_id, relationship_id, revision_hash, outcome, stage FROM events ORDER BY rowid",
    "SELECT event_id, relationship_id, kind, recipient_task_id, state, attempt_count, hold_reason"
    " FROM deliveries ORDER BY rowid",
    "SELECT request_id, event_id, attempt_no, kind, internal_state, state, sealed FROM attempts"
    " ORDER BY rowid",
]


def reopen_without_new_tables(w):
    for table in NEW_TABLES:
        w.store.db.execute("DROP TABLE IF EXISTS " + table)
    path = w.store.path
    w.store.close()
    w.store = Store(path)
    w.linkage = Linkage(w.store, w.clock)
    w.registry = Registry(w.store, w.clock)


@scenario
def lrc1_existing_store(w):
    rid = w.register()
    with w.store.transaction() as db:
        for sql in LEGACY_ROWS:
            db.execute(sql.format(rid=rid))
    for sql in SNAPSHOTS:
        w.rows(sql)
    reopen_without_new_tables(w)
    for sql in SNAPSHOTS:
        w.rows(sql)
    w.rows("SELECT name FROM sqlite_master WHERE type = 'table' AND name IN (%s) ORDER BY name"
           % ",".join("'" + t + "'" for t in NEW_TABLES))
    w.supervise()
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


REGISTERED = ("relationshipId", "executionGeneration", "status", "issueKey")


def again(w, project):
    return w.registry.register(
        parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
        child=Endpoint(CHILD, HOST, cwd=ROOT, cxc_session="cxc-child"),
        issue_key=ISSUE, artifact_roots=[ROOT], allowed_recipients=[PARENT],
        dispatch_request_id="dispatch-1", dispatch_turn_id="turn-dispatch-1", project_key=project)


@scenario
def lrc2_register_with_project(w):
    w.supervise()
    rid = w.step(lambda: w.registry.register(
        parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint(CHILD, HOST),
        issue_key="REL-NEW", artifact_roots=[ROOT], allowed_recipients=[PARENT],
        dispatch_request_id="dispatch-new", dispatch_turn_id="turn-new",
        project_key=PROJECT)["relationshipId"])
    w.rows("SELECT project_key FROM relationship_scope WHERE relationship_id = ?", (rid,))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, "REL-NEW"))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT,
                                          linkage.ISSUE, "REL-NEW")))


@scenario
def lrc2_reregister(w):
    w.supervise()
    rid = w.register()
    w.step(lambda: w.linkage.attachment(rid))
    x = w.step(lambda: pick(again(w, PROJECT), REGISTERED))
    w.step(lambda: len(w.registry.get(x["relationshipId"])["generations"]))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.attachment(rid))
    w.step(lambda: pick(again(w, OTHER_PROJECT), REGISTERED))


def issue_state(w):
    w.step(lambda: w.linkage.binding(binding_id(linkage.CHILD, linkage.ISSUE, ISSUE, CHILD)))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT,
                                          linkage.ISSUE, ISSUE)))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lrc3_archive(w):
    rid = scoped(w)
    w.registry.set_status(rid, "archived", actor="test")
    issue_state(w)


@scenario
def lrc3_resume(w):
    rid = scoped(w)
    w.registry.set_status(rid, "cancelled", actor="test")
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.registry.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT],
                      expect_allowed_recipients=[PARENT], actor="test")
    issue_state(w)


@scenario
def lrc3_unscoped(w):
    rid = w.register()
    w.registry.set_status(rid, "archived", actor="test")
    w.rows("SELECT * FROM scope_bindings")
    w.rows("SELECT * FROM scope_links")


@scenario
def lrc3_replacement(w):
    original = scoped(w)
    replacement = w.registry.register(
        parent=Endpoint(PARENT, HOST, cwd="/parent"), child=Endpoint("01child-two", HOST),
        issue_key=ISSUE, artifact_roots=[ROOT], allowed_recipients=[PARENT],
        dispatch_request_id="dispatch-replacement", dispatch_turn_id="turn-replacement",
        supersedes=original, project_key=PROJECT)["relationshipId"]
    w.step(lambda: w.registry.get(original)["status"])
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT,
                                          linkage.ISSUE, ISSUE)))
    w.step(lambda: w.linkage.attachment(replacement))


@scenario
def lrc4_refused_supervision(w):
    w.linkage.bind_scope(role=linkage.CHILD, scope_key="ISS-OTHER", endpoint=w.parent(OTHER_PARENT))
    w.step(lambda: w.supervise(parent=w.parent(OTHER_PARENT)))
    w.step(lambda: w.linkage.owner(linkage.INITIATIVE, INITIATIVE))
    w.rows("SELECT scope_key FROM scope_bindings ORDER BY scope_key")
    w.rows("SELECT link_id FROM scope_links")
    w.conflicts()


@scenario
def lrc4_half_written(w):
    w.supervise()
    rid = w.register()
    with w.store.transaction() as db:
        db.execute("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"
                   " VALUES (?,?,?)", (rid, PROJECT, w.clock.iso()))
    w.step(lambda: w.linkage.up(relationship_id=rid))


@scenario
def lrc4_failure_partway(w):
    w.supervise()
    rid = w.register()

    class Interrupted(Exception):
        pass

    try:
        with w.store.transaction() as db:
            row = db.execute("SELECT * FROM relationships WHERE relationship_id = ?", (rid,)).fetchone()
            w.step(lambda: w.linkage.attach_in(db, row, PROJECT))
            w.step(lambda: [dict(r) for r in db.execute(
                "SELECT relationship_id, project_key FROM relationship_scope WHERE relationship_id = ?",
                (rid,)).fetchall()])
            raise Interrupted()
    except Interrupted:
        pass
    w.rows("SELECT 1 FROM relationship_scope WHERE relationship_id = ?", (rid,))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))
    w.step(lambda: w.linkage.link(link_id(linkage.EXECUTION, linkage.PROJECT, PROJECT,
                                          linkage.ISSUE, ISSUE)))
    w.step(lambda: w.linkage.attach_issue(rid, PROJECT))
    w.step(lambda: w.linkage.owner(linkage.ISSUE, ISSUE))


@scenario
def lrc5_concurrent_attach(w):
    w.supervise()
    w.supervise(initiative="INIT-2", project=OTHER_PROJECT,
                supervisor=w.supervisor("01supervisor-two"), parent=w.parent(OTHER_PARENT))
    rid = w.register()
    gate = threading.Barrier(2)
    reasons, wins = [], []

    def attach(project):
        store = Store(w.store.path)
        try:
            worker = Linkage(store, FakeClock())
            gate.wait(timeout=10)
            worker.attach_issue(rid, project)
            wins.append(project)
        except RelayError as problem:
            reasons.append(problem.reason.value)
        finally:
            store.close()

    threads = [threading.Thread(target=attach, args=(p,)) for p in (PROJECT, OTHER_PROJECT)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=20)
    w.step(lambda: {"rows": len(w.store.all("SELECT project_key FROM relationship_scope"
                                            " WHERE relationship_id = ?", (rid,))),
                    "wins": len(wins), "refusals": reasons})


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
            try:
                world.store.close()
            except Exception:  # an unreadable scenario closed it already
                pass
        out[name] = json.loads(json.dumps(world.out))
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
