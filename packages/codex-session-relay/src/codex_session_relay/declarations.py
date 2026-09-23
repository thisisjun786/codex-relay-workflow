"""What a child's own relay records in the relay store beside the marker facts it writes.

The Stop hook and CRW-180's reader read the marker, and keep reading it. The relay daemon reads
no marker file (hook-contract.md), so a turn that ended without a report is invisible to it
unless the declarations a child DID make are in the store as well: a turn whose outcome was
declared looks, from the store alone, exactly like one that declared nothing. Two records close
that, each written by the command that writes the marker fact and AFTER it, mirroring the fact
the marker then stands on rather than the caller's arguments - so a create-once conflict in the
marker cannot leave the two disagreeing.

- intent-claim records that this session's relay writes its declarations into this store. It is
  the cut-over omitted.derive relies on: a turn whose session carries no such record is a legacy
  admission, and nothing about it is derived from the store.
- intent-disposition records the turn's declared outcome.

Neither record replaces or gates the marker write. A store that cannot be written is reported
in the command's answer and its exit status, never dropped; a coordinator that recorded no store
at all is reported too, and is not a failure, because there is then no store to derive from.
"""

import sqlite3
from pathlib import Path

from .store import Store

CAPABILITY = "declarations/1"

RECORDED = "recorded"
UNCHANGED = "unchanged"
CONFLICT = "conflict"
NOT_RECORDED = "not_recorded"
FAILED = "failed"


def store_of(facts) -> str | None:
    """The store the coordinator recorded for this assignment, or None."""
    path = (facts.get("intent") or {}).get("dbPath") if isinstance(facts, dict) else None
    return path if isinstance(path, str) and path.strip() else None


def not_recorded(reason, detail, db_path=None) -> dict:
    return {"recorded": False, "state": NOT_RECORDED, "reason": reason, "store": db_path,
            "detail": detail}


def failure(reason, detail, db_path) -> dict:
    return {"recorded": False, "state": FAILED, "reason": reason, "store": db_path,
            "detail": detail + ". The marker fact stands; running the same command again"
                               " retries this record and changes nothing else"}


def _write(db_path, select, insert, same):
    """Insert once, or compare with what stands. Returns the record for the command's answer."""
    if db_path is None:
        return not_recorded(
            "no_store_recorded",
            "the assignment's intent names no relay store, so there is none to record this in;"
            " the relay derives nothing from a store for this assignment")
    path = Path(db_path).expanduser()
    if not path.is_file():
        return not_recorded(
            "store_absent",
            "the relay store the intent names does not exist, so nothing can be derived from"
            " it either; nothing was created", str(path))
    try:
        store = Store(path)
    except (OSError, sqlite3.Error) as error:
        return failure("store_unopenable", type(error).__name__ + ": " + str(error), str(path))
    try:
        with store.transaction() as db:
            existing = db.execute(*select).fetchone()
            if existing is None:
                db.execute(*insert)
                return {"recorded": True, "state": RECORDED, "reason": None,
                        "store": str(path), "detail": None}
        if same(existing):
            return {"recorded": False, "state": UNCHANGED, "reason": None, "store": str(path),
                    "detail": "already recorded, identically"}
        return {"recorded": False, "state": CONFLICT, "reason": "store_disagrees",
                "store": str(path),
                "detail": "this store already holds a different record for it, and the first"
                          " one stands, as it does in the marker: " + repr(dict(existing))}
    except (OSError, sqlite3.Error) as error:
        return failure("store_write_failed", type(error).__name__ + ": " + str(error),
                        str(path))
    finally:
        store.close()


def record_claim(db_path, *, assignment, session_id, dispatch_request_id, marker_root,
                 workspace, at) -> dict:
    """Record that this session's relay writes its declarations into this store."""
    return _write(
        db_path,
        ("SELECT dispatch_request_id, marker_root, workspace, capability FROM"
         " reporting_sessions WHERE assignment_id = ? AND session_id = ?",
         (assignment, session_id)),
        ("INSERT INTO reporting_sessions (assignment_id, session_id, dispatch_request_id,"
         " marker_root, workspace, capability, recorded_at) VALUES (?,?,?,?,?,?,?)",
         (assignment, session_id, dispatch_request_id, str(marker_root), str(workspace),
          CAPABILITY, at)),
        lambda row: (row["dispatch_request_id"], row["marker_root"], row["workspace"],
                     row["capability"]) == (dispatch_request_id, str(marker_root),
                                            str(workspace), CAPABILITY),
    )


def record_disposition(db_path, *, assignment, session_id, turn_id, outcome, declared_at,
                       at) -> dict:
    """Record the outcome this turn declared, create-once like the marker file it mirrors."""
    return _write(
        db_path,
        ("SELECT outcome FROM turn_declarations WHERE assignment_id = ? AND session_id = ?"
         " AND turn_id = ?", (assignment, session_id, turn_id)),
        ("INSERT INTO turn_declarations (assignment_id, session_id, turn_id, outcome,"
         " declared_at, recorded_at) VALUES (?,?,?,?,?,?)",
         (assignment, session_id, turn_id, outcome, declared_at, at)),
        lambda row: row["outcome"] == outcome,
    )
