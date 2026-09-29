"""Durable request deduplication, including interrupted and partial operations."""

import hashlib
import json
import os
import sqlite3
import time
from pathlib import Path

# The one status a retained receipt can be retried from. It says this request began nothing, so
# repeating it cannot repeat an effect. Every other status keeps the conservative contract, and
# in_progress_or_unknown keeps it for a reason worth stating: what a request began is recorded in
# memory and dies with the process, so a row left behind by a crash cannot say which side of the
# send it stopped on.
RETRYABLE_STATUSES = frozenset({"not_attempted"})

# How many earlier attempts a re-armed receipt keeps: enough to diagnose a flapping socket,
# bounded so a caller that retries all day cannot grow one row without limit.
PRIOR_ATTEMPTS_KEPT = 5


# What makes a directory the relay's state: its store, its ownership record, or one of its fence
# locks. A write-gate.lock without relay.sqlite3 is a store being created or an initialization
# that failed, and takeover.lock is created by a transfer controller; the relay admits neither as
# unfenced, so neither may the bridge. The first three are the names the relay's own store_absent
# check reads (cli.py, Go ownership.CheckStart).
RELAY_STATE_NAMES = ("relay.sqlite3", "takeover.json", "write-gate.lock", "takeover.lock")


class RelayFenceUnavailable(RuntimeError):
    """A ledger in a relay's state directory, and no relay package here to fence it.

    Raised before anything is created or opened: writing the ledger without the relay owner's
    admission is exactly what the fence exists to prevent.
    """


def _relay_state(directory: Path):
    """The first relay-state name present in directory, or None for a standalone bridge ledger.

    Only a name that is certainly absent is absent, as the relay's store_absent check reads it: a
    dangling link or an entry that cannot be examined still marks the directory as the relay's.
    """
    for name in RELAY_STATE_NAMES:
        try:
            os.lstat(directory / name)
        except (FileNotFoundError, NotADirectoryError):
            continue
        except OSError:
            return name
        return name
    return None


def _relay_admission():
    """The relay's ownership admission, imported only for a ledger inside a relay's state."""
    try:
        from codex_session_relay.ownership import Admission
    except ImportError as error:
        return None, error
    return Admission, None


class Ledger:
    def __init__(self, path: Path, *, candidate=None):
        self._admission = None
        relay = path.parent / "relay.sqlite3"
        marker = _relay_state(path.parent)
        if marker is not None:
            # A relay-pinned transport ledger shares its owner's admission for
            # the whole external operation. Standalone bridge ledgers have no
            # relay schema, ownership record or fence lock and retain their
            # existing format. A directory holding only a fence lock goes through
            # the same admission, which waits for a store being created and
            # refuses one that is partial, as the relay's own writers do.
            # A designated takeover candidate passes its explicit permit, so its
            # recovery may open the ledger during phase=starting (decision 28).
            # A standalone bridge installation has no relay package to take that
            # admission with, so it refuses rather than write the ledger unfenced.
            admission, missing = _relay_admission()
            if admission is None:
                raise RelayFenceUnavailable(
                    f"the ledger {path} is in the relay state directory {path.parent} (it holds "
                    f"{marker}), so only the "
                    "relay owner's admission may write it, and the codex-session-relay package "
                    f"that takes that admission is not importable here ({missing}); the ledger "
                    "was not opened. Run the bridge with codex-session-relay installed beside "
                    "it, or give it a --state-dir that is not a relay state directory."
                ) from missing
            self._admission = admission(relay, candidate=candidate)
        try:
            self._open(path)
        except BaseException:
            if self._admission is not None:
                self._admission.close()
            raise

    def _open(self, path: Path):
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_CREAT | os.O_RDWR, 0o600)
        os.close(descriptor)
        self.db = sqlite3.connect(path, timeout=10)
        self.db.execute("""
            CREATE TABLE IF NOT EXISTS operations (
                request_id TEXT PRIMARY KEY,
                fingerprint TEXT NOT NULL,
                receipt TEXT NOT NULL
            )
        """)
        self.db.commit()

    @staticmethod
    def _fingerprint(request_id: str, method: str, params: dict):
        if not request_id or len(request_id) > 128:
            raise ValueError("request_id must contain 1–128 characters")
        return hashlib.sha256(
            json.dumps([method, params], sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()

    def lookup(self, request_id: str, method: str, params: dict, *, legacy_params=None):
        fingerprint = self._fingerprint(request_id, method, params)
        row = self.db.execute(
            "SELECT fingerprint, receipt FROM operations WHERE request_id = ?", (request_id,)
        ).fetchone()
        if row is None:
            return None
        receipt = json.loads(row[1])
        if row[0] != fingerprint:
            legacy_match = (
                receipt.get("fingerprintVersion", 1) == 1
                and legacy_params is not None
                and row[0] == self._fingerprint(request_id, method, legacy_params())
            )
            if not legacy_match:
                raise ValueError(
                    "request_id already belongs to different arguments; no action taken"
                )
        return receipt

    def begin(self, request_id: str, method: str, params: dict, *, legacy_params=None):
        fingerprint = self._fingerprint(request_id, method, params)
        receipt = {
            "requestId": request_id,
            "operation": method,
            "status": "in_progress_or_unknown",
            "startedAt": time.time(),
            "retrySafe": False,
            "fingerprintVersion": 2,
        }
        with self.db:
            inserted = self.db.execute(
                "INSERT OR IGNORE INTO operations VALUES (?, ?, ?)",
                (request_id, fingerprint, json.dumps(receipt)),
            ).rowcount
            existing = self.lookup(request_id, method, params, legacy_params=legacy_params)
            if inserted:
                return True, existing
            if existing.get("status") in RETRYABLE_STATUSES:
                return self._rearm(request_id, receipt, existing)
        return False, existing

    def _rearm(self, request_id: str, receipt: dict, previous: dict):
        """Re-open a request that began nothing, keeping what its earlier attempts reported.

        Two processes sharing a state directory cannot both re-arm one row, and the guard is the
        transaction rather than a comparison: the ignored INSERT above already took this
        transaction's write lock, so the second process waits, then reads the in-progress receipt
        this one wrote and does not re-arm. The fingerprint column is never rewritten, which
        leaves a legacy row's recovery path exactly as it was.
        """
        history = [
            *previous.get("priorAttempts", []),
            {
                "status": previous.get("status"),
                "error": previous.get("error"),
                "updatedAt": previous.get("updatedAt"),
            },
        ][-PRIOR_ATTEMPTS_KEPT:]
        rearmed = {**receipt, "attempt": previous.get("attempt", 1) + 1, "priorAttempts": history}
        self.db.execute(
            "UPDATE operations SET receipt = ? WHERE request_id = ?",
            (json.dumps(rearmed), request_id),
        )
        return True, rearmed

    def save(self, receipt: dict):
        receipt = {**receipt, "updatedAt": time.time()}
        with self.db:
            self.db.execute(
                "UPDATE operations SET receipt = ? WHERE request_id = ?",
                (json.dumps(receipt), receipt["requestId"]),
            )
        return receipt

    def get(self, request_id: str):
        row = self.db.execute(
            "SELECT receipt FROM operations WHERE request_id = ?", (request_id,)
        ).fetchone()
        if row is None:
            raise ValueError("Unknown request_id")
        return json.loads(row[0])

    def close(self):
        try:
            self.db.close()
        finally:
            if self._admission is not None:
                self._admission.close()

    def import_legacy(self, path: Path):
        """Copy an old alias ledger without losing or silently resolving conflicting receipts."""
        source = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)
        try:
            rows = source.execute(
                "SELECT request_id, fingerprint, receipt FROM operations"
            ).fetchall()
        finally:
            source.close()
        with self.db:
            for request_id, fingerprint, receipt in rows:
                existing = self.db.execute(
                    "SELECT fingerprint, receipt FROM operations WHERE request_id = ?",
                    (request_id,),
                ).fetchone()
                if existing is not None:
                    if existing[0] != fingerprint or json.loads(existing[1]) != json.loads(receipt):
                        raise ValueError(
                            f"Conflicting retained request {request_id!r} in legacy socket ledger "
                            f"{path}; no requests will be dispatched. Preserve both ledgers."
                        )
                else:
                    self.db.execute(
                        "INSERT INTO operations VALUES (?, ?, ?)",
                        (request_id, fingerprint, receipt),
                    )


def open_endpoint_ledger(socket_path: Path, state_dir: Path, *, candidate=None):
    """Use one ledger for a canonical endpoint, importing the supplied legacy alias if present."""
    supplied = socket_path.expanduser().absolute()
    canonical = supplied.resolve()
    state_dir = state_dir.expanduser().resolve()

    def filename(path):
        endpoint = hashlib.sha256(str(path).encode()).hexdigest()[:16]
        return state_dir / f"operations-{endpoint}.sqlite3"

    ledger = Ledger(filename(canonical), candidate=candidate)
    legacy = filename(supplied)
    try:
        if legacy != filename(canonical) and legacy.exists():
            ledger.import_legacy(legacy)
    except BaseException:
        ledger.close()
        raise
    return canonical, ledger
