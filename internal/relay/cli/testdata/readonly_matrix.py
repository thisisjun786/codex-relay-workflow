"""Python half of readonly_test.go: the populated store of tests/test_fence_readonly.py with
its READ_FORMS, and the ownership stamps the matrix needs.

  fixture <dir>                                  build the store under <dir>; print {db, forms}
  stamp <db> <owner> <phase> <epoch> <takeover>  restamp schema_meta and takeover.json
  guard <base> <db>                              a ready_for_review Stop whose receipt lookup
                                                 reads <db>; print {root, stop, now}
"""
import json
import sqlite3
import sys
import tempfile
from pathlib import Path


def fixture(base):
    Path(base).mkdir(parents=True, exist_ok=True)
    tempfile.tempdir = base
    from codex_session_relay import faults
    from codex_session_relay.sync import COORDINATION_DOCUMENT, SyncOutbox
    from tests.test_fence_readonly import READ_FORMS
    from tests.test_supervisor_channel import ChannelTestCase

    case = ChannelTestCase()
    case.setUp()
    event = case.completed()
    staged = case.channel.stage(case.obligation(event))
    sync = SyncOutbox(case.store, case.clock)
    sync.set_target(case.rid, COORDINATION_DOCUMENT, "document")
    with case.store.transaction() as db:
        sync_id = sync.enqueue_in(db, relationship_id=case.rid, issue_key="REL-1",
                                  subject_kind="progress", summary="fixture", event_id=event)
        # Housekeeping a writer persists and a read-only reader only projects: a lapsed
        # reservation, a live one, a blocking notice of a withdrawn fault, and a lapsed claim.
        for fault, state in (("fault-open", "open"), ("fault-withdrawn", faults.WITHDRAWN)):
            db.execute("INSERT INTO fault_ledger (fault_id,product,fault_class,component,severity,"
                       "signature,scope,scope_key,state,first_seen_at,last_seen_at,updated_at)"
                       " VALUES (?,'example','delivery_refused','relay','serious','sig','{}','k',?,"
                       "'then','then','then')", (fault, state))
        for ident, fault, kind, state, lease in (
                ("lapsed", "fault-open", faults.DECISION, faults.RESERVED, 0),
                ("live", "fault-open", faults.DECISION, faults.RESERVED, 9_999_999_999),
                ("voided", "fault-withdrawn", faults.BLOCKING, faults.PENDING, None)):
            db.execute("INSERT INTO fault_notifications (notification_id,fault_id,product,kind,cycle,"
                       "state,lease_until,created_at,updated_at) VALUES (?,?,'example',?,1,?,?,"
                       "'then','then')", (ident, fault, kind, state, lease))
        db.execute("INSERT INTO fault_publications (publication_id,fault_id,kind,trigger_key,summary,"
                   "identity_digest,state,claim_token,lease_owner,lease_until,created_at,updated_at)"
                   " VALUES ('claim-lapsed','fault-open','append_comment','t','s','d','claimed',"
                   "'token','worker',0,'then','then')")
    root = Path(case.tmp)
    packet, record, stop = root / "packet.json", root / "record.json", root / "stop.json"
    packet.write_text(case.channel.get(staged["messageId"])["packet"])
    record.write_text("{}")
    stop.write_text("{}")
    values = {"event": event, "rid": case.rid, "message": staged["messageId"],
              "packet": str(packet), "record": str(record), "stop": str(stop),
              "markers": str(root / "markers"), "workspace": case.root, "assignment": "a" * 64,
              "now": case.clock.iso(), "sync": sync_id}
    case.store.close()
    forms = [[command, [option.format(**values) for option in options]]
             for command, options in READ_FORMS.items()]
    print(json.dumps({"db": str(case.store.path), "forms": forms}))


def stamp(db, owner, phase, epoch, takeover):
    from codex_session_relay import inbox, ownership

    path, epoch = Path(db), int(epoch)
    connection = sqlite3.connect(path)
    with connection:
        for key, value in (("owner", owner), ("owner_epoch", str(epoch)), ("takeover_id", takeover)):
            connection.execute("UPDATE schema_meta SET value=? WHERE key=?", (value, key))
    connection.close()
    record = json.loads((path.parent / "takeover.json").read_bytes())
    other = "go" if owner == "python" else "python"
    transition = None
    if phase == "draining":
        transition = {"id": "t-drain", "from": owner, "to": other, "targetEpoch": epoch + 1}
    elif takeover:
        transition = {"id": takeover, "from": other, "to": owner, "targetEpoch": epoch}
    record.update(owner=owner, epoch=epoch, phase=phase, transition=transition,
                  database=ownership.physical(path), relayRPCSocket=str(path.parent / "control.sock"))
    if phase == "starting":
        record["controller"] = {"bootId": "boot", "pid": 1, "startTicks": 1}
    (path.parent / "takeover.json").write_bytes(inbox.canonical(record))


def guard(base, db):
    import contextlib

    from codex_session_relay import intent, marker
    from codex_session_relay.clock import FakeClock
    from codex_session_relay.models import Endpoint
    from codex_session_relay.registry import Registry
    from codex_session_relay.store import Store

    base, now = Path(base), "2026-01-01T00:06:00+00:00"
    work, root = base / "work", base / "markers"
    work.mkdir(parents=True)
    with contextlib.closing(Store(db)) as store:
        relation = Registry(store, FakeClock()).register(
            parent=Endpoint("parent", "host"), child=Endpoint("child", "host", cwd=str(work)),
            issue_key="T-1", artifact_roots=[str(work)], allowed_recipients=["parent"],
            dispatch_request_id="dispatch", dispatch_turn_id="turn")
    assignment = marker.assignment_id("dispatch")
    intent.declare_intent(root, workspace=work, dispatch_request_id="dispatch", issue_key="T-1",
                          declared_at=now, db_path=db)
    intent.publish_claim(root, workspace=work, assignment=assignment, session_id="child",
                         dispatch_request_id="dispatch", first_turn_id="turn", at=now)
    intent.bind(root, workspace=work, assignment=assignment, session_id="child", task_id="child", at=now)
    intent.register_relationship(root, workspace=work, assignment=assignment,
                                 relationship_id=relation["relationshipId"],
                                 dispatch_request_id="dispatch", db_path=db, at=now)
    intent.publish_disposition(root, workspace=work, assignment=assignment, session_id="child",
                               turn_id="turn", outcome="ready_for_review", at=now)
    stop = base / "stop.json"
    stop.write_text(json.dumps({"cwd": str(work), "session_id": "child", "turn_id": "turn",
                                "stop_hook_active": False}))
    print(json.dumps({"root": str(root), "stop": str(stop), "now": now}))


if __name__ == "__main__":
    {"fixture": fixture, "stamp": stamp, "guard": guard}[sys.argv[1]](*sys.argv[2:])
