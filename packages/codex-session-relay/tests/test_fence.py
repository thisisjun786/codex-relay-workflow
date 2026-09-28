"""The final Python writer fence and decision 25 durable ingress."""

import argparse
import concurrent.futures
import errno
import hashlib
import json
import os
import selectors
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest import mock

import pytest
from codex_session_relay import cli, inbox, ownership, stopadapter
from codex_session_relay.errors import RefusalReason, RelayError
from codex_session_relay.intent import registration_hold
from codex_session_relay.service import RelayService
from codex_session_relay.store import Store, probe, resolve_state_dir


@pytest.fixture
def database():
    # Keep real AF_UNIX tests below sockaddr_un's pathname limit even when the
    # package gate supplies a deeply nested pytest basetemp.
    with tempfile.TemporaryDirectory(prefix="fence-") as temporary:
        path = Path(temporary) / "relay.sqlite3"
        store = Store(path)
        store.close()
        yield path


def stamp(path, *, owner="python", phase="active", epoch=1, protocol=1):
    db = sqlite3.connect(path)
    with db:
        for key, value in (("owner", owner), ("owner_epoch", str(epoch)),
                           ("writer_protocol", str(protocol))):
            db.execute("UPDATE schema_meta SET value=? WHERE key=?", (value, key))
    db.close()
    record = json.loads((path.parent / "takeover.json").read_bytes())
    record.update(owner=owner, epoch=epoch, phase=phase, protocol=protocol,
                  database=ownership.physical(path), relayRPCSocket=str(path.parent / "control.sock"))
    (path.parent / "takeover.json").write_bytes(inbox.canonical(record))


def files(directory):
    return {str(path.relative_to(directory)): path.read_bytes()
            for path in directory.rglob("*") if path.is_file()}


def test_first_open_stamps_and_reopen_preserves(database):
    before = ownership.metadata(database)
    assert before["python_compatibility_build"] == ownership.BUILD
    assert before["owner"] == "python"
    assert before["owner_epoch"] == "1"
    assert before["version"] == "1"
    lock = database.parent / "write-gate.lock"
    inode = lock.stat().st_ino
    assert lock.stat().st_mode & 0o777 == 0o600
    store = Store(database)
    store.close()
    assert lock.stat().st_ino == inode
    assert ownership.metadata(database) == before


@pytest.mark.parametrize("owner,phase,protocol", [("go", "active", 1),
                                                 ("python", "draining", 1),
                                                 ("python", "active", 2)])
def test_refusal_leaves_fresh_fixture_copy_byte_identical(database, tmp_path, owner, phase, protocol):
    copy = tmp_path / "copy"
    shutil.copytree(database.parent, copy)
    path = copy / database.name
    stamp(path, owner=owner, phase=phase, protocol=protocol)
    before = files(copy)
    assert not Path(str(path) + "-wal").exists()
    with pytest.raises(RelayError) as caught:
        Store(path)
    assert caught.value.reason == RefusalReason.STORE_OWNED_BY_OTHER
    assert files(copy) == before
    with registration_hold(path) as (connection, detail):
        assert connection is None
        assert "store_owned_by_other" in str(detail)
    assert files(copy) == before


def test_transaction_revalidates_epoch_and_admitted_writer_finishes_draining(database):
    store = Store(database)
    try:
        record = json.loads((database.parent / "takeover.json").read_bytes())
        record["phase"] = "draining"
        (database.parent / "takeover.json").write_bytes(inbox.canonical(record))
        with store.transaction() as db:
            db.execute("INSERT INTO journal (at,kind,subject,detail) VALUES ('t','admitted','s','d')")
        assert store.one("SELECT count(*) FROM journal")[0] == 1
        store.db.execute("UPDATE schema_meta SET value='2' WHERE key='owner_epoch'")
        with pytest.raises(RelayError), store.transaction():
            pytest.fail("a changed epoch entered a transaction")
        assert store.one("SELECT count(*) FROM journal")[0] == 1
    finally:
        store.close()


def line(process):
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        assert selector.select(timeout=10), "child did not reach the exact lock handshake"
        return process.stdout.readline()


def test_shared_gate_blocks_exclusive_process_until_connection_close(database):
    writer = subprocess.Popen([sys.executable, "-u", "-c", """
import sys
from codex_session_relay.store import Store
store = Store(sys.argv[1])
print('admitted', flush=True)
sys.stdin.readline()
store.close()
""", str(database)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    helper = None
    try:
        assert line(writer) == b"admitted\n"
        helper = subprocess.Popen([sys.executable, "-u", "-c", """
import fcntl, os, sys
fd = os.open(sys.argv[1], os.O_RDWR)
try:
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    print('blocked', flush=True)
else:
    raise AssertionError('exclusive lock passed a live shared writer')
fcntl.flock(fd, fcntl.LOCK_EX)
print('exclusive', flush=True)
os.close(fd)
""", str(database.parent / "write-gate.lock")], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        assert line(helper) == b"blocked\n"
        assert writer.stdin is not None
        writer.stdin.write(b"close\n")
        writer.stdin.flush()
        assert writer.wait(timeout=10) == 0
        assert line(helper) == b"exclusive\n"
        assert helper.wait(timeout=10) == 0
    finally:
        for process in (writer, helper):
            if process is not None:
                if process.poll() is None:
                    process.kill()
                process.communicate(timeout=10)


CASES = {
    "emit": ["--relationship", "relationship-1", "--generation", "1", "--outcome", "failed",
             "--turn-thread", "child-1", "--turn-id", "turn-1"],
    "ack": ["--event", "event-1", "--ack-turn", "parent-turn", "--ack-proof", "proof-1"],
    "fault-notification-ack": ["--notification", "notice-1", "--token", "token-1", "--ref", "receipt-1"],
}


LEGACY_READBACK = ["--message", "message-1", "--turn", "supervisor-turn", "--proof", "proof-1",
                   "--as", "supervisor-1"]


@pytest.mark.parametrize("command", inbox.REPLAY_KEYS)
def test_decision25_envelope_golden(command):
    parser = cli.build_parser()
    request = inbox.envelope(parser, parser.parse_args([
        command, *(LEGACY_READBACK if command == "supervisor-read" else CASES[command])]))
    fixture = Path(__file__).resolve().parents[3] / "contract/golden/takeover-inbox" / (command + ".json")
    assert inbox.canonical(request) == fixture.read_bytes()
    assert request["payloadDigest"] == "sha256:" + hashlib.sha256(inbox.canonical(request["arguments"])).hexdigest()
    assert "attempt" not in request["arguments"]


@pytest.mark.parametrize("command", CASES)
@pytest.mark.parametrize("owner,phase", [("go", "active"), ("python", "draining")])
def test_ingress_queues_at_cli_without_touching_database(database, command, owner, phase, capsys):
    stamp(database, owner=owner, phase=phase)
    before = database.read_bytes()
    assert cli.main(["--state", str(database.parent), command, *CASES[command]]) == 0
    answer = json.loads(capsys.readouterr().out)
    assert answer["status"] == "durably_queued"
    assert database.read_bytes() == before
    request = database.parent / "takeover-inbox" / answer["operationId"]
    assert request.is_file()
    assert request.stat().st_mode & 0o777 == 0o600
    assert not Path(str(database) + "-wal").exists()


def test_retry_conflict_and_failed_write(database):
    parser = cli.build_parser()
    request = inbox.envelope(parser, parser.parse_args(["ack", *CASES["ack"]]))
    first = inbox.enqueue(database.parent, request)
    before = files(database.parent)
    assert inbox.enqueue(database.parent, request) == first
    assert files(database.parent) == before
    changed = inbox.envelope(parser, parser.parse_args(["ack", *CASES["ack"], "--reject", "stale_generation"]))
    with pytest.raises(RelayError) as caught:
        inbox.enqueue(database.parent, changed)
    assert caught.value.reason == RefusalReason.INBOX_CONFLICT
    assert files(database.parent) == before
    request = inbox.envelope(parser, parser.parse_args(["emit", *CASES["emit"]]))
    with (mock.patch.object(inbox, "write_request", side_effect=OSError(errno.ENOSPC, "disk full")),
          pytest.raises(OSError)):
        inbox.enqueue(database.parent, request)
    assert files(database.parent) == before


def test_a_failed_directory_sync_never_removes_a_retry_another_sender_acknowledged(database):
    parser = cli.build_parser()
    request = inbox.envelope(parser, parser.parse_args(["ack", *CASES["ack"]]))
    final = database.parent / "takeover-inbox" / request["operationId"]
    real_sync = inbox.sync_directory
    retry = None

    def fail_after_retry(directory):
        nonlocal retry
        if Path(directory) == final.parent and final.exists():
            # The second sender observes the immutable final name, syncs it, and receives the
            # durable acknowledgment before the original sender learns its own fsync failed.
            with mock.patch.object(inbox, "sync_directory", side_effect=real_sync):
                retry = inbox.enqueue(database.parent, request)
            raise OSError(errno.ENOSPC, "directory sync failed")
        return real_sync(directory)

    with (mock.patch.object(inbox, "sync_directory", side_effect=fail_after_retry),
          pytest.raises(OSError, match="directory sync failed")):
        inbox.enqueue(database.parent, request)
    assert retry is not None and retry["status"] == "durably_queued"
    assert final.read_bytes() == inbox.canonical(request)


def test_enospc_is_host_error_not_acceptance(database, capsys):
    stamp(database, owner="go")
    with mock.patch.object(inbox, "write_request", side_effect=OSError(errno.ENOSPC, "disk full")):
        assert cli.main(["--state", str(database.parent), "ack", *CASES["ack"]]) == 3
    answer = json.loads(capsys.readouterr().out)
    assert answer["error"] == "host"
    assert answer["reason"] == "inbox_unavailable"
    assert list((database.parent / "takeover-inbox").iterdir()) == []


def test_replay_crash_after_commit_before_unlink_does_not_repeat_handler(database):
    parser = cli.build_parser()
    request = inbox.envelope(parser, parser.parse_args(["ack", *CASES["ack"]]))
    inbox.enqueue(database.parent, request)
    args = parser.parse_args(["--state", str(database.parent), "ack", *CASES["ack"]])
    services = cli.Services(args)
    actual = cli.cmd_ack
    calls = []

    def handler(service, parsed):
        calls.append(parsed.event)
        return actual(service, parsed)

    inbox.subcommand(parser, "ack").set_defaults(handler=handler)
    entry = database.parent / "takeover-inbox" / request["operationId"]
    unlink = Path.unlink

    def crash(path, *args, **kwargs):
        if path == entry:
            raise RuntimeError("crash after commit")
        return unlink(path, *args, **kwargs)

    try:
        with (mock.patch.object(Path, "unlink", crash),
              pytest.raises(RuntimeError, match="crash after commit")):
            inbox.replay(services, parser)
        marker = services.store.meta("inbox:" + request["operationId"])
        assert json.loads(marker)["exit"] == 2
        assert entry.exists()
        assert calls == ["event-1"]
        inbox.replay(services, parser)
        assert calls == ["event-1"]
        assert not entry.exists()
        assert services.store.meta("inbox:" + request["operationId"]) == marker
    finally:
        services.close()


@pytest.mark.parametrize("argv", [["criteria-register", "--relationship", "r", "--criterion", "c=title"],
                                  ["store-challenge", "--actor", "test"],
                                  ["service", "enable"]])
def test_nonqueued_writer_refusal_is_byte_identical(database, argv, capsys):
    stamp(database, owner="go")
    before = files(database.parent)
    assert cli.main(["--state", str(database.parent), *argv]) == 2
    answer = json.loads(capsys.readouterr().out)
    assert answer["reason"] == "store_owned_by_other"
    assert files(database.parent) == before


def writing_commands():
    parser = cli.build_parser()
    commands = next(action for action in parser._actions
                    if isinstance(action, argparse._SubParsersAction)).choices
    for command, child in commands.items():
        if command in inbox.COMMAND_KEYS or command in cli.READ_ONLY_COMMANDS:
            continue
        if command == "service":
            for action in ("enable", "disable", "declare", "start", "restart", "run", "stop"):
                options = ["--forget-execution-policy"] if action == "declare" else []
                yield command + "-" + action, [command, action, *options]
            continue
        arguments = [command]
        for action in child._actions:
            if action.required and action.option_strings:
                value = str(next(iter(action.choices))) if action.choices else "1" if action.type in (int, float) else "synthetic"
                arguments.extend([action.option_strings[0], value])
        for group in child._mutually_exclusive_groups:
            if group.required:
                action = group._group_actions[0]
                arguments.append(action.option_strings[0])
                if action.nargs != 0:
                    arguments.append(str(next(iter(action.choices))) if action.choices else "synthetic")
        if command == "fault-policy":
            arguments += ["--fault-class", "synthetic"]
        if command == "fault-limit":
            arguments += ["--kind", "synthetic"]
        yield command, arguments


@pytest.mark.parametrize("command,argv", list(writing_commands()))
def test_every_nonqueued_cli_writer_refuses_before_effects(database, command, argv):
    stamp(database, owner="go")
    before = files(database.parent)
    executable = Path(sys.executable).parent / "codex-session-relay"
    result = subprocess.run([str(executable), "--state", str(database.parent), *argv],
                            capture_output=True, timeout=15, check=False)
    assert result.returncode == 2, (command, result.stdout, result.stderr)
    answer = json.loads(result.stdout)
    assert answer["reason"] == "store_owned_by_other", (command, answer)
    assert result.stderr == b""
    assert files(database.parent) == before


def test_foreign_owner_read_only_command_and_doctor(database, capsys):
    stamp(database, owner="go")
    assert cli.main(["--state", str(database.parent), "settings-show", "--task", "unknown"]) == 0
    assert json.loads(capsys.readouterr().out)["settings"] is None
    assert cli.main(["--state", str(database.parent), "doctor"]) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["ownership"]["owner"] == "go"
    assert report["ownership"]["python_compatibility_build"] == ownership.BUILD
    assert report["access"]["dbWritable"] is False


def test_intent_register_explicit_database_ignores_an_unrelated_foreign_selection(database, tmp_path,
                                                                                   capsys):
    from codex_session_relay import intent
    from codex_session_relay.registry import Endpoint

    selected = database
    explicit = tmp_path / "explicit" / "relay.sqlite3"
    store = Store(explicit)
    try:
        services = cli.Services(cli.build_parser().parse_args([
            "--state", str(explicit.parent), "store-challenge", "--write", "--actor", "seed",
        ]))
        try:
            registered = services.registry.register(
                parent=Endpoint("parent", "host"), child=Endpoint("child", "host"),
                issue_key="REL-1", artifact_roots=[str(tmp_path)],
                allowed_recipients=["parent"], dispatch_request_id="dispatch-explicit",
            )
        finally:
            services.close()
        relationship = registered["relationshipId"]
    finally:
        store.close()
    markers = tmp_path / "markers"
    workspace = tmp_path / "work"
    workspace.mkdir()
    declared = intent.declare_intent(
        markers, workspace=workspace, dispatch_request_id="dispatch-explicit", issue_key="REL-1",
        declared_at="2026-09-29T00:00:00+00:00", db_path=explicit,
    )
    stamp(selected, owner="go")
    before = files(selected.parent)
    assert cli.main([
        "--state", str(selected.parent), "intent-register", "--marker-root", str(markers),
        "--workspace", str(workspace), "--assignment", declared["assignmentId"],
        "--relationship", relationship, "--dispatch-request-id", "dispatch-explicit",
        "--db-path", str(explicit),
    ]) == 0
    answer = json.loads(capsys.readouterr().out)
    assert answer["relationshipId"] == relationship
    assert files(selected.parent) == before


def test_service_start_refuses_draining_without_launch(database):
    stamp(database, phase="draining")
    service = RelayService(resolve_state_dir(database.parent))
    with mock.patch.object(service, "default_launcher") as launch:
        result = service.start()
    assert result["reason"] == "store_owned_by_other"
    launch.assert_not_called()


def test_guard_socket_first_never_spawns_python(database):
    stamp(database, owner="go")
    verdict = {"decision": "release", "hook_output": {}}
    config = {"relayExecutable": "must-not-execute", "markerRoot": str(database.parent / "markers"),
              "dbPath": str(database), "mode": "observe"}
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(database.parent / "control.sock"))
        server.listen(1)
        server.settimeout(10)

        def answer():
            connection, _ = server.accept()
            with connection:
                connection.settimeout(10)
                with connection.makefile("rb") as stream:
                    request = json.loads(stream.readline())
                assert request["method"] == "guard-evaluate"
                assert request["params"]["stopInput"] == {"turn_id": "one"}
                connection.sendall(json.dumps(verdict).encode() + b"\n")

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(answer)
            with mock.patch.object(stopadapter.subprocess, "Popen", side_effect=AssertionError("spawned")):
                result = stopadapter.invoke_guard(config, b'{"turn_id":"one"}')
            future.result(timeout=10)
    assert result["code"] == 0
    assert json.loads(result["stdout"]) == verdict


def test_untrusted_guard_socket_falls_back_without_sending_or_trusting(database):
    stamp(database, owner="python")
    verdict = {"decision": "release", "hook_output": {}}
    state = database.parent
    state.chmod(0o777)
    received = []
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(state / "control.sock"))
        server.listen(1)
        server.settimeout(0.2)

        def forged():
            try:
                connection, _ = server.accept()
            except TimeoutError:
                return
            with connection:
                received.append(connection.recv(1))
                try:
                    connection.sendall(json.dumps(verdict).encode() + b"\n")
                except BrokenPipeError:
                    pass

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(forged)
            result = stopadapter.socket_guard({
                "relayExecutable": "unused", "markerRoot": str(state / "markers"),
                "dbPath": str(database), "mode": "observe",
            }, b"{}")
            future.result(timeout=5)
    assert result is None
    assert all(chunk == b"" for chunk in received)


def test_go_owner_unreachable_never_falls_back(database):
    stamp(database, owner="go")
    config = {"relayExecutable": "must-not-execute", "markerRoot": str(database.parent / "markers"),
              "dbPath": str(database), "mode": "observe"}
    with mock.patch.object(stopadapter.subprocess, "Popen", side_effect=AssertionError("spawned")):
        result = stopadapter.invoke_guard(config, b'{}')
    assert json.loads(result["stdout"])["reason"] == "store_owned_by_other"


def test_declarations_and_pinned_transport_ledger_refuse_without_files(database):
    from codex_session_relay import declarations
    from codex_thread_bridge.ledger import Ledger

    stamp(database, owner="go")
    before = files(database.parent)
    for opener in (lambda: declarations._open(database),
                   lambda: declarations.Held(database),
                   lambda: Ledger(database.parent / "operations-test.sqlite3")):
        with pytest.raises(RelayError) as caught:
            opener()
        assert caught.value.reason == RefusalReason.STORE_OWNED_BY_OTHER
        assert files(database.parent) == before


def test_probe_foreign_owner_never_takes_write_transaction(database):
    stamp(database, owner="go")
    before = files(database.parent)
    with mock.patch.object(ownership.Admission, "revalidate", side_effect=AssertionError("admitted")):
        result = probe(resolve_state_dir(database.parent))
    assert result["access"]["dbWritable"] is False
    assert "store_owned_by_other" in result["access"]["detail"]
    assert files(database.parent) == before


def test_supervisor_stops_replacing_workers_at_draining(database):
    service = RelayService(resolve_state_dir(database.parent))
    service.enable(actor="test")
    launches = []

    class Worker:
        pid = os.getpid()

        def wait(self):
            stamp(database, phase="draining")
            return 0

    def spawn(**kwargs):
        launches.append(kwargs)
        return Worker()

    result = service.supervise(spawn=spawn, max_segments=2, sleeper=lambda _: None)
    assert result["segments"] == [0]
    assert len(launches) == 1


def test_python_control_server_answers_full_verdict(database):
    from codex_session_relay.control import GuardServer

    server = GuardServer(database.parent)
    try:
        result = stopadapter.invoke_guard({
            "relayExecutable": "must-not-execute", "markerRoot": str(database.parent / "markers"),
            "dbPath": str(database), "mode": "observe"}, b'{}')
        assert result["code"] == 0
        verdict = json.loads(result["stdout"])
        assert "decision" in verdict and "hook_output" in verdict and "record" in verdict
    finally:
        server.close()
    assert not (database.parent / "control.sock").exists()


def test_python_control_server_maps_a_host_error_without_calling_it_a_refusal(database):
    from codex_session_relay import control

    server = control.GuardServer(database.parent)
    try:
        with mock.patch.object(control.guard, "evaluate", side_effect=ValueError("broken host")):
            result = stopadapter.invoke_guard({
                "relayExecutable": "must-not-execute", "markerRoot": str(database.parent / "markers"),
                "dbPath": str(database), "mode": "observe",
            }, b"{}")
        said, value = stopadapter.read_guard_stdout(result["stdout"])
        assert result["code"] == stopadapter.GUARD_EXIT_HOST
        assert stopadapter.outcome_of(result, said, value) == stopadapter.GUARD_HOST_ERROR
    finally:
        server.close()


def test_python_control_server_rejects_a_foreign_peer_before_evaluation(database):
    from codex_session_relay import control

    server = control.GuardServer(database.parent)
    try:
        with (mock.patch.object(control, "peer_uid", return_value=os.getuid() + 1),
              mock.patch.object(control.guard, "evaluate", side_effect=AssertionError("evaluated")),
              socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client):
            client.settimeout(5)
            client.connect(str(server.path))
            client.sendall(b'{}\n')
            with pytest.raises((ConnectionResetError, BrokenPipeError)):
                client.recv(1)
    finally:
        server.close()


def test_missing_mirror_and_owner_mismatch_refuse_without_reset(database):
    record_path = database.parent / "takeover.json"
    original = record_path.read_bytes()
    record_path.unlink()
    before = files(database.parent)
    with pytest.raises(RelayError):
        Store(database)
    assert files(database.parent) == before
    record = json.loads(original)
    record["owner"] = "go"
    record_path.write_bytes(inbox.canonical(record))
    before = files(database.parent)
    with pytest.raises(RelayError):
        Store(database)
    assert files(database.parent) == before


def test_live_wal_owner_is_not_read_from_stale_main_file(database):
    db = sqlite3.connect(database)
    try:
        db.execute("UPDATE schema_meta SET value='go' WHERE key='owner'")
        db.commit()
        record = json.loads((database.parent / "takeover.json").read_bytes())
        record["owner"] = "go"
        (database.parent / "takeover.json").write_bytes(inbox.canonical(record))
        before = files(database.parent)
        assert ownership.metadata(database)["owner"] == "go"
        with pytest.raises(ownership.OwnershipRefused) as caught:
            Store(database)
        assert caught.value.queueable is True
        assert files(database.parent) == before
    finally:
        db.close()


def test_operation_id_percent_encoding_and_limit():
    parser = cli.build_parser()
    args = parser.parse_args(["ack", "--event", "event/雪", "--ack-turn", "turn", "--ack-proof", "proof"])
    assert inbox.envelope(parser, args)["operationId"] == "ack.event%2F%E9%9B%AA"
    args.event = "x" * 200
    with pytest.raises(ValueError, match="200"):
        inbox.envelope(parser, args)


def test_successful_receipt_replay_commits_result_and_marker_together(database, tmp_path):
    from codex_session_relay.models import Endpoint

    parser = cli.build_parser()
    args = parser.parse_args(["--state", str(database.parent), "emit", *CASES["emit"]])
    services = cli.Services(args)
    try:
        relationship = services.registry.register(
            parent=Endpoint("parent", "host"), child=Endpoint("child-1", "host"),
            issue_key="TEST-1", artifact_roots=[str(tmp_path)], allowed_recipients=["parent"],
            dispatch_request_id="dispatch-1", dispatch_turn_id="turn-1")
        args.relationship = relationship["relationshipId"]
        args.no_enqueue = True
        args.turn_status = "failed"
        request = inbox.envelope(parser, args)
        inbox.enqueue(database.parent, request)
        services.store.fault_hook = lambda: (_ for _ in ()).throw(RuntimeError("commit failed"))
        with pytest.raises(RuntimeError, match="commit failed"):
            inbox.replay(services, parser)
        assert services.store.meta("inbox:" + request["operationId"]) is None
        assert services.store.one("SELECT count(*) FROM events")[0] == 0
        assert (database.parent / "takeover-inbox" / request["operationId"]).exists()
        services.store.fault_hook = None
        entry = database.parent / "takeover-inbox" / request["operationId"]
        real_unlink = Path.unlink

        def crash_after_commit(path, *positional, **keywords):
            if path == entry:
                raise RuntimeError("after commit")
            return real_unlink(path, *positional, **keywords)

        with (mock.patch.object(Path, "unlink", crash_after_commit),
              pytest.raises(RuntimeError, match="after commit")):
            inbox.replay(services, parser)
        assert services.store.one("SELECT count(*) FROM events")[0] == 1
        with mock.patch.object(services.intake, "accept_child_receipt", side_effect=AssertionError("reapplied")):
            inbox.replay(services, parser)
        marker = json.loads(services.store.meta("inbox:" + request["operationId"]))
        assert marker["exit"] == 0
        assert marker["answer"]["receipt"]["relationshipId"] == args.relationship
        assert services.store.one("SELECT count(*) FROM events")[0] == 1
        assert not (database.parent / "takeover-inbox" / request["operationId"]).exists()
    finally:
        services.close()
