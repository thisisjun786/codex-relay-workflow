import hashlib

import pytest

from codex_thread_bridge.ledger import Ledger, open_endpoint_ledger


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
