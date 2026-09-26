"""Record the Python registry's caller-visible answers for internal/relay/registry parity tests.

Run from the repository root with HOME/XDG_*/CODEX_HOME/TMPDIR in a scratch directory:
  uv run --no-sync python internal/relay/registry/testdata/gen_python.py \
      > internal/relay/registry/testdata/python_registry.json
Every scenario uses FakeClock(1_700_000_000) and literal roots, so no value depends on the run.
Each scenario is a list of steps: {"ok": <json>} or {"refused": {"reason", "detail"}}.
"""
import json
import os
import sys
import tempfile

from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RelayError
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry, contract_record, record_settings
from codex_session_relay.store import Store

sys.path.insert(0, "packages/codex-session-relay")
from tests.support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, task_settings  # noqa: E402

ROOT = "/work"
SCENARIOS = {}


def scenario(fn):
    SCENARIOS[fn.__name__] = fn
    return fn


def step(call):
    try:
        value = call()
    except RelayError as error:
        return {"refused": {"reason": error.reason.value if error.reason else None,
                            "detail": error.detail}}
    return {"ok": value}


def fresh():
    directory = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
    store = Store(os.path.join(directory, "relay.sqlite3"))
    return store, Registry(store, FakeClock())


def register(registry, **kw):
    return registry.register(
        parent=kw.pop("parent", Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent")),
        child=kw.pop("child", Endpoint(CHILD, HOST, cwd=ROOT, cxc_session="cxc-child")),
        issue_key=kw.pop("issue_key", ISSUE),
        artifact_roots=kw.pop("roots", [ROOT]),
        allowed_recipients=kw.pop("recipients", [PARENT]),
        dispatch_request_id=kw.pop("dispatch_request_id", "dispatch-1"),
        dispatch_turn_id=kw.pop("dispatch_turn_id", DISPATCH_TURN),
        **kw,
    )


def full(record):
    return dict(record)


@scenario
def identity(s, r):
    first = register(r)
    return [step(lambda: full(first)), step(lambda: contract_record(first)),
            step(lambda: contract_record(register(r))),
            step(lambda: contract_record(register(r, roots=["/somewhere/else"]))),
            step(lambda: r.get("rel-0000000000000000"))]


@scenario
def generations(s, r):
    rid = register(r)["relationshipId"]
    return [step(lambda: r.open_generation(rid, dispatch_request_id="dispatch-2", reason="needs_changes_revision")),
            step(lambda: r.open_generation(rid, dispatch_request_id="dispatch-2", reason="needs_changes_revision")),
            step(lambda: r.open_generation(rid, dispatch_request_id="d3", reason="needs_changes_revision", dispatch_turn_id="t3")),
            step(lambda: r.open_generation(rid, dispatch_request_id="d4", reason="bogus")),
            step(lambda: r.open_generation(rid, dispatch_request_id="d4", reason="needs_changes_revision", dispatch_turn_id=" ")),
            step(lambda: contract_record(r.get(rid)))]


@scenario
def anchors(s, r):
    rid = register(r, dispatch_turn_id=None)["relationshipId"]
    return [step(lambda: r.generation(rid, 1)),
            step(lambda: r.bind_anchor(rid, 1, dispatch_turn_id="t", source="latest_turn")),
            step(lambda: r.bind_anchor(rid, 1, dispatch_turn_id="", source="dispatch_receipt")),
            step(lambda: r.bind_anchor(rid, 1, dispatch_turn_id="turn-exact", source="dispatch_receipt")),
            step(lambda: r.bind_anchor(rid, 1, dispatch_turn_id="turn-exact", source="dispatch_receipt")),
            step(lambda: r.bind_anchor(rid, 1, dispatch_turn_id="some-other-turn", source="dispatch_receipt")),
            step(lambda: r.bind_anchor(rid, 7, dispatch_turn_id="x", source="dispatch_receipt"))]


@scenario
def lifecycle(s, r):
    rid = register(r)["relationshipId"]
    out = []
    for status in ("paused", "cancelled", "archived"):
        out.append(step(lambda: contract_record(r.set_status(rid, status, actor="test"))))
        out.append(step(lambda: r.require_active(rid)))
        out.append(step(lambda: r.open_generation(rid, dispatch_request_id="after", reason="needs_changes_revision", dispatch_turn_id="ta")))
        out.append(step(lambda: contract_record(r.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="test"))))
    out.append(step(lambda: r.open_generation(rid, dispatch_request_id="after", reason="needs_changes_revision", dispatch_turn_id="ta")))
    out.append(step(lambda: contract_record(r.set_status(rid, "paused", actor="t"))))
    out.append(step(lambda: r.set_status(rid, "active", actor="t")))
    out.append(step(lambda: contract_record(r.set_status(rid, "cancelled", actor="t"))))
    out.append(step(lambda: r.set_status(rid, "paused", actor="t")))
    return out


@scenario
def resume_restatement(s, r):
    rid = register(r, recipients=[PARENT, CHILD])["relationshipId"]
    r.set_status(rid, "paused", actor="test")
    return [step(lambda: r.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="t")),
            step(lambda: r.resume(rid, expect_generation=99, expect_artifact_roots=["/elsewhere"], expect_allowed_recipients=[CHILD], actor="t")),
            step(lambda: r.resume("rel-0000000000000000", expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="t")),
            step(lambda: contract_record(r.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT, CHILD], actor="t")))]


@scenario
def supersession(s, r):
    original = register(r)
    replacement = r.register(parent=Endpoint("01new-parent", HOST), child=Endpoint(CHILD, HOST), issue_key=ISSUE,
                             artifact_roots=[ROOT], allowed_recipients=["01new-parent"],
                             dispatch_request_id="dispatch-replacement", dispatch_turn_id="turn-replacement",
                             supersedes=original["relationshipId"])
    rid = original["relationshipId"]
    return [step(lambda: full(r.get(rid))), step(lambda: contract_record(replacement)),
            step(lambda: r.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="t"))]


@scenario
def superseded_resume(s, r):
    rid = register(r)["relationshipId"]
    r.supersede(rid, new_relationship_id="rel-newnewnewnew00")
    return [step(lambda: r.resume(rid, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="t"))]


def replacement(r, child="different-child"):
    return r.register(parent=Endpoint(PARENT, HOST), child=Endpoint(child, HOST), issue_key=ISSUE,
                      artifact_roots=[ROOT], allowed_recipients=[PARENT],
                      dispatch_request_id="replacement-after-cancel", dispatch_turn_id="new-child-turn")


@scenario
def resume_ownership(s, r):
    original = register(r)["relationshipId"]
    r.set_status(original, "cancelled", actor="authorized cancel")
    other = replacement(r)["relationshipId"]
    out = [step(lambda: r.resume(original, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="old"))]
    r.set_status(other, "paused", actor="user pause")
    out.append(step(lambda: r.resume(original, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="old")))
    out.append(step(lambda: register(r, child=Endpoint("third-child", HOST), dispatch_request_id="d9")))
    r.supersede(other, new_relationship_id="rel-0000000000000000")
    out.append(step(lambda: contract_record(r.resume(original, expect_generation=1, expect_artifact_roots=[ROOT], expect_allowed_recipients=[PARENT], actor="ok"))))
    return out


@scenario
def returning_tenure(s, r):
    rid = register(r)["relationshipId"]
    r.set_status(rid, "cancelled", actor="t")
    return [step(lambda: register(r)),
            step(lambda: contract_record(replacement(r)))]


@scenario
def settings_records(s, r):
    parent = task_settings("/parent")
    out = [step(lambda: record_settings(s, r.clock, PARENT, parent, source="creation_result"))]
    out.append(step(lambda: record_settings(s, r.clock, PARENT, dict(parent, reasoningEffort="high"), source="user_transition", role="parent")))
    out.append(step(lambda: record_settings(s, r.clock, PARENT, parent, source="x", role="child")))
    out.append(step(lambda: s.all("SELECT task_id, settings, source FROM authorized_settings")))
    return out


def main():
    out = {}
    for name, fn in SCENARIOS.items():
        store, registry = fresh()
        try:
            result = fn(store, registry)
        finally:
            store.close()
        out[name] = json.loads(json.dumps(result, default=lambda row: [row[k] for k in row.keys()]))
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
