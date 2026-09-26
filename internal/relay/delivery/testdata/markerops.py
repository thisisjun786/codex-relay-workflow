"""Drives the real Python marker and intent modules through a JSON list of operations.

argv: <tree>. stdin: {"ops": [...], "env": {...}}. Every operation's answer is printed in order as
one JSON list: {"ok": value} for a return, {"reason", "detail"} for a RelayError, {"error": ...}
for any other exception. The Go test runs the same list through its own port over the same tree
and compares the whole list. Paths are relative to the tree: markers/ is the marker root, work/
the workspace, state/relay.sqlite3 the relay store.
"""
import json
import os
import sqlite3
import sys
from pathlib import Path

TREE = Path(sys.argv[1])
spec = json.load(sys.stdin)
for key, value in (spec.get("env") or {}).items():
    if value is None:
        os.environ.pop(key, None)
    else:
        os.environ[key] = value.replace("<tree>", str(TREE))

from codex_session_relay import intent, marker  # noqa: E402
from codex_session_relay.errors import RelayError  # noqa: E402
from codex_session_relay.store import Store  # noqa: E402

ROOT = TREE / "markers"
WORK = TREE / "work"
WORK.mkdir(parents=True, exist_ok=True)
DB = TREE / "state" / "relay.sqlite3"
store = None
held = None


def the_store():
    global store
    if store is None:
        store = Store(str(DB))
    return store


def path(value):
    return str(value).replace("<tree>", str(TREE)) if isinstance(value, str) else value


def assignment(op):
    if "assignment" in op:
        return op["assignment"]
    return marker.assignment_id(op.get("dispatch", "dispatch-request-1"))


def adir(op):
    return marker.assignment_dir(ROOT, WORK, marker.assignment_id(op.get("dispatch", "dispatch-request-1")))


def facts(op):
    found, unreadable = marker.read_assignment(adir(op))
    return found, unreadable


def run(op):
    global held
    kind = op["op"]
    if kind == "declare":
        kw = {k: op[k] for k in ("criteria_source", "baseline_revision", "authorized_settings") if k in op}
        if "db_path" in op:
            kw["db_path"] = path(op["db_path"])
        return intent.declare_intent(ROOT, workspace=Path(path(op.get("workspace", str(WORK)))),
                                     dispatch_request_id=op.get("dispatch", "dispatch-request-1"),
                                     issue_key=op.get("issue_key", "REL-1"),
                                     declared_at=op.get("declared_at", "2026-01-01T00:00:00+00:00"), **kw)
    if kind == "attempt":
        return intent.record_attempt(ROOT, workspace=WORK, assignment=assignment(op), outcome=op["outcome"],
                                     task_id=op.get("task_id"), at=op.get("at", "2026-01-01T00:00:00+00:00"))
    if kind == "claim":
        return intent.publish_claim(ROOT, workspace=WORK, assignment=assignment(op), session_id=op["session"],
                                    dispatch_request_id=op.get("claim_dispatch", "dispatch-request-1"),
                                    first_turn_id="turn-1", at=op.get("at", "2026-01-01T00:00:00+00:00"))
    if kind == "bind":
        return intent.bind(ROOT, workspace=WORK, assignment=assignment(op), session_id=op["session"],
                           task_id=op["task"], at=op.get("at", "2026-01-01T00:00:00+00:00"))
    if kind == "open_generation":
        s = the_store()
        rid, generation, current = op["relationship_id"], op.get("generation", 1), op.get("current")
        s.db.execute(
            "INSERT OR IGNORE INTO relationships (relationship_id, issue_key, status, parent_task_id,"
            " parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots,"
            " allowed_recipients, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (rid, "REL-1", "active", "01parent-task", "host-a", "01child-task", "host-a",
             generation if current is None else current, "[]", "[]", "2026-01-01T00:00:00+00:00",
             "2026-01-01T00:00:00+00:00"))
        if current is not None:
            s.db.execute("UPDATE relationships SET execution_generation = ? WHERE relationship_id = ?",
                         (current, rid))
        s.db.execute(
            "INSERT OR IGNORE INTO generations (relationship_id, execution_generation, dispatch_request_id,"
            " anchor_state, opened_at) VALUES (?,?,?,?,?)",
            (rid, generation, op.get("generation_dispatch", "dispatch-request-1"), "bound",
             "2026-01-01T00:00:00+00:00"))
        return None
    if kind == "register":
        db_path = path(op.get("db_path", "<tree>/state/relay.sqlite3"))
        return intent.register_relationship(ROOT, workspace=WORK, assignment=assignment(op),
                                            relationship_id=op["relationship_id"],
                                            dispatch_request_id=op.get("register_dispatch", "dispatch-request-1"),
                                            at="2026-01-01T00:00:00+00:00", db_path=db_path)
    if kind == "hold_begin":
        the_store().db.execute("BEGIN IMMEDIATE")
        return None
    if kind == "hold_end":
        the_store().db.execute("ROLLBACK")
        return None
    if kind == "relay_tables":
        s = the_store()
        return {"relationships": [dict(r) for r in s.all("SELECT * FROM relationships")],
                "generations": [dict(r) for r in s.all("SELECT * FROM generations")],
                "journal": s.one("SELECT COUNT(*) AS n FROM journal")["n"]}
    if kind == "resolution":
        found, _ = facts(op)
        entries = []
        for fact_id in op.get("facts", []):
            fact = next(f for f in (found.get("attempts", []) + found.get("claims", [])
                                    + found.get("conflicts", [])) if f["factId"] == fact_id)
            digest = "0" * 64 if op.get("digest") == "zero" else marker.fact_digest(fact)
            entries.append({"factId": fact_id, "digest": digest})
        return intent.publish_resolution(ROOT, workspace=WORK, assignment=assignment(op),
                                         chosen_task_id=op.get("task"), chosen_session_id=op.get("session"),
                                         reason=op.get("reason", "r"), at="2026-01-01T00:05:00+00:00",
                                         adjudicated=entries)
    if kind == "publish":
        target = adir(op) / op["path"] if "path" in op else Path(path(op["target"]))
        return marker.publish(target, op["payload"], root=ROOT if op.get("confined") else None)
    if kind == "write_raw":
        target = adir(op) / op["path"] if "path" in op else Path(path(op["target"]))
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(op["text"], encoding="utf-8")
        return None
    if kind == "unlink":
        (adir(op) / op["path"]).unlink()
        return None
    if kind == "exists":
        return (TREE / op["path"]).exists() if "tree_path" not in op else (TREE / op["tree_path"]).exists()
    if kind == "exists_in":
        return (adir(op) / op["path"]).exists()
    if kind == "listdir":
        target = Path(path(op["target"]))
        return sorted(p.name for p in target.iterdir())
    if kind == "read_file":
        return json.loads(Path(path(op["target"])).read_text(encoding="utf-8"))
    if kind == "facts":
        found, unreadable = facts(op)
        return {"facts": found, "unreadable": unreadable}
    if kind == "state":
        return intent.derive_assignment_state(facts(op)[0], op.get("now", "2026-01-01T00:05:00+00:00"))
    if kind == "malformed":
        return intent.malformed(facts(op)[0])
    if kind == "counters":
        return intent.malformed_counters(op["value"])
    if kind == "contested":
        return intent.identity_contested(facts(op)[0])
    if kind == "covered":
        found = facts(op)[0]
        fact = next(f for f in found.get("claims", []) if f["factId"] == op["fact"])
        return intent.covered(fact, intent._resolutions(found))
    if kind == "claimant":
        found = facts(op)[0]
        return intent.claimant(next(f for f in found.get("claims", []) if f["factId"] == op["fact"]))
    if kind == "correlated":
        return intent.correlated(facts(op)[0], op["session"])
    if kind == "select":
        workspace = Path(path(op.get("workspace", str(WORK))))
        found, got, unreadable = intent.select_assignment(ROOT, workspace, op["session"])
        return {"assignment": found.name if found is not None else None,
                "facts": got, "unreadable": unreadable}
    if kind == "disposition":
        return intent.publish_disposition(ROOT, workspace=WORK, assignment=assignment(op),
                                          session_id=op["session"], turn_id=op["turn"],
                                          outcome=op["outcome"], at="2026-01-01T00:00:00+00:00")
    if kind == "read_disposition":
        found, readable = marker.read_disposition(adir(op), op["session"], op["turn"])
        return {"found": found, "readable": readable}
    if kind == "digest":
        return marker.fact_digest(op["payload"])
    if kind == "named":
        return [marker.named(v) for v in op["values"]]
    if kind == "same":
        return [marker.same_identity(a, b) for a, b in op["pairs"]]
    if kind == "workspace_key":
        return marker.workspace_key(Path(path(op["workspace"])))
    if kind == "symlink":
        os.symlink(path(op["to"]), path(op["link"]))
        return None
    if kind == "marker_root":
        chosen = marker.resolve_marker_root(path(op.get("explicit")))
        return chosen.to_record()
    if kind == "set_env":
        if op["value"] is None:
            os.environ.pop(op["name"], None)
        else:
            os.environ[op["name"]] = path(op["value"])
        return None
    raise ValueError("unknown op " + kind)


out = []
for op in spec["ops"]:
    try:
        out.append({"ok": run(op)})
    except RelayError as error:
        out.append({"reason": error.reason.value if error.reason else None, "detail": error.detail})
    except Exception as error:  # noqa: BLE001
        out.append({"error": type(error).__name__ + ": " + str(error)})
if store is not None:
    store.close()
print(json.dumps(out))
