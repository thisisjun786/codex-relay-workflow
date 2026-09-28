"""Owner-independent read matrix and terminal inbox replay regressions."""

import json
import sqlite3
from contextlib import redirect_stdout
from io import StringIO
from pathlib import Path
from unittest import mock

import pytest
from codex_session_relay import cli, faults, inbox
from codex_session_relay.clock import FakeClock
from codex_session_relay.store import Store

from .test_fence import LEGACY_READBACK, files, stamp
from .test_supervisor_channel import PARENT, PROJECT, SUPERVISOR, ChannelTestCase

# Every read command and every conditional read form, not a failure-only shortlist.
READ_FORMS = {
    "ack-proof": ["--event", "{event}", "--turn", "read-turn"],
    "assignment-find": ["--issue", "REL-1"],
    "assignment-show": ["--relationship", "{rid}"],
    "capacity-show": [],
    "criteria-show": ["--relationship", "{rid}"],
    "dispositions-show": ["--relationship", "{rid}"],
    "doctor": [],
    "fault-attention": [],
    "fault-next": [],
    "fault-notifications": [],
    "fault-show": [],
    "guard-evaluate": ["--marker-root", "{markers}", "--stop-input", "{stop}", "--now", "{now}", "--no-record"],
    "intent-show": ["--marker-root", "{markers}", "--workspace", "{workspace}", "--now", "{now}"],
    "linkage-completion": ["--project", PROJECT],
    "linkage-counterpart": ["--from-task", PARENT, "--to-task", SUPERVISOR],
    "linkage-down": ["--scope-kind", "project", "--scope", PROJECT],
    "linkage-outstanding": ["--project", PROJECT],
    "linkage-up": ["--relationship", "{rid}"],
    "managed-show": ["--request-id", "absent"],
    "merge-evidence": ["--repository", "example/project", "--pull-request", "1"],
    "merge-turn-show": ["--parent-task", PARENT],
    "packet-check": ["--packet", "{packet}", "--record", "{record}"],
    "product-show": [],
    "region-show": ["--repository", "example/project"],
    "reporting-derive": ["--relationship", "{rid}"],
    "reporting-show": ["--marker-root", "{markers}", "--workspace", "{workspace}",
                       "--assignment", "{assignment}", "--session", "child", "--turn", "turn"],
    "revision-head": ["--relationship", "{rid}"],
    "route-show": [],
    "settings-show": ["--task", PARENT],
    "show": ["--event", "{event}"],
    "status": [],
    "store-identity": [],
    "supervisor-select": ["--event", "{event}"],
    "supervisor-show": ["--message", "{message}"],
    "supervisor-standing": ["--project", PROJECT],
    "sync-next": [],
    "sync-operation": ["--sync", "{sync}"],
    "sync-status": [],
    "fault-policy": ["--product", "example"],
    "fault-limit": ["--product", "example"],
    "service": ["status"],
    "store-challenge": ["--read", "absent"],
}


@pytest.fixture
def populated_store(tmp_path):
    fixture = ChannelTestCase()
    fixture.setUp()
    try:
        event = fixture.completed()
        staged = fixture.channel.stage(fixture.obligation(event))
        from codex_session_relay.sync import COORDINATION_DOCUMENT, SyncOutbox

        sync = SyncOutbox(fixture.store, fixture.clock)
        sync.set_target(fixture.rid, COORDINATION_DOCUMENT, "document")
        with fixture.store.transaction() as db:
            sync_id = sync.enqueue_in(db, relationship_id=fixture.rid, issue_key="REL-1",
                                      subject_kind="progress", summary="fixture", event_id=event)
        packet = tmp_path / "packet.json"
        packet.write_text(fixture.channel.get(staged["messageId"])["packet"])
        record = tmp_path / "record.json"
        record.write_text("{}")
        stop = tmp_path / "stop.json"
        stop.write_text("{}")
        values = {"event": event, "rid": fixture.rid, "message": staged["messageId"],
                  "packet": str(packet), "record": str(record), "stop": str(stop),
                  "markers": str(tmp_path / "markers"), "workspace": fixture.root,
                  "assignment": "a" * 64, "now": fixture.clock.iso(), "sync": sync_id}
        fixture.store.close()
        yield fixture.store.path, values
    finally:
        fixture.doCleanups()


def invoke(path, command, options):
    output = StringIO()
    with redirect_stdout(output), mock.patch.object(cli, "SystemClock", FakeClock):
        code = cli.main(["--state", str(path.parent), command, *options])
    return code, json.loads(output.getvalue())


def test_read_matrix_names_every_read_form():
    conditional = {"fault-policy", "fault-limit", "service", "store-challenge"}
    assert set(READ_FORMS) == cli.READ_ONLY_COMMANDS | conditional


@pytest.mark.parametrize("command", READ_FORMS)
def test_readonly_matrix_matches_python_owner(populated_store, command):
    path, values = populated_store
    options = [option.format(**values) for option in READ_FORMS[command]]
    # Forge transport is the sole nonlocal dependency. Keep the real CLI handler
    # while providing its completed external read; no store or transaction mocked.
    from codex_session_relay import forge
    from codex_session_relay.control import GuardServer

    with mock.patch.object(forge, "collect", return_value={"verdict": forge.READY}):
        python_code, expected = invoke(path, command, options)
        assert python_code == 0, (command, expected)
        stamp(path, owner="go")
        server = GuardServer(path.parent) if command == "guard-evaluate" else None
        try:
            actual_code, actual = invoke(path, command, options)
        finally:
            if server is not None:
                server.close()
    assert actual_code == 0, (command, actual)
    if command == "doctor":
        # Ownership/access are deliberately different diagnostics, not domain data.
        assert actual["ownership"]["owner"] == "go"
        assert expected["ownership"]["owner"] == "python"
        for field in ("ownership", "access", "actorReachability", "accessReceipt"):
            actual.pop(field)
            expected.pop(field)
    assert actual == expected, command


def test_readonly_sees_committed_live_wal_not_immutable_main(populated_store):
    path, _ = populated_store
    writer = sqlite3.connect(path, isolation_level=None)
    try:
        writer.execute("PRAGMA wal_autocheckpoint=0")
        writer.execute("UPDATE schema_meta SET value='go' WHERE key='owner'")
        # No checkpoint: immutable would ignore the WAL and report stale Python ownership.
        stale = sqlite3.connect(path.as_uri() + "?mode=ro&immutable=1", uri=True)
        try:
            assert stale.execute("SELECT value FROM schema_meta WHERE key='owner'").fetchone()[0] == "python"
        finally:
            stale.close()
        reader = Store(path, read_only=True)
        try:
            with reader.transaction():
                assert reader.meta("owner") == "go"
        finally:
            reader.close()
    finally:
        writer.close()


def test_readonly_transaction_is_deferred_and_cannot_write(populated_store):
    path, _ = populated_store
    stamp(path, owner="go")
    store = Store(path, read_only=True)
    statements = []
    store.db.set_trace_callback(statements.append)
    try:
        with store.transaction() as db:
            assert db.execute("SELECT count(*) FROM relationships").fetchone()[0] == 1
        assert "BEGIN" in statements
        assert "BEGIN IMMEDIATE" not in statements
        with pytest.raises(sqlite3.OperationalError, match="readonly"), store.transaction() as db:
            db.execute("DELETE FROM relationships")
    finally:
        store.close()


def test_notification_projection_matches_housekeeping_without_writing(populated_store):
    path, _ = populated_store
    store = Store(path)
    try:
        with store.transaction() as db:
            for ident, state, lease in (("expired", faults.RESERVED, 0),
                                       ("live", faults.RESERVED, 9_999_999_999)):
                db.execute("INSERT INTO fault_notifications "
                           "(notification_id,fault_id,product,kind,cycle,state,lease_until,created_at,updated_at)"
                           " VALUES (?,?,?, ?,1,?,?,?,?)",
                           (ident, "fault", "example", faults.DECISION, state, lease, "then", "then"))
        clock = FakeClock()
        reader = Store(path, read_only=True)
        try:
            before = reader.db.total_changes
            projected = faults.FaultLedger(reader, clock).notifications(state=faults.UNCERTAIN)
            assert reader.db.total_changes == before
        finally:
            reader.close()
        written = faults.FaultLedger(store, clock).notifications(state=faults.UNCERTAIN)
        assert projected == written
        assert [entry["notificationId"] for entry in projected["notifications"]] == ["expired"]
    finally:
        store.close()


def test_supervisor_read_refuses_queue_and_legacy_replay_does_not_block_writer(populated_store):
    path, _ = populated_store
    stamp(path, owner="go")
    before = files(path.parent)
    code, answer = invoke(path, "supervisor-read", LEGACY_READBACK)
    assert code == 2 and answer["reason"] == "store_owned_by_other"
    assert files(path.parent) == before
    # Already accepted by the preceding fence build: retain/decode its wire bytes.
    legacy = Path(__file__).resolve().parents[3] / "contract/golden/takeover-inbox/supervisor-read.json"
    request = json.loads(legacy.read_bytes())
    inbox.enqueue(path.parent, request)
    stamp(path, owner="python", epoch=3)
    code, answer = invoke(path, "store-challenge", ["--write", "--actor", "after-rollback"])
    assert code == 0 and answer["nonce"]
    store = Store(path)
    try:
        marker = json.loads(store.meta("inbox:" + request["operationId"]))
        assert marker["exit"] == 4
        assert marker["answer"]["error"] == "usage"
        assert not (path.parent / "takeover-inbox" / request["operationId"]).exists()
    finally:
        store.close()


@pytest.mark.parametrize("error", [cli.SystemExit2("missing replay argument", 4),
                                   cli.PayloadExit({"error": "usage", "detail": "missing replay argument"}, 4)])
def test_usage_replay_result_is_terminal(populated_store, error):
    path, _ = populated_store
    parser = cli.build_parser()
    args = parser.parse_args(["--state", str(path.parent), "ack", "--event", "event", "--ack-turn", "turn", "--ack-proof", "proof"])
    request = inbox.envelope(parser, args)
    inbox.enqueue(path.parent, request)
    services = cli.Services(args)
    try:
        with mock.patch.object(services.ack, "acknowledge", side_effect=error):
            inbox.replay(services, parser)
        marker = json.loads(services.store.meta("inbox:" + request["operationId"]))
        assert marker["exit"] == 4
        assert marker["answer"] == {"error": "usage", "detail": "missing replay argument"}
        assert not (path.parent / "takeover-inbox" / request["operationId"]).exists()
    finally:
        services.close()
