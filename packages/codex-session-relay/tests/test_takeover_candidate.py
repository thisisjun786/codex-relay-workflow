"""Decision 28: real inherited channels, fenced recovery, and durable activation."""

import json
import os
import socket
import stat
import sqlite3
import subprocess
import sys
import tempfile
from contextlib import contextmanager
from dataclasses import replace
from pathlib import Path

import pytest

from codex_session_relay import cli, inbox, ownership, takeover
from codex_session_relay.service import ServiceIntent
from codex_session_relay.store import Store


@pytest.fixture
def starting(monkeypatch):
    with tempfile.TemporaryDirectory(prefix="candidate-") as directory:
        path = Path(directory) / "relay.sqlite3"
        # The scope-registry authority the candidate runs under also keys the record's scopeKey.
        monkeypatch.setenv("CODEX_SESSION_RELAY_SCOPE_DIR", str(path.parent / "scope"))
        host = str(path.parent / "host.sock")
        store = Store(path, socket_path=host)
        store.close()
        with sqlite3.connect(path) as db:
            db.execute("UPDATE schema_meta SET value='3' WHERE key='owner_epoch'")
            db.execute("UPDATE schema_meta SET value='reverse-3' WHERE key='takeover_id'")
        record = ownership.mirror(path)
        record.update(epoch=3, phase="starting", controller=takeover.identity(os.getpid()),
                      transition={"id": "reverse-3", "from": "go", "to": "python", "targetEpoch": 3})
        publish(path, record)
        ServiceIntent(path.parent / "service.json").write(enabled=True, actor="test")
        yield path, record


def publish(path, record):
    (path.parent / "takeover.json").write_bytes(inbox.canonical(record))


def permit(record):
    controller = record["controller"]
    return ownership.CandidatePermit(record["transition"]["id"], record["epoch"],
                                     controller["bootId"], controller["pid"], controller["startTicks"])


LAUNCH = """
import os, sys
fd = int(sys.argv[1])
os.dup2(fd, 3, inheritable=True)
if fd != 3:
    os.close(fd)
os.execv(sys.executable, [sys.executable, '-m', 'codex_session_relay.cli', *sys.argv[2:]])
"""

# The real candidate entry point with two test-only injections: a short channel bound, and a
# recovery that outlasts it (the wait sits after recovery and the control-socket bind, where
# the candidate calls ready()).
SLOW_RECOVERY = """
import os, sys, time
fd = int(sys.argv[1])
os.dup2(fd, 3, inheritable=True)
if fd != 3:
    os.close(fd)
from codex_session_relay import takeover
takeover.CHANNEL_TIMEOUT = float(os.environ["CRW_TEST_CHANNEL_TIMEOUT"])
ready = takeover.CandidateChannel.ready
def slow(self):
    time.sleep(float(os.environ["CRW_TEST_RECOVERY_SECONDS"]))
    return ready(self)
takeover.CandidateChannel.ready = slow
from codex_session_relay.cli import main
sys.exit(main(sys.argv[2:]))
"""


@contextmanager
def candidate(path, *, channel_env="3", extra=(), environment=None, launch=LAUNCH):
    # exec preserves the direct-parent relation; remap only in the fresh process,
    # not preexec_fn in a multithreaded pytest parent.
    parent, child = socket.socketpair()
    environment = dict(os.environ, CRW_TAKEOVER_CHANNEL_FD=channel_env,
                       CODEX_SESSION_RELAY_SCOPE_DIR=str(path.parent / "scope"),
                       XDG_STATE_HOME=str(path.parent / "xdg"), **(environment or {}))
    argv = [sys.executable, "-c", launch, str(child.fileno()), "--state", str(path.parent),
            "--socket", str(path.parent / "host.sock"),
            "service", "run", "--takeover-candidate", "--allow-isolated-scope", "--max-segments", "0", *extra]
    process = subprocess.Popen(argv, env=environment, pass_fds=(child.fileno(),),
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    child.close()
    parent.settimeout(10)
    try:
        yield process, parent
    finally:
        parent.close()
        if process.poll() is None:
            process.kill()
        process.communicate(timeout=10)


def send(channel, message):
    channel.sendall(inbox.canonical(message) + b"\n")


def receive(channel):
    line = bytearray()
    while not line.endswith(b"\n"):
        byte = channel.recv(1)
        assert byte, "candidate exited before the protocol reply"
        line.extend(byte)
    return json.loads(line)


def not_started(path):
    record_path = path.parent / "daemon.json"
    if record_path.exists():
        record = json.loads(record_path.read_bytes())
        assert record.get("readyAt") is None
        assert record.get("workerPid") is None


def no_serving(path):
    assert not (path.parent / "control.sock").exists()
    not_started(path)


@pytest.mark.parametrize("field,value", [("kind", "ready"), ("phase", "active"),
                                        ("owner", "go"), ("controller", None),
                                        ("transition", None), ("epoch", True)])
def test_invalid_start_writes_nothing(starting, field, value):
    path, record = starting
    before = {p.name: p.read_bytes() for p in path.parent.iterdir() if p.is_file()}
    supplied = dict(record)
    if field == "kind":
        message = {"kind": value, "record": supplied}
    else:
        supplied[field] = value
        message = {"kind": "start", "record": supplied}
    with candidate(path) as (process, channel):
        send(channel, message)
        channel.shutdown(socket.SHUT_WR)
        stdout, _ = process.communicate(timeout=10)
        assert process.returncode != 0
        assert stdout == b""
        assert channel.recv(1) == b""
    assert {p.name: p.read_bytes() for p in path.parent.iterdir() if p.is_file()} == before


@pytest.mark.parametrize("field", ["pid", "bootId", "startTicks"])
def test_mismatched_controller_is_refused(starting, field):
    path, record = starting
    record["controller"][field] = "wrong-boot" if field == "bootId" else record["controller"][field] + 1
    publish(path, record)
    with candidate(path) as (process, channel):
        send(channel, {"kind": "start", "record": record})
        channel.shutdown(socket.SHUT_WR)
        stdout, _ = process.communicate(timeout=10)
        assert process.returncode != 0
        assert stdout == b""
        assert channel.recv(1) == b""
    no_serving(path)


def test_candidate_requires_private_fd_designation(starting):
    path, record = starting
    with candidate(path, channel_env="4") as (process, channel):
        send(channel, {"kind": "start", "record": record})
        channel.shutdown(socket.SHUT_WR)
        stdout, _ = process.communicate(timeout=10)
        assert process.returncode != 0
        assert stdout == b""
    assert not (path.parent / "daemon.json").exists()
    no_serving(path)


def test_start_read_deadline_is_absolute(starting, monkeypatch):
    path, _ = starting
    parent, child = socket.socketpair()
    try:
        # Time itself is under test: expire between deadline creation and first read.
        parent.shutdown(socket.SHUT_WR)
        clock = iter((100.0, 121.0))
        monkeypatch.setattr(takeover.time, "monotonic", lambda: next(clock))
        with pytest.raises(ownership.OwnershipRefused, match="deadline"):
            takeover.CandidateChannel(child, path)
    finally:
        parent.close()
        child.close()


def test_candidate_message_has_the_same_bounded_frame_in_both_directions(starting, monkeypatch):
    path, _ = starting
    monkeypatch.setattr(takeover, "MAX_CANDIDATE_BYTES", 32)
    parent, child = socket.socketpair()
    try:
        parent.sendall(b"x" * 33)
        with pytest.raises(ownership.OwnershipRefused, match="size limit"):
            takeover.CandidateChannel(child, path)
    finally:
        parent.close()
        child.close()

    left, right = socket.socketpair()
    try:
        channel = object.__new__(takeover.CandidateChannel)
        channel.connection = left
        with pytest.raises(ownership.OwnershipRefused, match="size limit"):
            channel.send({"kind": "x", "payload": "y" * 64})
        right.setblocking(False)
        with pytest.raises(BlockingIOError):
            right.recv(1)
    finally:
        left.close()
        right.close()


def test_starting_ordinary_writer_refused_and_ingress_queued(starting, capsys):
    path, _ = starting
    with pytest.raises(ownership.OwnershipRefused):
        Store(path)
    with pytest.raises(ownership.OwnershipRefused):
        ownership.check_start(path)
    assert cli.main(["--state", str(path.parent), "ack", "--event", "event-1",
                     "--ack-turn", "turn-1", "--ack-proof", "proof-1"]) == 0
    assert json.loads(capsys.readouterr().out)["status"] == "durably_queued"


@pytest.mark.parametrize("change", ["epoch", "transition", "controller"])
def test_stale_permit_refused_on_first_admission(starting, change):
    path, record = starting
    original = permit(record)
    wrong = {"epoch": {"epoch": 2}, "transition": {"transition_id": "previous"},
             "controller": {"start_ticks": original.start_ticks + 1}}[change]
    with pytest.raises(ownership.OwnershipRefused):
        Store(path, candidate=replace(original, **wrong))


@pytest.mark.parametrize("change", ["epoch", "transition", "controller"])
def test_stale_permit_refused_on_transaction_revalidation(starting, change):
    path, record = starting
    store = Store(path, candidate=permit(record))
    try:
        with store.transaction() as db:
            db.execute("INSERT INTO journal(at,kind,subject,detail) VALUES ('t','recovery','s','d')")
        if change == "epoch":
            store.db.execute("UPDATE schema_meta SET value='4' WHERE key='owner_epoch'")
            record["epoch"] = 4
        elif change == "transition":
            store.db.execute("UPDATE schema_meta SET value='next' WHERE key='takeover_id'")
            record["transition"]["id"] = "next"
        else:
            record["controller"]["startTicks"] += 1
        publish(path, record)
        with pytest.raises(ownership.OwnershipRefused), store.transaction():
            pytest.fail("stale permit admitted a second transaction")
        assert store.one("SELECT count(*) FROM journal WHERE kind='recovery'")[0] == 1
    finally:
        store.close()


@pytest.mark.parametrize("layout", ["xdg", "pinned"])
@pytest.mark.parametrize("activation", ["active", "eof-active", "eof-starting"])
def test_candidate_recovery_and_durable_activation(starting, activation, layout):
    path, record = starting
    parser = cli.build_parser()
    request = inbox.envelope(parser, parser.parse_args([
        "ack", "--event", "a" * 32, "--ack-turn", "turn", "--ack-proof", "proof"]))
    inbox.enqueue(path.parent, request)
    # The production layout: Go launches the candidate with CODEX_SESSION_RELAY_STATE=S, so
    # the relay-pinned transport ledger sits beside relay.sqlite3 and needs the permit too.
    environment = {"CODEX_SESSION_RELAY_STATE": str(path.parent)} if layout == "pinned" else None
    with candidate(path, environment=environment) as (process, channel):
        send(channel, {"kind": "start", "record": record})
        ready = receive(channel)
        assert ready == {"kind": "ready", "identity": takeover.identity(process.pid, build=ownership.BUILD),
                         "storeId": record["storeId"], "epoch": 3, "transitionId": "reverse-3"}
        # Recovery and the control socket come before readiness; scheduled work does not.
        assert stat.S_ISSOCK((path.parent / "control.sock").lstat().st_mode)
        not_started(path)
        if layout == "pinned":
            assert list(path.parent.glob("operations-*.sqlite3"))
        with sqlite3.connect(path) as db:
            replayed = db.execute("SELECT value FROM schema_meta WHERE key=?",
                                  ("inbox:" + request["operationId"],)).fetchone()
        assert replayed is not None
        assert json.loads(replayed[0])["exit"] == 2
        assert not (path.parent / "takeover-inbox" / request["operationId"]).exists()
        if activation != "eof-starting":
            record.update(phase="active", holder=ready["identity"], controller=None)
            publish(path, record)
        if activation == "active":
            send(channel, {"kind": "active", "record": record})
            assert receive(channel) == {"kind": "activated"}
        else:
            channel.shutdown(socket.SHUT_WR)
        stdout, stderr = process.communicate(timeout=10)
        if activation == "eof-starting":
            assert process.returncode != 0, (stdout, stderr)
            assert stdout == b""
            no_serving(path)
        else:
            assert process.returncode == 0, (stdout, stderr)
            assert json.loads(stdout)["ok"] is True
            assert json.loads((path.parent / "daemon.json").read_bytes())["readyAt"]
        # Cleanup releases the lifetime gate even on rejected activation.
        import fcntl
        with (path.parent / "write-gate.lock").open("r+") as gate:
            fcntl.flock(gate, fcntl.LOCK_EX | fcntl.LOCK_NB)


def test_recovery_longer_than_the_channel_bound_still_activates(starting):
    """Devin 4127894020: the start bound does not carry over into the activation exchange.

    Recovery is bounded by the controller's --ready-timeout (Go's default 600 s); after ready
    the candidate waits for active under a fresh channel bound, as Go's candidate and the Go
    controller's Activate do. Scaled down: a 1 s bound and a 2 s recovery.
    """
    path, record = starting
    slow = {"CRW_TEST_CHANNEL_TIMEOUT": "1", "CRW_TEST_RECOVERY_SECONDS": "2"}
    with candidate(path, environment=slow, launch=SLOW_RECOVERY) as (process, channel):
        send(channel, {"kind": "start", "record": record})
        ready = receive(channel)
        assert ready["kind"] == "ready", ready
        record.update(phase="active", holder=ready["identity"], controller=None)
        publish(path, record)
        send(channel, {"kind": "active", "record": record})
        assert receive(channel) == {"kind": "activated"}
        stdout, stderr = process.communicate(timeout=10)
        assert process.returncode == 0, (stdout, stderr)
        assert json.loads(stdout)["ok"] is True
    assert json.loads((path.parent / "daemon.json").read_bytes())["readyAt"]


@pytest.mark.parametrize("mismatch", ["holder", "epoch", "takeover", "stamp", "answer"])
def test_active_message_cannot_replace_durable_agreement(starting, mismatch):
    path, record = starting
    with candidate(path) as (process, channel):
        send(channel, {"kind": "start", "record": record})
        ready = receive(channel)
        record.update(phase="active", holder=ready["identity"], controller=None)
        if mismatch == "holder":
            record["holder"]["pid"] += 1
        elif mismatch == "epoch":
            record["epoch"] += 1
        elif mismatch == "takeover":
            record["transition"]["id"] = "another"
        elif mismatch == "stamp":
            with sqlite3.connect(path) as db:
                db.execute("UPDATE schema_meta SET value='another' WHERE key='takeover_id'")
        publish(path, record)
        send(channel, {"kind": "wrong" if mismatch == "answer" else "active", "record": record})
        stdout, _ = process.communicate(timeout=10)
        assert process.returncode != 0
        assert stdout == b""
    no_serving(path)


def test_control_socket_bind_failure_never_reports_ready(starting):
    path, record = starting
    (path.parent / "control.sock").write_bytes(b"not a socket")
    with candidate(path) as (process, channel):
        send(channel, {"kind": "start", "record": record})
        stdout, _ = process.communicate(timeout=10)
        assert process.returncode != 0
        assert stdout == b""
        assert channel.recv(1) == b""
    assert ownership.mirror(path)["phase"] == "starting"
    assert (path.parent / "control.sock").read_bytes() == b"not a socket"
    not_started(path)


def test_failed_recovery_never_reports_ready(starting):
    path, record = starting
    queued = path.parent / "takeover-inbox"
    queued.mkdir()
    (queued / "broken").write_text("not json", encoding="utf-8")
    with candidate(path) as (process, channel):
        send(channel, {"kind": "start", "record": record})
        stdout, _ = process.communicate(timeout=10)
        assert process.returncode != 0
        assert stdout == b""
        assert channel.recv(1) == b""
    no_serving(path)


def test_worker_inherits_locks_but_not_controller_channel(starting, monkeypatch):
    from codex_session_relay.service import RelayService
    from codex_session_relay.store import resolve_state_dir

    path, _ = starting
    service = RelayService(resolve_state_dir(str(path.parent), None))
    controller, channel = socket.socketpair()
    launcher = subprocess.Popen
    try:
        # The production spawn method selects the fds; this child observes the
        # actual exec result instead of asserting a mocked Popen call.
        channel.set_inheritable(False)
        channel_inode = os.fstat(channel.fileno()).st_ino

        def inspect_worker(argv, **kwargs):
            program = """
import json, os, sys
fds = []
for name in os.listdir('/proc/self/fd'):
    try:
        fds.append(os.fstat(int(name)).st_ino)
    except OSError:
        continue
print(json.dumps({'channel': int(sys.argv[1]) in fds, 'designated':
                  'CRW_TAKEOVER_CHANNEL_FD' in os.environ, 'locks':
                  all(int(value) in fds for value in sys.argv[2:])}), flush=True)
"""
            return launcher([sys.executable, "-c", program, str(channel_inode),
                             *[str(os.fstat(fd).st_ino) for fd in kwargs["pass_fds"]]], **kwargs)

        monkeypatch.delenv(takeover.CHANNEL_ENV, raising=False)
        monkeypatch.setattr(subprocess, "Popen", inspect_worker)
        with (path.parent / "daemon.lock").open("w+") as lock:
            process = service.spawn_worker(lock_fd=lock.fileno(), scope_fd=None, token="run",
                                           segment_seconds=1, allow_isolated=True)
            assert process.wait(timeout=10) == 0
        observed = json.loads((path.parent / "daemon.log").read_text())
        assert observed == {"channel": False, "designated": False, "locks": True}
    finally:
        controller.close()
        channel.close()
