"""The final Python writer fence and decision 25 durable ingress."""

import argparse
import concurrent.futures
import contextlib
import errno
import hashlib
import io
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


@pytest.fixture
def owner_markers(database, monkeypatch):
    """The in-process owner's own marker root is the one these tests' requests name.

    A control.sock owner evaluates only under the root it resolves for itself (control.py
    owner_paths), so the test configures this process as the relay is configured: through
    CODEX_SESSION_RELAY_MARKER_ROOT.
    """
    root = database.parent / "markers"
    monkeypatch.setenv("CODEX_SESSION_RELAY_MARKER_ROOT", str(root))
    return root


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


def test_refused_entry_is_judged_again_after_a_crash_between_commit_and_unlink(database):
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
        # Only a successful application deduplicates (decision 25): the refused request is
        # judged again, as a direct call would be, and its marker replaced.
        inbox.replay(services, parser)
        assert calls == ["event-1", "event-1"]
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
        # Marker-only commands never read the selected store (PR #185 thread 3); the store an
        # intent names is fenced by its own test below. intent-declare and intent-register,
        # without --no-db-path / --db-path, still record or confirm against the selection.
        if command in cli.MARKER_COMMANDS_BY_NAME and command not in ("intent-declare",
                                                                       "intent-register"):
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
              "dbPath": str(database), "mode": "observe", "socketPath": "/run/app.sock"}
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
                # The selection inputs the Go hook client sends (decision 24).
                assert request["params"]["socketPath"] == "/run/app.sock"
                assert request["params"]["program"] == "must-not-execute"
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


def test_python_control_server_answers_full_verdict(database, owner_markers):
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


def test_python_control_server_maps_a_host_error_without_calling_it_a_refusal(database, owner_markers):
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
            # The server may reset the connection before the request is written: the send
            # then fails with EPIPE rather than the read with ECONNRESET.
            with pytest.raises((ConnectionResetError, BrokenPipeError)):
                client.sendall(b'{}\n')
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


def queue(database, argv, capsys):
    """Publish one entry through the real CLI while another runtime owns the store."""
    stamp(database, owner="go")
    assert cli.main(["--state", str(database.parent), *argv]) == 0
    answer = json.loads(capsys.readouterr().out)
    assert answer["status"] == "durably_queued"
    stamp(database, owner="python")
    return answer


def write_as_python(database, capsys):
    code = cli.main(["--state", str(database.parent), "store-challenge", "--write", "--actor", "replay"])
    answer = json.loads(capsys.readouterr().out)
    return code, answer


def schema_meta(database):
    with sqlite3.connect(database) as db:
        return dict(db.execute("SELECT key,value FROM schema_meta"))


def test_reused_retired_id_with_different_bytes_is_a_terminal_conflict(database, capsys):
    first = queue(database, ["ack", *CASES["ack"]], capsys)
    assert write_as_python(database, capsys)[0] == 0
    retired = schema_meta(database)["inbox:" + first["operationId"]]
    assert json.loads(retired)["payloadDigest"] == first["payloadDigest"]
    # The name is free again, so a different request under the same ID is accepted...
    second = queue(database, ["ack", *CASES["ack"], "--reject", "stale_generation"], capsys)
    assert second["operationId"] == first["operationId"]
    assert second["payloadDigest"] != first["payloadDigest"]
    later = queue(database, ["ack", "--event", "event-2", "--ack-turn", "t", "--ack-proof", "p"], capsys)
    # ...and replay records it as a conflict instead of blocking every Python writer.
    code, answer = write_as_python(database, capsys)
    assert code == 0, answer
    meta = schema_meta(database)
    key = "inbox-conflict:" + first["operationId"] + ":" + second["payloadDigest"][7:23]
    assert meta[key] == inbox.canonical({
        "payloadDigest": second["payloadDigest"], "exit": 2,
        "answer": {"error": "refused", "reason": "inbox_conflict",
                   "detail": "committed inbox payload differs"}}).decode("utf-8")
    assert meta["inbox:" + first["operationId"]] == retired
    assert "inbox:" + later["operationId"] in meta
    assert [path.name for path in (database.parent / "takeover-inbox").iterdir()
            if not path.name.startswith(".")] == []


def test_only_a_successful_application_deduplicates_an_identical_retry(database):
    parser = cli.build_parser()
    args = parser.parse_args(["--state", str(database.parent), "ack", *CASES["ack"]])
    request = inbox.envelope(parser, args)
    answers = iter([RelayError(RefusalReason.ACK_PROOF_MISMATCH, "not yet listed"), {"ok": True}])
    calls = []

    def handler(service, parsed):
        calls.append(parsed.event)
        answer = next(answers)
        if isinstance(answer, Exception):
            raise answer
        return answer

    inbox.subcommand(parser, "ack").set_defaults(handler=handler)
    services = cli.Services(args)
    key = "inbox:" + request["operationId"]
    try:
        for expected_calls, expected_exit in ((1, 2), (2, 0), (2, 0)):
            inbox.enqueue(database.parent, request)
            inbox.replay(services, parser)
            assert len(calls) == expected_calls
            marker = json.loads(services.store.meta(key))
            assert marker["payloadDigest"] == request["payloadDigest"]
            assert marker["exit"] == expected_exit
            assert not (database.parent / "takeover-inbox" / request["operationId"]).exists()
        assert marker["answer"] == {"ok": True}
    finally:
        services.close()


def test_a_stale_replayer_never_retires_a_newer_entry_at_the_same_name(database):
    import threading

    parser = cli.build_parser()
    args = parser.parse_args(["--state", str(database.parent), "ack", *CASES["ack"]])
    request = inbox.envelope(parser, args)
    newer = inbox.envelope(parser, parser.parse_args(["ack", *CASES["ack"], "--reject", "stale_generation"]))
    inbox.enqueue(database.parent, request)
    inbox.subcommand(parser, "ack").set_defaults(handler=lambda service, parsed: {"ok": True})
    first, second = cli.Services(args), cli.Services(args)
    second_read = threading.Event()
    real_read, real_sync = inbox.read_entry, inbox.sync_directory
    published = []

    def read_entry(path):
        if threading.current_thread().name == "stale":
            second_read.set()
        return real_read(path)

    def sync_directory(directory):
        real_sync(directory)
        if threading.current_thread().name != "stale" and not published:
            # A sender refused during draining publishes new bytes the moment the name is free.
            published.append(None)
            published[0] = inbox.enqueue(database.parent, newer)
            newer_published.set()

    opened, go, newer_published = threading.Event(), threading.Event(), threading.Event()

    def stale():
        try:
            # Admitted and connected before the first replayer's transaction begins.
            store = second.store
            composing = store.composing

            @contextlib.contextmanager
            def after_newer_publication():
                # Whatever this replayer read, it decides only once the newer bytes exist.
                assert newer_published.wait(30)
                with composing() as db:
                    yield db

            store.composing = after_newer_publication
            opened.set()
            go.wait(30)
            inbox.replay(second, parser)
        finally:
            second.close()

    def handler(service, parsed):
        go.set()
        # Give the second replayer every chance to read the entry before this one retires it.
        second_read.wait(1)
        return {"ok": True}

    worker = threading.Thread(target=stale, name="stale")
    inbox.subcommand(parser, "ack").set_defaults(handler=handler)
    try:
        with (mock.patch.object(inbox, "read_entry", read_entry),
              mock.patch.object(inbox, "sync_directory", sync_directory)):
            worker.start()
            assert opened.wait(30)
            inbox.replay(first, parser)
            worker.join(30)
        assert not worker.is_alive()
        assert published
        entry = database.parent / "takeover-inbox" / request["operationId"]
        conflict = "inbox-conflict:" + request["operationId"] + ":" + newer["payloadDigest"][7:23]
        # The newer bytes were judged, never silently deleted as if they were the old ones.
        assert entry.exists() or first.store.meta(conflict) is not None
    finally:
        first.close()


@pytest.mark.parametrize("failure", ["ownership", "host"])
def test_ownership_or_host_failure_during_replay_retains_the_entry(database, failure):
    from codex_session_relay.errors import ReceiptRefused
    from codex_session_relay.hostadapter import HostUnavailable

    parser = cli.build_parser()
    args = parser.parse_args(["--state", str(database.parent), "ack", *CASES["ack"]])
    request = inbox.envelope(parser, args)
    later = inbox.envelope(parser, parser.parse_args(["ack", "--event", "event-2", "--ack-turn", "t",
                                                      "--ack-proof", "p"]))
    inbox.enqueue(database.parent, request)
    inbox.enqueue(database.parent, later)
    actual = cli.cmd_ack
    calls = []
    real_mirror = ownership.mirror
    broken = []

    def mirror(path):
        if broken:
            broken.clear()
            raise ownership.OwnershipRefused("takeover record unreadable: OSError: [Errno 24] too many")
        return real_mirror(path)

    def handler(service, parsed):
        calls.append(parsed.event)
        if parsed.event == "event-1" and len(calls) == 1:
            if failure == "host":
                try:
                    raise HostUnavailable("listing not exhausted")
                except HostUnavailable as error:
                    raise ReceiptRefused(RefusalReason.UNASSIGNED_TURN, "unconfirmed") from error
            broken.append(True)
            # The handler's own (joined) transaction revalidates admission.
            with service.store.transaction():
                pass
        return actual(service, parsed)

    inbox.subcommand(parser, "ack").set_defaults(handler=handler)
    services = cli.Services(args)
    entry = database.parent / "takeover-inbox" / request["operationId"]
    try:
        with mock.patch.object(ownership, "mirror", mirror):
            inbox.replay(services, parser)
        assert calls == ["event-1", "event-2"]
        assert services.store.meta("inbox:" + request["operationId"]) is None
        assert entry.exists()
        # The retained entry did not block the one after it.
        assert services.store.meta("inbox:" + later["operationId"]) is not None
        inbox.replay(services, parser)
        assert calls == ["event-1", "event-2", "event-1"]
        assert json.loads(services.store.meta("inbox:" + request["operationId"]))["exit"] == 2
        assert not entry.exists()
    finally:
        services.close()


def test_a_held_replay_lock_bounds_the_writer_wait_as_a_host_error(database, monkeypatch, capsys):
    import fcntl

    directory = database.parent / "takeover-inbox"
    directory.mkdir(mode=0o700)
    holder = os.open(directory / inbox.REPLAY_LOCK, os.O_RDWR | os.O_CREAT, 0o600)
    fcntl.flock(holder, fcntl.LOCK_EX)  # another replayer, stuck in a host call
    monkeypatch.setattr(ownership, "LOCK_WAIT_SECONDS", 0.3)
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(cli.main, ["--state", str(database.parent), "store-challenge", "--write"])
            try:
                code = future.result(timeout=10)
            finally:
                fcntl.flock(holder, fcntl.LOCK_UN)
    finally:
        os.close(holder)
    answer = json.loads(capsys.readouterr().out)
    assert code == 3, answer
    assert answer == {"error": "host", "detail": "LockWaitExpired: the takeover inbox replay lock"
                                                   " was not acquired within 0.3s; retry"}
    with sqlite3.connect(database) as db:
        assert db.execute("SELECT count(*) FROM store_challenge").fetchone()[0] == 0
    db.close()


def test_inbox_names_outside_the_grammar_are_ignored_and_never_retired(database, capsys):
    directory = database.parent / "takeover-inbox"
    directory.mkdir()
    ignored = {"ack.event-1~": b"editor backup", "a b": b"x", "x" * 201: b"x", ".tmp-x": b"x"}
    for name, raw in ignored.items():
        (directory / name).write_bytes(raw)
    (directory / "sub dir").mkdir()
    queued = queue(database, ["ack", *CASES["ack"]], capsys)
    code, answer = write_as_python(database, capsys)
    assert code == 0, answer
    assert "inbox:" + queued["operationId"] in schema_meta(database)
    for name, raw in ignored.items():
        assert (directory / name).read_bytes() == raw
    assert (directory / "sub dir").is_dir()


@pytest.mark.parametrize("kind", ["symlink", "directory", "fifo"])
def test_grammar_valid_non_regular_inbox_entry_is_refused_by_name(database, kind, capsys):
    directory = database.parent / "takeover-inbox"
    directory.mkdir()
    name = "ack.event-9"
    target = database.parent / "outside"
    target.write_bytes(b"{}")
    if kind == "symlink":
        (directory / name).symlink_to(target)
    elif kind == "directory":
        (directory / name).mkdir()
    else:
        os.mkfifo(directory / name)
    code, answer = write_as_python(database, capsys)
    assert code == 3
    reason = "symbolic link" if kind == "symlink" else "not a regular file"
    assert answer == {"error": "host",
                      "detail": f"ValueError: invalid takeover inbox entry: {name}: {reason}"}
    assert os.path.lexists(directory / name)


@pytest.mark.parametrize("version", [b"1.0", b"true"])
def test_inbox_version_must_be_the_integer_one(database, version, capsys):
    parser = cli.build_parser()
    request = inbox.envelope(parser, parser.parse_args(["ack", *CASES["ack"]]))
    raw = inbox.canonical(request).replace(b'"inboxVersion":1', b'"inboxVersion":' + version)
    assert inbox.canonical(json.loads(raw)) == raw
    directory = database.parent / "takeover-inbox"
    directory.mkdir()
    (directory / request["operationId"]).write_bytes(raw)
    code, answer = write_as_python(database, capsys)
    assert code == 3
    assert "invalid takeover inbox entry" in answer["detail"]
    assert (directory / request["operationId"]).read_bytes() == raw


def test_deterministic_ingress_rejects_are_usage_errors_without_io(database, capsys):
    stamp(database, owner="go")
    assert cli.main(["--state", str(database.parent), "ack", "--event", "x" * 197,
                     "--ack-turn", "t", "--ack-proof", "p"]) == 4
    answer = json.loads(capsys.readouterr().out)
    assert answer == {"error": "usage", "detail": "inbox operation ID exceeds 200 encoded characters"}
    assert not (database.parent / "takeover-inbox").exists()
    executable = Path(sys.executable).parent / "codex-session-relay"
    result = subprocess.run([str(executable), "--state", str(database.parent), "ack", "--event",
                             b"event-\xff", "--ack-turn", "t", "--ack-proof", "p"],
                            capture_output=True, timeout=15, check=False)
    assert result.returncode == 4, result
    answer = json.loads(result.stdout)
    assert answer["error"] == "usage"
    assert "utf-8" in answer["detail"]
    assert not (database.parent / "takeover-inbox").exists()


def guard_request(database, *, stop=None, deadline="2999-01-01T00:00:00+00:00", **extra):
    params = {"markerRoot": str(database.parent / "markers"), "stopInput": stop or {},
              "mode": "observe", "dbPath": str(database), "now": None, "noRecord": True,
              "deadline": deadline, **extra}
    return json.dumps({"protocol": 1, "method": "guard-evaluate", "params": params}).encode() + b"\n"


def ask(path, raw, *, timeout=10):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(timeout)
        client.connect(str(path))
        client.sendall(raw)
        answer = bytearray()
        while not answer.endswith(b"\n"):
            chunk = client.recv(65536)
            if not chunk:
                break
            answer.extend(chunk)
    return json.loads(answer) if answer else None


@pytest.mark.parametrize("fault", ["peer-credentials", "accept-emfile", "null-deadline", "nested-json",
                                   "mode-not-a-string", "now-not-a-string"])
def test_python_control_server_survives_every_per_connection_failure(database, owner_markers, fault):
    from codex_session_relay import control

    server = control.GuardServer(database.parent)
    try:
        if fault == "peer-credentials":
            with (mock.patch.object(control, "peer_uid", side_effect=OSError("credentials unavailable")),
                  pytest.raises((ConnectionResetError, BrokenPipeError))):
                # Closed unanswered, with the request unread: never evaluated. Whether the client
                # sees the reset on its send (EPIPE) or on its read (ECONNRESET) is kernel timing.
                ask(server.path, guard_request(database))
        elif fault == "accept-emfile":
            real_accept = socket.socket.accept
            failures = []

            def accept(self):
                if self is server.listener and not failures:
                    failures.append(True)
                    raise OSError(errno.EMFILE, "Too many open files")
                return real_accept(self)

            with mock.patch.object(socket.socket, "accept", accept):
                verdict = ask(server.path, guard_request(database))
            assert failures and "decision" in verdict
        elif fault == "null-deadline":
            assert ask(server.path, guard_request(database, deadline=None)) == {
                "error": "host", "detail": "TypeError: guard deadline must be a string"}
        elif fault == "mode-not-a-string":
            # Never evaluated with a mode, or recorded with a time, that is not a string.
            assert ask(server.path, guard_request(database, mode=5)) == {
                "error": "host", "detail": "TypeError: guard mode must be a string"}
        elif fault == "now-not-a-string":
            assert ask(server.path, guard_request(database, now=0)) == {
                "error": "host", "detail": "TypeError: guard now must be a string"}
        else:
            answer = ask(server.path, b'{"params":' + b"[" * 200000 + b"\n")
            assert answer["error"] == "host" and answer["detail"].startswith("RecursionError")
        # The only serving thread is still alive and answers the next Stop.
        verdict = ask(server.path, guard_request(database))
        assert "decision" in verdict and "hook_output" in verdict
    finally:
        server.close()


def test_python_control_server_bounds_the_whole_request_line(database, owner_markers, monkeypatch):
    """The read bound is on the request line, from the moment the peer is served, not on each read.

    A peer that sends its line in parts, each well within the bound, the whole line past it, is
    answered TimeoutError('timed out') when the bound is spent, as the Go owner answers it; a line
    sent in parts within the bound is served.
    """
    from codex_session_relay import control

    monkeypatch.setattr(control, "READ_TIMEOUT", 1.0)
    request = guard_request(database)
    parts = [request[:10], request[10:20], request[20:]]

    def trickled(gap):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(10)
            client.connect(str(server.path))
            for index, part in enumerate(parts):
                # gap between parts, and no more parts once the owner has answered.
                if index and select_readable(client, gap):
                    break
                client.sendall(part)
            answer = bytearray()
            while not answer.endswith(b"\n"):
                chunk = client.recv(65536)
                if not chunk:
                    break
                answer.extend(chunk)
        return json.loads(answer)

    server = control.GuardServer(database.parent)
    try:
        assert trickled(0.6) == {"error": "host", "detail": "TimeoutError: timed out"}
        verdict = trickled(0.05)
        assert "decision" in verdict and "hook_output" in verdict
    finally:
        server.close()


def select_readable(connection, seconds):
    """Whether connection has something to read within seconds."""
    with selectors.DefaultSelector() as selector:
        selector.register(connection, selectors.EVENT_READ)
        return bool(selector.select(seconds))


def test_darwin_peer_credentials_read_local_peercred(monkeypatch):
    # No darwin host runs this suite; the xucred layout is the one Go's peer_darwin.go reads.
    import struct as packing

    from codex_session_relay import control

    class Connection:
        def getsockopt(self, level, option, size):
            assert (level, option, size) == (0, 0x001, 76)
            return packing.pack("=II", 0, 501) + bytes(68)

    # The server reads its peer through the Stop client's reader: one implementation.
    assert control.peer_uid is stopadapter.peer_uid
    monkeypatch.delattr(stopadapter.socket, "SO_PEERCRED", raising=False)
    monkeypatch.delattr(stopadapter.socket, "SOL_LOCAL", raising=False)
    monkeypatch.delattr(stopadapter.socket, "LOCAL_PEERCRED", raising=False)
    monkeypatch.setattr(stopadapter.sys, "platform", "darwin")
    assert control.peer_uid(Connection()) == 501
    monkeypatch.setattr(stopadapter.sys, "platform", "sunos5")
    with pytest.raises(OSError, match="credentials unavailable"):
        control.peer_uid(Connection())


@pytest.mark.parametrize("platform", ["darwin", "linux"])
@pytest.mark.parametrize("peer", ["ours", "foreign"])
def test_the_stop_client_authenticates_the_owner_with_its_platform_credentials(
        database, monkeypatch, platform, peer):
    """PR #185 thread 4128791771: the Stop client reads the owner as the owner reads the client.

    Under a Go-owned store the Python Stop adapter has no local fallback, so a client that could
    read only Linux SO_PEERCRED refused every Darwin Stop as store_owned_by_other and lost the
    owner's verdict. No darwin host runs this suite: the darwin cases answer the xucred layout
    (SOL_LOCAL/LOCAL_PEERCRED, as Go's peer_darwin.go reads it) on a real connection through a
    faked getsockopt. The linux cases read the kernel's real SO_PEERCRED for our uid, and a
    forged ucred for a foreign one. Our uid gets the owner's verdict; a foreign uid is refused
    before a byte of the Stop is sent.
    """
    import struct as packing

    stamp(database, owner="go")
    state = database.parent
    ours = os.getuid()
    reported = ours if peer == "ours" else ours + 1
    verdict = {"decision": "release", "hook_output": {}}
    config = {"relayExecutable": "must-not-execute", "markerRoot": str(state / "markers"),
              "dbPath": str(database), "mode": "observe"}
    real = socket.socket.getsockopt
    asked = []

    def getsockopt(self, level, option, *size):
        asked.append((level, option, *size))
        if platform == "darwin" and (level, option, *size) == (0, 0x001, 76):
            return packing.pack("=II", 0, reported) + bytes(68)
        if platform == "linux" and peer == "foreign":
            return packing.pack("3i", os.getpid(), reported, os.getgid())
        return real(self, level, option, *size)

    if platform == "darwin":
        monkeypatch.delattr(socket, "SO_PEERCRED", raising=False)
        monkeypatch.delattr(socket, "SOL_LOCAL", raising=False)
        monkeypatch.delattr(socket, "LOCAL_PEERCRED", raising=False)
        monkeypatch.setattr(sys, "platform", "darwin")
    elif not hasattr(socket, "SO_PEERCRED"):
        pytest.fail("the linux case needs the kernel's SO_PEERCRED")
    received = []
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(state / "control.sock"))
        server.listen(1)
        server.settimeout(10)

        def owner():
            connection, _ = server.accept()
            with connection:
                connection.settimeout(10)
                with connection.makefile("rb") as stream:
                    line = stream.readline()
                received.append(line)
                if line:
                    connection.sendall(json.dumps(verdict).encode() + b"\n")

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(owner)
            with (mock.patch.object(socket.socket, "getsockopt", getsockopt),
                  mock.patch.object(stopadapter.subprocess, "Popen",
                                    side_effect=AssertionError("spawned"))):
                result = stopadapter.invoke_guard(config, b'{"turn_id":"one"}')
            future.result(timeout=10)
    if peer == "ours":
        assert json.loads(result["stdout"]) == verdict and result["code"] == 0
        assert json.loads(received[0])["params"]["stopInput"] == {"turn_id": "one"}
    else:
        assert json.loads(result["stdout"]) == {
            "error": "refused", "reason": "store_owned_by_other",
            "detail": "the owner could not answer guard-evaluate: guard peer uid"
                      f" {reported} differs from our uid {ours}"}
        assert received == [b""]
    # The platform's own credential call, and only that one.
    assert asked == [(0, 0x001, 76) if platform == "darwin"
                     else (socket.SOL_SOCKET, socket.SO_PEERCRED, packing.calcsize("3i"))]


def test_python_control_server_accepts_the_64_mib_frame(database, owner_markers):
    from codex_session_relay.control import GuardServer

    server = GuardServer(database.parent)
    try:
        verdict = ask(server.path, guard_request(database, stop={"last_assistant_message": "x" * (20 << 20)}),
                      timeout=30)
        assert "decision" in verdict, verdict
    finally:
        server.close()


@pytest.mark.parametrize("behaviour", ["large", "silent", "stall", "garbage", "reset", "unread"])
def test_go_owner_socket_answers_after_sending_are_never_invented_refusals(database, behaviour):
    stamp(database, owner="go")
    large = {"decision": "release", "hook_output": {}, "padding": "y" * (20 << 20)}
    config = {"relayExecutable": "must-not-execute", "markerRoot": str(database.parent / "markers"),
              "dbPath": str(database), "mode": "observe", "timeoutSeconds": 0.5}
    stop_serving = concurrent.futures.Future()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(database.parent / "control.sock"))
        server.listen(1)
        server.settimeout(10)

        def answer():
            connection, _ = server.accept()
            with connection:
                connection.settimeout(10)
                if behaviour == "unread":
                    # Accepts and never reads: a large Stop payload misses the deadline mid-send.
                    stop_serving.result(timeout=10)
                    return
                if behaviour == "reset":
                    # Closing with the request unread resets the sender's connection.
                    connection.recv(1)
                    return
                with connection.makefile("rb") as stream:
                    stream.readline()
                if behaviour == "large":
                    connection.sendall(json.dumps(large).encode() + b"\n")
                elif behaviour == "garbage":
                    connection.sendall(b"\xff not json\n")
                elif behaviour == "stall":
                    stop_serving.result(timeout=10)

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(answer)
            try:
                with mock.patch.object(stopadapter.subprocess, "Popen", side_effect=AssertionError("spawned")):
                    if behaviour == "reset":
                        with pytest.raises(OSError):
                            stopadapter.socket_guard(config, b"{}")
                        return
                    payload = (json.dumps({"last_assistant_message": "x" * (20 << 20)}).encode()
                               if behaviour == "unread" else b"{}")
                    result = stopadapter.invoke_guard(config, payload)
            finally:
                stop_serving.set_result(None)
                future.result(timeout=10)
    said, value = stopadapter.read_guard_stdout(result["stdout"])
    outcome = stopadapter.outcome_of(result, said, value)
    expected = {"large": stopadapter.GUARD_ANSWERED, "silent": stopadapter.GUARD_SAID_NOTHING,
                "stall": stopadapter.GUARD_TIMED_OUT, "garbage": stopadapter.GUARD_OUTPUT_UNREADABLE,
                "unread": stopadapter.GUARD_TIMED_OUT}
    assert outcome == expected[behaviour], (result, said)
    if behaviour == "large":
        assert json.loads(result["stdout"]) == large


@pytest.mark.parametrize(("reply", "limit"), [
    pytest.param(b"", None, id="silent"),
    pytest.param(b"null\n", None, id="null"),
    pytest.param(b"\xff not json\n", None, id="not-json"),
    # JSON once each byte that is not UTF-8 is replaced with U+FFFD, and still not an answer:
    # the owner's error record does not lend it its exit status, nor is it printed with exit 0.
    pytest.param(b'{"a": "\xff"}\n', None, id="object-not-utf8"),
    pytest.param(b'{"error": "refused", "reason": "\xff"}\n', None, id="refusal-not-utf8"),
    pytest.param(b'{"error": "host", "detail": "\xc3"}\n', None, id="host-not-utf8"),
    pytest.param(b'"\xff"\n', None, id="string-not-utf8"),
    # One byte over the frame limit (16 here), where those 17 bytes are a whole JSON string.
    pytest.param(b'"' + b"a" * 15 + b'"\n', 16, id="over-the-frame-limit"),
])
def test_guard_evaluate_cli_reports_an_owner_that_says_nothing_readable_as_a_host_error(
        database, tmp_path, reply, limit):
    stamp(database, owner="go")
    stop = tmp_path / "stop.json"
    # A Stop that reads its receipt, so the owner is asked: a marker-only one never is.
    stop.write_text(json.dumps(ready_for_review(tmp_path / "markers", tmp_path / "work")))
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(database.parent / "control.sock"))
        server.listen(1)
        server.settimeout(10)

        def owner():
            connection, _ = server.accept()
            with connection, connection.makefile("rb") as stream:
                stream.readline()
                connection.sendall(reply)

        with (concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor,
              mock.patch.object(stopadapter, "MAX_GUARD_FRAME",
                                limit or stopadapter.MAX_GUARD_FRAME)):
            future = executor.submit(owner)
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                code = cli.main(["--state", str(database.parent), "guard-evaluate", "--marker-root",
                                 str(tmp_path / "markers"), "--stop-input", str(stop),
                                 "--db-path", str(database)])
            future.result(timeout=10)
    assert code == 3
    assert json.loads(output.getvalue()) == {
        "error": "host", "detail": "the owner closed control.sock without a readable guard-evaluate answer"}


@pytest.mark.parametrize("reply", [
    pytest.param(b'{"decision": "release", "state": "\xff", "hook_output": {}}\n', id="verdict-not-utf8"),
    pytest.param(b'{"decision": "block", "state": "declared", "hook_output": {"decision": "block",'
                 b' "reason": "verify \xff", "continue": true}}\n', id="hold-not-utf8"),
    pytest.param(b'{"error": "refused", "reason": "\xff"}\n', id="error-record-not-utf8"),
])
def test_the_stop_adapters_pinned_route_reads_an_answer_that_is_not_utf8_as_unreadable(
        database, tmp_path, reply):
    """cutover.md: once sent, unreadable bytes are guard_output_unreadable, as Go classifies them.

    Each byte that is not UTF-8 replaced with U+FFFD, these answers parse: the adapter neither
    acts on the verdict (a hold would be printed) nor reads the owner's error record.
    """
    stamp(database, owner="go")
    path = tmp_path / stopadapter.CONFIG_NAME
    path.write_text(json.dumps({
        "configVersion": stopadapter.CONFIG_VERSION, "event": stopadapter.EVENT,
        "relayExecutable": str(tmp_path / "must-not-execute"), "markerRoot": str(tmp_path / "markers"),
        "dbPath": str(database), "mode": stopadapter.OBSERVE, "timeoutSeconds": 5,
        "journalRoot": str(tmp_path / "journal"), "journalPolicy": stopadapter.EVERY_INVOCATION,
        "installedBy": "CRW-115", "isolationAssertedBy": None, "owner": stopadapter.OWNER_PLUGIN,
        "adapterInterpreter": sys.executable,
        "adapterEntryPoint": str(Path(stopadapter.__file__).resolve()),
    }), encoding="utf-8")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(database.parent / "control.sock"))
        server.listen(1)
        server.settimeout(10)

        def owner():
            connection, _ = server.accept()
            with connection, connection.makefile("rb") as stream:
                stream.readline()
                connection.sendall(reply)

        with (concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor,
              mock.patch.object(stopadapter.subprocess, "Popen", side_effect=AssertionError("spawned"))):
            future = executor.submit(owner)
            printed = stopadapter.run(b"{}", codex_home=tmp_path / "codex-home", environ={},
                                      settings=str(path))
            future.result(timeout=10)
    assert printed is None
    [row] = [json.loads(entry.read_text()) for entry in (tmp_path / "journal").rglob("*.json")]
    assert row["adapterOutcome"] == stopadapter.GUARD_OUTPUT_UNREADABLE, row
    assert row["stdoutReading"] == stopadapter.SAID_SOMETHING_UNREADABLE
    assert (row["held"], row["guardState"], row["guardDecision"]) == (False, None, None)


def test_a_marker_only_stop_is_judged_whoever_owns_the_store_it_never_reads(database, tmp_path):
    """PR #185 4128954348: a verdict that reads no receipt is published as before the fence.

    A child that declared itself waiting for input is released on its own declaration, and no
    store is read for it. Its default store belongs to Go and no control.sock listens, which
    refuses any Stop that does read that store; this one is judged here, answered and recorded
    exactly as on a store Python owns, and the Go store is left as it was.
    """
    from codex_session_relay import marker

    stamp(database, owner="go")
    assert not (database.parent / "control.sock").exists()
    unfenced = tmp_path / "python-state"
    Store(unfenced / "relay.sqlite3").close()
    program = str(Path(sys.executable).parent / "codex-session-relay")
    now = "2026-01-01T00:00:00.123456+00:00"
    stop = {"cwd": None, "session_id": "s", "turn_id": "t", "stop_hook_active": False}
    before = files(database.parent)
    answers = {}
    for name, state in (("go-owned", database.parent), ("python-owned", unfenced)):
        root, work = tmp_path / name / "markers", tmp_path / name / "work"
        work.mkdir(parents=True)
        directory = marker.assignment_dir(root, work, marker.assignment_id("dispatch"))
        for path, record in {
            "intent.json": {"dispatchRequestIdHash": marker.assignment_id("dispatch")},
            "bound.json": {"sessionId": "s"},
            "relationship.json": {"relationshipId": "r"},
            "claims/s/claim.json": {"sessionId": "s", "dispatchRequestId": "dispatch"},
            "dispositions/s/t.json": {"sessionId": "s", "turnId": "t",
                                      "outcome": "blocked_needs_input"},
        }.items():
            marker.publish(directory / path, record, root=root)
        ran = subprocess.run(
            [program, "--state", str(state), "guard-evaluate", "--marker-root", str(root),
             "--mode", "hold", "--now", now],
            input=json.dumps(dict(stop, cwd=str(work))).encode(), cwd=tmp_path,
            capture_output=True, timeout=30, check=False)
        assert ran.returncode == 0 and not ran.stderr, (name, ran)
        answer = json.loads(ran.stdout)
        recorded = directory / (answer["recordedAs"] + ".json")
        assert json.loads(recorded.read_bytes()) == answer["record"], name
        answers[name] = answer
    verdict = answers["go-owned"]
    assert verdict["decision"] == "release", verdict
    assert verdict["observation"] == "declared_blocked_needs_input", verdict
    assert verdict["recordedAs"] == "hook/s/t/0", verdict
    assert verdict == answers["python-owned"]
    assert files(database.parent) == before, "a marker-only Stop touched the Go-owned store"


@pytest.mark.parametrize("case", ["wrong_socket", "override", "ambiguous", "unidentified"])
def test_python_control_server_shares_the_cli_selection_refusals(case):
    """Decision 24: a Go-shaped control request gets the CLI's refusal, never a bypass."""
    import contextlib as context

    from codex_session_relay import marker

    now = "2026-01-01T00:00:00.123456+00:00"
    with tempfile.TemporaryDirectory(prefix="sel-") as temporary:
        home = Path(temporary)
        environment = dict(os.environ, HOME=str(home), CODEX_HOME=str(home),
                           XDG_STATE_HOME=str(home / "xdg"),
                           CODEX_SESSION_RELAY_MARKER_ROOT=str(home / "markers"),
                           CODEX_SESSION_RELAY_STATE=str(home / "state") if case in (
                               "wrong_socket", "override") else "")
        wanted = home / "app.sock"
        if case == "wrong_socket":
            with context.closing(Store(home / "state/relay.sqlite3", socket_path=str(home / "other.sock"))):
                pass
        else:
            for name in ["a", "b"] if case in ("ambiguous", "override") else ["a"]:
                with context.closing(Store(home / "xdg/codex-session-relay" / name / "relay.sqlite3",
                                           socket_path=None if case == "unidentified" else str(wanted))):
                    pass
            if case == "override":
                with context.closing(Store(home / "state/relay.sqlite3", socket_path=str(wanted))):
                    pass
        with mock.patch.dict(os.environ, environment, clear=True):
            state = resolve_state_dir(None, str(wanted)).path
        state.mkdir(mode=0o700, parents=True, exist_ok=True)
        root, work = home / "markers", home / "work"
        work.mkdir()
        directory = marker.assignment_dir(root, work, marker.assignment_id("dispatch"))
        for name, record in {
            "intent.json": {"dispatchRequestIdHash": marker.assignment_id("dispatch")},
            "bound.json": {"sessionId": "s"},
            "relationship.json": {"relationshipId": "r"},
            "claims/s/claim.json": {"sessionId": "s", "dispatchRequestId": "dispatch"},
            "dispositions/s/t.json": {"sessionId": "s", "turnId": "t", "outcome": "ready_for_review"},
        }.items():
            marker.publish(directory / name, record, root=root)
        stop = {"cwd": str(work), "session_id": "s", "turn_id": "t", "stop_hook_active": False}
        program = str(Path(sys.executable).parent / "codex-session-relay")
        expected = subprocess.run(
            [program, "--socket", str(wanted), "guard-evaluate", "--marker-root", str(root),
             "--mode", "hold", "--now", now],
            input=json.dumps(stop).encode(), env=environment, cwd=home, capture_output=True,
            timeout=15, check=False)
        assert expected.returncode == 2 and not expected.stderr, expected
        server = subprocess.Popen(
            [sys.executable, "-c", ("import sys\nfrom codex_session_relay.control import GuardServer\n"
                                    "server = GuardServer(sys.argv[1])\nprint('ready', flush=True)\n"
                                    "sys.stdin.readline()\nserver.close()\n"), str(state)],
            env=environment, cwd=home, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE)
        try:
            assert line(server) == b"ready\n"
            params = {"markerRoot": str(root), "stopInput": stop, "mode": "hold", "dbPath": None,
                      "now": now, "noRecord": False, "deadline": "2999-01-01T00:00:00Z",
                      "socketPath": str(wanted), "program": program}
            answer = ask(state / "control.sock", json.dumps(
                {"protocol": 1, "method": "guard-evaluate", "params": params}).encode() + b"\n")
        finally:
            server.communicate(b"stop\n", timeout=15)
        assert answer == json.loads(expected.stdout), case
        assert answer["error"] == "refused"
        assert not list(root.glob("*/*/hook/*/*/*.json")), "a refusal recorded an observation"


def strip_fence(path):
    """A pre-fence (legacy) store: the same database without any ownership half or gate."""
    with sqlite3.connect(path) as db:
        db.executemany("DELETE FROM schema_meta WHERE key=?", [(key,) for key in ownership.KEYS])
    db.close()
    (path.parent / "takeover.json").unlink()
    (path.parent / "write-gate.lock").unlink()


def test_first_fence_of_a_legacy_store_refuses_a_disagreeing_socket(tmp_path):
    path = tmp_path / "state" / "relay.sqlite3"
    Store(path, socket_path=str(tmp_path / "a.sock")).close()
    strip_fence(path)
    with pytest.raises(ownership.OwnershipRefused, match="requested socket disagrees with the recorded"):
        Store(path, socket_path=str(tmp_path / "b.sock"))
    assert ownership.mirror(path) is None
    assert not any(key in ownership.metadata(path) for key in ownership.KEYS)
    # The store's own socket fences it, recording the same socket in both halves.
    Store(path, socket_path=str(tmp_path / "a.sock")).close()
    record = ownership.mirror(path)
    assert record["appServerSocket"] == ownership.metadata(path)["socket_path"] == str(tmp_path / "a.sock")
    assert record["scopeKey"] == ownership.scope_key(str(tmp_path / "a.sock"))


def relay(*argv):
    executable = Path(sys.executable).parent / "codex-session-relay"
    result = subprocess.run([str(executable), *argv], capture_output=True, timeout=60, check=False)
    return result.returncode, json.loads(result.stdout) if result.stdout.strip() else None


def test_the_documented_socketless_first_flow_binds_the_store_to_its_first_socket(tmp_path):
    """crw-run relay.md: `--state S register` (no socket), later `--state S --socket K deliver`."""
    state, app = tmp_path / "state", tmp_path / "app.sock"
    path = state / "relay.sqlite3"
    assert relay("--state", str(state), "store-challenge", "--write")[0] == 0
    assert ownership.mirror(path)["appServerSocket"] is None
    # A read-only command never binds, even when it names a socket.
    assert relay("--state", str(state), "--socket", str(app), "status")[0] == 0
    assert ownership.mirror(path)["appServerSocket"] is None
    assert "socket_path" not in ownership.metadata(path)
    # The first socketed writable opener binds both halves in one transition.
    assert relay("--state", str(state), "--socket", str(app), "store-challenge", "--write")[0] == 0
    record, meta = ownership.mirror(path), ownership.metadata(path)
    assert record["appServerSocket"] == meta["socket_path"] == str(app)
    assert record["scopeKey"] == ownership.scope_key(str(app))
    assert (record["owner"], record["epoch"], record["phase"], record["transition"]) == (
        "python", 1, "active", None)
    ownership.check_start(path)
    # Socketless and same-socket opens keep working; another socket is refused.
    assert relay("--state", str(state), "store-challenge", "--write")[0] == 0
    assert relay("--state", str(state), "--socket", str(app), "store-challenge", "--write")[0] == 0
    with pytest.raises(ownership.OwnershipRefused, match="requested socket disagrees with ownership record"):
        Store(path, socket_path=str(tmp_path / "other.sock"))
    assert ownership.mirror(path)["appServerSocket"] == str(app)


def test_a_torn_socket_binding_is_completed_only_by_its_own_socket(database, tmp_path, monkeypatch):
    app, other = str(tmp_path / "app.sock"), str(tmp_path / "other.sock")
    record = ownership.mirror(database)
    with monkeypatch.context() as patched:
        patched.setattr(ownership, "_publish", mock.Mock(side_effect=OSError(errno.ENOSPC, "full")))
        with pytest.raises(OSError):
            Store(database, socket_path=app)
    # The DB half committed first; the mirror is unchanged.
    assert ownership.metadata(database)["socket_path"] == app
    assert ownership.mirror(database) == record
    for opener in (lambda: Store(database), lambda: Store(database, socket_path=other),
                   lambda: Store(database, socket_path=app, bind_socket=False),
                   lambda: ownership.check_start(database)):
        with pytest.raises(ownership.OwnershipRefused):
            opener()
    ownership.check_start(database, socket=app)
    # The CLI's own preflight lets the completing opener through.
    code, answer = relay("--state", str(database.parent), "--socket", app, "store-challenge", "--write")
    assert code == 0, answer
    assert ownership.mirror(database)["appServerSocket"] == app
    assert ownership.mirror(database)["scopeKey"] == ownership.scope_key(app)
    Store(database).close()


def test_service_preflights_let_the_completing_opener_through_a_torn_binding(database, tmp_path):
    app = str(tmp_path / "app.sock")
    with sqlite3.connect(database) as db:
        db.execute("INSERT INTO schema_meta VALUES ('socket_path', ?)", (app,))
    db.close()
    service = RelayService(resolve_state_dir(database.parent), socket_path=app)
    with mock.patch.object(service, "default_launcher"):
        assert service.start().get("reason") != "store_owned_by_other"
    service.enable(actor="test")
    launches = []

    class Worker:
        pid = os.getpid()

        def wait(self):
            return 0

    def spawn(**kwargs):
        launches.append(kwargs)
        return Worker()

    result = service.supervise(spawn=spawn, max_segments=2, sleeper=lambda _: None)
    assert result["segments"] == [0, 0] and len(launches) == 2
    # Without the socket, the same preflight refuses the torn record.
    with pytest.raises(Exception, match="scope without socket"):
        RelayService(resolve_state_dir(database.parent)).supervise(
            spawn=spawn, max_segments=1, sleeper=lambda _: None)


def test_python_never_binds_a_store_it_does_not_own_or_that_is_mid_transition(database, tmp_path):
    app = str(tmp_path / "app.sock")
    for owner, phase in (("go", "active"), ("python", "draining")):
        stamp(database, owner=owner, phase=phase)
        before = files(database.parent)
        with pytest.raises(ownership.OwnershipRefused) as caught:
            Store(database, socket_path=app)
        assert caught.value.queueable
        assert files(database.parent) == before
    stamp(database)
    Store(database, socket_path=app).close()
    assert ownership.mirror(database)["appServerSocket"] == app


def test_socket_binding_waits_for_other_writers_within_the_declared_bound(database, tmp_path, monkeypatch):
    app = str(tmp_path / "app.sock")
    monkeypatch.setattr(ownership, "LOCK_WAIT_SECONDS", 0.3)
    holder = Store(database)
    try:
        before = files(database.parent)
        with pytest.raises(ownership.LockWaitExpired):
            Store(database, socket_path=app)
        assert files(database.parent) == before
    finally:
        holder.close()
    Store(database, socket_path=app).close()
    assert ownership.mirror(database)["appServerSocket"] == app


@pytest.mark.parametrize("pause", ["before-first-lock", "after-gate-placed"])
def test_concurrent_first_openers_never_see_a_partial_store(tmp_path, pause):
    """The gate is placed already held EX, so a racing first opener waits instead of refusing."""
    path = tmp_path / "state" / "relay.sqlite3"
    first = subprocess.Popen([sys.executable, "-u", "-c", """
import fcntl, os, sys
from codex_session_relay.store import Store
pause, paused = sys.argv[2], []
def wait():
    if not paused:
        paused.append(True)
        print('paused', flush=True)
        sys.stdin.readline()
real_flock, real_link = fcntl.flock, os.link
def flock(fd, operation):
    if pause == 'before-first-lock':
        wait()
    return real_flock(fd, operation)
def link(source, target, **options):
    real_link(source, target, **options)
    if pause == 'after-gate-placed' and os.path.basename(target) == 'write-gate.lock':
        wait()
fcntl.flock, os.link = flock, link
Store(sys.argv[1]).close()
print('created', flush=True)
""", str(path), pause], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    second = None
    try:
        assert line(first) == b"paused\n"
        second = subprocess.Popen([sys.executable, "-c", """
import sys
from codex_session_relay.store import Store
Store(sys.argv[1]).close()
""", str(path)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if pause == "after-gate-placed":
            with pytest.raises(subprocess.TimeoutExpired):
                second.wait(timeout=1)  # waits on the gate its creator holds
        else:
            assert second.wait(timeout=30) == 0, second.stderr.read()
        first.stdin.write(b"go\n")
        first.stdin.flush()
        assert line(first) == b"created\n", first.stderr.read()
        assert second.wait(timeout=30) == 0, second.stderr.read()
        assert first.wait(timeout=30) == 0
    finally:
        for process in (first, second):
            if process is not None and process.poll() is None:
                process.kill()
                process.wait()
    assert ownership.metadata(path)["owner"] == "python"
    ownership.check_start(path)
    assert sorted(name.name for name in path.parent.iterdir() if name.name.startswith(".write-gate")) == []


@pytest.mark.parametrize("drift,refusal", [
    ("meta-socket", "scope without socket"),
    ("scope-key", "invalid socket/scope identity"),
    ("mirror-socket", "invalid socket/scope identity"),
])
def test_a_drifted_scope_identity_refuses_like_go(tmp_path, drift, refusal):
    path = tmp_path / "state" / "relay.sqlite3"
    socket_path = str(tmp_path / "app.sock")
    Store(path, socket_path=None if drift == "meta-socket" else socket_path).close()
    record = ownership.mirror(path)
    if drift == "meta-socket":
        with sqlite3.connect(path) as db:
            db.execute("INSERT INTO schema_meta VALUES ('socket_path', ?)", (socket_path,))
        db.close()
    elif drift == "scope-key":
        record["scopeKey"] = None
    else:
        record["appServerSocket"] = str(tmp_path / "other.sock")
    (path.parent / "takeover.json").write_bytes(inbox.canonical(record))
    with pytest.raises(ownership.OwnershipRefused, match=refusal):
        Store(path)
    with pytest.raises(ownership.OwnershipRefused, match=refusal):
        ownership.check_start(path)


def test_a_gate_without_a_database_is_partial_and_never_initialized(tmp_path):
    state = tmp_path / "state"
    state.mkdir(mode=0o700)
    (state / "write-gate.lock").touch(mode=0o600)
    with pytest.raises(ownership.OwnershipRefused, match="partial store"):
        Store(state / "relay.sqlite3")
    assert sorted(path.name for path in state.iterdir()) == ["write-gate.lock"]
    # A truly absent store is still created by the first writable opener.
    fresh = tmp_path / "fresh" / "relay.sqlite3"
    Store(fresh).close()
    assert ownership.metadata(fresh)["owner"] == "python"


@pytest.mark.parametrize("argv", [["status"], ["store-identity"], ["fault-show"],
                                  ["show", "--event", "e"]])
def test_read_only_commands_never_create_an_absent_store(tmp_path, argv):
    state = tmp_path / "state"
    executable = Path(sys.executable).parent / "codex-session-relay"
    result = subprocess.run([str(executable), "--state", str(state), *argv],
                            capture_output=True, timeout=15, check=False)
    assert result.returncode == 2, result
    assert json.loads(result.stdout) == {
        "error": "refused", "reason": "store_absent",
        "detail": f"no relay store exists at {state / 'relay.sqlite3'}; a read-only command"
                  " never creates one"}
    assert not state.exists() or list(state.iterdir()) == []


def test_doctor_reports_a_stamp_without_its_mirror(database, capsys):
    (database.parent / "takeover.json").unlink()
    assert cli.main(["--state", str(database.parent), "doctor"]) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["ownership"]["owner"] == "python"
    assert report["ownership"]["detail"] == "takeover record missing"


def test_marker_commands_are_fenced_by_the_store_their_intent_names(database, tmp_path):
    """PR #185 thread 3: marker-only commands skip the selection; the intent's store fences."""
    from codex_session_relay import intent, marker

    markers, workspace = tmp_path / "markers", tmp_path / "work"
    workspace.mkdir()
    declared = intent.declare_intent(
        markers, workspace=workspace, dispatch_request_id="dispatch-go", issue_key="REL-1",
        declared_at="2026-09-29T00:00:00+00:00", db_path=database)
    stamp(database, owner="go")
    unrelated = tmp_path / "selected"
    executable = Path(sys.executable).parent / "codex-session-relay"
    common = ["--marker-root", str(markers), "--workspace", str(workspace),
              "--assignment", declared["assignmentId"]]

    def run(*argv):
        before = files(database.parent)
        result = subprocess.run([str(executable), "--state", str(unrelated), *argv, *common],
                                capture_output=True, timeout=15, check=False)
        assert files(database.parent) == before, argv
        assert not unrelated.exists(), argv
        return result.returncode, json.loads(result.stdout)

    directory = marker.assignment_dir(markers, workspace, declared["assignmentId"])
    code, answer = run("intent-attempt", "--outcome", "accepted", "--task-id", "task-1")
    assert code == 0, answer
    code, answer = run("intent-bind", "--session", "session-1", "--task-id", "task-1")
    assert code == 0, answer
    code, answer = run("intent-resolve", "--chosen-task", "task-1", "--chosen-session", "session-1",
                       "--reason", "test", "--adjudicate", "operator")
    assert answer.get("reason") != "store_owned_by_other", answer
    for argv, published in ((["intent-disposition", "--session", "session-1", "--turn", "t1",
                              "--outcome", "ready_for_review"], "dispositions"),
                            (["intent-claim", "--session", "session-1",
                              "--dispatch-request-id", "dispatch-go"], "claims")):
        code, answer = run(*argv)
        assert code == 2 and answer["reason"] == "store_owned_by_other", (argv, answer)
        assert not list((directory / published).rglob("*.json")), argv


# Runs the relay CLI in a fresh interpreter with an audit hook that only OBSERVES: every
# write-capable open, directory creation, temporary file or directory, copy and rename the
# process asks Python for is logged to an inherited descriptor (writing to it raises no event).
# SQLite's own C-level sidecar creation is invisible here and is checked on disk instead.
_AUDITED_CLI = r'''
import os, sys
log = os.fdopen(int(os.environ.pop("CRW_TEST_AUDIT_FD")), "w", buffering=1)
WRITE = os.O_WRONLY | os.O_RDWR | os.O_CREAT | os.O_TRUNC | os.O_APPEND
EVENTS = ("os.mkdir", "tempfile.mkdtemp", "tempfile.mkstemp", "shutil.copyfile",
          "shutil.copytree", "os.rename", "os.replace", "os.link", "os.symlink")
def audit(event, args):
    if event == "open":
        path, mode, flags = args
        if isinstance(path, (str, bytes, os.PathLike)) and (
                (isinstance(mode, str) and any(c in mode for c in "wax+")) or (flags or 0) & WRITE):
            log.write("open " + os.fsdecode(path) + "\n")
    elif event in EVENTS:
        log.write(event + " " + repr(args) + "\n")
sys.addaudithook(audit)
from codex_session_relay.cli import main
sys.exit(main(sys.argv[1:]))
'''


def audited_guard_evaluate(state, markers, stop, cwd):
    """One real guard-evaluate process; returns (exit code, answer, logged write requests)."""
    read, write = os.pipe()
    try:
        done = subprocess.run(
            [sys.executable, "-c", _AUDITED_CLI, "--state", str(state), "guard-evaluate",
             "--marker-root", str(markers)],
            input=json.dumps(stop).encode(), capture_output=True, timeout=30, check=False,
            cwd=cwd, pass_fds=(write,), env=dict(os.environ, CRW_TEST_AUDIT_FD=str(write)))
        os.close(write)
        write = None
        with os.fdopen(read, "r") as stream:
            read = None
            logged = stream.read().splitlines()
    finally:
        for descriptor in (read, write):
            if descriptor is not None:
                os.close(descriptor)
    return done.returncode, json.loads(done.stdout), logged


@pytest.mark.parametrize("case", ["active", "draining", "live-wal-owner"])
def test_a_stop_verifies_its_owner_without_copying_the_store_or_making_sidecars(database, tmp_path,
                                                                                case):
    """The read-only Stop path (cutover.md Lock order) reads the ownership halves in place.

    No temporary copy of the store (the hook comparison's writesOutsideRoot) and no SQLite
    sidecar beside it; the verdict is still check_start's: a draining store refuses, and an owner
    change committed only to the live WAL is read, not the stale main file, so the mirror that
    disagrees with it refuses. Those Stops read their receipt from D, the store their intent
    records, so its owner decides them; the active case's unmanaged Stop reads no receipt and
    asks no owner (PR #185 4128954348). An active Python store admitting a Stop that reads its
    receipt is test_a_stop_spends_one_receipt_read_and_one_evaluation[local], whose receipt read
    is what creates the sidecars this check forbids.
    """
    markers = tmp_path / "markers"
    if case == "active":
        workspace = tmp_path / "unmanaged-workspace"
        workspace.mkdir()
        stop = {"cwd": str(workspace), "session_id": "s", "turn_id": "t", "stop_hook_active": False}
    else:
        stop = ready_for_review(markers, tmp_path / "managed-workspace", db_path=database)
    writer = None
    if case == "draining":
        stamp(database, phase="draining")
    elif case == "live-wal-owner":
        # A connection that keeps its commit in the WAL: the durable owner is go, D says python.
        writer = sqlite3.connect(database)
        writer.execute("PRAGMA wal_autocheckpoint=0")
        writer.execute("UPDATE schema_meta SET value='go' WHERE key='owner'")
        writer.commit()
        with contextlib.closing(sqlite3.connect(f"{database.as_uri()}?mode=ro&immutable=1",
                                                uri=True)) as stale:
            assert dict(stale.execute("SELECT key,value FROM schema_meta"))["owner"] == "python"
    before = files(database.parent)
    names = sorted(path.name for path in database.parent.iterdir())
    try:
        code, answer, logged = audited_guard_evaluate(database.parent, markers, stop, tmp_path)
        # The live connection's own files, untouched by the reader: nothing added or removed.
        assert sorted(path.name for path in database.parent.iterdir()) == names
    finally:
        if writer is not None:
            writer.close()
    assert logged == [], logged
    if case != "live-wal-owner":
        assert files(database.parent) == before
        assert not Path(str(database) + "-wal").exists() and not Path(str(database) + "-shm").exists()
    assert not list(markers.rglob("hook")), "an observation was recorded"
    if case == "active":
        assert code == 0 and answer["decision"] == "release", answer
        assert answer["state"] == "unmanaged", answer
    else:
        assert code == 2 and answer["reason"] == "store_owned_by_other", answer
        assert answer["detail"] == ("the relay store is draining" if case == "draining"
                                    else "ownership record disagrees with the durable store")


# A live retained Python owner in its own environment, as a daemon serves control.sock. It
# reports how many requests it answered, so a test can tell a routed Stop from a local one.
_COUNTING_OWNER = """
import sys
from codex_session_relay import control
answer = control.GuardServer._answer
def counted(self, connection):
    print("asked", flush=True)
    return answer(self, connection)
control.GuardServer._answer = counted
server = control.GuardServer(sys.argv[1])
print("ready", flush=True)
sys.stdin.readline()
server.close()
"""


@contextlib.contextmanager
def python_owner(state, environment):
    """Yields a list that holds, once the owner has stopped, how many requests it answered."""
    server = subprocess.Popen([sys.executable, "-c", _COUNTING_OWNER, str(state)], env=environment,
                              stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    asked = []
    try:
        assert line(server) == b"ready\n", server
        yield asked
    finally:
        out, err = server.communicate(b"stop\n", timeout=15)
        assert server.returncode == 0, err
        asked.append(out.count(b"asked\n"))


def ready_for_review(root, work, *, session="s", turn="t", db_path=None):
    """A managed assignment whose Stop reads the receipt store: the one its intent records as
    dbPath when db_path is given, otherwise the one a fallback resolves."""
    from codex_session_relay import marker

    work.mkdir(exist_ok=True)
    directory = marker.assignment_dir(root, work, marker.assignment_id("dispatch"))
    intent = {"dispatchRequestIdHash": marker.assignment_id("dispatch")}
    if db_path is not None:
        intent["dbPath"] = str(db_path)
    for name, record in {
        "intent.json": intent,
        "bound.json": {"sessionId": session},
        "relationship.json": {"relationshipId": "r"},
        f"claims/{session}/claim.json": {"sessionId": session, "dispatchRequestId": "dispatch"},
        f"dispositions/{session}/{turn}.json": {"sessionId": session, "turnId": turn,
                                                "outcome": "ready_for_review"},
    }.items():
        marker.publish(directory / name, record, root=root)
    return {"cwd": str(work), "session_id": session, "turn_id": turn, "stop_hook_active": False}


@pytest.mark.parametrize("case", ["override", "ambiguous"])
def test_a_routed_stop_is_refused_by_the_owner_as_the_owners_fallback_refuses_it(case):
    """Devin 4127894191: the Python client forwards socketPath and program (decision 24).

    The hook's own environment sees no ambiguity for this socket, so its local preflight lets
    the Stop through and routes it to the owner of the store it selected. The owner's
    configuration does see two stores recording the socket; given the socket and program the
    Go hook client also sends, it refuses exactly as the CLI's local fallback refuses under that
    configuration: override pinned by CODEX_SESSION_RELAY_STATE, or the owner's own directory
    standing in as --state when its discovery is ambiguous. Without them it judged the Stop.
    """
    now = "2026-01-01T00:00:00.123456+00:00"
    with tempfile.TemporaryDirectory(prefix="route-") as temporary:
        home = Path(temporary)
        wanted, state, root = home / "app.sock", home / "state", home / "markers"
        base = dict(os.environ, HOME=str(home), CODEX_HOME=str(home))
        base.pop("CODEX_SESSION_RELAY_STATE", None)
        owner = dict(base, XDG_STATE_HOME=str(home / "xdg"),
                     CODEX_SESSION_RELAY_MARKER_ROOT=str(root))
        if case == "override":
            owner["CODEX_SESSION_RELAY_STATE"] = str(state)
        hook = dict(base, XDG_STATE_HOME=str(home / "hook-xdg"),
                    CODEX_SESSION_RELAY_STATE=str(state))
        for name in ("a", "b"):
            Store(home / "xdg/codex-session-relay" / name / "relay.sqlite3",
                  socket_path=str(wanted)).close()
        Store(state / "relay.sqlite3", socket_path=str(wanted)).close()
        stop = ready_for_review(root, home / "work")
        program = str(Path(sys.executable).parent / "codex-session-relay")
        argv = [program, "--socket", str(wanted), "guard-evaluate", "--marker-root", str(root),
                "--mode", "hold", "--now", now]

        def run(arguments, environment):
            return subprocess.run(arguments, input=json.dumps(stop).encode(), env=environment,
                                  cwd=home, capture_output=True, timeout=30, check=False)

        # The owner's answer: its own configuration, locally, with its store as the owner
        # resolves it (the pin it runs under, or its own directory as --state).
        expected = run(argv if case == "override" else [program, "--state", str(state), *argv[1:]],
                       owner)
        assert expected.returncode == 2 and not expected.stderr, expected
        refusal = json.loads(expected.stdout)
        assert refusal["reason"] == "ambiguous_state_directory" and "overriddenBy" in refusal
        # This Stop's own configuration refuses nothing: judged locally it is not a refusal.
        judged = run([*argv[:-4], "--now", now, "--no-record"], hook)
        assert judged.returncode == 0 and "decision" in json.loads(judged.stdout), judged
        with python_owner(state, owner) as asked:
            routed = run(argv, hook)
        assert asked == [1], "the Stop was not routed to the owner"
        assert routed.returncode == 2 and not routed.stderr, routed
        assert json.loads(routed.stdout) == refusal
        assert not list(root.glob("*/*/hook/*/*/*.json")), "a refusal recorded an observation"


def test_the_owner_writes_only_under_its_own_marker_root_and_reads_only_its_own_store():
    """Devin 4127894432: a control request cannot choose where the owner writes.

    A same-user client that names another markerRoot with a Stop that records an observation,
    or another dbPath whose read would create SQLite sidecars beside that store, gets a host
    error and nothing is written. The owner's own root and store, under any spelling that is
    the same file, are evaluated with the owner's own paths (control.py owner_paths; Go's
    HandleControl answers the same, Test33OwnerEvaluatesOnlyItsOwnLocations).
    """
    now = "2026-01-01T00:00:00+00:00"
    with tempfile.TemporaryDirectory(prefix="own-") as temporary:
        home = Path(temporary)
        own, foreign, work = home / "markers", home / "elsewhere", home / "work"
        stop = ready_for_review(own, work)
        ready_for_review(foreign, work)
        state, other = home / "state", home / "other"
        Store(state / "relay.sqlite3").close()
        Store(other / "relay.sqlite3").close()
        (home / "link").symlink_to(state, target_is_directory=True)
        environment = dict(os.environ, CODEX_SESSION_RELAY_MARKER_ROOT=str(own))

        def ask_owner(root, db):
            params = {"markerRoot": root, "stopInput": stop, "mode": "observe", "dbPath": db,
                      "now": now, "noRecord": False, "deadline": "2999-01-01T00:00:00Z"}
            return ask(state / "control.sock", json.dumps(
                {"protocol": 1, "method": "guard-evaluate", "params": params}).encode() + b"\n")

        untouched = {path: files(path) for path in (own, foreign, other)}
        own_db = str(state / "relay.sqlite3")
        with python_owner(state, environment) as asked:
            refused = [ask_owner(str(foreign), own_db), ask_owner("markers", own_db),
                       ask_owner(None, own_db), ask_owner(str(own), str(other / "relay.sqlite3")),
                       ask_owner(str(own), "state/relay.sqlite3")]
            assert {path: files(path) for path in (own, foreign, other)} == untouched
            accepted = [ask_owner(str(own), own_db),
                        ask_owner(str(own) + "/", str(home / "link" / "relay.sqlite3"))]
        assert asked == [7]
        root_detail = ("control.sock evaluates Stops only under this owner's marker root "
                       + str(own) + "; the request named ")
        store_detail = "control.sock reads only this owner's store " + own_db + "; the request named "
        assert refused == [
            {"error": "host", "detail": root_detail + str(foreign)},
            {"error": "host", "detail": root_detail + "markers"},
            {"error": "host", "detail": root_detail + "no path"},
            {"error": "host", "detail": store_detail + str(other / "relay.sqlite3")},
            {"error": "host", "detail": store_detail + "state/relay.sqlite3"},
        ]
        assert [answer["state"] for answer in accepted] == ["receipt_missing"] * 2, accepted
        assert [answer["recordedAs"] for answer in accepted] == ["hook/s/t/0", "hook/s/t/1"]
        assert files(foreign) == untouched[foreign] and files(other) == untouched[other]
        assert len(list(own.glob("*/*/hook/s/t/*.json"))) == 2


# The relay CLI in a fresh interpreter that reports, on an inherited descriptor, every SQLite
# connection it opens and the two functions that opened it: the bounded reads a Stop spends in
# this process, counted without timing anything.
_CONNECTING_CLI = r'''
import os, sys
log = os.fdopen(int(os.environ.pop("CRW_TEST_AUDIT_FD")), "w", buffering=1)
def audit(event, args):
    if event == "sqlite3.connect":
        caller = sys._getframe(1)
        log.write(caller.f_code.co_name + " " + caller.f_back.f_code.co_name + "\n")
sys.addaudithook(audit)
from codex_session_relay.cli import main
sys.exit(main(sys.argv[1:]))
'''


def connecting_guard_evaluate(argv, stop, environment, cwd):
    """One real guard-evaluate process: (exit code, answer, SQLite opens, seconds it took)."""
    import time

    read, write = os.pipe()
    try:
        started = time.monotonic()
        done = subprocess.run(
            [sys.executable, "-c", _CONNECTING_CLI, *argv], input=json.dumps(stop).encode(),
            capture_output=True, timeout=60, check=False, cwd=cwd, pass_fds=(write,),
            env=dict(environment, CRW_TEST_AUDIT_FD=str(write)))
        elapsed = time.monotonic() - started
        os.close(write)
        write = None
        with os.fdopen(read, "r") as stream:
            read = None
            opened = [tuple(entry.split()) for entry in stream.read().splitlines()]
    finally:
        for descriptor in (read, write):
            if descriptor is not None:
                os.close(descriptor)
    assert not done.stderr, done
    return done.returncode, json.loads(done.stdout), opened, elapsed


@contextlib.contextmanager
def held_exclusively(path):
    """The slow receipt read, injected at the store: a writer in exclusive locking mode.

    Every other connection's read of path then waits its whole bounded busy timeout
    (intent.SQLITE_TIMEOUT) and fails, so each bounded read a Stop makes costs exactly that.
    """
    writer = sqlite3.connect(path, isolation_level=None)
    try:
        writer.execute("PRAGMA locking_mode=EXCLUSIVE")
        writer.execute("BEGIN IMMEDIATE")
        writer.execute("INSERT OR REPLACE INTO schema_meta VALUES ('test:held', '1')")
        writer.execute("COMMIT")
        yield
    finally:
        writer.close()


RECEIPT_READ = ("read_only_connection", "lookup_receipt")
SELECTION_READ = ("store_socket", "_selection_refusal")


@pytest.mark.parametrize("case", ["intent", "fallback", "local"])
def test_a_stop_spends_one_receipt_read_and_one_evaluation(case):
    """Devin 4128457287: a routed Stop reads its receipt and is evaluated once, at the owner.

    guard-evaluate used to evaluate the Stop read-only before routing it, only for the selection
    refusal that evaluation might raise: one bounded SQLite read of the receipt store plus
    deliverable verification, which the owner then repeated. Held exclusively, every read of the
    store costs its whole bound, so the Stop's time counts its reads:

    - intent (the coordinator recorded dbPath): the owner's receipt read alone, under one bound
      where it was two; this process opens no store at all.
    - fallback (the run's own selection): this process keeps the one selection read its local
      refusals need (decision 24: both sides' selection applies), and no receipt read; the owner
      makes its own selection read and the receipt read. Three bounds where there were four.
    - local (no owner listening, store owned by Python, not held): one selection read, the owner
      check and one receipt read. The selection is resolved once and reused by the evaluation,
      where it was resolved and read twice, and the receipt was read twice.
    """
    from codex_session_relay.intent import SQLITE_TIMEOUT

    now = "2026-01-01T00:00:00.123456+00:00"
    with tempfile.TemporaryDirectory(prefix="once-") as temporary:
        home = Path(temporary)
        wanted, state, root = home / "app.sock", home / "state", home / "markers"
        base = dict(os.environ, HOME=str(home), CODEX_HOME=str(home),
                    XDG_STATE_HOME=str(home / "xdg"), CODEX_SESSION_RELAY_MARKER_ROOT=str(root))
        base.pop("CODEX_SESSION_RELAY_STATE", None)
        Store(state / "relay.sqlite3", socket_path=str(wanted)).close()
        pinned = case == "intent"
        stop = ready_for_review(root, home / "work",
                                db_path=state / "relay.sqlite3" if pinned else None)
        environment = base if pinned else dict(base, CODEX_SESSION_RELAY_STATE=str(state))
        argv = ["--socket", str(wanted), "guard-evaluate", "--marker-root", str(root),
                "--now", now]
        if case == "local":
            code, answer, opened, _elapsed = connecting_guard_evaluate(argv, stop, environment, home)
            assert code == 0 and answer["state"] == "receipt_missing", answer
            assert opened == [SELECTION_READ, ("stop_metadata", "check_stop"), RECEIPT_READ], opened
            return
        with python_owner(state, environment) as asked, held_exclusively(state / "relay.sqlite3"):
            code, answer, opened, elapsed = connecting_guard_evaluate(argv, stop, environment, home)
        assert asked == [1], "the Stop was not routed to the owner"
        # The owner's one receipt read waited out its bound: an answer, not a released turn.
        assert code == 0 and answer["state"] == "state_unreadable", answer
        assert answer["reason"] == "Cannot read receipts.", answer
        assert RECEIPT_READ not in opened, opened
        assert opened == ([] if pinned else [SELECTION_READ]), opened
        # Reads the owner and this process made, each costing the whole bound: one (intent) or
        # three (fallback). One more is the second receipt read this fix removed.
        reads = 1 if pinned else 3
        assert reads * SQLITE_TIMEOUT <= elapsed < (reads + 1) * SQLITE_TIMEOUT, elapsed
