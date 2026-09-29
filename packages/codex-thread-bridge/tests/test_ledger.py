import concurrent.futures
import fcntl
import hashlib
import os
import subprocess
import sys

import pytest

from codex_thread_bridge.ledger import Ledger, RelayFenceUnavailable, open_endpoint_ledger


def old_ledger_path(state, socket):
    digest = hashlib.sha256(str(socket.absolute()).encode()).hexdigest()[:16]
    return state / f"operations-{digest}.sqlite3"


def test_socket_aliases_share_ledger_and_retain_identity_after_restart(tmp_path):
    socket = tmp_path / "actual.sock"
    socket.touch()
    alias = tmp_path / "alias.sock"
    alias.symlink_to(socket)
    canonical, ledger = open_endpoint_ledger(alias, tmp_path / "state")
    assert canonical == socket
    _, receipt = ledger.begin("create", "create_thread", {"cwd": "/checkout"})
    ledger.save({**receipt, "threadId": "thread-1", "status": "accepted"})
    ledger.close()
    _, restarted = open_endpoint_ledger(socket, tmp_path / "state")
    try:
        fresh, receipt = restarted.begin("create", "create_thread", {"cwd": "/checkout"})
        assert not fresh and receipt["threadId"] == "thread-1"
        assert len(list((tmp_path / "state").glob("*.sqlite3"))) == 1
    finally:
        restarted.close()


def test_import_legacy_socket_alias_preserves_receipts(tmp_path):
    socket = tmp_path / "actual.sock"
    socket.touch()
    alias = tmp_path / "alias.sock"
    alias.symlink_to(socket)
    state = tmp_path / "state"
    legacy_path = old_ledger_path(state, alias)
    legacy = Ledger(legacy_path)
    _, receipt = legacy.begin("create", "create_thread", {"cwd": "/checkout"})
    legacy.save({**receipt, "threadId": "thread-1", "status": "accepted"})
    legacy.close()
    _, ledger = open_endpoint_ledger(alias, state)
    try:
        assert ledger.get("create")["threadId"] == "thread-1"
        assert legacy_path.exists()
    finally:
        ledger.close()
    # Repeated imports and a subsequent canonical-path startup are safe.
    for spelling in [alias, socket]:
        _, ledger = open_endpoint_ledger(spelling, state)
        try:
            assert not ledger.begin("create", "create_thread", {"cwd": "/checkout"})[0]
        finally:
            ledger.close()


def test_legacy_conflict_stops_and_rolls_back_import(tmp_path):
    socket = tmp_path / "actual.sock"
    socket.touch()
    alias = tmp_path / "alias.sock"
    alias.symlink_to(socket)
    state = tmp_path / "state"
    legacy = Ledger(old_ledger_path(state, alias))
    legacy.begin("only-in-alias", "create_thread", {})
    _, receipt = legacy.begin("same-id", "create_thread", {})
    legacy.save({**receipt, "threadId": "alias-thread"})
    legacy.close()
    _, canonical = open_endpoint_ledger(socket, state)
    _, receipt = canonical.begin("same-id", "create_thread", {})
    canonical.save({**receipt, "threadId": "canonical-thread"})
    canonical.close()
    with pytest.raises(ValueError, match="Conflicting retained request"):
        open_endpoint_ledger(alias, state)
    _, canonical = open_endpoint_ledger(socket, state)
    try:
        assert canonical.get("same-id")["threadId"] == "canonical-thread"
        with pytest.raises(ValueError, match="Unknown request_id"):
            canonical.get("only-in-alias")
    finally:
        canonical.close()


def test_only_an_unattempted_receipt_re_arms_the_same_request_id(tmp_path):
    """The ledger reopens a request that began nothing and keeps every other answer as it was."""
    ledger = Ledger(tmp_path / "state" / "operations.sqlite3")
    params = {"cwd": "/checkout"}
    try:
        fresh, receipt = ledger.begin("create", "create_thread", params)
        assert fresh and receipt["status"] == "in_progress_or_unknown"
        ledger.save({**receipt, "status": "not_attempted", "error": "TransportError: gone"})

        again, rearmed = ledger.begin("create", "create_thread", params)
        assert again and rearmed["status"] == "in_progress_or_unknown"
        assert rearmed["attempt"] == 2 and not rearmed["retrySafe"]
        assert [a["status"] for a in rearmed["priorAttempts"]] == ["not_attempted"]
        assert rearmed["priorAttempts"][0]["error"] == "TransportError: gone"

        # Anything that may have reached the host keeps its id, including the row a crash leaves:
        # what a request began is held in memory and does not survive the process that held it.
        for status in ("outcome_unknown", "failed", "accepted", "in_progress_or_unknown"):
            ledger.save({**rearmed, "status": status})
            blocked, retained = ledger.begin("create", "create_thread", params)
            assert not blocked and retained["status"] == status
    finally:
        ledger.close()


def test_a_re_armed_receipt_keeps_a_bounded_history(tmp_path):
    ledger = Ledger(tmp_path / "state" / "operations.sqlite3")
    params = {"cwd": "/checkout"}
    try:
        _, receipt = ledger.begin("create", "create_thread", params)
        for attempt in range(8):
            ledger.save({**receipt, "status": "not_attempted", "error": f"failure {attempt}"})
            _, receipt = ledger.begin("create", "create_thread", params)
        assert receipt["attempt"] == 9
        assert [a["error"] for a in receipt["priorAttempts"]] == [
            f"failure {n}" for n in range(3, 8)
        ]
    finally:
        ledger.close()


def test_two_ledgers_racing_the_same_row_re_arm_it_once(tmp_path):
    """Two processes must not both take a re-armed row, so measure it with two real connections.

    Nothing here compares receipts to decide the winner: the ignored INSERT inside begin already
    holds the write transaction, so the second connection waits and then reads what the first
    one wrote.
    """
    import threading

    path = tmp_path / "state" / "operations.sqlite3"
    params = {"cwd": "/checkout"}
    first = Ledger(path)
    _, receipt = first.begin("create", "create_thread", params)
    first.save({**receipt, "status": "not_attempted"})
    first.close()

    start = threading.Barrier(2)
    results = {}

    def attempt(name):
        ledger = Ledger(path)
        try:
            start.wait(timeout=5)
            results[name] = ledger.begin("create", "create_thread", params)
        finally:
            ledger.close()

    workers = [threading.Thread(target=attempt, args=(name,)) for name in ("a", "b")]
    for worker in workers:
        worker.start()
    for worker in workers:
        worker.join(timeout=10)

    assert sorted(fresh for fresh, _ in results.values()) == [False, True]
    loser = next(retained for fresh, retained in results.values() if not fresh)
    assert loser["status"] == "in_progress_or_unknown" and loser["attempt"] == 2


# A standalone bridge installation: codex-session-relay is not installed, so importing it finds
# nothing, exactly as the interpreter reports a missing package.
ABSENT_RELAY_FINDER = """
class AbsentRelay:
    def find_spec(self, name, path=None, target=None):
        if name == "codex_session_relay" or name.startswith("codex_session_relay."):
            raise ModuleNotFoundError(f"No module named {name!r}", name=name)
        return None
"""
# The finder installed for a whole fresh interpreter. In process, only the class is executed and
# monkeypatch installs it, so the relay is importable again for the tests that follow.
ABSENT_RELAY = "import sys\n" + ABSENT_RELAY_FINDER + "\n\nsys.meta_path.insert(0, AbsentRelay())\n"


def relay_state(tmp_path, owned_by):
    """A state directory the relay owns: its store, or only its ownership record."""
    state = tmp_path / "state"
    state.mkdir()
    (state / owned_by).write_bytes(b"")
    return state


@pytest.mark.parametrize(
    "owned_by", ["relay.sqlite3", "takeover.json", "write-gate.lock", "takeover.lock"])
def test_a_relay_state_directory_without_the_relay_package_fails_closed(
        tmp_path, monkeypatch, owned_by):
    """PR #185 thread 4128457357: a standalone bridge never writes a relay-fenced ledger.

    A ledger in a relay's state directory is written only under the relay owner's admission. A
    bridge installed without codex-session-relay cannot take it, so it refuses with a diagnostic
    naming the directory and the missing package, and creates nothing. A fence lock alone is
    relay state too (PR #185 thread 4128791903): write-gate.lock without the store is a store
    being created or a failed initialization, and takeover.lock is a transfer controller's.
    """
    namespace = {}
    exec(ABSENT_RELAY_FINDER, namespace)
    monkeypatch.setattr(sys, "meta_path", [namespace["AbsentRelay"](), *sys.meta_path])
    for name in [name for name in sys.modules if name.split(".")[0] == "codex_session_relay"]:
        monkeypatch.delitem(sys.modules, name)
    state = relay_state(tmp_path, owned_by)
    socket = tmp_path / "app.sock"
    socket.touch()
    with pytest.raises(RelayFenceUnavailable) as refused:
        open_endpoint_ledger(socket, state)
    detail = str(refused.value)
    assert f"relay state directory {state}" in detail and f"it holds {owned_by}" in detail
    assert "No module named 'codex_session_relay" in detail and "was not opened" in detail
    assert isinstance(refused.value.__cause__, ModuleNotFoundError)
    assert sorted(path.name for path in state.iterdir()) == [owned_by]


def test_the_standalone_bridge_server_stops_with_the_diagnostic_not_a_traceback(tmp_path):
    """The console entry point in a fresh interpreter without the relay package: exit 1, the
    diagnostic alone on stderr, nothing on stdout, and no ledger in the relay's directory."""
    state = relay_state(tmp_path, "relay.sqlite3")
    socket = tmp_path / "app.sock"
    socket.touch()
    script = ABSENT_RELAY + (
        "from codex_thread_bridge.server import main\n"
        "sys.argv[0] = 'codex-thread-bridge'\n"
        "main()\n"
    )
    environment = {key: value for key, value in os.environ.items()
                   if key not in ("CODEX_THREAD_BRIDGE_EXECUTION_POLICY",
                                  "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST")}
    done = subprocess.run(
        [sys.executable, "-c", script, "--socket", str(socket), "--state-dir", str(state)],
        stdin=subprocess.DEVNULL, capture_output=True, timeout=60, check=False, env=environment)
    assert done.returncode == 1, done
    assert done.stdout == b"", done
    stderr = done.stderr.decode()
    assert "Traceback" not in stderr, stderr
    assert stderr.startswith(f"the ledger {state}/operations-") and stderr.endswith(
        "or give it a --state-dir that is not a relay state directory.\n"), stderr
    assert sorted(path.name for path in state.iterdir()) == ["relay.sqlite3"]


@pytest.mark.parametrize("creation", ["completes", "fails"])
def test_a_directory_holding_only_the_relay_write_gate_is_decided_by_the_relay_admission(
        tmp_path, creation):
    """PR #185 thread 4128791903: write-gate.lock alone is relay state, not a standalone directory.

    A gate without relay.sqlite3 is a relay store being created, or one whose initialization
    failed. The ledger goes through the relay admission, which waits while the creator holds the
    gate EX, then admits the finished store (and holds its gate SH for the ledger's lifetime) or
    refuses the partial one. Nothing is written before it decides.
    """
    from codex_session_relay.ownership import OwnershipRefused
    from codex_session_relay.store import Store

    state = tmp_path / "state"
    state.mkdir()
    # A real relay store whose finished files are kept out of sight until the creator is done:
    # the same inodes return under the same names, so the store's recorded identity holds.
    Store(state / "relay.sqlite3").close()
    finished = [path.name for path in state.iterdir() if path.name != "write-gate.lock"]
    for name in finished:
        (state / name).rename(tmp_path / name)
    gate = os.open(state / "write-gate.lock", os.O_RDWR)
    fcntl.flock(gate, fcntl.LOCK_EX)  # the creator's hold, as ownership._create_gate places it
    socket = tmp_path / "app.sock"
    socket.touch()
    # One worker thread: the ledger's SQLite connection is used and closed where it was opened.
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
        opening = executor.submit(open_endpoint_ledger, socket, state)
        try:
            done, _ = concurrent.futures.wait([opening], timeout=0.5)
            assert not done, "the ledger did not wait for the store being created"
            assert sorted(path.name for path in state.iterdir()) == ["write-gate.lock"]
            if creation == "completes":
                for name in finished:
                    (tmp_path / name).rename(state / name)
        finally:
            os.close(gate)  # the creator is done: its EX goes with its descriptor
        if creation == "fails":
            with pytest.raises(OwnershipRefused):
                opening.result(timeout=30)
            assert sorted(path.name for path in state.iterdir()) == ["write-gate.lock"]
            return
        _, ledger = opening.result(timeout=30)
        probe = os.open(state / "write-gate.lock", os.O_RDWR)
        try:
            with pytest.raises(BlockingIOError):
                fcntl.flock(probe, fcntl.LOCK_EX | fcntl.LOCK_NB)  # the admission holds SH
            executor.submit(ledger.close).result(timeout=30)
            fcntl.flock(probe, fcntl.LOCK_EX | fcntl.LOCK_NB)
        finally:
            os.close(probe)
    assert len(list(state.glob("operations-*.sqlite3"))) == 1


def test_a_directory_holding_only_the_relay_transfer_lock_is_refused_by_the_relay_admission(
        tmp_path):
    """takeover.lock alone is relay state too: the admission, not the bridge, answers, and a
    directory without the write gate is a store it will not admit. Nothing is written."""
    from codex_session_relay.ownership import OwnershipRefused

    state = relay_state(tmp_path, "takeover.lock")
    socket = tmp_path / "app.sock"
    socket.touch()
    with pytest.raises(OwnershipRefused, match="no write admission gate"):
        open_endpoint_ledger(socket, state)
    assert sorted(path.name for path in state.iterdir()) == ["takeover.lock"]
