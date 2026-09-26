"""Record the Python role-policy answers for the ROL-* parity tests.

  uv run --no-sync python internal/relay/registry/testdata/gen_rolepolicy.py \
      > internal/relay/registry/testdata/python_rolepolicy.json

Every scenario is a list of steps in a small language both sides execute (rolepolicy_test.go
runs the same steps against the Go registry). Each step records Python's whole answer. The
policy directory is written as ${DIR} and a policy digest as ${DIGEST}, because both depend on
the run: the Go test substitutes its own, and asserts the digests equal wherever the policy text
does not name the directory.
"""
import argparse
import json
import os
import sys
import tempfile
import types
from pathlib import Path

sys.path.insert(0, "packages/codex-session-relay")

from codex_session_relay import rolepolicy  # noqa: E402
from codex_session_relay.cli import cmd_register, cmd_settings_show  # noqa: E402
from codex_session_relay.clock import FakeClock  # noqa: E402
from codex_session_relay.delivery import authorized_settings  # noqa: E402
from codex_session_relay.errors import RelayError  # noqa: E402
from codex_session_relay.linkage import ROLE_SCOPE  # noqa: E402
from codex_session_relay.models import Endpoint  # noqa: E402
from codex_session_relay.registry import CLEAR_EXCEPTION, Registry, contract_record, record_settings  # noqa: E402
from codex_session_relay.store import Store  # noqa: E402
from tests.support import CHILD, HOST, PARENT, task_settings  # noqa: E402
from tests.test_rolepolicy import (  # noqa: E402
    CHILD_EFFORT, CHILD_MODEL, INTERIM_PARENT, PARENT_EFFORT, PARENT_MODEL, POLICY, SUPERSEDED_CHILD,
    SUPERSEDED_PARENT,
)

VAR = rolepolicy.ENVIRONMENT_VARIABLE
SUPERSEDED_POLICY = {"roles": {"supervisor": {"expectation": "record"},
                               "parent": {"model": SUPERSEDED_PARENT[0], "reasoningEffort": SUPERSEDED_PARENT[1]},
                               "child": {"model": CHILD_MODEL, "reasoningEffort": CHILD_EFFORT}}}
ONE_TASK = {**POLICY, "exceptions": {"one-task": {"model": "gpt-6-astra", "reasoningEffort": "high",
                                                  "cwd": ["${DIR}"], "role": "parent"}}}
PARENT_PAIR = {"model": PARENT_MODEL, "reasoningEffort": PARENT_EFFORT}
CHILD_PAIR = {"model": CHILD_MODEL, "reasoningEffort": CHILD_EFFORT}
ASTRA = {"model": "gpt-6-astra", "reasoningEffort": "high"}

S = {}


def scenario(name, *steps):
    S[name] = list(steps)


def settings(cwd="/parent", **overrides):
    return {"cwd": cwd, "overrides": overrides}


# ROL-2
scenario("resolve_unset", ["resolve", {}])
scenario("resolve_no_roles", ["policy", {"allowed": [{"model": "anthropic/claude-opus-5", "efforts": ["xhigh"]}]}], ["resolve", {"file": True}])
scenario("resolve_declared", ["policy", POLICY], ["resolve", {"file": True}])
# ROL-3
scenario("check_record_pairs", ["policy", POLICY],
         ["check_record", settings("${DIR}", model=SUPERSEDED_PARENT[0], reasoningEffort=SUPERSEDED_PARENT[1]), "parent"],
         ["check_record", settings("${DIR}", **PARENT_PAIR), "parent"],
         ["check_record", settings("${DIR}", model=INTERIM_PARENT[0], reasoningEffort=INTERIM_PARENT[1]), "parent"],
         ["check_record", settings("${DIR}", model=SUPERSEDED_CHILD[0], reasoningEffort=SUPERSEDED_CHILD[1]), "child"],
         ["check_record", settings("${DIR}", **CHILD_PAIR), "child"])
scenario("gate_stale_for_role", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["raw_settings", PARENT, settings(reasoningEffort=PARENT_EFFORT)], ["gate", PARENT])
# ROL-4
scenario("gate_released_by_re_record", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["raw_settings", PARENT, settings()], ["gate", PARENT],
         ["record", PARENT, settings(**PARENT_PAIR), "user_transition", "parent", None], ["gate", PARENT])
# ROL-5
scenario("gate_unconfigured", ["unset"], ["bind", "parent", "PROJ-1", PARENT], ["raw_settings", PARENT, settings()], ["gate", PARENT])
scenario("gate_partial_policy", ["policy", {"roles": {"parent": PARENT_PAIR}}], ["bind", "child", "ISSUE-9", PARENT],
         ["raw_settings", PARENT, settings()], ["gate", PARENT])
# ROL-6
scenario("gate_unbound", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["record", CHILD, settings("/child"), "creation_result", None, None], ["bound_role", CHILD], ["gate", CHILD])
# ROL-7
scenario("settings_after_binding", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings(), "creation_result", "child", None], ["stored", PARENT])
scenario("binding_after_settings", ["policy", POLICY],
         ["record", CHILD, settings(**PARENT_PAIR), "creation_result", "parent", None],
         ["bind", "child", "ISSUE-9", CHILD], ["bindings", CHILD])
scenario("pair_not_the_bound_roles", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings(reasoningEffort=PARENT_EFFORT), "creation_result", "parent", None])
scenario("matching_role_and_pair", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings(**PARENT_PAIR), "creation_result", "parent", None], ["bound_role", PARENT])
# ROL-8
scenario("two_live_roles", ["policy", POLICY],
         ["sql", "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at)"
                 " VALUES ('b1','parent','project','PROJ-1','01parent-task', 'host-a','active',1,'t','t')"],
         ["sql", "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at)"
                 " VALUES ('b2','child','issue','ISSUE-9','01parent-task', 'host-a','active',1,'t','t')"],
         ["bound_role", PARENT], ["raw_settings", PARENT, settings()], ["gate", PARENT], ["show", PARENT])
scenario("superseded_binding", ["policy", POLICY],
         ["sql", "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, superseded_by, created_at, updated_at)"
                 " VALUES ('b3','child','issue','ISSUE-9','01parent-task', 'host-a','active',1,'b4','t','t')"],
         ["bound_role", PARENT])
# ROL-9
scenario("re_record_keeps_cited_role", ["policy", POLICY],
         ["record", PARENT, settings(**PARENT_PAIR), "creation_result", "parent", None],
         ["record", PARENT, settings(**PARENT_PAIR), "user_transition", None, None],
         ["stored", PARENT], ["bind", "supervisor", "INIT-1", PARENT])
# ROL-11
scenario("settings_free_supervisor", ["policy", POLICY], ["bind", "supervisor", "INIT-1", PARENT],
         ["raw_settings", PARENT, settings()], ["gate", PARENT],
         ["record", CHILD, settings("/child", **CHILD_PAIR), "creation_result", "child", None],
         ["bind", "child", "ISSUE-77", CHILD], ["gate", CHILD])
scenario("loaded_as_something_else", ["loaded_mismatch"])
# ROL-12
scenario("unloaded_guard", ["policy", POLICY],
         ["unloaded", settings(**PARENT_PAIR), "parent"],
         ["unloaded", settings(model=SUPERSEDED_PARENT[0], reasoningEffort=PARENT_EFFORT), "parent"],
         ["unloaded", settings(model=PARENT_MODEL, reasoningEffort=SUPERSEDED_PARENT[1]), "parent"])
# ROL-13
scenario("legacy_unauthorized_citation", ["policy", POLICY], ["bind", "supervisor", "INIT-1", PARENT],
         ["raw_settings", PARENT, settings("${DIR}", citedException="not-written", **PARENT_PAIR)], ["gate", PARENT])
# ROL-14
scenario("exceptions", ["policy", ONE_TASK], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings("/somewhere-else", **ASTRA), "creation_result", "parent", "one-task"],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", "parent", "not-written-by-anyone"],
         ["record", PARENT, settings("${DIR}", **PARENT_PAIR), "creation_result", "parent", "not-written"],
         ["record", PARENT, settings("${DIR}", **PARENT_PAIR), "creation_result", "parent", None],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", "parent", "one-task"])
scenario("exception_for_undeclared_role",
         ["policy", {"roles": {"parent": PARENT_PAIR}, "exceptions": {"for-a-child": {**ASTRA, "cwd": ["${DIR}"], "role": "child"}}}],
         ["check_record", settings("${DIR}", citedException="for-a-child", **ASTRA), "child"],
         ["check_binding", "child", "child", settings("${DIR}", citedException="for-a-child", **ASTRA)],
         ["check_record", settings("${DIR}"), "child"])
# ROL-15
scenario("exception_equal_to_the_role_pair",
         ["policy", {**POLICY, "exceptions": {"same-pair": {**PARENT_PAIR, "cwd": ["${DIR}"], "role": "parent"}}}],
         ["unloaded", settings("${DIR}", citedException="same-pair", **PARENT_PAIR), "parent"],
         ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings("${DIR}", **PARENT_PAIR), "creation_result", "parent", "same-pair"],
         ["record", PARENT, settings("${DIR}", **PARENT_PAIR), "user_transition", None, None],
         ["stored", PARENT], ["unloaded_stored", PARENT, "parent"])
# ROL-16
scenario("citation_carry_forward", ["policy", ONE_TASK], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", "parent", "one-task"],
         ["record", PARENT, settings("${DIR}", **ASTRA), "user_transition", None, None], ["stored", PARENT],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", None, None], ["stored", PARENT])
scenario("supervisor_user_transition_drops",
         ["bind", "supervisor", "INIT-1", PARENT],
         ["policy", {**POLICY, "exceptions": {"temporary": {**ASTRA, "cwd": ["${DIR}"], "role": "supervisor"}}}],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", "supervisor", "temporary"],
         ["record", PARENT, settings("${DIR}", model="gpt-6-astra", reasoningEffort="max"), "user_transition", None, None],
         ["stored", PARENT])
scenario("explicit_clear",
         ["bind", "supervisor", "INIT-2", PARENT],
         ["policy", {**POLICY, "exceptions": {"temporary": {**ASTRA, "cwd": ["${DIR}"], "role": "supervisor"}}}],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", "supervisor", "temporary"],
         ["policy", POLICY],
         ["record", PARENT, settings("${DIR}", **ASTRA), "user_transition", None, "__CLEAR__"], ["stored", PARENT])
scenario("sentinel_named_exception", ["bind", "parent", "PROJ-1", PARENT],
         ["policy", {**POLICY, "exceptions": {"__clear__": {**ASTRA, "cwd": ["${DIR}"], "role": "parent"}}}],
         ["record", PARENT, settings("${DIR}", **ASTRA), "creation_result", "parent", "__clear__"], ["stored", PARENT],
         ["record", PARENT, settings("${DIR}", **ASTRA), "user_transition", None, None], ["stored", PARENT])
# ROL-17 and ROL-21
scenario("register_refuses_parent_citation_for_child", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["register_cli", {"project": "PROJ-1", "child_settings": settings("${ROOT}", **PARENT_PAIR), "child_role": "parent"}],
         ["count", "SELECT relationship_id FROM relationships"], ["bound_role", CHILD], ["stored", CHILD])
scenario("register_refuses_incomplete_settings", ["policy", POLICY],
         ["register_cli", {"child_settings_raw": {"model": "anthropic/claude-opus-5"}}],
         ["count", "SELECT relationship_id FROM relationships"])
# ROL-18
scenario("replay_survives_policy_edit", ["policy", POLICY],
         ["record", PARENT, settings(**PARENT_PAIR), "creation_result", "parent", None],
         ["bind", "parent", "PROJ-1", PARENT], ["policy", SUPERSEDED_POLICY], ["bind", "parent", "PROJ-1", PARENT], ["bound_role", PARENT])
scenario("new_binding_refused_under_policy", ["policy", POLICY],
         ["record", PARENT, settings(**CHILD_PAIR), "creation_result", "child", None],
         ["bind", "parent", "PROJ-9", PARENT], ["bound_role", PARENT])
# ROL-19
scenario("settings_show_deliverable", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings(**PARENT_PAIR), "creation_result", "parent", None], ["show", PARENT],
         ["policy", SUPERSEDED_POLICY], ["show", PARENT])
scenario("settings_show_unresolved", ["unset"], ["bind", "parent", "PROJ-1", PARENT],
         ["record", PARENT, settings(**PARENT_PAIR), "creation_result", None, None], ["show", PARENT])
# ROL-22
scenario("archive_never_blocked", ["policy", POLICY], ["bind", "parent", "PROJ-1", PARENT],
         ["register", "PROJ-1"], ["raw_settings", CHILD, settings("/child", citedRole="supervisor")], ["status", "archived"])


def build(spec, directory, root):
    cwd = spec["cwd"].replace("${DIR}", directory).replace("${ROOT}", root)
    return task_settings(cwd, **spec["overrides"])


def answer(call):
    try:
        return {"ok": call()}
    except RelayError as error:
        return {"refused": {"reason": error.reason.value if error.reason else None, "detail": error.detail}}


def refusal_value(refusal):
    if refusal is None:
        return None
    return {"refused": {"reason": refusal.reason.value, "detail": refusal.detail}}


def bound(value):
    if isinstance(value, rolepolicy.Contested):
        return {"contested": value.roles}
    return value


def run(steps):
    directory = str(Path(tempfile.mkdtemp(dir=os.environ["TMPDIR"])).resolve())
    root = os.path.join(directory, "work")
    os.makedirs(root)
    clock = FakeClock()
    store = Store(os.path.join(directory, "state", "relay.sqlite3"))
    registry = Registry(store, clock)
    rolepolicy.reset()
    os.environ.pop(VAR, None)
    out = []
    digest = None
    rid = None
    for step in steps:
        op, args = step[0], step[1:]
        result = None
        if op == "policy":
            text = json.dumps(args[0])
            path = os.path.join(directory, "execution-policy.json")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(text.replace("${DIR}", directory))
            os.environ[VAR] = path
            rolepolicy.reset()
            digest = rolepolicy.declared().digest
            # The digest split in two, so the ${DIGEST} substitution below leaves it readable.
            result = {"text": text, "digest": digest,
                      "digestParts": [digest[:8], digest[8:]] if digest else None}
        elif op == "unset":
            os.environ.pop(VAR, None)
            rolepolicy.reset()
            rolepolicy.declared()
        elif op == "resolve":
            env = {VAR: os.environ[VAR]} if args[0].get("file") else {}
            resolved = rolepolicy.declared(env)
            result = {"declared": bool(resolved), "detail": getattr(resolved, "detail", None),
                      "summary": resolved.summary(),
                      "expectations": {role: (lambda e: None if e is None else
                                              {"expectation": e.expectation, "model": e.model,
                                               "reasoningEffort": e.reasoning_effort})(resolved.expectation(role))
                                       for role in ("supervisor", "parent", "child")} if resolved else None}
        elif op == "bind":
            role, key, task = args
            result = answer(lambda: registry.linkage.bind_scope(
                role=role, scope_key=key, endpoint=Endpoint(task, "host-a", cwd="/parent", cxc_session="cxc-parent")))
        elif op == "record":
            task, spec, source, role, exception = args
            if exception == "__CLEAR__":
                exception = CLEAR_EXCEPTION
            result = answer(lambda: record_settings(store, clock, task, build(spec, directory, root), source=source,
                                                    role=role, exception=exception))
        elif op == "raw_settings":
            task, spec = args
            store.db.execute("INSERT OR REPLACE INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)",
                             (task, json.dumps(build(spec, directory, root)), "test-raw", clock.iso()))
        elif op == "sql":
            store.db.execute(args[0])
        elif op == "gate":
            def gate():
                s = authorized_settings(store, args[0])
                return {"settings": s.data, "settingsFree": s.settings_free_resume}
            result = answer(gate)
        elif op == "show":
            services = types.SimpleNamespace(registry=registry, store=store, clock=clock)
            result = answer(lambda: cmd_settings_show(services, argparse.Namespace(task=args[0])))
        elif op == "stored":
            row = store.one("SELECT settings FROM authorized_settings WHERE task_id = ?", (args[0],))
            result = None if row is None else json.loads(row["settings"])
        elif op == "bound_role":
            result = bound(rolepolicy.bound_role(store, args[0]))
        elif op == "bindings":
            result = [dict(r) for r in store.all("SELECT binding_id FROM scope_bindings WHERE task_id = ?", (args[0],))]
        elif op == "check_record":
            result = rolepolicy.check_record(build(args[0], directory, root), args[1], rolepolicy.declared())
        elif op == "check_binding":
            result = rolepolicy.check_binding(args[0], args[1], build(args[2], directory, root), rolepolicy.declared())
        elif op == "unloaded":
            result = refusal_value(rolepolicy.check_unloaded_transmission(
                build(args[0], directory, root), args[1], rolepolicy.declared(), "notLoaded"))
        elif op == "unloaded_stored":
            row = store.one("SELECT settings FROM authorized_settings WHERE task_id = ?", (args[0],))
            result = refusal_value(rolepolicy.check_unloaded_transmission(
                json.loads(row["settings"]), args[1], rolepolicy.declared(), "notLoaded"))
        elif op == "loaded_mismatch":
            from codex_session_relay.settings import SETTINGS_DIFFER_AFTER_LOAD, SETTINGS_NOT_PRESERVED, TaskSettings
            record = task_settings("/parent")
            loaded = {key: record[key] for key in ("sandbox", "cwd", "runtimeWorkspaceRoots", "reasoningEffort")}
            response = dict(loaded, approvalPolicy="never", model="someone/else-entirely", activePermissionProfile=None,
                            thread={"id": PARENT, "environments": record["environments"]})
            findings = TaskSettings(record).mismatches(response, transmitted=False)
            first = findings[0]
            code = SETTINGS_DIFFER_AFTER_LOAD if first["code"] == SETTINGS_NOT_PRESERVED else first["code"]
            result = {"response": response, "findings": findings, "code": code}
        elif op == "register_cli":
            spec = args[0]
            child_settings = (json.dumps(spec["child_settings_raw"]) if "child_settings_raw" in spec
                              else json.dumps(build(spec["child_settings"], directory, root)))
            namespace = argparse.Namespace(
                parent_task=PARENT, parent_host="host-a", parent_cwd="/parent", parent_cxc_session="cxc-parent",
                child_task=CHILD, child_host="host-a", child_cwd=root, child_cxc_session="cxc-child",
                issue="ISS-1", artifact_root=[root], allowed_recipient=[PARENT], scope_ref=None,
                dispatch_request_id="dispatch-1", dispatch_turn_id=None, supersedes=None,
                project=spec.get("project"), parent_settings=None, parent_role=None, parent_exception=None,
                child_settings=child_settings, child_role=spec.get("child_role"), child_exception=None)
            services = types.SimpleNamespace(registry=registry, store=store, clock=clock)
            try:
                result = answer(lambda: cmd_register(services, namespace))
            except Exception as error:  # noqa: BLE001 - recorded as Python raised it
                result = {"host": f"{type(error).__name__}: {error}"}
        elif op == "count":
            result = len(store.all(args[0]))
        elif op == "register":
            def register():
                return contract_record(registry.register(
                    parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
                    child=Endpoint(CHILD, HOST, cwd=root, cxc_session="cxc-child"),
                    issue_key="REL-1", artifact_roots=[root], allowed_recipients=[PARENT],
                    dispatch_request_id="dispatch-1", dispatch_turn_id="turn-dispatch-1", project_key=args[0]))
            result = answer(register)
            rid = result.get("ok", {}).get("relationshipId")
        elif op == "status":
            result = answer(lambda: contract_record(registry.set_status(rid, args[0], actor="test")))
        else:
            raise ValueError(op)
        text = json.dumps(result).replace(root, "${ROOT}").replace(directory, "${DIR}")
        if digest:
            text = text.replace(digest, "${DIGEST}")
        out.append({"step": step, "result": json.loads(text)})
    store.close()
    os.environ.pop(VAR, None)
    rolepolicy.reset()
    return out


result = {"__roles__": [{"step": ["roles"], "result": sorted(ROLE_SCOPE)}]}
for name, steps in S.items():
    result[name] = run(steps)
json.dump(result, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
