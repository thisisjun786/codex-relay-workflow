"""Owner-independent read matrix and terminal inbox replay regressions."""

import json
import shutil
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
def test_readonly_matrix_matches_python_owner(populated_store, command, monkeypatch):
    path, values = populated_store
    # A control.sock owner evaluates only under its own marker root (control.py owner_paths).
    monkeypatch.setenv("CODEX_SESSION_RELAY_MARKER_ROOT", values["markers"])
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


def test_fault_next_projects_expired_leases_without_writing(populated_store):
    path, _ = populated_store
    store = Store(path)
    try:
        ledger = faults.FaultLedger(store, FakeClock())
        ledger.set_target(product="crw", project="CRW", team="team-relay", project_ref="proj-CRW")
        ledger.record(faults.observation(
            product="crw", fault_class="report_omitted", severity=faults.BROKEN,
            signature={"relationship": "rel-1", "turn": "turn-1"}, occurrence_key="one",
            scope={"projectKey": "CRW", "issueKey": "CRW-1"}, detail="missing",
            evidence=[{"kind": "row", "ref": "events", "observed": {"rows": 0}}],
        ))
        publication = ledger.next()[0]["publication_id"]
        ledger.claim(publication, owner="operator")
        with store.transaction() as db:
            db.execute("UPDATE fault_publications SET lease_until=0 WHERE publication_id=?",
                       (publication,))
        stamp(path, owner="go")
        with sqlite3.connect(path) as db:
            before_rows = list(db.execute("SELECT * FROM fault_publications ORDER BY rowid"))
        before_entries = sorted(entry.name for entry in path.parent.iterdir())
        code, answer = invoke(path, "fault-next", [])
        assert code == 0, answer
        assert [item["publication_id"] for item in answer["publications"]] == [publication]
        with sqlite3.connect(path) as db:
            assert list(db.execute("SELECT * FROM fault_publications ORDER BY rowid")) == before_rows
        assert sorted(entry.name for entry in path.parent.iterdir()) == before_entries
    finally:
        store.close()


def test_the_owners_fault_next_releases_a_lapsed_claim(populated_store):
    """Read-only decides how a form opens a store, not what the owner's housekeeping does.

    On the store Python owns, fault-next expires a lapsed claim as it did before the fence
    (cutover.md, Read-only clients under a foreign owner; decision 31): the claim's attempt
    ends lease_lapsed, the write is pending again, and fault-claim takes it. Nothing else
    calls expire_leases, so a projection here would leave the write claimed for good.
    """
    path, _ = populated_store
    store = Store(path)
    try:
        ledger = faults.FaultLedger(store, FakeClock())
        ledger.set_target(product="crw", project="CRW", team="team-relay", project_ref="proj-CRW")
        ledger.record(faults.observation(
            product="crw", fault_class="report_omitted", severity=faults.BROKEN,
            signature={"relationship": "rel-1", "turn": "turn-1"}, occurrence_key="one",
            scope={"projectKey": "CRW", "issueKey": "CRW-1"}, detail="missing",
            evidence=[{"kind": "row", "ref": "events", "observed": {"rows": 0}}],
        ))
        publication = ledger.next()[0]["publication_id"]
        ledger.claim(publication, owner="operator")
        with store.transaction() as db:
            db.execute("UPDATE fault_publications SET lease_until=0 WHERE publication_id=?",
                       (publication,))
    finally:
        store.close()
    code, answer = invoke(path, "fault-next", [])
    assert code == 0, answer
    assert [item["publication_id"] for item in answer["publications"]] == [publication]
    with sqlite3.connect(path) as db:
        assert db.execute("SELECT state, claim_token, lease_until FROM fault_publications"
                          " WHERE publication_id=?", (publication,)).fetchone() == (
            faults.PENDING, None, None)
        assert db.execute("SELECT outcome, ended FROM fault_publication_attempts"
                          " WHERE publication_id=? ORDER BY attempt_id", (publication,)
                          ).fetchall() == [("lease_lapsed", 1)]
    code, claimed = invoke(path, "fault-claim", ["--publication", publication,
                                                 "--owner", "operator"])
    assert code == 0, claimed
    assert claimed["publicationId"] == publication


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


def rows(path):
    with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as db:
        tables = [name for (name,) in db.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
        return {table: sorted(map(repr, db.execute(f'SELECT * FROM "{table}"'))) for table in tables}


@pytest.fixture
def expired_store(populated_store):
    """The populated store with every lease the schema has already lapsed (PR #185 thread 6)."""
    path, values = populated_store
    store = Store(path)
    try:
        ledger = faults.FaultLedger(store, FakeClock())
        ledger.set_target(product="crw", project="CRW", team="team-relay", project_ref="proj-CRW")
        ledger.record(faults.observation(
            product="crw", fault_class="report_omitted", severity=faults.BROKEN,
            signature={"relationship": "rel-1", "turn": "turn-1"}, occurrence_key="one",
            scope={"projectKey": "CRW", "issueKey": "CRW-1"}, detail="missing",
            evidence=[{"kind": "row", "ref": "events", "observed": {"rows": 0}}],
        ))
        ledger.claim(ledger.next()[0]["publication_id"], owner="operator")
        with store.transaction() as db:
            db.execute("INSERT INTO fault_notifications "
                       "(notification_id,fault_id,product,kind,cycle,state,lease_until,created_at,updated_at)"
                       " VALUES ('expired','fault','crw',?,1,?,0,'then','then')",
                       (faults.DECISION, faults.RESERVED))
            for table in ("deliveries", "sync_outbox", "supervisor_messages", "fault_publications"):
                db.execute(f"UPDATE {table} SET lease_until=0")
    finally:
        store.close()
    return path, values


@pytest.mark.parametrize("command", READ_FORMS)
def test_read_only_commands_never_write_expired_leases_under_a_foreign_owner(expired_store, command,
                                                                             monkeypatch):
    path, values = expired_store
    # A control.sock owner evaluates only under its own marker root (control.py owner_paths).
    monkeypatch.setenv("CODEX_SESSION_RELAY_MARKER_ROOT", values["markers"])
    options = [option.format(**values) for option in READ_FORMS[command]]
    from codex_session_relay import forge
    from codex_session_relay.control import GuardServer

    stamp(path, owner="go")
    before = rows(path)
    with mock.patch.object(forge, "collect", return_value={"verdict": forge.READY}):
        server = GuardServer(path.parent) if command == "guard-evaluate" else None
        try:
            code, answer = invoke(path, command, options)
        finally:
            if server is not None:
                server.close()
    assert code == 0, (command, answer)
    assert rows(path) == before, command


# The read-only forms that open the selected store through Services.store.
PARTIAL_READS = (["status"], ["show", "--event", "absent"], ["store-identity"],
                 ["store-challenge", "--read", "absent"], ["fault-show"], ["fault-next"],
                 ["sync-status"], ["route-show"])


@pytest.mark.parametrize("partial,detail", [
    ("gate", "partial store: write-gate.lock without a database"),
    ("mirror", "missing or unsupported writer protocol"),
    ("gate and mirror", "missing or unsupported writer protocol"),
])
def test_a_partial_store_is_refused_as_a_writer_refuses_it(tmp_path, partial, detail):
    """A partial store (a write gate or a mirror without D) is refused, never read or repaired.

    Decision 30: a read-only form answers exactly the refusal a writer's admission gives the
    same state, reason store_owned_by_other with exit 2, rather than a host error that invites a
    retry, and leaves S exactly as it found it. A reader never takes the gate.
    """
    source = tmp_path / "source" / "relay.sqlite3"
    Store(source).close()
    state = tmp_path / "state"
    state.mkdir(mode=0o700)
    if "gate" in partial:
        (state / "write-gate.lock").touch(mode=0o600)
    if "mirror" in partial:
        shutil.copyfile(source.parent / "takeover.json", state / "takeover.json")

    def snapshot():
        return sorted(path.name for path in state.iterdir()), files(state)

    def answer(argv):
        output = StringIO()
        with redirect_stdout(output):
            code = cli.main(["--state", str(state), *argv])
        return code, json.loads(output.getvalue())

    before = snapshot()
    refused = {"error": "refused", "reason": "store_owned_by_other", "detail": detail}
    assert answer(["store-challenge", "--write"]) == (2, refused)
    assert snapshot() == before
    for argv in PARTIAL_READS:
        assert answer(argv) == (2, refused), argv
        assert snapshot() == before, argv


def _cli(argv):
    output = StringIO()
    with redirect_stdout(output):
        code = cli.main(argv)
    return code, json.loads(output.getvalue())


# The service and daemon commands act on S and the scope registry before any admitted open.
# `service start`, `restart` and `run` are left out: were the refusal lost they would spawn or
# run a supervisor here; the Go parity test drives the rest of them.
PARTIAL_STARTS = (["service", "enable"], ["service", "disable"], ["service", "stop"],
                  ["service", "declare", "--forget-execution-policy"],
                  ["daemon", "--allow-isolated-scope", "--max-ticks", "0"])


@pytest.mark.parametrize("argv", PARTIAL_STARTS)
@pytest.mark.parametrize("dangling", [False, True], ids=["gate", "gate-beside-a-dangling-link"])
def test_a_gate_without_a_database_is_refused_before_service_or_daemon_touch_anything(
        tmp_path, monkeypatch, argv, dangling):
    """Go store.StartPreflight's partial-store refusal, in the fence (decision 31).

    check_start passes a gate without D as unfenced, and no admitted open follows before a
    service command or the daemon writes daemon.lock, daemon.json or service.json into S or a
    claim into the scope registry. cli.main refuses it first, with a writer's words, reason
    store_owned_by_other and exit 2; D is Path.resolve()'s, so a D link naming no file is no D.
    A daemon asks for its --socket before that, as Go's does.
    """
    scopes = tmp_path / "scopes"
    monkeypatch.setenv("CODEX_SESSION_RELAY_SCOPE_DIR", str(scopes))
    state = tmp_path / "state"
    state.mkdir(mode=0o700)
    (state / "write-gate.lock").touch(mode=0o600)
    if dangling:
        (state / "relay.sqlite3").symlink_to(state / "nowhere.sqlite3")
    before = sorted(path.name for path in state.iterdir())
    refused = {"error": "refused", "reason": "store_owned_by_other",
               "detail": "partial store: write-gate.lock without a database"}
    assert _cli(["--state", str(state), "--socket", str(tmp_path / "app.sock"), *argv]) == (2, refused)
    assert sorted(path.name for path in state.iterdir()) == before
    assert not scopes.exists()
    if argv[0] == "daemon":
        assert _cli(["--state", str(state), *argv]) == (4, {
            "error": "usage", "detail": "this command needs --socket to reach the host"})
        assert sorted(path.name for path in state.iterdir()) == before
        assert not scopes.exists()


def test_a_dangling_database_link_is_judged_as_the_file_it_names(tmp_path):
    """D is the file every opener opens, Path.resolve()'s (Go's storeAbsent and partialStore).

    A D link naming no file, beside no mirror and no gate, is an absent store: a read-only form
    refuses it as store_absent and creates nothing through the link. Beside a gate it is a
    partial store, refused with a writer's words rather than read (a host error, exit 3) after
    taking the gate.
    """
    state = tmp_path / "state"
    state.mkdir(mode=0o700)
    (state / "relay.sqlite3").symlink_to(state / "nowhere.sqlite3")
    absent = {"error": "refused", "reason": "store_absent",
              "detail": f"no relay store exists at {state / 'relay.sqlite3'}; a read-only command"
                        " never creates one"}
    for argv in PARTIAL_READS:
        assert _cli(["--state", str(state), *argv]) == (2, absent), argv
        assert sorted(path.name for path in state.iterdir()) == ["relay.sqlite3"], argv
    (state / "write-gate.lock").touch(mode=0o600)
    partial = {"error": "refused", "reason": "store_owned_by_other",
               "detail": "partial store: write-gate.lock without a database"}
    for argv in (*PARTIAL_READS, ["store-challenge", "--write"]):
        assert _cli(["--state", str(state), *argv]) == (2, partial), argv
        assert sorted(path.name for path in state.iterdir()) == ["relay.sqlite3", "write-gate.lock"], argv
